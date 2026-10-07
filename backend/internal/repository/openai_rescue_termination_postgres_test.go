package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestIndependentReviewStaleEditAfterEntry(t *testing.T) {
	for _, groups := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("groups=%v/current_enabled=%v", groups, enabled), func(t *testing.T) {
				repo, db, account, _ := newRescueTerminationPostgres(t)
				if enabled {
					account.Schedulable = false
					lane := verifiedRescueLane(t, repo, db, account.ID)
					require.NoError(t, lane.GraduateRescue(t.Context(), account.ID, "qualification_pass"))
					current, err := repo.GetByID(t.Context(), account.ID)
					require.NoError(t, err)
					account.GroupIDs = current.GroupIDs
				} else {
					_, err := db.Exec(`UPDATE accounts SET extra=extra - 'openai_rescue_lane' WHERE id=$1`, account.ID)
					require.NoError(t, err)
					account, err = repo.GetByID(t.Context(), account.ID)
					require.NoError(t, err)
					entered, err := repo.EnterOpenAIRescue(t.Context(), account, map[string]any{"entered_at": time.Now().UTC().Format(time.RFC3339Nano)}, account.GroupIDs[0], true)
					require.NoError(t, err)
					require.True(t, entered)
				}
				account.Name = "saved stale form"
				var err error
				if groups {
					err = repo.UpdateWithAccountGroups(t.Context(), account, account.GroupIDs, nil, nil, nil, nil)
				} else {
					err = repo.UpdateWithAccountBillingSettings(t.Context(), account, nil, nil, nil, nil)
				}
				require.NoError(t, err)
				current, err := repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				require.Equal(t, enabled, current.Schedulable)
				require.Equal(t, current.Schedulable, account.Schedulable)
				require.Equal(t, "saved stale form", current.Name)
			})
		}
	}
}

func TestIndependentReviewCorruptSnapshotPostgres(t *testing.T) {
	for _, snapshot := range []string{"missing", "null", `"broken"`, `[1,"bad"]`, `[1.5]`, `[0]`, `[1,1]`} {
		t.Run(snapshot, func(t *testing.T) {
			repo, db, account, _ := newRescueTerminationPostgres(t)
			query := `UPDATE accounts SET extra=extra #- '{openai_rescue_lane,orig_group_ids}' WHERE id=$1`
			if snapshot == "missing" {
				_, err := db.Exec(query, account.ID)
				require.NoError(t, err)
			} else {
				_, err := db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_rescue_lane,orig_group_ids}',$2::jsonb) WHERE id=$1`, account.ID, snapshot)
				require.NoError(t, err)
			}
			before, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			lane := service.NewOpenAIRescueLane(repo, nil, func() service.OpenAIRescueLaneConfig {
				return service.OpenAIRescueLaneConfig{Enabled: true, GroupID: account.GroupIDs[0]}
			}, func(context.Context, int64) error { return nil })
			_, _, _, err = lane.RunReconcileSweep(t.Context())
			require.NoError(t, err)
			current, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			oldFields := before.Extra["openai_rescue_lane"].(map[string]any)
			fields := current.Extra["openai_rescue_lane"].(map[string]any)
			require.Equal(t, oldFields["orig_group_ids"], fields["orig_group_ids"])
			_, oldPresent := oldFields["orig_group_ids"]
			_, present := fields["orig_group_ids"]
			require.Equal(t, oldPresent, present)
			stopped, err := lane.TerminateRescue(t.Context(), account.ID)
			require.Error(t, err)
			require.False(t, stopped)
			require.Error(t, lane.GraduateRescue(t.Context(), account.ID, "test"))
			after, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			require.Equal(t, current.Extra, after.Extra)
			require.Equal(t, current.GroupIDs, after.GroupIDs)
		})
	}
}

func TestIndependentReviewPauseBeforeTermination(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprint(paused), func(t *testing.T) {
			repo, db, account, _ := newRescueTerminationPostgres(t)
			_, err := db.Exec(`UPDATE accounts SET schedulable=false WHERE id=$1`, account.ID)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO openai_downgrade_probe_controls(account_id,manual_paused) VALUES($1,$2)`, account.ID, paused)
			require.NoError(t, err)
			stopped, err := repo.TerminateOpenAIRescue(t.Context(), account.ID, time.Now())
			require.NoError(t, err)
			require.True(t, stopped)
			var actual bool
			require.NoError(t, db.QueryRow(`SELECT manual_paused FROM openai_downgrade_probe_controls WHERE account_id=$1`, account.ID).Scan(&actual))
			require.Equal(t, paused, actual, "termination must preserve manual pause")
			current, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			entered, err := repo.EnterOpenAIRescue(t.Context(), current, map[string]any{"entered_at": time.Now().UTC().Format(time.RFC3339Nano)}, account.GroupIDs[0], true)
			require.NoError(t, err)
			require.True(t, entered)
			require.NoError(t, db.QueryRow(`SELECT manual_paused FROM openai_downgrade_probe_controls WHERE account_id=$1`, account.ID).Scan(&actual))
			require.Equal(t, paused, actual, "reentry must preserve manual pause")
			store := &openAIDowngradeProbeRepository{db: db}
			allowed, err := store.CanRunOpenAIDowngradeProbe(t.Context(), account.ID)
			require.NoError(t, err)
			require.Equal(t, !paused, allowed)
			runner := service.NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			result, err := runner.ReenableOpenAIAccount(t.Context(), account.ID, false)
			if paused {
				require.ErrorContains(t, err, "manual-paused")
				require.Nil(t, result)
				require.NoError(t, db.QueryRow(`SELECT manual_paused FROM openai_downgrade_probe_controls WHERE account_id=$1`, account.ID).Scan(&actual))
				require.True(t, actual)
				result, err = runner.ReenableOpenAIAccount(t.Context(), account.ID, true)
				require.NoError(t, err)
				require.True(t, result.Unpaused)
			} else {
				require.NoError(t, err)
				require.True(t, result.ProbeQueued)
				require.False(t, result.Unpaused)
			}
		})
	}
}

type staleRescueSweepRepository struct {
	*accountRepository
	afterSnapshot func()
}

func (r *staleRescueSweepRepository) ListByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	accounts, err := r.accountRepository.ListByPlatform(ctx, platform)
	if err == nil {
		r.afterSnapshot()
	}
	return accounts, err
}

func TestIndependentReviewStaleSweepExitPostgres(t *testing.T) {
	repo, db, account, _ := newRescueTerminationPostgres(t)
	_, err := db.Exec(`UPDATE accounts SET extra=jsonb_set(extra,'{openai_rescue_lane,exit_reason}','"auth_rejected"') WHERE id=$1`, account.ID)
	require.NoError(t, err)
	var newMarker any
	snapshotRepo := &staleRescueSweepRepository{accountRepository: repo, afterSnapshot: func() {
		stopped, err := repo.TerminateOpenAIRescue(t.Context(), account.ID, time.Now())
		require.NoError(t, err)
		require.True(t, stopped)
		current, err := repo.GetByID(t.Context(), account.ID)
		require.NoError(t, err)
		entered, err := repo.EnterOpenAIRescue(t.Context(), current, map[string]any{"entered_at": time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)}, account.GroupIDs[0], true)
		require.NoError(t, err)
		require.True(t, entered)
		current, err = repo.GetByID(t.Context(), account.ID)
		require.NoError(t, err)
		newMarker = current.Extra["openai_rescue_lane"]
	}}
	lane := service.NewOpenAIRescueLane(snapshotRepo, NewOpenAIDowngradeProbeRepository(db), func() service.OpenAIRescueLaneConfig {
		return service.OpenAIRescueLaneConfig{Enabled: true, GroupID: account.GroupIDs[0]}
	}, nil)
	_, _, _, err = lane.RunReconcileSweep(t.Context())
	require.NoError(t, err)
	current, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, newMarker, current.Extra["openai_rescue_lane"])
	require.Equal(t, account.GroupIDs, current.GroupIDs)
	var events int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM openai_downgrade_probe_events WHERE event_type='rescue_auth_rejected'`).Scan(&events))
	require.Zero(t, events)
}

func TestOpenAIRescuePostgresManualEnableCannotBypassRescue(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		for _, action := range []string{"single", "rescue_scheduler", "bulk", "manual_bulk", "internal_sync"} {
			t.Run(fmt.Sprintf("stopped=%v/%s", stopped, action), func(t *testing.T) {
				repo, db, account, _ := newRescueTerminationPostgres(t)
				_, err := db.Exec(`UPDATE accounts SET schedulable=false WHERE id=$1`, account.ID)
				require.NoError(t, err)
				if stopped {
					_, err = repo.TerminateOpenAIRescue(t.Context(), account.ID, time.Now())
					require.NoError(t, err)
				}
				before, err := repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				enabled := true
				switch action {
				case "single":
					err = repo.SetSchedulable(t.Context(), account.ID, true)
				case "rescue_scheduler":
					err = repo.SetSchedulableInRescueLane(t.Context(), account.ID, account.GroupIDs[0], true)
				case "internal_sync":
					next := *before
					next.Schedulable = true
					err = repo.Update(t.Context(), &next)
				default:
					_, err = repo.BulkUpdate(t.Context(), []int64{account.ID}, service.AccountBulkUpdate{Schedulable: &enabled, ManualScheduling: action == "manual_bulk"})
				}
				if stopped {
					require.ErrorIs(t, err, service.ErrOpenAIRescueTerminated)
				} else {
					require.ErrorIs(t, err, service.ErrOpenAIReenableBlocked)
				}
				after, err := repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				require.Equal(t, before, after)
			})
		}
	}
}

func TestOpenAIRescuePostgresOrdinaryEditWaitsAndRollsBack(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			repo, db, account, _ := newRescueTerminationPostgres(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			writer, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = writer.Rollback() }()
			var pid int
			require.NoError(t, writer.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
			_, err = writer.ExecContext(ctx, `UPDATE accounts SET schedulable=false WHERE id=$1`, account.ID)
			require.NoError(t, err)
			if failure {
				_, err = db.Exec(`CREATE FUNCTION reject_edit_outbox() RETURNS trigger LANGUAGE plpgsql AS
					$$ BEGIN RAISE EXCEPTION 'injected edit failure'; END $$;
					CREATE TRIGGER reject_edit_outbox BEFORE INSERT ON scheduler_outbox FOR EACH ROW EXECUTE FUNCTION reject_edit_outbox();`)
				require.NoError(t, err)
			}
			name := account.Name
			account.Name = "concurrent stale edit"
			finished := make(chan error, 1)
			go func() { finished <- repo.UpdateWithAccountBillingSettings(ctx, account, nil, nil, nil, nil) }()
			require.Eventually(t, func() bool {
				var blocked bool
				err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked)
				return err == nil && blocked
			}, 3*time.Second, 10*time.Millisecond)
			require.NoError(t, writer.Commit())
			err = <-finished
			if failure {
				require.ErrorContains(t, err, "injected edit failure")
			} else {
				require.NoError(t, err)
				name = account.Name
			}
			current, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.False(t, current.Schedulable)
			require.Equal(t, name, current.Name)
			var events int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM scheduler_outbox`).Scan(&events))
			if failure {
				require.Zero(t, events)
			} else {
				require.Equal(t, 1, events)
			}
		})
	}
}

func newRescueTerminationPostgres(t *testing.T) (*accountRepository, *sql.DB, *service.Account, int64) {
	t.Helper()
	repo, db, account, origin := newRescueGraduationPostgres(t)
	for _, name := range []string{"240_openai_probe_rate_limit_streak.sql"} {
		migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, err)
		_, err = db.Exec(string(migration))
		require.NoError(t, err)
	}
	return repo, db, account, origin
}

func TestOpenAIRescuePostgresTerminateAndRestart(t *testing.T) {
	repo, db, account, origin := newRescueTerminationPostgres(t)
	future := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	_, err := db.Exec(`UPDATE accounts SET updated_at=$2 WHERE id=$1`, account.ID, future)
	require.NoError(t, err)
	lane := service.NewOpenAIRescueLane(repo, nil, nil, nil) // stop works with rescue disabled
	stopped, err := lane.TerminateRescue(t.Context(), account.ID)
	require.NoError(t, err)
	require.True(t, stopped)
	current, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{origin}, current.GroupIDs)
	require.False(t, current.Schedulable)
	require.True(t, current.UpdatedAt.After(future))
	require.Nil(t, service.GetOpenAIRescueLaneMarker(current))
	require.True(t, service.OpenAIRescueManuallyTerminated(current))
	stoppedIDs, err := repo.ListOpenAIRescueTerminatedAccountIDs(t.Context())
	require.NoError(t, err)
	require.Equal(t, []int64{account.ID}, stoppedIDs)
	require.NotContains(t, current.Extra, "openai_rescue_rescue_count")
	require.NotContains(t, current.Extra, "openai_rescue_rescued_at")
	var paused bool
	require.NoError(t, db.QueryRow(`SELECT COALESCE((SELECT manual_paused FROM openai_downgrade_probe_controls WHERE account_id=$1),false)`, account.ID).Scan(&paused))
	require.False(t, paused)
	var state string
	require.NoError(t, db.QueryRow(`SELECT state FROM openai_downgrade_probe_states WHERE account_id=$1`, account.ID).Scan(&state))
	require.Equal(t, service.OpenAIDowngradeStatePendingReplace, state)
	current.Name = "edited after termination"
	require.NoError(t, repo.UpdateWithAccountGroups(t.Context(), current, []int64{}, nil, nil, nil, nil))
	stopped, err = lane.TerminateRescue(t.Context(), account.ID)
	require.NoError(t, err)
	require.False(t, stopped)
	current, err = repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Empty(t, current.GroupIDs, "repeated stop cannot restore stale original groups")
	var events int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM openai_downgrade_probe_events WHERE event_type='rescue_terminated'`).Scan(&events))
	require.Equal(t, 1, events)
	marker := map[string]any{"entered_at": time.Now().UTC().Format(time.RFC3339Nano)}
	entered, err := repo.EnterOpenAIRescue(t.Context(), current, marker, account.GroupIDs[0], false)
	require.ErrorIs(t, err, service.ErrOpenAIRescueTerminated)
	require.False(t, entered)
	entered, err = repo.EnterOpenAIRescue(t.Context(), current, marker, account.GroupIDs[0], true)
	require.NoError(t, err)
	require.True(t, entered)
	current, err = repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.False(t, current.Schedulable)
	require.False(t, service.OpenAIRescueManuallyTerminated(current))
	stoppedIDs, err = repo.ListOpenAIRescueTerminatedAccountIDs(t.Context())
	require.NoError(t, err)
	require.Empty(t, stoppedIDs)
	require.Empty(t, service.GetOpenAIRescueLaneMarker(current).OrigGroupIDs)
	require.NoError(t, db.QueryRow(`SELECT COALESCE((SELECT manual_paused FROM openai_downgrade_probe_controls WHERE account_id=$1),false)`, account.ID).Scan(&paused))
	require.False(t, paused)
}

func TestOpenAIRescuePostgresTerminationRollsBack(t *testing.T) {
	for _, failure := range []string{"groups", "account", "state", "event", "outbox", "missing_snapshot", "bad_snapshot", "deleted_group"} {
		t.Run(failure, func(t *testing.T) {
			repo, db, account, origin := newRescueTerminationPostgres(t)
			if failure == "deleted_group" {
				_, err := db.Exec(`UPDATE groups SET deleted_at=clock_timestamp() WHERE id=$1`, origin)
				require.NoError(t, err)
			} else if failure == "missing_snapshot" || failure == "bad_snapshot" {
				query := `UPDATE accounts SET extra=extra #- '{openai_rescue_lane,orig_group_ids}' WHERE id=$1`
				if failure == "bad_snapshot" {
					query = `UPDATE accounts SET extra=jsonb_set(extra,'{openai_rescue_lane,orig_group_ids}','"broken"') WHERE id=$1`
				}
				_, err := db.Exec(query, account.ID)
				require.NoError(t, err)
				account, err = repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
			} else {
				table := map[string]string{"groups": "account_groups", "account": "accounts", "state": "openai_downgrade_probe_states", "event": "openai_downgrade_probe_events", "outbox": "scheduler_outbox"}[failure]
				_, err := db.Exec(`CREATE FUNCTION reject_stop() RETURNS trigger LANGUAGE plpgsql AS
					$$ BEGIN RAISE EXCEPTION 'injected stop failure'; END $$;
					CREATE TRIGGER reject_stop_write BEFORE INSERT OR UPDATE OR DELETE ON ` + table + `
					FOR EACH ROW EXECUTE FUNCTION reject_stop();`)
				require.NoError(t, err)
			}
			stopped, err := repo.TerminateOpenAIRescue(t.Context(), account.ID, time.Now())
			require.Error(t, err)
			require.False(t, stopped)
			current, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			require.Equal(t, account.GroupIDs, current.GroupIDs)
			require.Equal(t, account.Extra, current.Extra)
			require.Equal(t, account.Schedulable, current.Schedulable)
			var count int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM openai_downgrade_probe_events WHERE event_type='rescue_terminated'`).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestOpenAIRescuePostgresTerminateMissingProbeState(t *testing.T) {
	repo, db, account, _ := newRescueTerminationPostgres(t)
	_, err := db.Exec(`DELETE FROM openai_downgrade_probe_states WHERE account_id=$1`, account.ID)
	require.NoError(t, err)
	stopped, err := repo.TerminateOpenAIRescue(t.Context(), account.ID, time.Now())
	require.NoError(t, err)
	require.True(t, stopped)
	var state string
	require.NoError(t, db.QueryRow(`SELECT state FROM openai_downgrade_probe_states WHERE account_id=$1`, account.ID).Scan(&state))
	require.Equal(t, service.OpenAIDowngradeStatePendingReplace, state)
}

func TestOpenAIRescuePostgresGroupEditRechecksAfterConcurrentEntry(t *testing.T) {
	repo, db, account, origin := newRescueTerminationPostgres(t)
	_, err := db.Exec(`UPDATE accounts SET extra=extra - 'openai_rescue_lane' WHERE id=$1`, account.ID)
	require.NoError(t, err)
	account, err = repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	writer, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = writer.Rollback() }()
	var pid int
	require.NoError(t, writer.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	_, err = writer.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{openai_rescue_lane}',jsonb_build_object('orig_group_ids',jsonb_build_array($2::bigint))) WHERE id=$1`, account.ID, origin)
	require.NoError(t, err)
	beforeName := account.Name
	account.Name = "stale edit must roll back"
	finished := make(chan error, 1)
	go func() { finished <- repo.UpdateWithAccountGroups(ctx, account, []int64{origin}, nil, nil, nil, nil) }()
	require.Eventually(t, func() bool {
		var blocked bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked)
		return err == nil && blocked
	}, 3*time.Second, 10*time.Millisecond)
	require.NoError(t, writer.Commit())
	select {
	case err := <-finished:
		require.ErrorIs(t, err, service.ErrOpenAIRescueGroupLocked)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	current, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, beforeName, current.Name)
	require.Equal(t, account.GroupIDs, current.GroupIDs)
}

func TestOpenAIRescuePostgresEntryRollsBackAndPreservesManualPause(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "paused", true: "write_failure"}[fail], func(t *testing.T) {
			repo, db, account, origin := newRescueTerminationPostgres(t)
			_, err := db.Exec(`UPDATE accounts SET extra=extra - 'openai_rescue_lane',schedulable=false WHERE id=$1`, account.ID)
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO openai_downgrade_probe_controls(account_id,manual_paused) VALUES($1,true)`, account.ID)
			require.NoError(t, err)
			account, err = repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			if fail {
				_, err = db.Exec(`CREATE FUNCTION reject_entry() RETURNS trigger LANGUAGE plpgsql AS
					$$ BEGIN RAISE EXCEPTION 'injected entry failure'; END $$;
					CREATE TRIGGER reject_entry_write BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION reject_entry();`)
				require.NoError(t, err)
			}
			entered, err := repo.EnterOpenAIRescue(t.Context(), account, map[string]any{"entered_at": time.Now().UTC().Format(time.RFC3339Nano)}, origin, true)
			if fail {
				require.Error(t, err)
				require.False(t, entered)
			} else {
				require.NoError(t, err)
				require.True(t, entered)
			}
			current, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			var paused bool
			require.NoError(t, db.QueryRow(`SELECT manual_paused FROM openai_downgrade_probe_controls WHERE account_id=$1`, account.ID).Scan(&paused))
			require.True(t, paused, "entry must not release an unrelated manual pause")
			if fail {
				require.Equal(t, account.GroupIDs, current.GroupIDs)
				require.Equal(t, account.Extra, current.Extra)
			}
		})
	}
}

func TestOpenAIRescuePostgresStoppedAccountRejectsDelayedWrites(t *testing.T) {
	repo, db, account, origin := newRescueTerminationPostgres(t)
	var stateRevision time.Time
	require.NoError(t, db.QueryRow(`SELECT updated_at FROM openai_downgrade_probe_states WHERE account_id=$1`, account.ID).Scan(&stateRevision))
	enabled := true
	mutation := &service.OpenAIDowngradeMutation{
		AccountID: account.ID, ExpectedAccountUpdatedAt: account.UpdatedAt,
		ExpectedStateUpdatedAt: stateRevision, ExpectedStatus: account.Status, ExpectedSchedulable: account.Schedulable,
		Schedulable: &enabled, State: &service.OpenAIDowngradeProbeState{AccountID: account.ID, State: service.OpenAIDowngradeStateOnDuty, ProbeMode: "normal"},
	}
	stopped, err := repo.TerminateOpenAIRescue(t.Context(), account.ID, time.Now())
	require.NoError(t, err)
	require.True(t, stopped)
	probeRepo := &openAIDowngradeProbeRepository{db: db}
	require.ErrorIs(t, probeRepo.CommitOpenAIDowngradeMutation(t.Context(), mutation), service.ErrOpenAIProbeStale)
	rescueIDs := account.GroupIDs
	changed, err := repo.MutateOpenAIRescue(t.Context(), account.ID, account.Extra["openai_rescue_lane"], &rescueIDs, map[string]any{"openai_rescue_suspected": true}, true)
	require.NoError(t, err)
	require.False(t, changed)
	changed, err = repo.UpdateOpenAIRescueMarker(t.Context(), account.ID, account.Extra["openai_rescue_lane"], account.Extra["openai_rescue_lane"])
	require.NoError(t, err)
	require.False(t, changed)
	changed, err = repo.GraduateOpenAIRescue(t.Context(), account.ID, account.Extra["openai_rescue_lane"], []int64{origin}, rescueGraduationUpdates())
	require.NoError(t, err)
	require.False(t, changed)
	_, err = repo.EnterOpenAIRescue(t.Context(), account, map[string]any{"entered_at": time.Now().Format(time.RFC3339Nano)}, rescueIDs[0], false)
	require.ErrorIs(t, err, service.ErrOpenAIRescueTerminated)
}

func TestOpenAIRescuePostgresGroupEditIsAtomic(t *testing.T) {
	for _, scenario := range []string{"change", "clear", "unchanged", "bulk", "after_exit", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			repo, db, account, origin := newRescueTerminationPostgres(t)
			requested := []int64{origin}
			if scenario == "clear" {
				requested = []int64{}
			}
			if scenario == "unchanged" {
				requested = account.GroupIDs
			}
			if scenario == "after_exit" || scenario == "rollback" {
				_, err := repo.TerminateOpenAIRescue(t.Context(), account.ID, time.Now())
				require.NoError(t, err)
				account, err = repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				requested = []int64{}
			}
			if scenario == "rollback" {
				_, err := db.Exec(`CREATE FUNCTION reject_groups() RETURNS trigger LANGUAGE plpgsql AS
					$$ BEGIN RAISE EXCEPTION 'injected group failure'; END $$;
					CREATE TRIGGER reject_group_write BEFORE DELETE ON account_groups FOR EACH ROW EXECUTE FUNCTION reject_groups();`)
				require.NoError(t, err)
			}
			beforeName := account.Name
			account.Name = "must commit with groups"
			var err error
			var ordinaryID int64
			if scenario == "bulk" {
				ordinary, createErr := repo.client.Account.Create().SetName("ordinary").SetPlatform(service.PlatformAnthropic).SetType(service.AccountTypeAPIKey).SetCredentials(map[string]any{}).Save(t.Context())
				require.NoError(t, createErr)
				ordinaryID = ordinary.ID
				_, err = repo.BulkUpdateWithAccountGroups(t.Context(), []int64{ordinaryID, account.ID}, service.AccountBulkUpdate{Name: &account.Name}, requested)
			} else {
				err = repo.UpdateWithAccountGroups(t.Context(), account, requested, nil, nil, nil, nil)
			}
			if scenario == "unchanged" || scenario == "after_exit" {
				require.NoError(t, err)
			} else if scenario == "rollback" {
				require.ErrorContains(t, err, "injected group failure")
			} else {
				require.ErrorIs(t, err, service.ErrOpenAIRescueGroupLocked)
			}
			current, getErr := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, getErr)
			if err == nil {
				require.Equal(t, account.Name, current.Name)
				require.ElementsMatch(t, requested, current.GroupIDs)
			} else {
				require.Equal(t, beforeName, current.Name)
				require.Equal(t, account.GroupIDs, current.GroupIDs)
			}
			if ordinaryID > 0 {
				ordinary, err := repo.GetByID(t.Context(), ordinaryID)
				require.NoError(t, err)
				require.Equal(t, "ordinary", ordinary.Name)
				require.Empty(t, ordinary.GroupIDs)
			}
		})
	}
}

func TestOpenAIRescuePostgresConcurrentStopAndGraduation(t *testing.T) {
	for range 5 {
		repo, db, account, origin := newRescueTerminationPostgres(t)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		start := make(chan struct{})
		errors := make(chan error, 2)
		go func() { <-start; _, err := repo.TerminateOpenAIRescue(ctx, account.ID, time.Now()); errors <- err }()
		go func() {
			<-start
			_, err := repo.GraduateOpenAIRescue(ctx, account.ID, account.Extra["openai_rescue_lane"], []int64{origin}, rescueGraduationUpdates())
			errors <- err
		}()
		close(start)
		require.NoError(t, <-errors)
		require.NoError(t, <-errors)
		current, err := repo.GetByID(ctx, account.ID)
		require.NoError(t, err)
		require.Equal(t, []int64{origin}, current.GroupIDs)
		require.Nil(t, service.GetOpenAIRescueLaneMarker(current))
		if service.OpenAIRescueManuallyTerminated(current) {
			require.False(t, current.Schedulable)
			require.NotContains(t, current.Extra, "openai_rescue_rescue_count")
		} else {
			require.Equal(t, float64(1), current.Extra["openai_rescue_rescue_count"])
		}
		var terminalEvents int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM openai_downgrade_probe_events WHERE event_type='rescue_terminated'`).Scan(&terminalEvents))
		require.LessOrEqual(t, terminalEvents, 1)
		cancel()
	}
}
