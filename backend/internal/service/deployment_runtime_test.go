package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/deployment"
	"github.com/stretchr/testify/require"
)

func TestDeploymentStandbySkipsAutonomousWorkBeforeDependencies(t *testing.T) {
	t.Setenv("DEPLOYMENT_SOCKET", "/fixture/control.sock")
	deployment.Background.SetOpen(false)
	ctx := t.Context()
	// Missing dependencies would panic if these entry points reached the work.
	require.NoError(t, (&UpstreamBillingProbeService{}).RunDue(ctx))
	require.NoError(t, (&OpenCodeGoUsageService{}).RunDue(ctx))
	require.NoError(t, (&OllamaCloudUsageService{}).RunDue(ctx))
	require.NoError(t, (&OpenAIQuotaAutoResetService{}).evaluateAccount(ctx, 1))
	(&OpenAIQuotaAutoResetService{}).scanEnabledAccounts(ctx)
	_, err := (&BatchImageBillingRecoveryService{}).ReleaseStaleUnsubmittedOnce(ctx)
	require.NoError(t, err)
	_, err = (&BatchImageWorker{}).MoveDueDelayedOnce(ctx)
	require.NoError(t, err)
	_, err = (&BatchImageWorker{}).RecoverStaleActiveOnce(ctx)
	require.NoError(t, err)
	(&ContentModerationService{}).runCleanupOnce()
	require.Zero(t, deployment.Background.Pending())
}

func waitRuntimeSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("runtime task did not reach expected state")
	}
}

func runtimeBarrier(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	ch := make(chan struct{})
	release := sync.OnceFunc(func() { close(ch) })
	t.Cleanup(release)
	return ch, release
}

type runtimeAccountRepo struct {
	AccountRepository
	update func(map[int64]time.Time) error
}

func (r *runtimeAccountRepo) BatchUpdateLastUsed(_ context.Context, updates map[int64]time.Time) error {
	return r.update(updates)
}

func TestDeploymentDeferredCountsInflightAndPreservesNewerUpdate(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "retry"}[fail], func(t *testing.T) {
			block, release := runtimeBarrier(t)
			started, finished := make(chan struct{}), make(chan struct{})
			older := time.Now().Add(-time.Hour)
			svc := NewDeferredService(&runtimeAccountRepo{update: func(updates map[int64]time.Time) error {
				close(started)
				<-block
				if fail {
					return errors.New("database unavailable")
				}
				return nil
			}}, nil, time.Second)
			svc.lastUsedUpdates.Store(int64(1), older)
			go func() { svc.flushLastUsed(); close(finished) }()
			waitRuntimeSignal(t, started)
			require.Equal(t, 1, svc.Pending(), "extracted batch is still pending")
			svc.ScheduleLastUsedUpdate(1)
			newer, _ := svc.lastUsedUpdates.Load(int64(1))
			require.Equal(t, 2, svc.Pending(), "inflight batch and newer update both count")
			release()
			waitRuntimeSignal(t, finished)
			require.Equal(t, 1, svc.Pending())
			got, _ := svc.lastUsedUpdates.Load(int64(1))
			require.Equal(t, newer, got)
			svc.accountRepo = &runtimeAccountRepo{update: func(updates map[int64]time.Time) error {
				require.Equal(t, newer, updates[1])
				return nil
			}}
			svc.flushLastUsed()
			require.Zero(t, svc.Pending())
		})
	}
}

func TestDeploymentDeferredFlushHonorsDeadlineWhileAnotherFlushOwnsLock(t *testing.T) {
	svc := NewDeferredService(&runtimeAccountRepo{update: func(map[int64]time.Time) error {
		t.Fatal("timed out flush must not write")
		return nil
	}}, nil, time.Hour)
	svc.ScheduleLastUsedUpdate(1)
	svc.flushMu.Lock()
	defer svc.flushMu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, svc.Flush(ctx), context.DeadlineExceeded)
	require.Equal(t, 1, svc.Pending())
}

type runtimeBillingCache struct {
	BillingCache
	started chan struct{}
	block   <-chan struct{}
}

func (c *runtimeBillingCache) DeductUserBalance(context.Context, int64, float64) error {
	c.started <- struct{}{}
	<-c.block
	return nil
}

func TestDeploymentBillingTailRemainsAfterUsageReturns(t *testing.T) {
	block, release := runtimeBarrier(t)
	cache := &runtimeBillingCache{started: make(chan struct{}, 1), block: block}
	billing := NewBillingCacheService(cache, nil, nil, nil, nil, nil, &config.Config{}, nil)
	usage := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1})
	defer func() { release(); usage.Stop(); billing.Stop() }()
	require.Equal(t, UsageRecordSubmitModeEnqueued, usage.Submit(func(context.Context) {
		require.True(t, billing.enqueueCacheWrite(cacheWriteTask{kind: cacheWriteDeductBalance}))
	}))
	waitRuntimeSignal(t, cache.started)
	usage.Stop()
	require.Zero(t, usage.Pending())
	require.EqualValues(t, 1, billing.PendingWrites())
	release()
	billing.Stop()
	require.Zero(t, billing.PendingWrites())
}

func TestDeploymentAsyncWaitIncludesUsageAndBillingQueues(t *testing.T) {
	producerBlock, releaseProducer := runtimeBarrier(t)
	billingBlock, releaseBilling := runtimeBarrier(t)
	producerStarted := make(chan struct{})
	cache := &runtimeBillingCache{started: make(chan struct{}, 1), block: billingBlock}
	billing := NewBillingCacheService(cache, nil, nil, nil, nil, nil, &config.Config{}, nil)
	usage := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1})
	t.Cleanup(func() { releaseProducer(); releaseBilling(); usage.Stop(); billing.Stop() })
	runDeploymentAsync(func() {
		close(producerStarted)
		<-producerBlock
		usage.Submit(func(context.Context) { billing.QueueDeductBalance(1, 1) })
	})
	waitRuntimeSignal(t, producerStarted)
	require.Zero(t, usage.Pending(), "the producer has not submitted billing yet")
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	go func() { result <- WaitDeploymentAsync(ctx) }()
	releaseProducer()
	waitRuntimeSignal(t, cache.started)
	select {
	case err := <-result:
		t.Fatalf("settlement returned with a blocked billing write: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseBilling()
	require.NoError(t, <-result)
	require.Zero(t, usage.Pending())
	require.Zero(t, billing.PendingWrites())
}

func TestDeploymentAsyncWaitIncludesUsageWorker(t *testing.T) {
	block, release := runtimeBarrier(t)
	started := make(chan struct{})
	usage := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1})
	t.Cleanup(func() { release(); usage.Stop() })
	usage.Submit(func(context.Context) { close(started); <-block })
	waitRuntimeSignal(t, started)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, WaitDeploymentAsync(ctx), context.DeadlineExceeded)
	release()
	require.NoError(t, WaitDeploymentAsync(t.Context()))
}

func TestDeploymentBillingPendingQueueFullAndStopRace(t *testing.T) {
	for i := 0; i < 20; i++ {
		svc := &BillingCacheService{cacheWriteChan: make(chan cacheWriteTask, 1)}
		require.True(t, svc.enqueueCacheWrite(cacheWriteTask{}))
		require.False(t, svc.enqueueCacheWrite(cacheWriteTask{}))
		require.EqualValues(t, 1, svc.PendingWrites())
		svc.cacheWriteWg.Add(1)
		go svc.cacheWriteWorker(svc.cacheWriteChan)
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				svc.enqueueCacheWrite(cacheWriteTask{})
				svc.Stop()
			}()
		}
		wg.Wait()
		require.Zero(t, svc.PendingWrites())
		require.False(t, svc.enqueueCacheWrite(cacheWriteTask{}))
	}
}

type runtimeEmailCache struct {
	EmailCache
	started chan struct{}
	block   <-chan struct{}
	calls   atomic.Int64
}

func (c *runtimeEmailCache) GetVerificationCode(context.Context, string) (*VerificationCodeData, error) {
	c.calls.Add(1)
	c.started <- struct{}{}
	<-c.block
	return &VerificationCodeData{CreatedAt: time.Now()}, nil
}

func TestDeploymentEmailStopDrainsAndRejectsNewTasks(t *testing.T) {
	block, release := runtimeBarrier(t)
	cache := &runtimeEmailCache{started: make(chan struct{}, 2), block: block}
	svc := NewEmailQueueService(&EmailService{cache: cache}, 1)
	defer func() { release(); svc.Stop() }()
	require.NoError(t, svc.EnqueueVerifyCode("local@example.invalid", "local"))
	waitRuntimeSignal(t, cache.started)
	require.NoError(t, svc.EnqueueVerifyCode("local@example.invalid", "local"))
	require.EqualValues(t, 2, svc.Pending())
	finished := make(chan struct{})
	go func() { svc.Stop(); close(finished) }()
	require.Eventually(t, func() bool {
		svc.mu.RLock()
		defer svc.mu.RUnlock()
		return svc.stopped
	}, time.Second, time.Millisecond)
	require.ErrorContains(t, svc.EnqueuePasswordReset("local@example.invalid", "local", "unused"), "stopped")
	select {
	case <-finished:
		t.Fatal("Stop returned before the queued email completed")
	default:
	}
	release()
	waitRuntimeSignal(t, finished)
	require.Zero(t, svc.Pending())
	require.EqualValues(t, 2, cache.calls.Load())
	svc.Stop()
}

func TestDeploymentHandoffWaitsForAcceptedEmail(t *testing.T) {
	block, release := runtimeBarrier(t)
	cache := &runtimeEmailCache{started: make(chan struct{}, 1), block: block}
	svc := NewEmailQueueService(&EmailService{cache: cache}, 1)
	t.Cleanup(func() { release(); svc.Stop() })
	require.NoError(t, svc.EnqueueVerifyCode("local@example.invalid", "local"))
	waitRuntimeSignal(t, cache.started)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, WaitDeploymentAsync(ctx), context.DeadlineExceeded)
	release()
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, WaitDeploymentAsync(ctx))
}

func TestDeploymentEmailQueueFullAndConcurrentStop(t *testing.T) {
	before := deploymentAsyncTasks.Load()
	svc := &EmailQueueService{taskChan: make(chan EmailTask, 1), workers: 1}
	require.NoError(t, svc.enqueue(EmailTask{}))
	require.ErrorContains(t, svc.enqueue(EmailTask{}), "full")
	require.EqualValues(t, 1, svc.Pending())
	svc.start()
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = svc.enqueue(EmailTask{}); svc.Stop() }()
	}
	wg.Wait()
	require.Zero(t, svc.Pending())
	require.Equal(t, before, deploymentAsyncTasks.Load())
}

type runtimeAuditRepo struct {
	AuditLogRepository
	insert func([]*AuditLog) (int64, error)
}

func (r *runtimeAuditRepo) BatchInsert(_ context.Context, batch []*AuditLog) (int64, error) {
	return r.insert(batch)
}

func TestDeploymentAuditPendingIncludesBatchAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			block, release := runtimeBarrier(t)
			started, finished := make(chan struct{}), make(chan struct{})
			svc := NewAuditLogService(&runtimeAuditRepo{insert: func(batch []*AuditLog) (int64, error) {
				close(started)
				<-block
				if fail {
					return 0, errors.New("database unavailable")
				}
				return int64(len(batch)), nil
			}}, nil)
			svc.Start()
			defer func() { release(); svc.Stop() }()
			svc.Record(&AuditLog{})
			go func() { svc.Stop(); close(finished) }()
			waitRuntimeSignal(t, started)
			require.EqualValues(t, 1, svc.Pending(), "batch remains counted during database write")
			svc.Record(&AuditLog{})
			require.EqualValues(t, 1, svc.Pending(), "no enqueue after Stop")
			release()
			waitRuntimeSignal(t, finished)
			require.Zero(t, svc.Pending())
			if fail {
				require.EqualValues(t, 1, atomic.LoadUint64(&svc.writeFailed))
			}
		})
	}
}

func TestDeploymentAuditOverflowAndStopRace(t *testing.T) {
	svc := NewAuditLogService(&runtimeAuditRepo{insert: func(batch []*AuditLog) (int64, error) { return int64(len(batch)), nil }}, nil)
	svc.queue = make(chan *AuditLog, 1)
	svc.Record(&AuditLog{})
	svc.Record(&AuditLog{})
	require.EqualValues(t, 1, svc.Pending())
	require.EqualValues(t, 1, atomic.LoadUint64(&svc.droppedCount))
	svc.Start()
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); svc.Record(&AuditLog{}); svc.Stop() }()
	}
	wg.Wait()
	require.Zero(t, svc.Pending())
}

func TestDeploymentAsyncChildSurvivesParentAndDoneIsIdempotent(t *testing.T) {
	before := deploymentAsyncTasks.Load()
	parent, child := beginDeploymentAsync(), beginDeploymentAsync()
	defer child()
	defer parent()
	parent()
	parent()
	require.Equal(t, before+1, deploymentAsyncTasks.Load())
	child()
	child()
	require.Equal(t, before, deploymentAsyncTasks.Load())
}
