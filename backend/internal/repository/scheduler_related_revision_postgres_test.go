package repository

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

func TestSchedulerRevisionPostgresSnapshotReadFence(t *testing.T) {
	for _, change := range []string{
		"UPDATE proxies SET status = 'disabled' WHERE id = 3",
		"INSERT INTO account_groups(account_id, group_id) VALUES(1, 7)",
		"UPDATE groups SET status = 'disabled' WHERE id = 7",
		"UPDATE accounts SET deleted_at = NOW() WHERE id = 1",
		"DELETE FROM accounts WHERE id = 1",
	} {
		t.Run(change, func(t *testing.T) {
			db := newSchedulerRevisionPostgres(t)
			_, err := db.Exec("INSERT INTO account_groups(account_id, group_id) VALUES(1, 8)")
			require.NoError(t, err)
			// The group-edit case needs a binding to that exact group.
			if change == "UPDATE groups SET status = 'disabled' WHERE id = 7" {
				_, err = db.Exec("INSERT INTO account_groups(account_id, group_id) VALUES(1, 7)")
				require.NoError(t, err)
			}
			snapshot := []*dbent.Account{{ID: 1, UpdatedAt: schedulerRevision(t, db, 1)}}
			repo := probeControlAccountRepo(db)
			require.NoError(t, repo.verifyAccountSnapshotRevisions(context.Background(), snapshot))
			_, err = db.Exec(change)
			require.NoError(t, err)
			require.ErrorIs(t, repo.verifyAccountSnapshotRevisions(context.Background(), snapshot), errAccountSnapshotChanged)
		})
	}
}

func newSchedulerRevisionPostgres(t *testing.T) *sql.DB {
	t.Helper()
	db := newProbePostgres(t)
	_, err := db.Exec(`
		CREATE TABLE groups (id BIGINT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'active', deleted_at TIMESTAMPTZ);
		CREATE TABLE account_groups (
			account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
			priority INTEGER NOT NULL DEFAULT 1,
			PRIMARY KEY(account_id, group_id)
		);
		INSERT INTO proxies(id) VALUES(3), (4);
		INSERT INTO groups(id) VALUES(7), (8);
		INSERT INTO accounts(id, proxy_id, updated_at) VALUES
			(1, 3, '2020-01-01Z'), (2, 3, '2020-01-01Z'), (3, 4, '2020-01-01Z');
	`)
	require.NoError(t, err)
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "238_scheduler_account_revisions.sql"))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	return db
}

func schedulerRevision(t *testing.T, db *sql.DB, id int64) time.Time {
	t.Helper()
	var revision time.Time
	require.NoError(t, db.QueryRow("SELECT updated_at FROM accounts WHERE id = $1", id).Scan(&revision))
	return revision
}

func schedulerRelatedSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	var value string
	require.NoError(t, db.QueryRow(`
		SELECT jsonb_build_object(
			'accounts', (SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM accounts a),
			'proxies', (SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM proxies p),
			'groups', (SELECT jsonb_agg(to_jsonb(g) ORDER BY id) FROM groups g),
			'bindings', (SELECT jsonb_agg(to_jsonb(b) ORDER BY account_id, group_id) FROM account_groups b),
			'outbox', (SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM scheduler_outbox o)
		)::text
	`).Scan(&value))
	return value
}

func TestSchedulerRevisionPostgresMonotonicAcrossTransactions(t *testing.T) {
	db := newSchedulerRevisionPostgres(t)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var start time.Time
	require.NoError(t, tx.QueryRow("SELECT NOW()").Scan(&start))
	_, err = db.Exec("UPDATE accounts SET schedulable = FALSE, updated_at = NOW() WHERE id = 1")
	require.NoError(t, err)
	current := schedulerRevision(t, db, 1)
	require.True(t, current.After(start))
	var next time.Time
	require.NoError(t, tx.QueryRow("UPDATE accounts SET updated_at = NOW() WHERE id = 1 RETURNING updated_at").Scan(&next))
	require.True(t, next.After(current), "an older transaction must not lower the cache high-water mark")
	var again time.Time
	require.NoError(t, tx.QueryRow("UPDATE accounts SET updated_at = NOW() WHERE id = 1 RETURNING updated_at").Scan(&again))
	require.True(t, again.After(next), "two writes in one transaction must have distinct revisions")
	require.NoError(t, tx.Commit())
	require.Equal(t, again, schedulerRevision(t, db, 1))
}

func TestSchedulerRevisionPostgresMembershipLifecycle(t *testing.T) {
	db := newSchedulerRevisionPostgres(t)
	before := schedulerRevision(t, db, 1)
	_, err := db.Exec("INSERT INTO account_groups(account_id, group_id) VALUES(1,7), (1,8)")
	require.NoError(t, err)
	inserted := schedulerRevision(t, db, 1)
	require.True(t, inserted.After(before))
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = 1").Scan(&count))
	require.Equal(t, 1, count, "bulk membership writes publish once per account")

	_, err = db.Exec("UPDATE account_groups SET priority = 5 WHERE account_id = 1")
	require.NoError(t, err)
	reordered := schedulerRevision(t, db, 1)
	require.True(t, reordered.After(inserted))
	other := schedulerRevision(t, db, 2)
	_, err = db.Exec("UPDATE account_groups SET account_id = 2 WHERE account_id = 1 AND group_id = 7")
	require.NoError(t, err)
	require.True(t, schedulerRevision(t, db, 1).After(reordered))
	require.True(t, schedulerRevision(t, db, 2).After(other))

	before = schedulerRevision(t, db, 1)
	_, err = db.Exec("DELETE FROM account_groups WHERE account_id = 1")
	require.NoError(t, err)
	require.True(t, schedulerRevision(t, db, 1).After(before))
	var containsOldGroup bool
	require.NoError(t, db.QueryRow(`
		SELECT payload->'group_ids' @> '[8]'::jsonb FROM scheduler_outbox
		WHERE account_id = 1 ORDER BY id DESC LIMIT 1
	`).Scan(&containsOldGroup))
	require.True(t, containsOldGroup, "clearing the last group still invalidates its old bucket")
}

func TestSchedulerRevisionPostgresProxyAndGroupChanges(t *testing.T) {
	db := newSchedulerRevisionPostgres(t)
	_, err := db.Exec("INSERT INTO account_groups(account_id, group_id) VALUES(1,7), (2,7)")
	require.NoError(t, err)
	for _, query := range []string{
		"UPDATE proxies SET status = 'disabled' WHERE id = 3",
		"UPDATE groups SET status = 'disabled' WHERE id = 7",
		"UPDATE groups SET deleted_at = NOW() WHERE id = 7",
		"DELETE FROM groups WHERE id = 7",
	} {
		before := []time.Time{schedulerRevision(t, db, 1), schedulerRevision(t, db, 2), schedulerRevision(t, db, 3)}
		_, err := db.Exec(query)
		require.NoError(t, err, query)
		require.True(t, schedulerRevision(t, db, 1).After(before[0]), query)
		require.True(t, schedulerRevision(t, db, 2).After(before[1]), query)
		require.Equal(t, before[2], schedulerRevision(t, db, 3), "unrelated account must be unchanged")
	}
}

func TestSchedulerRevisionPostgresNotificationFailureRollsBack(t *testing.T) {
	for _, query := range []string{
		"DELETE FROM account_groups WHERE account_id = 1",
		"UPDATE account_groups SET account_id = 2 WHERE account_id = 1",
		"UPDATE proxies SET status = 'disabled' WHERE id = 3",
		"UPDATE groups SET deleted_at = NOW() WHERE id = 7",
		"DELETE FROM groups WHERE id = 7",
	} {
		t.Run(query, func(t *testing.T) {
			db := newSchedulerRevisionPostgres(t)
			_, err := db.Exec("INSERT INTO account_groups(account_id, group_id) VALUES(1,7)")
			require.NoError(t, err)
			_, err = db.Exec(`
				CREATE FUNCTION reject_scheduler_notification() RETURNS TRIGGER LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'injected notification failure'; END; $$;
				CREATE TRIGGER reject_scheduler_notification BEFORE INSERT ON scheduler_outbox
				FOR EACH ROW EXECUTE FUNCTION reject_scheduler_notification();
			`)
			require.NoError(t, err)
			before := schedulerRelatedSnapshot(t, db)
			_, err = db.Exec(query)
			require.ErrorContains(t, err, "injected notification failure")
			require.Equal(t, before, schedulerRelatedSnapshot(t, db))
		})
	}
}

func TestSchedulerRevisionPostgresDeletedAndNoOpRelations(t *testing.T) {
	db := newSchedulerRevisionPostgres(t)
	_, err := db.Exec("UPDATE accounts SET deleted_at = NOW() WHERE id = 2")
	require.NoError(t, err)
	deleted := schedulerRevision(t, db, 2)
	before := schedulerRelatedSnapshot(t, db)
	_, err = db.Exec("DELETE FROM account_groups WHERE account_id = 999")
	require.NoError(t, err)
	require.Equal(t, before, schedulerRelatedSnapshot(t, db))
	_, err = db.Exec("UPDATE proxies SET status = 'disabled' WHERE id = 3")
	require.NoError(t, err)
	require.Equal(t, deleted, schedulerRevision(t, db, 2))
}
