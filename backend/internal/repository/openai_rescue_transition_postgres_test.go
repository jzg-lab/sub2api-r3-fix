package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func newRescueTransitionPostgres(t *testing.T, kind service.OpenAIRescueTransition) (*accountRepository, *sql.DB, service.OpenAIRescueTransitionUpdate) {
	t.Helper()
	repo, db, graduation := newRescueGraduationPostgres(t)
	m := service.OpenAIRescueTransitionUpdate{
		AccountID: 7, ExpectedUpdatedAt: graduation.ExpectedUpdatedAt, Kind: kind,
		OldGroupIDs: []int64{99}, GroupIDs: []int64{11, 12},
	}
	switch kind {
	case service.OpenAIRescueTransitionEnter:
		_, err := db.ExecContext(t.Context(), "UPDATE accounts SET extra = extra - 'openai_rescue_lane' WHERE id = 7")
		require.NoError(t, err)
		m.Extra = map[string]any{
			"openai_rescue_lane": service.OpenAIRescueLaneMarker{
				EnteredAt: time.Now().UTC(), OrigGroupIDs: []int64{99},
			},
			"openai_rescue_auth_rejected_at": nil,
		}
	case service.OpenAIRescueTransitionExit:
		m.Extra = map[string]any{
			"openai_rescue_lane": nil, "openai_rescue_suspected": false,
			"openai_rescue_auth_rejected_at": time.Now().UTC().Format(time.RFC3339),
		}
	}
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT updated_at FROM accounts WHERE id = 7").Scan(&m.ExpectedUpdatedAt))
	return repo, db, m
}

func TestOpenAIRescueTransitionPostgresCommitAndReplay(t *testing.T) {
	for _, kind := range []service.OpenAIRescueTransition{
		service.OpenAIRescueTransitionEnter, service.OpenAIRescueTransitionExit, service.OpenAIRescueTransitionRebind,
	} {
		t.Run(string(kind), func(t *testing.T) {
			repo, db, m := newRescueTransitionPostgres(t, kind)
			version, err := repo.CommitOpenAIRescueTransition(t.Context(), m)
			require.NoError(t, err)
			require.True(t, version.After(m.ExpectedUpdatedAt))
			var groups pq.Int64Array
			var schedulable, marked bool
			require.NoError(t, db.QueryRowContext(t.Context(), `
				SELECT schedulable, COALESCE(jsonb_typeof(extra->'openai_rescue_lane'), '') = 'object'
				FROM accounts WHERE id = 7
			`).Scan(&schedulable, &marked))
			require.False(t, schedulable, "rebinding an isolated rescue account must not enable traffic")
			require.Equal(t, kind != service.OpenAIRescueTransitionExit, marked)
			var storedVersion time.Time
			require.NoError(t, db.QueryRowContext(t.Context(),
				"SELECT updated_at FROM accounts WHERE id = 7").Scan(&storedVersion))
			require.True(t, version.Equal(storedVersion), "return the post-trigger revision for the next marker write")
			require.NoError(t, db.QueryRowContext(t.Context(),
				"SELECT array_agg(group_id ORDER BY priority) FROM account_groups WHERE account_id = 7").Scan(&groups))
			require.Equal(t, pq.Int64Array{11, 12}, groups)
			var notifications int
			require.NoError(t, db.QueryRowContext(t.Context(),
				"SELECT count(*) FROM scheduler_outbox WHERE account_id = 7").Scan(&notifications))
			require.Equal(t, 3, notifications)
			after := rescueGraduationSnapshot(t, db)
			_, err = repo.CommitOpenAIRescueTransition(t.Context(), m)
			require.ErrorIs(t, err, service.ErrOpenAIProbeStale)
			require.Equal(t, after, rescueGraduationSnapshot(t, db))
		})
	}
}

func TestOpenAIRescueTransitionPostgresWithdrawsDriftedScheduling(t *testing.T) {
	for _, kind := range []service.OpenAIRescueTransition{
		service.OpenAIRescueTransitionEnter, service.OpenAIRescueTransitionExit, service.OpenAIRescueTransitionRebind,
	} {
		t.Run(string(kind), func(t *testing.T) {
			repo, db, m := newRescueTransitionPostgres(t, kind)
			require.NoError(t, db.QueryRowContext(t.Context(), `
				UPDATE accounts SET schedulable = TRUE WHERE id = 7 RETURNING updated_at
			`).Scan(&m.ExpectedUpdatedAt))
			version, err := repo.CommitOpenAIRescueTransition(t.Context(), m)
			require.NoError(t, err)
			var schedulable bool
			var storedVersion time.Time
			require.NoError(t, db.QueryRowContext(t.Context(), `
				SELECT schedulable, updated_at FROM accounts WHERE id = 7
			`).Scan(&schedulable, &storedVersion))
			require.False(t, schedulable, "only verified graduation may admit rescue traffic")
			require.True(t, version.Equal(storedVersion))
			require.True(t, version.After(m.ExpectedUpdatedAt))
		})
	}
}

func TestOpenAIRescueTransitionPostgresRollbackAtEveryWriteBoundary(t *testing.T) {
	for _, kind := range []service.OpenAIRescueTransition{
		service.OpenAIRescueTransitionEnter, service.OpenAIRescueTransitionExit, service.OpenAIRescueTransitionRebind,
	} {
		for _, boundary := range []struct{ table, operation string }{
			{"accounts", "UPDATE"}, {"account_groups", "DELETE"},
			{"account_groups", "INSERT"}, {"scheduler_outbox", "INSERT"},
		} {
			t.Run(string(kind)+"_"+boundary.table+"_"+boundary.operation, func(t *testing.T) {
				repo, db, m := newRescueTransitionPostgres(t, kind)
				before := rescueGraduationSnapshot(t, db)
				_, err := db.ExecContext(t.Context(), `
					CREATE FUNCTION reject_transition_write() RETURNS trigger LANGUAGE plpgsql AS
					$$ BEGIN RAISE EXCEPTION 'injected transition write failure'; END $$;
					CREATE TRIGGER reject_write BEFORE `+boundary.operation+` ON `+pq.QuoteIdentifier(boundary.table)+`
					FOR EACH ROW EXECUTE FUNCTION reject_transition_write();
				`)
				require.NoError(t, err)
				_, err = repo.CommitOpenAIRescueTransition(t.Context(), m)
				require.ErrorContains(t, err, "injected transition write failure")
				require.Equal(t, before, rescueGraduationSnapshot(t, db))
			})
		}
	}
}

func TestOpenAIRescueTransitionPostgresRejectsChangedAccount(t *testing.T) {
	for _, kind := range []service.OpenAIRescueTransition{
		service.OpenAIRescueTransitionEnter, service.OpenAIRescueTransitionExit, service.OpenAIRescueTransitionRebind,
	} {
		for _, change := range []string{"generation", "deleted", "marker", "cancelled"} {
			t.Run(string(kind)+"_"+change, func(t *testing.T) {
				repo, db, m := newRescueTransitionPostgres(t, kind)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				wantErr := service.ErrOpenAIProbeStale
				switch change {
				case "generation":
					_, err := db.ExecContext(t.Context(), "UPDATE accounts SET updated_at = clock_timestamp() WHERE id = 7")
					require.NoError(t, err)
				case "deleted":
					_, err := db.ExecContext(t.Context(), "UPDATE accounts SET deleted_at = clock_timestamp() WHERE id = 7")
					require.NoError(t, err)
				case "marker":
					query := "UPDATE accounts SET extra = extra - 'openai_rescue_lane' WHERE id = 7"
					if kind == service.OpenAIRescueTransitionEnter {
						query = `UPDATE accounts SET extra = extra || '{"openai_rescue_lane":{}}'::jsonb WHERE id = 7`
					}
					_, err := db.ExecContext(t.Context(), query)
					require.NoError(t, err)
				case "cancelled":
					cancel()
					wantErr = context.Canceled
				}
				before := rescueGraduationSnapshot(t, db)
				_, err := repo.CommitOpenAIRescueTransition(ctx, m)
				require.ErrorIs(t, err, wantErr)
				require.Equal(t, before, rescueGraduationSnapshot(t, db))
			})
		}
	}
}

func TestOpenAIRescueTransitionPostgresWaitCannotOverwriteGraduation(t *testing.T) {
	for _, kind := range []service.OpenAIRescueTransition{service.OpenAIRescueTransitionExit, service.OpenAIRescueTransitionRebind} {
		t.Run(string(kind), func(t *testing.T) {
			repo, db, graduation := newRescueGraduationPostgres(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			lock, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = lock.Rollback() }()
			_, err = lock.ExecContext(ctx, `
				UPDATE accounts SET updated_at = clock_timestamp(),
					extra = extra - 'openai_rescue_lane' WHERE id = 7
			`)
			require.NoError(t, err)
			result := make(chan error, 1)
			go func() {
				_, err := repo.CommitOpenAIRescueTransition(ctx, service.OpenAIRescueTransitionUpdate{
					AccountID: 7, ExpectedUpdatedAt: graduation.ExpectedUpdatedAt, Kind: kind, GroupIDs: []int64{44},
				})
				result <- err
			}()
			require.NoError(t, lock.Commit())
			before := rescueGraduationSnapshot(t, db)
			require.ErrorIs(t, <-result, service.ErrOpenAIProbeStale)
			require.Equal(t, before, rescueGraduationSnapshot(t, db))
		})
	}
}
