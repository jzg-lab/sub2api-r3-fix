package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func downgradeDueColumns() []string {
	return strings.Fields(`account_id state original_proxy_id current_proxy_id probe_mode
		consecutive_failures consecutive_successes first_failure_at circuit_opened_at recovery_deadline
		next_probe_at swap_count_7d last_swap_at last_probe_at auth_consecutive_failures astra_consecutive_failures
		astra_consecutive_successes astra_next_probe_at updated_at consecutive_429s`)
}

func TestProbeAnswerVerdictPersistence(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		transport, inconclusive, correct bool
		want                             any
	}{
		{"ambiguous", true, true, false, nil},
		{"transport", false, false, false, nil},
		{"wrong", true, false, false, false},
		{"correct", true, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			tokens := 300
			mock.ExpectBegin()
			mock.ExpectExec("INSERT INTO openai_downgrade_probe_results").
				WithArgs(int64(7), nil, "normal", tc.transport, tc.want, tokens, nil, int64(0), 200, "", 0).
				WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectCommit()
			repo := &openAIDowngradeProbeRepository{db: db}
			require.NoError(t, repo.RecordOpenAIDowngradeProbe(context.Background(), &service.OpenAIDowngradeProbeResult{
				AccountID: 7, Mode: "normal", TransportOK: tc.transport,
				AnswerCorrect: tc.correct, AnswerInconclusive: tc.inconclusive, ReasoningTokens: &tokens, HTTPStatus: 200,
			}))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestDowngradeDueQueryFiltersBeforeIPRank(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	matcher := sqlmock.QueryMatcherFunc(func(_, actual string) error {
		query := strings.Join(strings.Fields(actual), " ")
		rankFilter := strings.Index(query, "WHERE probe_rank = 1")
		require.Greater(t, rankFilter, 0)
		dueQuery := query[:rankFilter]
		for _, predicate := range []string{
			"JOIN accounts a ON a.id = s.account_id",
			"a.deleted_at IS NULL",
			"a.platform = 'openai' AND a.type = 'oauth'",
			"a.parent_account_id IS NULL",
			"AND s.state <> 'pending_replace'",
			"a.auto_pause_on_expired IS NOT TRUE OR a.expires_at IS NULL OR a.expires_at > $1",
			"WHERE c.account_id = a.id AND c.manual_paused",
			"a.status = 'active' OR (a.status = 'error' AND EXISTS",
			"WHERE c.account_id = a.id AND c.owned_error = a.error_message",
			"s.state <> 'on_duty' OR a.schedulable IS TRUE OR s.probe_mode = 'qualification' OR a.status = 'error'",
			// r17aq：auth 一振暂停（schedulable=false 是探针落的）必须继续
			// 被 ListDue 拾取，否则暂停号永不再探=死锁。
			"OR s.auth_consecutive_failures > 0",
			"jsonb_typeof(a.extra->'openai_rescue_lane') = 'object'",
			"a.extra->'openai_rescue_lane'->>'entered_at' ~",
			"jsonb_path_query_first_tz(",
			"'$.datetime()', '{}'::jsonb, true",
			"COALESCE(a.extra->'openai_rescue_lane'->>'exit_reason', '') = ''",
		} {
			require.Contains(t, dueQuery, predicate, "ineligible rows must not occupy an IP rank")
		}
		// Manual proxy changes must throttle the live account IP, not the stale state IP.
		require.Contains(t, dueQuery, "LEFT JOIN proxies p ON p.id = a.proxy_id")
		require.Equal(t, 2, strings.Count(dueQuery, "'proxy:' || a.proxy_id::text"))
		require.Contains(t, dueQuery, "recent.proxy_id = a.proxy_id")
		require.NotContains(t, dueQuery, "p.id = s.current_proxy_id")
		require.Contains(t, query, "ORDER BY next_probe_at, account_id LIMIT $2")
		return nil
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	defer db.Close()
	repo := &openAIDowngradeProbeRepository{db: db}
	for _, limit := range []int{-1, 0, 7} {
		expectedLimit := limit
		if expectedLimit <= 0 {
			expectedLimit = 100
		}
		rows := sqlmock.NewRows(downgradeDueColumns()).AddRow(
			int64(12), "on_duty", int64(3), int64(4), "qualification", 1, 2,
			nil, nil, nil, now, 1, nil, now, 0, 0, 0, nil, now, 5)
		mock.ExpectQuery("due").WithArgs(now, expectedLimit).WillReturnRows(rows).RowsWillBeClosed()
		result, queryErr := repo.ListDueOpenAIDowngradeStates(context.Background(), now, limit)
		require.NoError(t, queryErr)
		require.Len(t, result, 1)
		require.Equal(t, int64(12), result[0].AccountID)
		require.Equal(t, int64(3), *result[0].OriginalProxyID)
		require.Equal(t, int64(4), *result[0].CurrentProxyID)
		require.Equal(t, "qualification", result[0].ProbeMode)
		require.Equal(t, 2, result[0].ConsecutiveSuccesses)
		require.Equal(t, now, result[0].UpdatedAt)
		require.Equal(t, 5, result[0].Consecutive429s)
		require.NoError(t, mock.ExpectationsWereMet())
	}
}

func TestDowngradeDueQueryErrorsAndEmptyResult(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	dbErr := errors.New("due query unavailable")
	for _, stage := range []string{"query", "scan", "iterate", "empty"} {
		t.Run(stage, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := &openAIDowngradeProbeRepository{db: db}
			expectation := mock.ExpectQuery("WITH due AS").WithArgs(now, 10)
			switch stage {
			case "query":
				expectation.WillReturnError(dbErr)
			case "scan":
				expectation.WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(1)).RowsWillBeClosed()
			case "iterate":
				rows := sqlmock.NewRows(downgradeDueColumns()).AddRow(
					1, "on_duty", nil, nil, "normal", 0, 0, nil, nil, nil, now, 0, nil, nil, 0, 0, 0, nil, now, 0)
				expectation.WillReturnRows(rows.RowError(0, dbErr)).RowsWillBeClosed()
			default:
				expectation.WillReturnRows(sqlmock.NewRows(downgradeDueColumns())).RowsWillBeClosed()
			}
			result, queryErr := repo.ListDueOpenAIDowngradeStates(context.Background(), now, 10)
			if stage == "empty" {
				require.NoError(t, queryErr)
			} else {
				require.Error(t, queryErr)
				if stage != "scan" {
					require.ErrorIs(t, queryErr, dbErr)
				}
			}
			require.Empty(t, result)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestRecordOpenAIDowngradeProbeIsAtomicWithDerivedStats(t *testing.T) {
	injected := errors.New("derived stats unavailable")
	proxyID := int64(3)
	result := &service.OpenAIDowngradeProbeResult{
		AccountID: 7,
		ProxyID:   &proxyID,
	}

	for _, failure := range []string{"stats_upsert", "score_sync", "success"} {
		t.Run(failure, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			mock.ExpectBegin()
			mock.ExpectExec("INSERT INTO openai_downgrade_probe_results").
				WillReturnResult(sqlmock.NewResult(1, 1))
			stats := mock.ExpectExec("INSERT INTO proxy_outcome_stats")
			if failure == "stats_upsert" {
				stats.WillReturnError(injected)
				mock.ExpectRollback()
			} else {
				stats.WillReturnResult(sqlmock.NewResult(1, 1))
				score := mock.ExpectExec("UPDATE proxies p")
				if failure == "score_sync" {
					score.WillReturnError(injected)
					mock.ExpectRollback()
				} else {
					score.WillReturnResult(sqlmock.NewResult(0, 1))
					mock.ExpectCommit()
				}
			}

			repo := &openAIDowngradeProbeRepository{db: db}
			recordErr := repo.RecordOpenAIDowngradeProbe(context.Background(), result)
			if failure == "success" {
				require.NoError(t, recordErr)
			} else {
				require.ErrorIs(t, recordErr, injected)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
