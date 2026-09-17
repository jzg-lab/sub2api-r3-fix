package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func probeRecoveryFixture() *service.OpenAIDowngradeMutation {
	mutation := probeCommitFixture()
	mutation.Schedulable = nil
	mutation.State.State = service.OpenAIDowngradeStateOnDuty
	mutation.RateLimitClear = &service.OpenAIDowngradeRateLimitObservation{
		LimitedAt: mutation.ExpectedAccountUpdatedAt.Add(-time.Hour),
		ResetAt:   mutation.ExpectedAccountUpdatedAt.Add(7 * 24 * time.Hour),
	}
	mutation.Results[0].HTTPStatus = http.StatusOK
	mutation.Results[0].TransportOK = true
	mutation.Events[0].Type = service.OpenAIDowngradeEventRateLimitRecheckRecovered
	mutation.Events[0].Details = json.RawMessage(`{"http":200}`)
	return mutation
}

func TestOpenAIProbeRecoveryCommitCASArguments(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mutation := probeRecoveryFixture()
	before := *mutation.State
	mock.ExpectBegin()
	expectProbeCommitLocks(mock, mutation)
	mock.ExpectExec(`UPDATE accounts SET .*AND \(\$7::timestamptz IS NULL OR \(rate_limited_at = \$7 AND rate_limit_reset_at = \$8::timestamptz\)\)`).
		WithArgs(mutation.AccountID, false, nil, nil, "{}", nil,
			mutation.RateLimitClear.LimitedAt, mutation.RateLimitClear.ResetAt).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	repo := &openAIDowngradeProbeRepository{db: db}
	require.ErrorIs(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation), service.ErrOpenAIProbeStale)
	require.Equal(t, before, *mutation.State)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAIProbeRecoveryCommitRejectsInvalidEvidence(t *testing.T) {
	for _, invalid := range []string{"missing_result", "transport_failure", "429", "simultaneous_extension"} {
		t.Run(invalid, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mutation := probeRecoveryFixture()
			switch invalid {
			case "missing_result":
				mutation.Results = nil
			case "transport_failure":
				mutation.Results[0].TransportOK = false
			case "429":
				mutation.Results[0].HTTPStatus = http.StatusTooManyRequests
			case "simultaneous_extension":
				mutation.RateLimitResetAt = &mutation.RateLimitClear.ResetAt
			}
			repo := &openAIDowngradeProbeRepository{db: db}
			require.ErrorContains(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation),
				"invalid OpenAI probe rate-limit recovery")
			require.NoError(t, mock.ExpectationsWereMet(), "reject before opening a transaction")
		})
	}
}

func TestOpenAIProbePostgresRecoveryCASAndReplay(t *testing.T) {
	for _, change := range []string{"none", "rearmed", "extended", "shortened", "cleared"} {
		t.Run(change, func(t *testing.T) {
			db := newProbePostgres(t)
			seedProbePostgres(t, db)
			mutation := probeRecoveryFixture()
			observation := mutation.RateLimitClear
			_, err := db.Exec("UPDATE accounts SET rate_limited_at=$1, rate_limit_reset_at=$2 WHERE id=7",
				observation.LimitedAt, observation.ResetAt)
			require.NoError(t, err)
			changes := map[string]string{
				"rearmed":   "rate_limited_at=rate_limited_at+INTERVAL '1 microsecond'",
				"extended":  "rate_limit_reset_at=rate_limit_reset_at+INTERVAL '1 hour'",
				"shortened": "rate_limit_reset_at=rate_limit_reset_at-INTERVAL '1 hour'",
				"cleared":   "rate_limited_at=NULL, rate_limit_reset_at=NULL",
			}
			if clause, ok := changes[change]; ok {
				_, err = db.Exec("UPDATE accounts SET " + clause + " WHERE id=7")
				require.NoError(t, err)
			}
			before := probePostgresSnapshot(t, db)
			repo := &openAIDowngradeProbeRepository{db: db}
			err = repo.CommitOpenAIDowngradeMutation(context.Background(), mutation)
			if change != "none" {
				require.ErrorIs(t, err, service.ErrOpenAIProbeStale)
				require.Equal(t, before, probePostgresSnapshot(t, db))
				return
			}
			require.NoError(t, err)
			var limitedAt, resetAt sql.NullTime
			require.NoError(t, db.QueryRow("SELECT rate_limited_at, rate_limit_reset_at FROM accounts WHERE id=7").
				Scan(&limitedAt, &resetAt))
			require.False(t, limitedAt.Valid)
			require.False(t, resetAt.Valid)
			var events, outbox int
			require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM openai_downgrade_probe_events WHERE event_type=$1",
				service.OpenAIDowngradeEventRateLimitRecheckRecovered).Scan(&events))
			require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM scheduler_outbox").Scan(&outbox))
			require.Equal(t, 1, events)
			require.Equal(t, 1, outbox)
			committed := probePostgresSnapshot(t, db)
			require.ErrorIs(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation), service.ErrOpenAIProbeStale)
			require.Equal(t, committed, probePostgresSnapshot(t, db))
		})
	}
}

func TestOpenAIProbePostgresRecoveryRollsBackEveryWrite(t *testing.T) {
	for _, table := range []string{
		"accounts", "openai_downgrade_probe_results", "openai_downgrade_probe_events",
		"openai_downgrade_probe_states", "scheduler_outbox",
	} {
		t.Run(table, func(t *testing.T) {
			db := newProbePostgres(t)
			seedProbePostgres(t, db)
			mutation := probeRecoveryFixture()
			_, err := db.Exec("UPDATE accounts SET rate_limited_at=$1, rate_limit_reset_at=$2 WHERE id=7",
				mutation.RateLimitClear.LimitedAt, mutation.RateLimitClear.ResetAt)
			require.NoError(t, err)
			before := probePostgresSnapshot(t, db)
			stateBefore := *mutation.State
			_, err = db.Exec(`
				CREATE FUNCTION reject_recovery_write() RETURNS trigger LANGUAGE plpgsql AS
				$$ BEGIN RAISE EXCEPTION 'injected recovery failure'; END $$;
				CREATE TRIGGER reject_recovery BEFORE INSERT OR UPDATE ON ` + pq.QuoteIdentifier(table) + `
				FOR EACH ROW EXECUTE FUNCTION reject_recovery_write();
			`)
			require.NoError(t, err)
			repo := &openAIDowngradeProbeRepository{db: db}
			require.ErrorContains(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation), "injected recovery failure")
			require.Equal(t, before, probePostgresSnapshot(t, db))
			require.Equal(t, stateBefore, *mutation.State)
		})
	}
}
