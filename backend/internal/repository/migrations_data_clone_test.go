package repository

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

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
	snapshot := func() map[string]string {
		out := make(map[string]string)
		for _, table := range []string{"accounts", "users", "api_keys", "groups", "account_groups", "user_subscriptions"} {
			var digest string
			require.NoError(t, db.QueryRowContext(t.Context(), fmt.Sprintf(
				"SELECT md5(COALESCE(string_agg(md5(to_jsonb(r)::text), '' ORDER BY to_jsonb(r)::text), '')) FROM %s r", table)).Scan(&digest))
			out[table] = digest
		}
		return out
	}
	before := snapshot()
	for range 2 {
		require.NoError(t, ApplyMigrations(t.Context(), db))
	}
	require.Equal(t, before, snapshot(), "upgrade must not rewrite customer data or scheduling/groups")
	for name, alias := range migrationFilenameAliases {
		var checksum string
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT checksum FROM schema_migrations WHERE filename=$1", alias.filename).Scan(&checksum))
		require.Equal(t, alias.checksum, checksum)
		require.ErrorIs(t, db.QueryRowContext(t.Context(), "SELECT checksum FROM schema_migrations WHERE filename=$1", name).Scan(&checksum), sql.ErrNoRows)
	}
}
