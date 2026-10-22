package repository

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBatchUpdateLastUsedUsesMonotonicDatabaseUpdate(t *testing.T) {
	repo, mock := atomicCreateRepository(t)
	require.NoError(t, repo.BatchUpdateLastUsed(t.Context(), nil))
	stamp := time.Now().UTC()
	query := "UPDATE accounts SET last_used_at = GREATEST(last_used_at, CASE id WHEN $1 THEN $2::timestamptz END), updated_at = NOW() WHERE id = ANY($3) AND deleted_at IS NULL"
	mock.ExpectExec(regexp.QuoteMeta(query)).WithArgs(int64(71), stamp, "{71}").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO scheduler_outbox`).
		WithArgs(service.SchedulerOutboxEventAccountLastUsed, nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.BatchUpdateLastUsed(t.Context(), map[int64]time.Time{71: stamp}))
}

func TestBatchUpdateLastUsedDatabaseFailureDoesNotPublishOutbox(t *testing.T) {
	repo, mock := atomicCreateRepository(t)
	injected := errors.New("database unavailable")
	mock.ExpectExec(`UPDATE accounts SET last_used_at`).WillReturnError(injected)
	require.ErrorIs(t, repo.BatchUpdateLastUsed(t.Context(), map[int64]time.Time{71: time.Now()}), injected)
}
