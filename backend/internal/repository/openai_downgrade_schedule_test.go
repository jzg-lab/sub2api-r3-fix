package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func seedLongProbeSchedule(t *testing.T, db *sql.DB, now time.Time, age time.Duration) {
	t.Helper()
	seedProbePostgres(t, db)
	last := now.Add(-age)
	_, err := db.Exec(`
		UPDATE openai_downgrade_probe_states
		SET last_probe_at=$1, next_probe_at=$2, updated_at=$1 WHERE account_id=7
	`, last, now.Add(7*24*time.Hour))
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO openai_downgrade_probe_results(account_id, proxy_id, http_status, created_at)
		VALUES(7, 3, 429, $1)
	`, last.Add(time.Second))
	require.NoError(t, err)
	_, err = db.Exec(`
		UPDATE accounts SET rate_limited_at=$1, rate_limit_reset_at=$2 WHERE id=7
	`, last, now.Add(7*24*time.Hour))
	require.NoError(t, err)
}

func TestOpenAIProbeScheduleReconciliationArgumentsAndErrors(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &openAIDowngradeProbeRepository{db: db}
	ctx := context.Background()
	now := time.Now()
	_, err = repo.ReconcileOpenAIRateLimitProbeSchedules(ctx, now, 0)
	require.Error(t, err)
	_, err = repo.ReconcileOpenAIRateLimitProbeSchedules(ctx, time.Time{}, time.Hour)
	require.Error(t, err)
	failure := errors.New("write failed")
	mock.ExpectExec("WITH candidates AS MATERIALIZED").WithArgs(now, float64(22*60*60)).
		WillReturnError(failure)
	count, err := repo.ReconcileOpenAIRateLimitProbeSchedules(ctx, now, 22*time.Hour)
	require.ErrorIs(t, err, failure)
	require.Zero(t, count)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAIProbePostgresScheduleReconciliationAdoption(t *testing.T) {
	for _, age := range []time.Duration{time.Hour, 48 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			db := newProbePostgres(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			seedLongProbeSchedule(t, db, now, age)
			var accountBefore string
			require.NoError(t, db.QueryRow("SELECT to_jsonb(a)::text FROM accounts a WHERE id=7").Scan(&accountBefore))
			repo := &openAIDowngradeProbeRepository{db: db}
			count, err := repo.ReconcileOpenAIRateLimitProbeSchedules(context.Background(), now, 22*time.Hour)
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
			state, err := repo.GetOpenAIDowngradeState(context.Background(), 7)
			require.NoError(t, err)
			earliest := now.Add(-age + 22*time.Hour)
			latest := now.Add(-age + 27*time.Hour + 30*time.Minute)
			if earliest.Before(now.Add(15 * time.Minute)) {
				earliest = now.Add(15 * time.Minute)
			}
			if latest.Before(now.Add(45 * time.Minute)) {
				latest = now.Add(45 * time.Minute)
			}
			require.False(t, state.NextProbeAt.Before(earliest))
			require.False(t, state.NextProbeAt.After(latest))
			require.True(t, now.Equal(state.UpdatedAt))
			require.True(t, now.Add(-age).Equal(*state.LastProbeAt))
			var accountAfter string
			require.NoError(t, db.QueryRow("SELECT to_jsonb(a)::text FROM accounts a WHERE id=7").Scan(&accountAfter))
			require.Equal(t, accountBefore, accountAfter, "schedule adoption cannot clear cooldown or change routing")
			var eventType string
			var before, after time.Time
			require.NoError(t, db.QueryRow(`
				SELECT event_type, (details->>'previous_next_probe_at')::timestamptz,
					(details->>'next_probe_at')::timestamptz
				FROM openai_downgrade_probe_events WHERE account_id=7
			`).Scan(&eventType, &before, &after))
			require.Equal(t, "rate_limit_schedule_reconciled", eventType)
			require.True(t, now.Add(7*24*time.Hour).Equal(before))
			require.True(t, state.NextProbeAt.Equal(after))
			snapshot := probePostgresSnapshot(t, db)
			restarted := &openAIDowngradeProbeRepository{db: db}
			count, err = restarted.ReconcileOpenAIRateLimitProbeSchedules(context.Background(), now.Add(time.Minute), 22*time.Hour)
			require.NoError(t, err)
			require.Zero(t, count)
			require.Equal(t, snapshot, probePostgresSnapshot(t, db), "replay cannot redraw jitter or postpone adoption")
			due, err := restarted.ListDueOpenAIDowngradeStates(context.Background(), state.NextProbeAt, 10)
			require.NoError(t, err)
			require.Len(t, due, 1, "the existing real due consumer must see the adopted schedule")
		})
	}
}

func TestOpenAIProbePostgresScheduleReconciliationEligibility(t *testing.T) {
	for name, change := range map[string]string{
		"valid_sparse":   "UPDATE openai_downgrade_probe_states SET next_probe_at=last_probe_at+INTERVAL '26 hours'",
		"already_due":    "UPDATE openai_downgrade_probe_states SET next_probe_at=last_probe_at",
		"no_probe":       "UPDATE openai_downgrade_probe_states SET last_probe_at=NULL",
		"no_evidence":    "DELETE FROM openai_downgrade_probe_results",
		"latest_success": "INSERT INTO openai_downgrade_probe_results(account_id,proxy_id,http_status) VALUES(7,3,200)",
		"old_evidence":   "UPDATE openai_downgrade_probe_results SET created_at=created_at-INTERVAL '2 hours'",
		"wrong_route":    "UPDATE openai_downgrade_probe_results SET proxy_id=4",
		"new_route":      "UPDATE accounts SET proxy_id=4",
		"manual_pause": `
			UPDATE accounts SET schedulable=FALSE WHERE id=7;
			INSERT INTO openai_downgrade_probe_controls(account_id,manual_paused) VALUES(7,TRUE);
		`,
		"unschedulable": "UPDATE accounts SET schedulable=FALSE",
		"disabled":      "UPDATE accounts SET status='disabled'",
		"unowned_error": "UPDATE accounts SET status='error', error_message='unowned'",
		"deleted":       "UPDATE accounts SET deleted_at=NOW()",
		"expired":       "UPDATE accounts SET expires_at=NOW()-INTERVAL '1 minute'",
		"shadow":        "UPDATE accounts SET parent_account_id=8",
		"platform":      "UPDATE accounts SET platform='anthropic'",
		"api_key":       "UPDATE accounts SET type='apikey'",
	} {
		t.Run(name, func(t *testing.T) {
			db := newProbePostgres(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			seedLongProbeSchedule(t, db, now, time.Hour)
			_, err := db.Exec(change)
			require.NoError(t, err)
			before := probePostgresSnapshot(t, db)
			repo := &openAIDowngradeProbeRepository{db: db}
			count, err := repo.ReconcileOpenAIRateLimitProbeSchedules(context.Background(), now, 22*time.Hour)
			require.NoError(t, err)
			require.Zero(t, count)
			require.Equal(t, before, probePostgresSnapshot(t, db))
		})
	}
}

func TestOpenAIProbePostgresScheduleReconciliationOwnedError(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		name := "owned"
		if revoked {
			name = "explicit_rewrite_revokes_ownership"
		}
		t.Run(name, func(t *testing.T) {
			db := newProbePostgres(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			seedLongProbeSchedule(t, db, now, time.Hour)
			_, err := db.Exec(`
				UPDATE accounts SET status='error', schedulable=FALSE, error_message='probe-owned';
				INSERT INTO openai_downgrade_probe_controls(account_id,owned_error) VALUES(7,'probe-owned');
				UPDATE openai_downgrade_probe_states SET state='circuit_open';
			`)
			require.NoError(t, err)
			if revoked {
				_, err = db.Exec("UPDATE accounts SET error_message='probe-owned' WHERE id=7")
				require.NoError(t, err)
			}
			before := probePostgresSnapshot(t, db)
			repo := &openAIDowngradeProbeRepository{db: db}
			count, err := repo.ReconcileOpenAIRateLimitProbeSchedules(context.Background(), now, 22*time.Hour)
			require.NoError(t, err)
			if revoked {
				require.Zero(t, count)
				require.Equal(t, before, probePostgresSnapshot(t, db))
			} else {
				require.EqualValues(t, 1, count)
			}
		})
	}
}

func TestOpenAIProbePostgresScheduleReconciliationRollback(t *testing.T) {
	db := newProbePostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedLongProbeSchedule(t, db, now, time.Hour)
	_, err := db.Exec(`
		CREATE FUNCTION reject_schedule_event() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'schedule event failed'; END $$;
		CREATE TRIGGER reject_event BEFORE INSERT ON openai_downgrade_probe_events
		FOR EACH ROW EXECUTE FUNCTION reject_schedule_event();
	`)
	require.NoError(t, err)
	before := probePostgresSnapshot(t, db)
	repo := &openAIDowngradeProbeRepository{db: db}
	count, err := repo.ReconcileOpenAIRateLimitProbeSchedules(context.Background(), now, 22*time.Hour)
	require.ErrorContains(t, err, "schedule event failed")
	require.Zero(t, count)
	require.Equal(t, before, probePostgresSnapshot(t, db))
}

func TestOpenAIProbePostgresScheduleReconciliationConcurrentState(t *testing.T) {
	db := newProbePostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedLongProbeSchedule(t, db, now, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	var id int64
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT account_id FROM openai_downgrade_probe_states WHERE account_id=7 FOR UPDATE").Scan(&id))
	type result struct {
		count int64
		err   error
	}
	done := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		repo := &openAIDowngradeProbeRepository{db: db}
		count, err := repo.ReconcileOpenAIRateLimitProbeSchedules(ctx, now, 22*time.Hour)
		done <- result{count, err}
	}()
	defer func() {
		cancel()
		_ = tx.Rollback()
		// On an early assertion failure, still join the bounded database worker.
		select {
		case <-finished:
		case <-time.After(6 * time.Second):
		}
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM pg_stat_activity
				WHERE pid<>pg_backend_pid() AND wait_event_type='Lock'
				AND query LIKE '%WITH candidates AS MATERIALIZED%')
		`).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 10*time.Millisecond)
	_, err = tx.ExecContext(ctx, `
		UPDATE openai_downgrade_probe_states
		SET updated_at=$1, next_probe_at=$2 WHERE account_id=7
	`, now.Add(time.Second), now.Add(3*24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	got := <-done
	require.NoError(t, got.err)
	require.Zero(t, got.count, "an old reconciliation must not overwrite a newer schedule")
	state, err := (&openAIDowngradeProbeRepository{db: db}).GetOpenAIDowngradeState(ctx, 7)
	require.NoError(t, err)
	require.True(t, now.Add(3*24*time.Hour).Equal(state.NextProbeAt))
	var events int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM openai_downgrade_probe_events").Scan(&events))
	require.Zero(t, events)
}

func TestOpenAIProbePostgresScheduleReconciliationBusyAccount(t *testing.T) {
	for name, change := range map[string]string{
		"unchanged": "",
		"manual_pause": `
			UPDATE accounts SET schedulable=FALSE, updated_at=clock_timestamp() WHERE id=7;
			INSERT INTO openai_downgrade_probe_controls(account_id,manual_paused) VALUES(7,TRUE);
		`,
		"route_changed": "UPDATE accounts SET proxy_id=4, updated_at=clock_timestamp() WHERE id=7",
		"deleted":       "UPDATE accounts SET deleted_at=clock_timestamp(), updated_at=clock_timestamp() WHERE id=7",
	} {
		t.Run(name, func(t *testing.T) {
			db := newProbePostgres(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			seedLongProbeSchedule(t, db, now, time.Hour)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer tx.Rollback()
			var id int64
			require.NoError(t, tx.QueryRowContext(ctx, "SELECT id FROM accounts WHERE id=7 FOR UPDATE").Scan(&id))
			if change != "" {
				_, err = tx.ExecContext(ctx, change)
				require.NoError(t, err)
			}
			before := probePostgresSnapshot(t, db)
			repo := &openAIDowngradeProbeRepository{db: db}
			count, err := repo.ReconcileOpenAIRateLimitProbeSchedules(ctx, now, 22*time.Hour)
			require.NoError(t, err)
			require.Zero(t, count, "an in-flight account write must not adopt an old schedule")
			require.Equal(t, before, probePostgresSnapshot(t, db))
			require.NoError(t, tx.Commit())

			before = probePostgresSnapshot(t, db)
			count, err = repo.ReconcileOpenAIRateLimitProbeSchedules(ctx, now, 22*time.Hour)
			require.NoError(t, err)
			if change == "" {
				require.EqualValues(t, 1, count, "a skipped account must be reconsidered after its writer releases")
			} else {
				require.Zero(t, count, "the next scan must respect the committed account change")
				require.Equal(t, before, probePostgresSnapshot(t, db))
			}
		})
	}
}
