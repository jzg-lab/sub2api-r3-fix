package repository

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	entmigrate "github.com/Wei-Shaw/sub2api/ent/migrate"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// This is opt-in acceptance against a complete restore in a network-isolated
// disposable cluster, never a production DSN.
func TestMigrationProductionDataCloneUpgrade(t *testing.T) {
	if os.Getenv("SUB2API_MIGRATION_DATA_CLONE") != "1" {
		t.Skip("requires a complete backup restored to an isolated disposable cluster")
	}
	socket := os.Getenv("SUB2API_PROBE_TEST_SOCKET")
	require.True(t, filepath.IsAbs(socket))
	require.Equal(t, "probe-pg-sock", filepath.Base(socket))
	require.NotContains(t, socket, "'")
	require.NotContains(t, socket, "\\")
	port, err := strconv.Atoi(os.Getenv("SUB2API_PROBE_TEST_PORT"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, port, 1024)
	require.LessOrEqual(t, port, 65535)
	db, err := sql.Open("postgres", fmt.Sprintf(
		"host='%s' port=%d dbname=production_data_clone sslmode=disable connect_timeout=3", socket, port))
	require.NoError(t, err)
	defer db.Close()
	var harvest int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM openai_downgrade_probe_states WHERE probe_mode='harvest'").Scan(&harvest))
	require.Zero(t, harvest, "nonempty harvest state requires an explicit data-transition audit")
	var resumed int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM accounts a
		WHERE a.platform='openai' AND a.type='oauth'
			AND a.parent_account_id IS NULL AND a.deleted_at IS NULL
			AND a.status='active' AND NOT a.schedulable
			AND a.extra->>'openai_downgrade_qualification'='true'
			AND (a.auto_pause_on_expired IS NOT TRUE OR a.expires_at IS NULL OR a.expires_at>NOW())
			AND NOT EXISTS (SELECT 1 FROM openai_downgrade_probe_controls c
				WHERE c.account_id=a.id AND c.manual_paused)
			AND NOT EXISTS (SELECT 1 FROM openai_downgrade_probe_states s
				WHERE s.account_id=a.id AND (s.state<>'on_duty' OR s.probe_mode<>'qualification'))
			AND NOT EXISTS (SELECT 1 FROM openai_downgrade_probe_results p
				WHERE p.account_id=a.id AND p.mode='qualification' AND p.transport_ok
					AND (p.http_status>=400 OR p.answer_correct IS FALSE))`).Scan(&resumed))
	require.Zero(t, resumed, "automatic import resumption requires a separate account-transition audit")
	rows, err := db.QueryContext(t.Context(), `SELECT id FROM accounts
		WHERE platform='openai' AND type='oauth' AND parent_account_id IS NULL
			AND (extra ? 'openai_downgrade_qualification' OR extra ? 'openai_oauth_qualified_proxy_id')`)
	require.NoError(t, err)
	retiredIDs := []int64{}
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		retiredIDs = append(retiredIDs, id)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	t.Logf("approved import-metadata retirement: %d accounts; automatic scheduling changes: %d", len(retiredIDs), resumed)
	ledgerRows, err := db.QueryContext(t.Context(), `SELECT filename, to_jsonb(m)::text FROM schema_migrations m`)
	require.NoError(t, err)
	history := map[string]string{}
	for ledgerRows.Next() {
		var name, record string
		require.NoError(t, ledgerRows.Scan(&name, &record))
		history[name] = record
	}
	require.NoError(t, ledgerRows.Err())
	require.NoError(t, ledgerRows.Close())
	snapshot := func(expected bool) map[string]string {
		out := make(map[string]string)
		for _, table := range []string{"accounts", "users", "api_keys", "groups", "account_groups", "user_subscriptions"} {
			var digest string
			if table == "accounts" {
				// Only 247's two retired import markers and the touched rows' timestamps
				// are allowed to change. Compare every other field and unknown extra key.
				require.NoError(t, db.QueryRowContext(t.Context(), `
					SELECT md5(COALESCE(string_agg(md5(j::text), '' ORDER BY j::text), ''))
					FROM (SELECT
						(to_jsonb(r) - CASE WHEN r.id=ANY($1) THEN 'updated_at' ELSE '' END)
						|| CASE WHEN $2 AND r.id=ANY($1) THEN jsonb_build_object('extra',
							COALESCE(r.extra, '{}'::jsonb) - 'openai_downgrade_qualification'
								- 'openai_oauth_qualified_proxy_id') ELSE '{}'::jsonb END AS j
					FROM accounts r) normalized`, pq.Array(retiredIDs), expected).Scan(&digest))
				out[table] = digest
				continue
			}
			if table == "groups" && expected {
				require.NoError(t, db.QueryRowContext(t.Context(), `
					SELECT md5(COALESCE(string_agg(md5(j::text), '' ORDER BY j::text), ''))
					FROM (SELECT to_jsonb(r) || jsonb_build_object('models_list_config',
						COALESCE(to_jsonb(r)->'models_list_config', '{}'::jsonb)) AS j
					FROM groups r) normalized`).Scan(&digest))
				out[table] = digest
				continue
			}
			require.NoError(t, db.QueryRowContext(t.Context(), fmt.Sprintf(
				"SELECT md5(COALESCE(string_agg(md5(to_jsonb(r)::text), '' ORDER BY to_jsonb(r)::text), '')) FROM %s r", table)).Scan(&digest))
			out[table] = digest
		}
		return out
	}
	before := snapshot(true)
	for range 2 {
		require.NoError(t, ApplyMigrations(t.Context(), db))
	}
	require.Equal(t, before, snapshot(false), "only explicitly approved import metadata may change; customer data and scheduling/groups must remain intact")
	columns, err := db.QueryContext(t.Context(), `SELECT table_name, column_name
		FROM information_schema.columns WHERE table_schema='public'`)
	require.NoError(t, err)
	available := map[string]bool{}
	for columns.Next() {
		var table, column string
		require.NoError(t, columns.Scan(&table, &column))
		available[table+"."+column] = true
	}
	require.NoError(t, columns.Err())
	require.NoError(t, columns.Close())
	for _, table := range entmigrate.Tables {
		for _, column := range table.Columns {
			require.Truef(t, available[table.Name+"."+column.Name],
				"runtime ORM column missing after migration: %s.%s", table.Name, column.Name)
		}
	}
	for name, original := range history {
		var record string
		require.NoError(t, db.QueryRowContext(t.Context(),
			`SELECT to_jsonb(m)::text FROM schema_migrations m WHERE filename=$1`, name).Scan(&record))
		require.Equal(t, original, record, "historical migration record changed: %s", name)
	}
	for name, alias := range migrationFilenameAliases {
		var checksum string
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT checksum FROM schema_migrations WHERE filename=$1", alias.filename).Scan(&checksum))
		require.Equal(t, alias.checksum, checksum)
		require.ErrorIs(t, db.QueryRowContext(t.Context(), "SELECT checksum FROM schema_migrations WHERE filename=$1", name).Scan(&checksum), sql.ErrNoRows)
	}
	// Exercise both the missing-column and configured-column cases in a temporary
	// table, without replacing the restored customer groups or their settings.
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(t.Context(), `CREATE TEMP TABLE groups (id bigint PRIMARY KEY) ON COMMIT DROP`)
	require.NoError(t, err)
	repair, err := migrations.FS.ReadFile("250_repair_group_models_list_config.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), string(repair))
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO groups (id, models_list_config)
		VALUES (1, '{"models":["fixture-model"]}'::jsonb); INSERT INTO groups (id) VALUES (2)`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), string(repair))
	require.NoError(t, err)
	var preserved, defaulted bool
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT models_list_config='{"models":["fixture-model"]}'::jsonb
		FROM groups WHERE id=1`).Scan(&preserved))
	require.True(t, preserved)
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT models_list_config='{}'::jsonb FROM groups WHERE id=2`).Scan(&defaulted))
	require.True(t, defaulted)
}
