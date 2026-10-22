package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestUsageRecordPendingIncludesSynchronousOverflow(t *testing.T) {
	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{
		WorkerCount: 1, QueueSize: 1, TaskTimeout: time.Second, OverflowPolicy: config.UsageRecordOverflowPolicySync,
	})
	block, release := runtimeBarrier(t)
	defer func() { release(); pool.Stop() }()
	started, syncStarted, finished := make(chan struct{}), make(chan struct{}), make(chan UsageRecordSubmitMode, 1)
	require.Equal(t, UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) { close(started); <-block }))
	waitRuntimeSignal(t, started)
	require.Equal(t, UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) { panic("queued failure") }))
	go func() {
		finished <- pool.Submit(func(context.Context) { close(syncStarted); <-block; panic("sync failure") })
	}()
	waitRuntimeSignal(t, syncStarted)
	require.EqualValues(t, 3, pool.Pending())
	release()
	require.Equal(t, UsageRecordSubmitModeSync, <-finished)
	pool.Stop()
	require.Zero(t, pool.Pending(), "panics and sync overflow release pending work")
}
