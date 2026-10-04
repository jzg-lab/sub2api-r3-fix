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

func seedReenablePostgres(t *testing.T, db *sql.DB) *service.OpenAIAccountReenableMutation {
	t.Helper()
	seedProbePostgres(t, db)
	mutation := reenableCommitFixture()
	mutation.Unpause = true
	_, err := db.Exec(`
		UPDATE accounts SET schedulable = FALSE, updated_at = $1 WHERE id = $2
	`, mutation.ExpectedAccountUpdatedAt, mutation.AccountID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO openai_downgrade_probe_controls(account_id, manual_paused)
		VALUES ($1, TRUE)
		ON CONFLICT (account_id) DO UPDATE SET manual_paused = TRUE
	`, mutation.AccountID)
	require.NoError(t, err)
	state := *mutation.State
	state.State = service.OpenAIDowngradeStatePendingReplace
	state.ProbeMode = "normal"
	state.ConsecutiveFailures = 3
	state.UpdatedAt = mutation.ExpectedStateUpdatedAt
	repo := &openAIDowngradeProbeRepository{db: db}
	require.NoError(t, repo.SaveOpenAIDowngradeState(context.Background(), &state))
	return mutation
}

func TestOpenAIReenablePostgresRollbackAtEveryWriteBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		table    string
		event    string
		atCommit bool
	}{
		{"clear_pause", "openai_downgrade_probe_controls", "", false},
		{"unpause_event", "openai_downgrade_probe_events", "manual_unpause", false},
		{"reenable_event", "openai_downgrade_probe_events", "manual_reenable", false},
		{"state_write", "openai_downgrade_probe_states", "", false},
		{"commit", "openai_downgrade_probe_states", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newProbePostgres(t)
			mutation := seedReenablePostgres(t, db)
			before := probePostgresSnapshot(t, db)
			stateBefore := *mutation.State
			trigger := "CREATE TRIGGER reject_reenable_write BEFORE INSERT OR UPDATE ON "
			when := ""
			if tc.event != "" {
				when = " WHEN (NEW.event_type = " + pq.QuoteLiteral(tc.event) + ")"
			}
			if tc.atCommit {
				trigger = "CREATE CONSTRAINT TRIGGER reject_reenable_write AFTER UPDATE ON "
			}
			trigger += pq.QuoteIdentifier(tc.table)
			if tc.atCommit {
				trigger += " DEFERRABLE INITIALLY DEFERRED"
			}
			_, err := db.Exec(`
				CREATE FUNCTION reject_reenable_write() RETURNS trigger LANGUAGE plpgsql AS
				$$ BEGIN RAISE EXCEPTION 'injected reenable write failure'; END $$;
			` + trigger + " FOR EACH ROW" + when + " EXECUTE FUNCTION reject_reenable_write()")
			require.NoError(t, err)
			repo := &openAIDowngradeProbeRepository{db: db}

			unpaused, err := repo.CommitOpenAIAccountReenable(context.Background(), mutation)

			require.ErrorContains(t, err, "injected reenable write failure")
			require.False(t, unpaused)
			require.Equal(t, before, probePostgresSnapshot(t, db))
			require.Equal(t, stateBefore, *mutation.State)
		})
	}
}

func TestOpenAIReenablePostgresSuccessAndReplay(t *testing.T) {
	for _, nilProxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "bound_proxy", true: "no_proxy"}[nilProxy], func(t *testing.T) {
			db := newProbePostgres(t)
			mutation := seedReenablePostgres(t, db)
			if nilProxy {
				_, err := db.Exec(`UPDATE accounts SET proxy_id = NULL WHERE id = $1`, mutation.AccountID)
				require.NoError(t, err)
				mutation.ExpectedProxyID = nil
				mutation.State.CurrentProxyID = nil
				mutation.State.OriginalProxyID = nil
			}
			repo := &openAIDowngradeProbeRepository{db: db}

			unpaused, err := repo.CommitOpenAIAccountReenable(context.Background(), mutation)

			require.NoError(t, err)
			require.True(t, unpaused)
			state, err := repo.GetOpenAIDowngradeState(context.Background(), mutation.AccountID)
			require.NoError(t, err)
			require.Equal(t, service.OpenAIDowngradeStateOnDuty, state.State)
			require.Equal(t, "qualification", state.ProbeMode)
			require.Equal(t, 1, state.ConsecutiveFailures)
			require.Equal(t, mutation.State.CurrentProxyID, state.CurrentProxyID)
			require.True(t, state.UpdatedAt.After(mutation.ExpectedStateUpdatedAt))
			var schedulable, paused bool
			require.NoError(t, db.QueryRow(`
				SELECT a.schedulable, c.manual_paused FROM accounts a
				JOIN openai_downgrade_probe_controls c ON c.account_id = a.id
				WHERE a.id = $1
			`, mutation.AccountID).Scan(&schedulable, &paused))
			require.False(t, schedulable, "qualification must precede return to traffic")
			require.False(t, paused)
			var unpauseEvents, reenableEvents int
			require.NoError(t, db.QueryRow(`
				SELECT COUNT(*) FILTER (WHERE event_type = 'manual_unpause'),
					COUNT(*) FILTER (WHERE event_type = 'manual_reenable')
				FROM openai_downgrade_probe_events WHERE account_id = $1
			`, mutation.AccountID).Scan(&unpauseEvents, &reenableEvents))
			require.Equal(t, 1, unpauseEvents)
			require.Equal(t, 1, reenableEvents)

			beforeReplay := probePostgresSnapshot(t, db)
			unpaused, err = repo.CommitOpenAIAccountReenable(context.Background(), mutation)
			require.ErrorIs(t, err, service.ErrOpenAIProbeStale)
			require.False(t, unpaused)
			require.Equal(t, beforeReplay, probePostgresSnapshot(t, db))
		})
	}
}

func TestOpenAIReenablePostgresCompetesWithProbeCommit(t *testing.T) {
	for _, order := range []string{"reenable_first", "probe_first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			db := newProbePostgres(t)
			reenable := seedReenablePostgres(t, db)
			reenable.Unpause = false
			_, err := db.Exec(`UPDATE openai_downgrade_probe_controls SET manual_paused = FALSE WHERE account_id = $1`, reenable.AccountID)
			require.NoError(t, err)
			probe := probeCommitFixture()
			probe.ExpectedAccountUpdatedAt = reenable.ExpectedAccountUpdatedAt
			probe.ExpectedStateUpdatedAt = reenable.ExpectedStateUpdatedAt
			probe.ExpectedSchedulable = false
			probe.Schedulable = nil
			repo := &openAIDowngradeProbeRepository{db: db}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			commitReenable := func() error {
				_, err := repo.CommitOpenAIAccountReenable(ctx, reenable)
				return err
			}
			commitProbe := func() error {
				return repo.CommitOpenAIDowngradeMutation(ctx, probe)
			}
			var reenableErr, probeErr error
			switch order {
			case "reenable_first":
				reenableErr = commitReenable()
				probeErr = commitProbe()
			case "probe_first":
				probeErr = commitProbe()
				reenableErr = commitReenable()
			case "concurrent":
				start := make(chan struct{})
				reenableDone, probeDone := make(chan error, 1), make(chan error, 1)
				go func() { <-start; reenableDone <- commitReenable() }()
				go func() { <-start; probeDone <- commitProbe() }()
				close(start)
				reenableErr, probeErr = <-reenableDone, <-probeDone
			}

			state, err := repo.GetOpenAIDowngradeState(ctx, reenable.AccountID)
			require.NoError(t, err)
			wantResults := 0
			if reenableErr == nil {
				require.ErrorIs(t, probeErr, service.ErrOpenAIProbeStale)
				require.Equal(t, service.OpenAIDowngradeStateOnDuty, state.State)
				require.Equal(t, "qualification", state.ProbeMode)
				require.NotEqual(t, "probe_first", order)
			} else {
				require.ErrorIs(t, reenableErr, service.ErrOpenAIProbeStale)
				require.NoError(t, probeErr)
				require.Equal(t, probe.State.State, state.State)
				require.NotEqual(t, "reenable_first", order)
				wantResults = 1
			}
			var events, results int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM openai_downgrade_probe_events`).Scan(&events))
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM openai_downgrade_probe_results`).Scan(&results))
			require.Equal(t, 1, events, "only the winning transaction may publish its audit event")
			require.Equal(t, wantResults, results)
		})
	}
}
