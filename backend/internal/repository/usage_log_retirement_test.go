package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDeploymentWaitRetainsCanceledUsageBatchUntilFlush(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := newUsageLogRepositoryWithSQL(nil, db)
	// Use the real queue handoff with an explicit consumer barrier, not a timer race.
	repo.bestEffortBatchCh = make(chan usageLogBestEffortRequest, 1)
	repo.bestEffortBatchOnce.Do(func() {})
	ctx, cancel := context.WithCancel(t.Context())
	returned := make(chan error, 1)
	go func() {
		returned <- repo.CreateBestEffort(ctx, &service.UsageLog{RequestID: "retirement-test", APIKeyID: 1, Model: "local"})
	}()
	queued := <-repo.bestEffortBatchCh
	cancel()
	require.Error(t, <-returned)
	short, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer stop()
	require.ErrorIs(t, service.WaitDeploymentAsync(short), context.DeadlineExceeded)
	mock.ExpectExec("INSERT INTO usage_logs").WillReturnResult(sqlmock.NewResult(1, 1))
	repo.flushBestEffortBatch(db, []usageLogBestEffortRequest{queued})
	deadline, done := context.WithTimeout(t.Context(), time.Second)
	defer done()
	require.NoError(t, service.WaitDeploymentAsync(deadline))
	require.NoError(t, mock.ExpectationsWereMet())
}
