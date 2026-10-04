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
	// r17aq：result 落账后紧跟 proxy_outcome_stats 聚合（结果带 proxy 绑定
	// 时：聚合 upsert + proxies.bucket_risk_score 同步两跳），它们同样是
	// 事务内写边界，失败必须回滚。
	type commitStep struct {
		name  string
		query string
	}
	steps := []commitStep{
		{"account_write", "UPDATE accounts SET"},
		{"result", "INSERT INTO openai_downgrade_probe_results"},
		{"stats_upsert", "INSERT INTO proxy_outcome_stats"},
		{"stats_score", "UPDATE proxies p"},
		{"event", "INSERT INTO openai_downgrade_probe_events"},
		{"state", "UPDATE openai_downgrade_probe_states"},
		{"outbox", "INSERT INTO scheduler_outbox"},
	}
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.name)
	}
	for _, failure := range append(append([]string{}, names...), "begin", "commit", "success") {
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
				failedWrite := false
				for _, step := range steps {
					expect := mock.ExpectExec(step.query)
					if step.name == failure {
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
	for _, invalid := range []string{
		"nil", "missing_state", "account_mismatch", "result_mismatch", "unowned_error", "new_error",
		"qualification_missing_pass", "qualification_proxy_mismatch",
	} {
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
			case "qualification_missing_pass":
				mutation.CompleteQualification = true
				mutation.Schedulable = &enabled
			case "qualification_proxy_mismatch":
				wrongProxyID := int64(4)
				reasoningTokens := 900
				mutation.CompleteQualification = true
				mutation.Schedulable = &enabled
				mutation.Results[0] = service.OpenAIDowngradeProbeResult{
					AccountID: mutation.AccountID, ProxyID: &wrongProxyID,
					TransportOK: true, AnswerCorrect: true, HTTPStatus: 200,
					ReasoningTokens: &reasoningTokens,
				}
			}
			repo := &openAIDowngradeProbeRepository{db: db}
			require.Error(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOpenAIProbeCommitQualificationWithFirstBucketAssignment(t *testing.T) {
	// 新号同轮分桶后完成资格(r15b/r17b 工作流):ExpectedProxyID 为 nil,
	// 合格结果挂在 mutation.ProxyID 上。曾因解引用 nil ExpectedProxyID 在
	// 生产触发 SIGSEGV 崩溃循环(9/20),此用例锁住该路径。
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	enabled := true
	bucketProxyID := int64(9)
	reasoningTokens := 900
	mutation := &service.OpenAIDowngradeMutation{
		AccountID: 7, ExpectedAccountUpdatedAt: now, ExpectedStateUpdatedAt: now,
		ExpectedProxyID: nil, ExpectedStatus: service.StatusActive, ExpectedSchedulable: false,
		ProxyChanged: true, ProxyID: &bucketProxyID,
		Schedulable: &enabled, CompleteQualification: true,
		State: &service.OpenAIDowngradeProbeState{
			AccountID: 7, State: service.OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
			NextProbeAt: now.Add(time.Hour), UpdatedAt: now,
		},
		Results: []service.OpenAIDowngradeProbeResult{{
			AccountID: 7, ProxyID: &bucketProxyID,
			TransportOK: true, AnswerCorrect: true, HTTPStatus: 200,
			ReasoningTokens: &reasoningTokens,
		}},
	}

	mock.ExpectBegin()
	expectValidOpenAIOAuthProxyLock(mock, bucketProxyID)
	expectProbeCommitLocks(mock, mutation)
	mock.ExpectExec("UPDATE accounts SET.*openai_downgrade_qualification").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO openai_downgrade_probe_results").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO proxy_outcome_stats").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE proxies p").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE openai_downgrade_probe_states").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO scheduler_outbox").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &openAIDowngradeProbeRepository{db: db}
	require.NoError(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAIProbeCommitCompletesQualificationAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mutation := probeCommitFixture()
	enabled := true
	reasoningTokens := 900
	mutation.ExpectedSchedulable = false
	mutation.Schedulable = &enabled
	mutation.CompleteQualification = true
	mutation.Events = nil
	mutation.Results = []service.OpenAIDowngradeProbeResult{{
		AccountID: mutation.AccountID, ProxyID: mutation.ExpectedProxyID,
		TransportOK: true, AnswerCorrect: true, HTTPStatus: 200,
		ReasoningTokens: &reasoningTokens,
	}}

	mock.ExpectBegin()
	expectValidOpenAIOAuthProxyLock(mock, *mutation.ExpectedProxyID)
	expectProbeCommitLocks(mock, mutation)
	mock.ExpectExec("UPDATE accounts SET.*openai_downgrade_qualification").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO openai_downgrade_probe_results").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO proxy_outcome_stats").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE proxies p").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE openai_downgrade_probe_states").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO scheduler_outbox").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &openAIDowngradeProbeRepository{db: db}
	require.NoError(t, repo.CommitOpenAIDowngradeMutation(context.Background(), mutation))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAIProbeCommitQualificationRejectsInvalidCurrentProxy(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mutation := probeCommitFixture()
	enabled := true
	reasoningTokens := 900
	mutation.ExpectedSchedulable = false
	mutation.Schedulable = &enabled
	mutation.CompleteQualification = true
	mutation.Results = []service.OpenAIDowngradeProbeResult{{
		AccountID: mutation.AccountID, ProxyID: mutation.ExpectedProxyID,
		TransportOK: true, AnswerCorrect: true, HTTPStatus: 200,
		ReasoningTokens: &reasoningTokens,
	}}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT status, expires_at.*FROM proxies.*FOR SHARE`).
		WithArgs(*mutation.ExpectedProxyID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "expires_at"}).
			AddRow(service.StatusDisabled, nil))
	mock.ExpectRollback()

	repo := &openAIDowngradeProbeRepository{db: db}
	err = repo.CommitOpenAIDowngradeMutation(context.Background(), mutation)

	require.ErrorIs(t, err, service.ErrOpenAIOAuthProxyInvalid)
	require.NoError(t, mock.ExpectationsWereMet())
}

func expectValidOpenAIOAuthProxyLock(mock sqlmock.Sqlmock, proxyID int64) {
	mock.ExpectQuery(`(?s)SELECT status, expires_at.*FROM proxies.*FOR SHARE`).
		WithArgs(proxyID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "expires_at"}).
			AddRow(service.StatusActive, nil))
}

func TestOpenAIProbeCommitRequiresOneUpdatedRow(t *testing.T) {
	for _, count := range []int64{0, 2} {
		require.ErrorIs(t, requireOpenAIProbeUpdatedRow(sqlmock.NewResult(0, count), nil), service.ErrOpenAIProbeStale)
	}
	require.NoError(t, requireOpenAIProbeUpdatedRow(sqlmock.NewResult(0, 1), nil))
	require.ErrorIs(t, requireOpenAIProbeUpdatedRow(sqlmock.NewErrorResult(sql.ErrTxDone), nil), sql.ErrTxDone)
	require.ErrorIs(t, requireOpenAIProbeUpdatedRow(nil, sql.ErrTxDone), sql.ErrTxDone)
}
