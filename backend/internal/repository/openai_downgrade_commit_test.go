package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func probeCommitFixture() *service.OpenAIDowngradeMutation {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	paused := false
	proxyID := int64(3)
	return &service.OpenAIDowngradeMutation{
		AccountID: 7, ExpectedAccountUpdatedAt: now, ExpectedStateUpdatedAt: now,
		ExpectedProxyID: &proxyID, ExpectedStatus: service.StatusActive, ExpectedSchedulable: true,
		Schedulable: &paused,
		State: &service.OpenAIDowngradeProbeState{
			AccountID: 7, State: service.OpenAIDowngradeStateCircuitOpen, ProbeMode: "normal",
			NextProbeAt: now.Add(time.Hour), UpdatedAt: now,
			Consecutive429s: 6,
		},
		Results: []service.OpenAIDowngradeProbeResult{{AccountID: 7, ProxyID: &proxyID}},
		Events: []service.OpenAIDowngradeMutationEvent{{
			ProxyID: &proxyID, Type: service.OpenAIDowngradeEventCircuitOpen, Details: json.RawMessage(`{}`),
		}},
	}
}

func TestOpenAIProbeCommitRollbackAtEveryWriteBoundary(t *testing.T) {
	steps := []string{"account_write", "result", "event", "state", "outbox"}
	for _, failure := range append(append([]string{}, steps...), "begin", "commit", "success") {
		t.Run(failure, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := &openAIDowngradeProbeRepository{db: db}
			mutation := probeCommitFixture()
			before := *mutation.State
			injected := errors.New("injected persistence failure")
			begin := mock.ExpectBegin()
			if failure == "begin" {
				begin.WillReturnError(injected)
			} else {
				expectProbeCommitLocks(mock, mutation)
				queries := []string{
					"UPDATE accounts SET", "INSERT INTO openai_downgrade_probe_results",
					"INSERT INTO openai_downgrade_probe_events", "UPDATE openai_downgrade_probe_states",
					"INSERT INTO scheduler_outbox",
				}
				failedWrite := false
				for i, step := range steps {
					expect := mock.ExpectExec(queries[i])
					if step == failure {
						expect.WillReturnError(injected)
						failedWrite = true
						break
					}
					expect.WillReturnResult(sqlmock.NewResult(0, 1))
				}
				if failedWrite {
					mock.ExpectRollback()
				} else if failure == "commit" {
					mock.ExpectCommit().WillReturnError(injected)
				} else {
					mock.ExpectCommit()
				}
			}
			err = repo.CommitOpenAIDowngradeMutation(context.Background(), mutation)
			if failure == "success" {
				require.NoError(t, err)
				require.True(t, mutation.State.UpdatedAt.After(before.UpdatedAt))
			} else {
				require.ErrorIs(t, err, injected)
				require.Equal(t, before, *mutation.State, "failed commit cannot publish a new generation")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func expectProbeCommitLocks(mock sqlmock.Sqlmock, mutation *service.OpenAIDowngradeMutation) {
	mock.ExpectQuery("SELECT updated_at FROM accounts").
		WithArgs(mutation.AccountID, mutation.ExpectedAccountUpdatedAt, mutation.ExpectedProxyID,
			mutation.ExpectedStatus, mutation.ExpectedSchedulable).
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(mutation.ExpectedAccountUpdatedAt)).
		RowsWillBeClosed()
	mock.ExpectQuery("SELECT updated_at FROM openai_downgrade_probe_states").
		WithArgs(mutation.AccountID, mutation.ExpectedStateUpdatedAt).
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(mutation.ExpectedStateUpdatedAt)).
		RowsWillBeClosed()
}

func TestOpenAIProbeCommitRejectsStaleAccountAndState(t *testing.T) {
	for _, changed := range []string{"account", "state"} {
		for _, databaseErr := range []error{nil, errors.New("database unavailable")} {
			t.Run(changed+"/"+probeCommitErrorName(databaseErr), func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer db.Close()
				repo := &openAIDowngradeProbeRepository{db: db}
				mutation := probeCommitFixture()
				mock.ExpectBegin()
				account := mock.ExpectQuery("SELECT updated_at FROM accounts")
				last := account
				if changed == "state" {
					account.WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(mutation.ExpectedAccountUpdatedAt))
					last = mock.ExpectQuery("SELECT updated_at FROM openai_downgrade_probe_states")
				}
				if databaseErr != nil {
					last.WillReturnError(databaseErr)
				} else {
					last.WillReturnRows(sqlmock.NewRows([]string{"updated_at"}))
				}
				mock.ExpectRollback()
				err = repo.CommitOpenAIDowngradeMutation(context.Background(), mutation)
				if databaseErr != nil {
					require.ErrorIs(t, err, databaseErr)
				} else {
					require.ErrorIs(t, err, service.ErrOpenAIProbeStale)
				}
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func probeCommitErrorName(err error) string {
	if err == nil {
		return "stale"
	}
	return "query_failure"
}

func TestOpenAIProbeCommitRejectsInvalidMutationBeforeTransaction(t *testing.T) {
	for _, invalid := range []string{"nil", "missing_state", "account_mismatch", "result_mismatch", "unowned_error", "new_error"} {
		t.Run(invalid, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mutation := probeCommitFixture()
			enabled := true
			switch invalid {
			case "nil":
				mutation = nil
			case "missing_state":
				mutation.State = nil
			case "account_mismatch":
				mutation.State.AccountID++
			case "result_mismatch":
				mutation.Results[0].AccountID++
			case "unowned_error":
				mutation.ExpectedStatus = service.StatusError
				mutation.Schedulable = &enabled
			case "new_error":
				message := "fixture"
				mutation.ErrorMessage = &message
				mutation.Schedulable = &enabled
			}
			repo := &openAIDowngradeProbeRepository{db: db}
			require.Error(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOpenAIProbeCommitRequiresOneUpdatedRow(t *testing.T) {
	for _, count := range []int64{0, 2} {
		require.ErrorIs(t, requireOpenAIProbeUpdatedRow(sqlmock.NewResult(0, count), nil), service.ErrOpenAIProbeStale)
	}
	require.NoError(t, requireOpenAIProbeUpdatedRow(sqlmock.NewResult(0, 1), nil))
	require.ErrorIs(t, requireOpenAIProbeUpdatedRow(sqlmock.NewErrorResult(sql.ErrTxDone), nil), sql.ErrTxDone)
	require.ErrorIs(t, requireOpenAIProbeUpdatedRow(nil, sql.ErrTxDone), sql.ErrTxDone)
}
