package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type blockingModerationLogRepo struct {
	contentModerationTestRepo
	entered           chan struct{}
	release           chan struct{}
	once              sync.Once
	panicAfterRelease bool
}

func (r *blockingModerationLogRepo) CreateLog(ctx context.Context, entry *ContentModerationLog) error {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	if r.panicAfterRelease {
		panic("injected audit worker failure")
	}
	return r.contentModerationTestRepo.CreateLog(ctx, entry)
}

func TestContentModerationHandoffWaitsForAcceptedAudit(t *testing.T) {
	for _, name := range []string{"complete", "panic"} {
		t.Run(name, func(t *testing.T) {
			repo := &blockingModerationLogRepo{entered: make(chan struct{}), release: make(chan struct{}), panicAfterRelease: name == "panic"}
			svc := NewContentModerationService(&contentModerationTestSettingRepo{}, repo, nil, nil, nil, nil, nil, nil)
			var once sync.Once
			finish := func() { once.Do(func() { close(repo.release) }) }
			t.Cleanup(func() { finish(); svc.Stop() })
			svc.enqueueRecord(ContentModerationCheckInput{}, defaultContentModerationConfig(), &ContentModerationLog{}, "", false, false)
			select {
			case <-repo.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("audit worker did not start")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			require.ErrorIs(t, WaitDeploymentAsync(ctx), context.DeadlineExceeded)
			finish()
			ctx, cancel = context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			require.NoError(t, WaitDeploymentAsync(ctx))
		})
	}
}

func TestContentModerationRejectedTasksDoNotLeakHandoffCount(t *testing.T) {
	before := deploymentAsyncTasks.Load()
	svc := &ContentModerationService{asyncQueue: make(chan contentModerationTask, 1)}
	t.Cleanup(func() {
		for len(svc.asyncQueue) > 0 {
			task := <-svc.asyncQueue
			if task.deploymentDone != nil {
				task.deploymentDone()
			}
		}
		require.Equal(t, before, deploymentAsyncTasks.Load())
	})
	cfg := defaultContentModerationConfig()
	cfg.QueueSize = 2
	svc.enqueueAsync(ContentModerationCheckInput{}, cfg, ContentModerationInput{}, "")
	require.Equal(t, before+1, deploymentAsyncTasks.Load())
	for _, queueSize := range []int{2, 1} {
		cfg.QueueSize = queueSize
		svc.enqueueAsync(ContentModerationCheckInput{}, cfg, ContentModerationInput{}, "")
		svc.enqueueRecord(ContentModerationCheckInput{}, cfg, &ContentModerationLog{}, "", false, false)
		require.Equal(t, before+1, deploymentAsyncTasks.Load())
	}
	svc.stopping.Store(true)
	svc.enqueueAsync(ContentModerationCheckInput{}, cfg, ContentModerationInput{}, "")
	svc.enqueueRecord(ContentModerationCheckInput{}, cfg, &ContentModerationLog{}, "", false, false)
	require.Equal(t, before+1, deploymentAsyncTasks.Load())
}

func TestContentModerationConfigurationSkipReleasesHandoffCount(t *testing.T) {
	svc := NewContentModerationService(&contentModerationTestSettingRepo{}, &contentModerationTestRepo{}, nil, nil, nil, nil, nil, nil)
	t.Cleanup(svc.Stop)
	svc.enqueueAsync(ContentModerationCheckInput{}, defaultContentModerationConfig(), ContentModerationInput{}, "")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.NoError(t, WaitDeploymentAsync(ctx))
	require.Empty(t, svc.asyncQueue)
	require.Zero(t, svc.asyncProcessed.Load(), "existing disabled configuration remains effective")
}

func TestContentModerationStopDrainsAcceptedAuditRecords(t *testing.T) {
	repo := &blockingModerationLogRepo{entered: make(chan struct{}), release: make(chan struct{})}
	svc := NewContentModerationService(&contentModerationTestSettingRepo{}, repo, nil, nil, nil, nil, nil, nil)
	var once sync.Once
	finish := func() { once.Do(func() { close(repo.release) }) }
	t.Cleanup(func() { finish(); svc.Stop() })
	cfg := defaultContentModerationConfig()
	for i := 0; i < 4; i++ {
		svc.enqueueRecord(ContentModerationCheckInput{}, cfg, &ContentModerationLog{Action: ContentModerationActionAllow}, "", false, false)
	}
	select {
	case <-repo.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not persist accepted log")
	}
	done := make(chan struct{})
	go func() { svc.Stop(); close(done) }()
	require.Eventually(t, svc.stopping.Load, time.Second, time.Millisecond)
	select {
	case <-done:
		t.Fatal("Stop returned before audit persistence finished")
	default:
	}
	before := svc.asyncEnqueued.Load()
	svc.enqueueRecord(ContentModerationCheckInput{}, cfg, &ContentModerationLog{}, "", false, false)
	require.Equal(t, before, svc.asyncEnqueued.Load())
	finish()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("accepted queue did not drain")
	}
	repo.mu.Lock()
	require.Len(t, repo.logs, 4)
	repo.mu.Unlock()
}

func TestContentModerationEnqueueConcurrentWithStop(t *testing.T) {
	before := deploymentAsyncTasks.Load()
	svc := NewContentModerationService(&contentModerationTestSettingRepo{}, &contentModerationTestRepo{}, nil, nil, nil, nil, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				svc.enqueueAsync(ContentModerationCheckInput{}, nil, ContentModerationInput{}, "")
				svc.enqueueRecord(ContentModerationCheckInput{}, nil, &ContentModerationLog{}, "", false, false)
			}
		}()
	}
	svc.Stop()
	wg.Wait()
	svc.Stop()
	require.Equal(t, before, deploymentAsyncTasks.Load())
}
