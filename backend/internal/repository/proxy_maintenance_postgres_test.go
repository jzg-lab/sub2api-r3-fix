package repository

import (
	"database/sql"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newProxyMaintenancePostgres(t *testing.T) (*sql.DB, *proxyRepository) {
	t.Helper()
	db := newProbePostgres(t)
	_, err := db.Exec(`
		ALTER TABLE proxies
			ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			ADD COLUMN name TEXT NOT NULL DEFAULT 'fixture',
			ADD COLUMN protocol TEXT NOT NULL DEFAULT 'socks5',
			ADD COLUMN host TEXT NOT NULL DEFAULT 'proxy.example.test',
			ADD COLUMN port INTEGER NOT NULL DEFAULT 1080,
			ADD COLUMN username TEXT,
			ADD COLUMN password TEXT,
			ADD COLUMN fallback_mode TEXT NOT NULL DEFAULT 'none',
			ADD COLUMN backup_proxy_id BIGINT,
			ADD COLUMN expiry_warn_days INTEGER NOT NULL DEFAULT 7;
		INSERT INTO proxies(id) VALUES(7), (8);
	`)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO accounts(id, proxy_id, extra, updated_at, deleted_at) VALUES
			(101, 7, '{}'::jsonb, NOW(), NULL),
			(102, 7, jsonb_build_object($1::text, 7), NOW(), NOW());
	`, service.OpenAIOAuthQualifiedProxyExtraKey)
	require.NoError(t, err)
	return db, newProxyRepositoryWithSQL(probeControlAccountRepo(db).client, db)
}

func TestProxyMaintenancePostgresRefreshesAllConsumers(t *testing.T) {
	for _, operation := range []string{"deactivate", "renew", "remove_expiry", "reactivate", "expire_now"} {
		t.Run(operation, func(t *testing.T) {
			db, repo := newProxyMaintenancePostgres(t)
			_, err := db.Exec("UPDATE proxies SET expires_at=NOW()+INTERVAL '1 hour' WHERE id=7")
			require.NoError(t, err)
			if operation == "reactivate" {
				_, err = db.Exec("UPDATE proxies SET status='inactive' WHERE id=7")
				require.NoError(t, err)
			}
			current, err := repo.GetByID(t.Context(), 7)
			require.NoError(t, err)
			var historyBefore, accountBefore time.Time
			require.NoError(t, db.QueryRow("SELECT updated_at FROM accounts WHERE id=102").Scan(&historyBefore))
			require.NoError(t, db.QueryRow("SELECT updated_at FROM accounts WHERE id=101").Scan(&accountBefore))
			switch operation {
			case "deactivate":
				current.Status = "inactive"
			case "reactivate":
				current.Status = service.StatusActive
			case "renew":
				expiry := time.Now().Add(48 * time.Hour).Truncate(time.Microsecond)
				current.ExpiresAt = &expiry
			case "remove_expiry":
				current.ExpiresAt = nil
			case "expire_now":
				expiry := time.Now().Add(-time.Second)
				current.ExpiresAt = &expiry
			}
			require.NoError(t, repo.Update(t.Context(), current))
			got, err := repo.GetByID(t.Context(), 7)
			require.NoError(t, err)
			require.Equal(t, current.Status, got.Status)
			require.True(t, sameProxyExpiry(current.ExpiresAt, got.ExpiresAt))
			var accountAfter, historyAfter time.Time
			var currentProxy, historicalProxy int64
			require.NoError(t, db.QueryRow("SELECT updated_at, proxy_id FROM accounts WHERE id=101").Scan(&accountAfter, &currentProxy))
			require.NoError(t, db.QueryRow("SELECT updated_at, proxy_id FROM accounts WHERE id=102").Scan(&historyAfter, &historicalProxy))
			require.True(t, accountAfter.After(accountBefore))
			require.Equal(t, historyBefore, historyAfter)
			require.EqualValues(t, 7, currentProxy)
			require.EqualValues(t, 7, historicalProxy)
			var payload string
			require.NoError(t, db.QueryRow("SELECT payload::text FROM scheduler_outbox WHERE event_type='account_bulk_changed'").Scan(&payload))
			require.JSONEq(t, `{"account_ids":[101]}`, payload)

			// No-op replay must not keep invalidating caches or adding events.
			require.NoError(t, repo.Update(t.Context(), current))
			var events int
			require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scheduler_outbox").Scan(&events))
			require.Equal(t, 1, events)
			var afterReplay time.Time
			require.NoError(t, db.QueryRow("SELECT updated_at FROM accounts WHERE id=101").Scan(&afterReplay))
			require.Equal(t, accountAfter, afterReplay)
			require.ErrorIs(t, repo.Delete(t.Context(), 7), service.ErrOpenAIOAuthProxyBindingProtected)
		})
	}
}

func TestProxyMaintenancePostgresProtectedRoutesAndHistory(t *testing.T) {
	for _, change := range []string{"protocol", "host", "port", "username", "password"} {
		t.Run(change, func(t *testing.T) {
			db, repo := newProxyMaintenancePostgres(t)
			_, err := db.Exec("UPDATE accounts SET deleted_at=NOW(); UPDATE proxies SET status='inactive' WHERE id=7")
			require.NoError(t, err)
			current, err := repo.GetByID(t.Context(), 7)
			require.NoError(t, err)
			before := proxyExpirySnapshot(t, db)
			switch change {
			case "protocol":
				current.Protocol = "http"
			case "host":
				current.Host = "other.example.test"
			case "port":
				current.Port++
			case "username":
				current.Username = "changed-fixture"
			case "password":
				current.Password = "changed-fixture"
			}
			require.ErrorIs(t, repo.Update(t.Context(), current), service.ErrOpenAIOAuthProxyBindingProtected)
			require.ErrorIs(t, repo.Delete(t.Context(), 7), service.ErrOpenAIOAuthProxyBindingProtected)
			require.Equal(t, before, proxyExpirySnapshot(t, db))
			require.NoError(t, repo.Delete(t.Context(), 8), "unreferenced proxy remains deletable")
		})
	}
}

func TestProxyMaintenancePostgresOutboxFailureRollsBack(t *testing.T) {
	db, repo := newProxyMaintenancePostgres(t)
	_, err := db.Exec(`
		CREATE FUNCTION reject_maintenance_event() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'injected maintenance event failure'; END $$;
		CREATE TRIGGER reject_write BEFORE INSERT ON scheduler_outbox
		FOR EACH ROW EXECUTE FUNCTION reject_maintenance_event();
	`)
	require.NoError(t, err)
	current, err := repo.GetByID(t.Context(), 7)
	require.NoError(t, err)
	before := proxyExpirySnapshot(t, db)
	current.Status = "inactive"
	require.ErrorContains(t, repo.Update(t.Context(), current), "injected maintenance event failure")
	require.Equal(t, before, proxyExpirySnapshot(t, db))
}

func TestProxyMaintenancePostgresActiveListsExcludeUnavailable(t *testing.T) {
	db, repo := newProxyMaintenancePostgres(t)
	_, err := db.Exec(`
		INSERT INTO proxies(id, status, expires_at, deleted_at) VALUES
			(10, 'active', NOW() - INTERVAL '1 hour', NULL),
			(11, 'active', NOW(), NULL),
			(12, 'inactive', NULL, NULL),
			(13, 'expired', NOW() + INTERVAL '1 hour', NULL),
			(14, 'active', NULL, NOW()),
			(15, 'active', NOW() + INTERVAL '1 hour', NULL);
	`)
	require.NoError(t, err)
	proxies, err := repo.ListActive(t.Context())
	require.NoError(t, err)
	var ids []int64
	for _, p := range proxies {
		ids = append(ids, p.ID)
	}
	require.ElementsMatch(t, []int64{7, 8, 15}, ids)
	withCounts, err := repo.ListActiveWithAccountCount(t.Context())
	require.NoError(t, err)
	ids = nil
	for _, p := range withCounts {
		ids = append(ids, p.ID)
		if p.ID == 7 {
			require.EqualValues(t, 1, p.AccountCount)
		}
	}
	require.ElementsMatch(t, []int64{7, 8, 15}, ids)
}

func TestProxyMaintenancePostgresPreservesBackupDependents(t *testing.T) {
	db, repo := newProxyMaintenancePostgres(t)
	_, err := db.Exec(`
		INSERT INTO proxies(id) VALUES(9);
		UPDATE proxies SET fallback_mode='proxy', backup_proxy_id=7 WHERE id=8;
	`)
	require.NoError(t, err)
	current, err := repo.GetByID(t.Context(), 7)
	require.NoError(t, err)
	current.Status = "inactive"
	require.NoError(t, repo.Update(t.Context(), current))
	var backup sql.NullInt64
	require.NoError(t, db.QueryRow("SELECT backup_proxy_id FROM proxies WHERE id=8").Scan(&backup))
	require.Equal(t, sql.NullInt64{Int64: 7, Valid: true}, backup, "maintenance must preserve incoming backup references")

	id := int64(9)
	current.FallbackMode = service.FallbackModeProxy
	current.BackupProxyID = &id
	require.NoError(t, repo.Update(t.Context(), current))
	require.NoError(t, db.QueryRow("SELECT backup_proxy_id FROM proxies WHERE id=7").Scan(&backup))
	require.Equal(t, sql.NullInt64{Int64: 9, Valid: true}, backup)
	require.NoError(t, db.QueryRow("SELECT backup_proxy_id FROM proxies WHERE id=9").Scan(&backup))
	require.False(t, backup.Valid, "a backup relation is directional, not reciprocal")
	require.NoError(t, db.QueryRow("SELECT backup_proxy_id FROM proxies WHERE id=8").Scan(&backup))
	require.Equal(t, sql.NullInt64{Int64: 7, Valid: true}, backup)
}
