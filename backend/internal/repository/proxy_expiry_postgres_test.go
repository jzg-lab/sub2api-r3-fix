package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newProxyExpiryPostgres(t *testing.T) (*sql.DB, *proxyRepository, service.Proxy) {
	t.Helper()
	db := newProbePostgres(t)
	_, err := db.Exec(`
		ALTER TABLE proxies ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			ADD COLUMN fallback_mode TEXT NOT NULL DEFAULT 'none',
			ADD COLUMN backup_proxy_id BIGINT;
		ALTER TABLE accounts ADD COLUMN proxy_fallback_origin_id BIGINT;
		INSERT INTO proxies(id, expires_at, fallback_mode) VALUES (3, NOW() - INTERVAL '1 hour', 'direct'), (4, NULL, 'none');
		INSERT INTO accounts(id, platform, type, proxy_id, credentials, updated_at) VALUES
			(1, 'openai', 'oauth', 3, '{}', NOW()),
			(2, 'openai', 'apikey', 3, '{}', NOW()),
			(3, 'anthropic', 'oauth', 3, '{}', NOW()),
			(4, 'openai', 'oauth', 3, '{"auth_mode":"personalAccessToken"}', NOW());
	`)
	require.NoError(t, err)
	expired := service.Proxy{ID: 3}
	require.NoError(t, db.QueryRow("SELECT updated_at FROM proxies WHERE id=3").Scan(&expired.UpdatedAt))
	accountRepo := probeControlAccountRepo(db)
	return db, newProxyRepositoryWithSQL(accountRepo.client, db), expired
}

func proxyExpirySnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var result string
	require.NoError(t, db.QueryRow(`
		SELECT jsonb_build_object(
			'proxies', (SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM proxies p),
			'accounts', (SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM accounts a),
			'outbox', (SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM scheduler_outbox o)
		)::text
	`).Scan(&result))
	return result
}

func TestProxyExpiryPostgresPreservesBrowserRoute(t *testing.T) {
	for _, mode := range []string{"direct", "backup", "none"} {
		t.Run(mode, func(t *testing.T) {
			db, repo, expired := newProxyExpiryPostgres(t)
			var target *int64
			if mode == "backup" {
				id := int64(4)
				target = &id
			}
			fallbackMode := mode
			if mode == "backup" {
				fallbackMode = service.FallbackModeProxy
			}
			_, err := db.Exec("UPDATE proxies SET fallback_mode=$1, backup_proxy_id=$2 WHERE id=3", fallbackMode, target)
			require.NoError(t, err)
			changed, err := repo.sweepOneExpiredProxy(context.Background(), expired, time.Now())
			require.NoError(t, err)
			if mode == "none" {
				require.Empty(t, changed)
			} else {
				require.ElementsMatch(t, []int64{2, 3, 4}, changed)
			}
			var assigned int64
			var origin sql.NullInt64
			require.NoError(t, db.QueryRow("SELECT proxy_id, proxy_fallback_origin_id FROM accounts WHERE id=1").Scan(&assigned, &origin))
			require.EqualValues(t, 3, assigned)
			require.False(t, origin.Valid)
			var status string
			require.NoError(t, db.QueryRow("SELECT status FROM proxies WHERE id=3").Scan(&status))
			require.Equal(t, service.StatusExpired, status)
			var notified int
			require.NoError(t, db.QueryRow(`
				SELECT COUNT(DISTINCT value) FROM scheduler_outbox,
				LATERAL jsonb_array_elements(payload->'account_ids')
				WHERE event_type='account_bulk_changed'
			`).Scan(&notified))
			require.Equal(t, 4, notified, "retained snapshots must also be invalidated")
			beforeReplay := proxyExpirySnapshot(t, db)
			changed, err = repo.sweepOneExpiredProxy(context.Background(), expired, time.Now())
			require.NoError(t, err)
			require.Empty(t, changed)
			require.Equal(t, beforeReplay, proxyExpirySnapshot(t, db))
		})
	}
}

func TestProxyExpiryPostgresOutboxFailureRollsBack(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained", true: "rerouted"}[change], func(t *testing.T) {
			db, repo, expired := newProxyExpiryPostgres(t)
			if !change {
				_, err := db.Exec("UPDATE proxies SET fallback_mode='none' WHERE id=3")
				require.NoError(t, err)
			}
			_, err := db.Exec(`
				CREATE FUNCTION reject_expiry_event() RETURNS trigger LANGUAGE plpgsql AS
				$$ BEGIN RAISE EXCEPTION 'injected expiry event failure'; END $$;
				CREATE TRIGGER reject_write BEFORE INSERT ON scheduler_outbox
				FOR EACH ROW EXECUTE FUNCTION reject_expiry_event();
			`)
			require.NoError(t, err)
			before := proxyExpirySnapshot(t, db)
			_, err = repo.sweepOneExpiredProxy(context.Background(), expired, time.Now())
			require.ErrorContains(t, err, "injected expiry event failure")
			require.Equal(t, before, proxyExpirySnapshot(t, db))
		})
	}
}

func TestProxyExpiryPostgresRejectsChangedSnapshot(t *testing.T) {
	for _, edit := range []string{
		"expires_at=NOW() + INTERVAL '1 day'",
		"updated_at=updated_at + INTERVAL '1 microsecond'",
		"status='disabled'",
		"deleted_at=NOW()",
	} {
		t.Run(edit, func(t *testing.T) {
			db, repo, expired := newProxyExpiryPostgres(t)
			_, err := db.Exec("UPDATE proxies SET " + edit + " WHERE id=3")
			require.NoError(t, err)
			before := proxyExpirySnapshot(t, db)
			changed, err := repo.sweepOneExpiredProxy(context.Background(), expired, time.Now())
			require.NoError(t, err)
			require.Empty(t, changed)
			require.Equal(t, before, proxyExpirySnapshot(t, db))
		})
	}
}

func TestProxyExpiryPostgresRejectsInvalidBackup(t *testing.T) {
	for _, edit := range []string{
		"expires_at=NOW() - INTERVAL '1 second'",
		"status='disabled'",
		"deleted_at=NOW()",
	} {
		t.Run(edit, func(t *testing.T) {
			db, repo, expired := newProxyExpiryPostgres(t)
			_, err := db.Exec("UPDATE proxies SET " + edit + " WHERE id=4")
			require.NoError(t, err)
			_, err = db.Exec("UPDATE proxies SET fallback_mode='proxy', backup_proxy_id=4 WHERE id=3")
			require.NoError(t, err)
			changed, err := repo.sweepOneExpiredProxy(context.Background(), expired, time.Now())
			require.NoError(t, err)
			require.Empty(t, changed)
			var moved int
			require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM accounts WHERE proxy_id IS DISTINCT FROM 3").Scan(&moved))
			require.Zero(t, moved)
		})
	}
}

func TestProxyExpiryPostgresRejectsUnboundExecutor(t *testing.T) {
	db, repo, expired := newProxyExpiryPostgres(t)
	tx, err := repo.client.Tx(context.Background())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	unbound := newProxyRepositoryWithSQL(tx.Client(), db)
	before := proxyExpirySnapshot(t, db)
	_, err = unbound.sweepOneExpiredProxy(context.Background(), expired, time.Now())
	require.ErrorContains(t, err, "matching transaction executor")
	require.Equal(t, before, proxyExpirySnapshot(t, db))
}

func TestProxyExpiryPostgresOuterTransactionRollsBack(t *testing.T) {
	for _, contextual := range []bool{false, true} {
		t.Run(map[bool]string{false: "bound", true: "context"}[contextual], func(t *testing.T) {
			db, repo, expired := newProxyExpiryPostgres(t)
			before := proxyExpirySnapshot(t, db)
			ctx := context.Background()
			tx, err := repo.client.Tx(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			if contextual {
				ctx = dbent.NewTxContext(ctx, tx)
			} else {
				repo = newProxyRepositoryWithSQL(tx.Client(), tx)
			}
			changed, err := repo.sweepOneExpiredProxy(ctx, expired, time.Now())
			require.NoError(t, err)
			require.Len(t, changed, 3)
			require.NoError(t, tx.Rollback())
			require.Equal(t, before, proxyExpirySnapshot(t, db))
		})
	}
}

func TestProxyExpiryPostgresUsesCurrentLockedChain(t *testing.T) {
	db, repo, expired := newProxyExpiryPostgres(t)
	_, err := db.Exec(`
		INSERT INTO proxies(id) VALUES (5), (6);
		UPDATE proxies SET fallback_mode='proxy', backup_proxy_id=4 WHERE id=3;
		UPDATE proxies SET expires_at=NOW() - INTERVAL '1 hour',
			fallback_mode='proxy', backup_proxy_id=5 WHERE id=4;
	`)
	require.NoError(t, err)
	edit, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = edit.Rollback() }()
	_, err = edit.Exec("UPDATE proxies SET backup_proxy_id=6 WHERE id=4")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := repo.sweepOneExpiredProxy(ctx, expired, time.Now())
		done <- err
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname=current_database() AND wait_event_type='Lock'
					AND query LIKE '%SELECT status, expires_at, fallback_mode, backup_proxy_id%'
			)
		`).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond, "expiry must wait for the current chain row")
	require.NoError(t, edit.Commit())
	require.NoError(t, <-done)
	var assigned int64
	require.NoError(t, db.QueryRow("SELECT proxy_id FROM accounts WHERE id=2").Scan(&assigned))
	require.EqualValues(t, 6, assigned, "must not use the stale destination 5")
	require.NoError(t, db.QueryRow("SELECT proxy_id FROM accounts WHERE id=1").Scan(&assigned))
	require.EqualValues(t, 3, assigned, "browser OAuth must retain its route")
}

func TestProxyExpiryPostgresAccountRevisionsNeverRegress(t *testing.T) {
	for _, mode := range []string{"direct", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			db, repo, expired := newProxyExpiryPostgres(t)
			_, err := db.Exec(`
				UPDATE accounts SET updated_at=clock_timestamp() + INTERVAL '1 hour';
				INSERT INTO accounts(id, platform, type, proxy_id, credentials, updated_at)
				VALUES (5, 'openai', 'oauth', 3, '{"auth_mode":"agentIdentity"}', clock_timestamp() + INTERVAL '1 hour');
			`)
			require.NoError(t, err)
			_, err = db.Exec("UPDATE proxies SET fallback_mode=$1, backup_proxy_id=4 WHERE id=3", mode)
			require.NoError(t, err)
			var before time.Time
			require.NoError(t, db.QueryRow("SELECT MAX(updated_at) FROM accounts").Scan(&before))
			_, err = db.Exec("UPDATE accounts SET updated_at=$1", before)
			require.NoError(t, err)
			changed, err := repo.sweepOneExpiredProxy(context.Background(), expired, time.Now())
			require.NoError(t, err)
			require.ElementsMatch(t, []int64{2, 3, 4, 5}, changed, "PAT and agent identity keep their existing fallback behavior")
			var stale int
			require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM accounts WHERE updated_at <= $1", before).Scan(&stale))
			require.Zero(t, stale, "neither retained nor moved account revisions may go backwards")
		})
	}
}

func TestProxyExpiryPostgresChainCycleAndDeletedHopRetainRoutes(t *testing.T) {
	for _, mode := range []string{"cycle", "deleted", "disabled", "none", "direct"} {
		t.Run(mode, func(t *testing.T) {
			db, repo, expired := newProxyExpiryPostgres(t)
			_, err := db.Exec(`
				UPDATE proxies SET fallback_mode='proxy', backup_proxy_id=4 WHERE id=3;
				UPDATE proxies SET expires_at=NOW() - INTERVAL '1 hour',
					fallback_mode='proxy', backup_proxy_id=3 WHERE id=4;
			`)
			require.NoError(t, err)
			switch mode {
			case "deleted":
				_, err = db.Exec("UPDATE proxies SET deleted_at=NOW() WHERE id=4")
			case "disabled":
				_, err = db.Exec("UPDATE proxies SET status='disabled', fallback_mode='direct' WHERE id=4")
			case "none", "direct":
				_, err = db.Exec("UPDATE proxies SET fallback_mode=$1 WHERE id=4", mode)
			}
			require.NoError(t, err)
			changed, err := repo.sweepOneExpiredProxy(context.Background(), expired, time.Now())
			require.NoError(t, err)
			if mode == "direct" {
				require.ElementsMatch(t, []int64{2, 3, 4}, changed)
			} else {
				require.Empty(t, changed)
			}
			var assigned int64
			require.NoError(t, db.QueryRow("SELECT proxy_id FROM accounts WHERE id=1").Scan(&assigned))
			require.EqualValues(t, 3, assigned)
		})
	}
}
