package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/singleflight"
)

func TestDeploymentWaitIncludesDelayedLeaderLockRelease(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	releaseUpstreamBillingProbeLeaderLock(func() { close(entered); <-release }, time.Now().Add(10*time.Millisecond))
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, WaitDeploymentAsync(ctx), context.DeadlineExceeded)
	finish()
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, WaitDeploymentAsync(ctx))
}

type blockingSlotCleanupCache struct {
	ConcurrencyCache
	entered chan struct{}
	release chan struct{}
}

func (c *blockingSlotCleanupCache) CleanupExpiredAccountSlotKeys(context.Context) error {
	close(c.entered)
	<-c.release
	return nil
}

func TestDeploymentSlotCleanupStopJoinsActiveWrite(t *testing.T) {
	cache := &blockingSlotCleanupCache{entered: make(chan struct{}), release: make(chan struct{})}
	svc := NewConcurrencyService(cache)
	var once sync.Once
	finish := func() { once.Do(func() { close(cache.release) }) }
	t.Cleanup(func() { finish(); svc.StopSlotCleanupWorker() })
	svc.StartSlotCleanupWorker(nil, time.Hour)
	<-cache.entered
	svc.slotStopMu.Lock()
	stop := svc.slotStopCh
	svc.slotStopMu.Unlock()
	finished := make(chan struct{})
	go func() { svc.StopSlotCleanupWorker(); close(finished) }()
	<-stop
	select {
	case <-finished:
		t.Fatal("cleanup returned before its active write completed")
	default:
	}
	finish()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not join its completed write")
	}
}

func TestDeploymentSingleflightWaitIncludesAbandonedFollowers(t *testing.T) {
	var group singleflight.Group
	entered, release := make(chan struct{}), make(chan struct{})
	first := deploymentDoChan(&group, "same", func() (any, error) {
		close(entered)
		<-release
		return "completed", nil
	})
	<-entered
	deploymentDoChan(&group, "same", func() (any, error) {
		panic("duplicate callback must not execute")
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := WaitDeploymentAsync(ctx)
	close(release)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, WaitDeploymentAsync(ctx))
	require.Equal(t, "completed", (<-first).Val)
}

func TestDeploymentAsyncWaitIncludesNestedChildren(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	runDeploymentAsync(func() {
		runDeploymentAsync(func() { close(started); <-release })
	})
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, WaitDeploymentAsync(ctx), context.DeadlineExceeded)
	finish()
	ctx, done := context.WithTimeout(t.Context(), 3*time.Second)
	defer done()
	require.NoError(t, WaitDeploymentAsync(ctx))
}

type blockingMessageQueueCache struct {
	UserMsgQueueCache
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingMessageQueueCache) ReconcileExpiredLockCandidates(context.Context, int) (int, error) {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return 0, nil
}

func TestUserMessageQueueStopJoinsCleanup(t *testing.T) {
	cache := &blockingMessageQueueCache{entered: make(chan struct{}), release: make(chan struct{})}
	svc := NewUserMessageQueueService(cache, nil, nil)
	var once sync.Once
	finish := func() { once.Do(func() { close(cache.release) }) }
	t.Cleanup(func() { finish(); svc.Stop() })
	svc.StartCleanupWorker(time.Millisecond)
	<-cache.entered
	done := make(chan struct{})
	go func() { svc.Stop(); close(done) }()
	<-svc.stopCh
	select {
	case <-done:
		t.Fatal("Stop returned before cleanup finished")
	default:
	}
	finish()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not join cleanup")
	}
	svc.StartCleanupWorker(time.Millisecond)
	svc.Stop()
}

type blockingIdempotencyCleanupRepo struct {
	IdempotencyRepository
	entered chan struct{}
	release chan struct{}
}

func (r *blockingIdempotencyCleanupRepo) DeleteExpired(context.Context, time.Time, int) (int64, error) {
	close(r.entered)
	<-r.release
	return 0, nil
}

func TestIdempotencyStopJoinsCleanup(t *testing.T) {
	repo := &blockingIdempotencyCleanupRepo{entered: make(chan struct{}), release: make(chan struct{})}
	svc := NewIdempotencyCleanupService(repo, nil)
	var once sync.Once
	finish := func() { once.Do(func() { close(repo.release) }) }
	t.Cleanup(func() { finish(); svc.Stop() })
	svc.Start()
	<-repo.entered
	done := make(chan struct{})
	go func() { svc.Stop(); close(done) }()
	<-svc.stopCh
	select {
	case <-done:
		t.Fatal("Stop returned before cleanup finished")
	default:
	}
	finish()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not join cleanup")
	}
	svc.Start()
	svc.Stop()
}
