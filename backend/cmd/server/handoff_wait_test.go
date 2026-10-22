package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/wsdrain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type handoffAccountRepo struct {
	service.AccountRepository
	write func(context.Context, map[int64]time.Time) error
}

func (r handoffAccountRepo) BatchUpdateLastUsed(ctx context.Context, values map[int64]time.Time) error {
	return r.write(ctx, values)
}

func handoffTestApplication(t *testing.T, write func(context.Context, map[int64]time.Time) error) *Application {
	t.Helper()
	app := &Application{
		UsageWorkers: service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1}),
		Billing:      service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, &config.Config{}, nil),
		Deferred:     service.NewDeferredService(handoffAccountRepo{write: write}, nil, time.Hour),
	}
	t.Cleanup(func() { app.UsageWorkers.Stop(); app.Billing.Stop() })
	return app
}

func TestHandoffWaitIncludesLateUsageThenFlushesWithoutTimer(t *testing.T) {
	flushed := make(chan map[int64]time.Time, 1)
	app := handoffTestApplication(t, func(_ context.Context, values map[int64]time.Time) error {
		flushed <- values
		return nil
	})
	producer, worker := make(chan struct{}), make(chan struct{})
	workerStarted := make(chan struct{})
	releaseProducer := sync.OnceFunc(func() { close(producer) })
	releaseWorker := sync.OnceFunc(func() { close(worker) })
	t.Cleanup(func() { releaseProducer(); releaseWorker() })
	service.RunDeploymentAsync(func() {
		<-producer
		app.UsageWorkers.Submit(func(context.Context) {
			close(workerStarted)
			<-worker
			app.Deferred.ScheduleLastUsedUpdate(42)
		})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.waitForHandoff(ctx, wsdrain.New()) }()
	releaseProducer()
	select {
	case <-workerStarted:
	case <-ctx.Done():
		t.Fatal("usage worker did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("handoff finished before usage: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseWorker()
	require.NoError(t, <-done)
	require.Contains(t, <-flushed, int64(42))
	require.Zero(t, app.Deferred.Pending())
}

func TestHandoffWaitKeepsFailedLastUsedWritesForRetry(t *testing.T) {
	failed := true
	app := handoffTestApplication(t, func(ctx context.Context, _ map[int64]time.Time) error {
		_, bounded := ctx.Deadline()
		require.True(t, bounded)
		if failed {
			return errors.New("database unavailable")
		}
		return nil
	})
	app.Deferred.ScheduleLastUsedUpdate(42)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.ErrorContains(t, app.waitForHandoff(ctx, wsdrain.New()), "database unavailable")
	require.Equal(t, 1, app.Deferred.Pending())
	failed = false
	require.NoError(t, app.waitForHandoff(ctx, wsdrain.New()))
	require.Zero(t, app.Deferred.Pending())
}
