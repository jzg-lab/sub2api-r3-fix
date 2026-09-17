package repository

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestOpenAIProbePostgresOwnershipMigrationPreservesHistoricalPauses(t *testing.T) {
	db := newProbePostgresWithOwnership(t, false)
	_, err := db.Exec(`
		INSERT INTO accounts(id, platform, type, schedulable, status, extra, parent_account_id, deleted_at, updated_at)
		VALUES
			(1, 'openai', 'oauth', FALSE, 'active', '{}', NULL, NULL, NOW()),
			(2, 'openai', 'oauth', FALSE, 'active', '{"openai_downgrade_qualification":true}', NULL, NULL, NOW()),
			(3, 'openai', 'oauth', FALSE, 'error', '{}', NULL, NULL, NOW()),
			(4, 'openai', 'oauth', TRUE, 'active', '{}', NULL, NULL, NOW()),
			(5, 'openai', 'apikey', FALSE, 'active', '{}', NULL, NULL, NOW()),
			(6, 'anthropic', 'oauth', FALSE, 'active', '{}', NULL, NULL, NOW()),
			(7, 'openai', 'oauth', FALSE, 'active', '{}', 1, NULL, NOW()),
			(8, 'openai', 'oauth', FALSE, 'active', '{}', NULL, NOW(), NOW());
	`)
	require.NoError(t, err)
	var before string
	const accountSnapshot = "SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM accounts a"
	require.NoError(t, db.QueryRow(accountSnapshot).Scan(&before))
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "239_openai_probe_ownership.sql"))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	var paused []int64
	require.NoError(t, db.QueryRow(`
		SELECT array_agg(account_id ORDER BY account_id)
		FROM openai_downgrade_probe_controls WHERE manual_paused AND owned_error IS NULL
	`).Scan(pq.Array(&paused)))
	require.Equal(t, []int64{1, 2, 3}, paused)
	var after string
	require.NoError(t, db.QueryRow(accountSnapshot).Scan(&after))
	require.Equal(t, before, after, "migration must not infer ownership or rewrite historical accounts")
}

func TestOpenAIProbePostgresControlReadFailsClosed(t *testing.T) {
	db := newProbePostgresWithOwnership(t, false)
	repo := &openAIDowngradeProbeRepository{db: db}
	allowed, err := repo.CanRunOpenAIDowngradeProbe(context.Background(), 1)
	require.Error(t, err)
	require.False(t, allowed)
	due, err := repo.ListDueOpenAIDowngradeStates(context.Background(), time.Now(), 1)
	require.Error(t, err)
	require.Empty(t, due)
}

func probeControlAccountRepo(db *sql.DB) *accountRepository {
	return &accountRepository{
		client: dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db))), sql: db,
	}
}

func readProbeMutationGeneration(t *testing.T, db *sql.DB, m *service.OpenAIDowngradeMutation) {
	t.Helper()
	require.NoError(t, db.QueryRow(`
		SELECT a.updated_at, a.status, a.schedulable, a.proxy_id, s.updated_at
		FROM accounts a JOIN openai_downgrade_probe_states s ON s.account_id = a.id
		WHERE a.id = $1
	`, m.AccountID).Scan(&m.ExpectedAccountUpdatedAt, &m.ExpectedStatus, &m.ExpectedSchedulable,
		&m.ExpectedProxyID, &m.ExpectedStateUpdatedAt))
}

func TestOpenAIProbePostgresManualSchedulingTargetsAndResume(t *testing.T) {
	db := newProbePostgres(t)
	mutation := seedProbePostgres(t, db)
	_, err := db.Exec(`
		INSERT INTO accounts(id, platform, type, parent_account_id, deleted_at, updated_at) VALUES
			(8, 'anthropic', 'oauth', NULL, NULL, NOW()),
			(9, 'openai', 'apikey', NULL, NULL, NOW()),
			(10, 'openai', 'oauth', 7, NULL, NOW()),
			(11, 'openai', 'oauth', NULL, NOW(), NOW());
	`)
	require.NoError(t, err)
	repo := probeControlAccountRepo(db)
	paused := false
	rows, err := repo.BulkUpdate(context.Background(), []int64{7, 7, 8, 9, 10, 11, 999},
		service.AccountBulkUpdate{Schedulable: &paused, ManualScheduling: true})
	require.NoError(t, err)
	require.EqualValues(t, 4, rows)
	var controls int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM openai_downgrade_probe_controls").Scan(&controls))
	require.Equal(t, 1, controls)
	probes := &openAIDowngradeProbeRepository{db: db}
	allowed, err := probes.CanRunOpenAIDowngradeProbe(context.Background(), 7)
	require.NoError(t, err)
	require.False(t, allowed)
	readProbeMutationGeneration(t, db, mutation)
	before := probePostgresSnapshot(t, db)
	require.ErrorIs(t, probes.CommitOpenAIDowngradeMutation(context.Background(), mutation), service.ErrOpenAIProbeStale)
	require.Equal(t, before, probePostgresSnapshot(t, db), "even a fresh probe may not undo manual pause")

	_, err = repo.BulkUpdate(context.Background(), []int64{7}, service.AccountBulkUpdate{Extra: map[string]any{"unrelated": true}})
	require.NoError(t, err)
	allowed, err = probes.CanRunOpenAIDowngradeProbe(context.Background(), 7)
	require.NoError(t, err)
	require.False(t, allowed, "ordinary Extra edits must not erase pause")
	before = probePostgresSnapshot(t, db)
	_, err = db.Exec("UPDATE accounts SET status = 'active', error_message = '', schedulable = TRUE WHERE id = 7")
	require.ErrorContains(t, err, "manually paused account cannot be scheduled")
	require.Equal(t, before, probePostgresSnapshot(t, db), "other recovery writers cannot override pause")
	resumed := true
	rows, err = repo.BulkUpdate(context.Background(), []int64{7},
		service.AccountBulkUpdate{Schedulable: &resumed, ManualScheduling: true})
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	allowed, err = probes.CanRunOpenAIDowngradeProbe(context.Background(), 7)
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestOpenAIProbePostgresManualSchedulingRollback(t *testing.T) {
	for _, table := range []string{"openai_downgrade_probe_controls", "scheduler_outbox"} {
		t.Run(table, func(t *testing.T) {
			db := newProbePostgres(t)
			seedProbePostgres(t, db)
			before := probePostgresSnapshot(t, db)
			_, err := db.Exec(`
				CREATE FUNCTION reject_control_write() RETURNS trigger LANGUAGE plpgsql AS
				$$ BEGIN RAISE EXCEPTION 'injected control failure'; END $$;
				CREATE TRIGGER reject_write BEFORE INSERT OR UPDATE ON ` + pq.QuoteIdentifier(table) + `
				FOR EACH ROW EXECUTE FUNCTION reject_control_write();
			`)
			require.NoError(t, err)
			paused := false
			count, err := probeControlAccountRepo(db).BulkUpdate(context.Background(), []int64{7},
				service.AccountBulkUpdate{Schedulable: &paused, ManualScheduling: true})
			require.ErrorContains(t, err, "injected control failure")
			require.Zero(t, count)
			require.Equal(t, before, probePostgresSnapshot(t, db))
		})
	}
}

func TestOpenAIProbePostgresManualPauseDuringProbe(t *testing.T) {
	db := newProbePostgres(t)
	mutation := seedProbePostgres(t, db)
	repo := probeControlAccountRepo(db)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := repo.client.Tx(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	paused := false
	_, err = repo.BulkUpdate(dbent.NewTxContext(ctx, tx), []int64{7},
		service.AccountBulkUpdate{Schedulable: &paused, ManualScheduling: true})
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() {
		result <- (&openAIDowngradeProbeRepository{db: db}).CommitOpenAIDowngradeMutation(ctx, mutation)
	}()
	require.NoError(t, tx.Commit())
	require.ErrorIs(t, <-result, service.ErrOpenAIProbeStale)
	var isPaused, schedulable bool
	require.NoError(t, db.QueryRow(`
		SELECT c.manual_paused, a.schedulable FROM accounts a
		JOIN openai_downgrade_probe_controls c ON c.account_id = a.id WHERE a.id = 7
	`).Scan(&isPaused, &schedulable))
	require.True(t, isPaused)
	require.False(t, schedulable)
}

func TestOpenAIProbePostgresOwnedErrorLifecycle(t *testing.T) {
	for _, rewrite := range []string{"none", "same_error", "same_status", "new_error"} {
		t.Run(rewrite, func(t *testing.T) {
			db := newProbePostgres(t)
			mutation := seedProbePostgres(t, db)
			repo := &openAIDowngradeProbeRepository{db: db}
			message := "probe-owned authentication failure"
			mutation.ErrorMessage = &message
			require.NoError(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation))
			allowed, err := repo.CanRunOpenAIDowngradeProbe(context.Background(), 7)
			require.NoError(t, err)
			require.True(t, allowed)
			// An ordinary cooldown/fallback update must retain the owner.
			readProbeMutationGeneration(t, db, mutation)
			mutation.ErrorMessage = nil
			require.NoError(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation))
			var owner sql.NullString
			require.NoError(t, db.QueryRow("SELECT owned_error FROM openai_downgrade_probe_controls WHERE account_id = 7").Scan(&owner))
			require.Equal(t, sql.NullString{String: message, Valid: true}, owner)
			switch rewrite {
			case "same_error":
				_, err = db.Exec("UPDATE accounts SET error_message = error_message WHERE id = 7")
			case "same_status":
				_, err = db.Exec("UPDATE accounts SET status = status WHERE id = 7")
			case "new_error":
				_, err = db.Exec("UPDATE accounts SET error_message = 'manual replacement' WHERE id = 7")
			}
			require.NoError(t, err)
			readProbeMutationGeneration(t, db, mutation)
			enabled := true
			mutation.Schedulable, mutation.RecoverOwnedError = &enabled, true
			before := probePostgresSnapshot(t, db)
			err = repo.CommitOpenAIDowngradeMutation(context.Background(), mutation)
			if rewrite != "none" {
				require.ErrorIs(t, err, service.ErrOpenAIProbeStale)
				require.Equal(t, before, probePostgresSnapshot(t, db))
				return
			}
			require.NoError(t, err)
			var status, errorMessage string
			var schedulable bool
			require.NoError(t, db.QueryRow("SELECT status, error_message, schedulable FROM accounts WHERE id = 7").
				Scan(&status, &errorMessage, &schedulable))
			require.Equal(t, service.StatusActive, status)
			require.Empty(t, errorMessage)
			require.True(t, schedulable)
			require.NoError(t, db.QueryRow("SELECT owned_error FROM openai_downgrade_probe_controls WHERE account_id = 7").Scan(&owner))
			require.False(t, owner.Valid)
		})
	}
}

func TestOpenAIProbePostgresOwnedErrorWriteRollback(t *testing.T) {
	db := newProbePostgres(t)
	mutation := seedProbePostgres(t, db)
	message := "probe-owned failure"
	mutation.ErrorMessage = &message
	before := probePostgresSnapshot(t, db)
	_, err := db.Exec(`
		CREATE FUNCTION reject_owner() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'injected ownership failure'; END $$;
		CREATE TRIGGER reject_write BEFORE INSERT OR UPDATE ON openai_downgrade_probe_controls
		FOR EACH ROW EXECUTE FUNCTION reject_owner();
	`)
	require.NoError(t, err)
	err = (&openAIDowngradeProbeRepository{db: db}).CommitOpenAIDowngradeMutation(context.Background(), mutation)
	require.ErrorContains(t, err, "injected ownership failure")
	require.Equal(t, before, probePostgresSnapshot(t, db))
}

func TestOpenAIProbePostgresDueFiltersBeforeRanking(t *testing.T) {
	db := newProbePostgres(t)
	seedProbePostgres(t, db)
	now := time.Now().UTC()
	_, err := db.Exec(`
		INSERT INTO accounts(id, proxy_id, updated_at, schedulable) VALUES
			(8, 3, NOW(), FALSE), (9, 3, NOW(), TRUE);
		UPDATE accounts SET schedulable = FALSE WHERE id = 7;
		INSERT INTO openai_downgrade_probe_controls(account_id, manual_paused) VALUES(7, TRUE);
		UPDATE accounts SET status = 'error', error_message = 'owned failure' WHERE id = 8;
		INSERT INTO openai_downgrade_probe_controls(account_id, owned_error) VALUES(8, 'owned failure');
	`)
	require.NoError(t, err)
	repo := &openAIDowngradeProbeRepository{db: db}
	for _, id := range []int64{8, 9} {
		_, err = repo.EnsureOpenAIDowngradeState(context.Background(), id, nil, now.Add(-time.Minute))
		require.NoError(t, err)
	}
	due, err := repo.ListDueOpenAIDowngradeStates(context.Background(), now, 100)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.EqualValues(t, 8, due[0].AccountID, "owned on_duty error must remain recoverable")
	_, err = db.Exec("UPDATE accounts SET error_message = error_message WHERE id = 8")
	require.NoError(t, err)
	due, err = repo.ListDueOpenAIDowngradeStates(context.Background(), now, 100)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.EqualValues(t, 9, due[0].AccountID, "unowned error and pause must not consume the IP's queue slot")
}
