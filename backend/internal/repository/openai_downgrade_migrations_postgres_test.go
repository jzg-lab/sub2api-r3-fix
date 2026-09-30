package repository

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func newProbeUpgradePostgres(t *testing.T) (*sql.DB, fstest.MapFS) {
	t.Helper()
	db := newProbePostgresWithMigrations(t, []string{
		"036_scheduler_outbox.sql", "152_scheduler_outbox_dedup_key.sql",
		"153_scheduler_outbox_pending_dedup_key_index_notx.sql", "237_openai_downgrade_probe.sql",
	})
	_, err := db.Exec(`
		CREATE TABLE groups (id BIGINT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'active');
		CREATE TABLE account_groups (
			account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
			PRIMARY KEY(account_id, group_id)
		);
		INSERT INTO proxies(id) VALUES(3);
		INSERT INTO groups(id) VALUES(7);
		INSERT INTO accounts(id, proxy_id, schedulable, updated_at)
		VALUES(1,3,FALSE,'2020-01-01Z'),(2,3,TRUE,'2020-01-01Z');
		INSERT INTO openai_downgrade_probe_states(account_id, original_proxy_id, current_proxy_id)
		VALUES(1,3,3),(2,3,3);
	`)
	require.NoError(t, err)
	pending := fstest.MapFS{}
	for _, name := range []string{
		"238_scheduler_account_revisions.sql",
		"239_openai_probe_ownership.sql",
		"240_openai_probe_rate_limit_streak.sql",
	} {
		content, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, err)
		pending[name] = &fstest.MapFile{Data: content}
	}
	return db, pending
}

func TestOpenAIProbePostgresMigrationUpgradeAndReplay(t *testing.T) {
	db, pending := newProbeUpgradePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, applyMigrationsFS(ctx, db, pending))
	var paused, hasOwner bool
	require.NoError(t, db.QueryRow(`
		SELECT manual_paused, owned_error IS NOT NULL
		FROM openai_downgrade_probe_controls WHERE account_id=1
	`).Scan(&paused, &hasOwner))
	require.True(t, paused, "unknown historical pauses must remain paused")
	require.False(t, hasOwner, "upgrade must not invent error ownership")
	var controls, streak int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM openai_downgrade_probe_controls").Scan(&controls))
	require.Equal(t, 1, controls, "upgrade must not pause a previously active account")
	require.NoError(t, db.QueryRow(
		"SELECT consecutive_429s FROM openai_downgrade_probe_states WHERE account_id=1").Scan(&streak))
	require.Zero(t, streak)

	beforeRevision := schedulerRevision(t, db, 2)
	_, err := db.Exec("INSERT INTO account_groups(account_id,group_id) VALUES(2,7)")
	require.NoError(t, err)
	require.True(t, schedulerRevision(t, db, 2).After(beforeRevision))
	_, err = db.Exec("UPDATE openai_downgrade_probe_states SET consecutive_429s=5 WHERE account_id=1")
	require.NoError(t, err)
	before := probePostgresSnapshot(t, db)
	require.NoError(t, applyMigrationsFS(ctx, db, pending))
	require.Equal(t, before, probePostgresSnapshot(t, db), "native replay must not rerun backfills")
	var applied int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&applied))
	require.Equal(t, 3, applied)

	original := pending["239_openai_probe_ownership.sql"]
	pending["239_openai_probe_ownership.sql"] = &fstest.MapFile{
		Data: append(append([]byte(nil), original.Data...), []byte("\n-- changed after deployment\n")...),
	}
	require.ErrorContains(t, applyMigrationsFS(ctx, db, pending), "checksum mismatch")
	require.Equal(t, before, probePostgresSnapshot(t, db))
}

func TestOpenAIProbePostgresMigrationFailureAndResume(t *testing.T) {
	db, pending := newProbeUpgradePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	original := pending["239_openai_probe_ownership.sql"]
	pending["239_openai_probe_ownership.sql"] = &fstest.MapFile{
		Data: append(append([]byte(nil), original.Data...),
			[]byte("\nDO $$ BEGIN RAISE EXCEPTION 'injected migration failure'; END $$;\n")...),
	}
	require.ErrorContains(t, applyMigrationsFS(ctx, db, pending), "injected migration failure")
	var applied []string
	rows, err := db.Query("SELECT filename FROM schema_migrations ORDER BY filename")
	require.NoError(t, err)
	for rows.Next() {
		var filename string
		require.NoError(t, rows.Scan(&filename))
		applied = append(applied, filename)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []string{"238_scheduler_account_revisions.sql"}, applied)
	var controlsAbsent, streakAbsent bool
	require.NoError(t, db.QueryRow(`
		SELECT to_regclass('openai_downgrade_probe_controls') IS NULL,
			NOT EXISTS(SELECT 1 FROM information_schema.columns
				WHERE table_schema=current_schema()
					AND table_name='openai_downgrade_probe_states'
					AND column_name='consecutive_429s')
	`).Scan(&controlsAbsent, &streakAbsent))
	require.True(t, controlsAbsent, "failed migration DDL must roll back")
	require.True(t, streakAbsent, "later migrations must not run after a failure")

	pending["239_openai_probe_ownership.sql"] = original
	require.NoError(t, applyMigrationsFS(ctx, db, pending))
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count))
	require.Equal(t, 3, count, "resume must retain 238 and apply only missing migrations")
}

func TestOpenAIProbeAuthStrikesMigrationFreshAndLegacyReplay(t *testing.T) {
	authStrikes, err := os.ReadFile(filepath.Join("..", "..", "migrations",
		"247_probe_auth_strikes.sql"))
	require.NoError(t, err)

	t.Run("fresh_database_orders_dependency_first", func(t *testing.T) {
		db := newProbePostgresWithMigrations(t, nil)
		probeBase, readErr := os.ReadFile(filepath.Join("..", "..", "migrations",
			"237_openai_downgrade_probe.sql"))
		require.NoError(t, readErr)
		pending := fstest.MapFS{
			"237_openai_downgrade_probe.sql": &fstest.MapFile{Data: probeBase},
			"247_probe_auth_strikes.sql":     &fstest.MapFile{Data: authStrikes},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, applyMigrationsFS(ctx, db, pending))

		var exists bool
		require.NoError(t, db.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM information_schema.columns
				WHERE table_schema=current_schema()
					AND table_name='openai_downgrade_probe_states'
					AND column_name='auth_consecutive_failures'
			)
		`).Scan(&exists))
		require.True(t, exists)
	})

	t.Run("legacy_filename_upgrade_is_idempotent", func(t *testing.T) {
		db := newProbePostgresWithMigrations(t, []string{
			"237_openai_downgrade_probe.sql",
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, applyMigrationsFS(ctx, db, fstest.MapFS{
			"228_probe_auth_strikes.sql": &fstest.MapFile{Data: authStrikes},
		}))
		require.NoError(t, applyMigrationsFS(ctx, db, fstest.MapFS{
			"247_probe_auth_strikes.sql": &fstest.MapFile{Data: authStrikes},
		}))
		require.NoError(t, applyMigrationsFS(ctx, db, fstest.MapFS{
			"247_probe_auth_strikes.sql": &fstest.MapFile{Data: authStrikes},
		}))

		var applied int
		require.NoError(t, db.QueryRow(`
			SELECT COUNT(*) FROM schema_migrations
			WHERE filename IN ('228_probe_auth_strikes.sql', '247_probe_auth_strikes.sql')
		`).Scan(&applied))
		require.Equal(t, 2, applied)
	})
}

func TestOpenAIProbePostgresMigrationLegacyWriterBoundary(t *testing.T) {
	db, pending := newProbeUpgradePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, applyMigrationsFS(ctx, db, pending))
	before := probePostgresSnapshot(t, db)
	_, err := db.Exec("UPDATE accounts SET schedulable=TRUE WHERE id=1")
	var constraintError *pq.Error
	require.ErrorAs(t, err, &constraintError)
	require.Equal(t, pq.ErrorCode("23514"), constraintError.Code)
	require.Equal(t, before, probePostgresSnapshot(t, db),
		"an old account-only resume cannot bypass durable manual pause")

	_, err = db.Exec("UPDATE accounts SET extra=extra || '{\"legacy_edit\":true}' WHERE id=2")
	require.NoError(t, err, "non-routing legacy edits remain compatible")
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET schedulable=TRUE WHERE id=1")
	require.NoError(t, err, "constraint is deferred to allow the current atomic resume")
	_, err = tx.ExecContext(ctx,
		"UPDATE openai_downgrade_probe_controls SET manual_paused=FALSE WHERE account_id=1")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var schedulable bool
	require.NoError(t, db.QueryRow("SELECT schedulable FROM accounts WHERE id=1").Scan(&schedulable))
	require.True(t, schedulable)
}
