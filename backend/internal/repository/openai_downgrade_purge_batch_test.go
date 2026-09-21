package repository

import (
	"context"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// r17x H 项回归:保留清理按批删除,直到单批返回不足一批。
func TestPurgeOpenAIDowngradeHistory_BatchedUntilExhausted(t *testing.T) {
	cutoffResults := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	cutoffEvents := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &openAIDowngradeProbeRepository{db: db}

	fullBatch := int64(openAIDowngradePurgeBatchSize)
	// results:两批满 + 一批不足 → 3 次调用
	for i := 0; i < 2; i++ {
		mock.ExpectExec("DELETE FROM openai_downgrade_probe_results").
			WithArgs(cutoffResults, openAIDowngradePurgeBatchSize).
			WillReturnResult(sqlmock.NewResult(0, fullBatch))
	}
	mock.ExpectExec("DELETE FROM openai_downgrade_probe_results").
		WithArgs(cutoffResults, openAIDowngradePurgeBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 123))
	// events:一批不足 → 1 次调用
	mock.ExpectExec("DELETE FROM openai_downgrade_probe_events").
		WithArgs(cutoffEvents, openAIDowngradePurgeBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 7))

	purgedResults, purgedEvents, err := repo.PurgeOpenAIDowngradeProbeHistory(
		context.Background(), cutoffResults, cutoffEvents)
	require.NoError(t, err)
	require.Equal(t, int64(2*openAIDowngradePurgeBatchSize+123), purgedResults)
	require.Equal(t, int64(7), purgedEvents)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 首批就删空(0 行)也正常收敛,不再发起后续批次。
func TestPurgeOpenAIDowngradeHistory_EmptyFirstBatch(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &openAIDowngradeProbeRepository{db: db}

	mock.ExpectExec("DELETE FROM openai_downgrade_probe_results").
		WithArgs(cutoff, openAIDowngradePurgeBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM openai_downgrade_probe_events").
		WithArgs(cutoff, openAIDowngradePurgeBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 0))

	purgedResults, purgedEvents, err := repo.PurgeOpenAIDowngradeProbeHistory(
		context.Background(), cutoff, cutoff)
	require.NoError(t, err)
	require.Zero(t, purgedResults)
	require.Zero(t, purgedEvents)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ctx 已取消时不发起任何 DELETE,直接返回取消错误。
func TestPurgeOpenAIDowngradeHistory_ContextCancelStopsBatches(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &openAIDowngradeProbeRepository{db: db}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	purgedResults, _, err := repo.PurgeOpenAIDowngradeProbeHistory(ctx, cutoff, cutoff)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, purgedResults)
	require.NoError(t, mock.ExpectationsWereMet(), "取消后不得发起任何 DELETE")
}

// 批删除必须走 ctid 受害者子查询(防全表单条 DELETE)。
func TestPurgeOpenAIDowngradeHistory_UsesCtidBatching(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		require.Contains(t, actual, "WITH victims AS")
		require.Contains(t, actual, "SELECT ctid")
		require.Contains(t, actual, "LIMIT $2")
		require.Contains(t, actual, "WHERE ctid IN (SELECT ctid FROM victims)")
		return nil
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	defer db.Close()
	repo := &openAIDowngradeProbeRepository{db: db}

	mock.ExpectExec("results").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("events").WillReturnResult(sqlmock.NewResult(0, 0))

	_, _, err = repo.PurgeOpenAIDowngradeProbeHistory(context.Background(), cutoff, cutoff)
	require.NoError(t, err)
}
