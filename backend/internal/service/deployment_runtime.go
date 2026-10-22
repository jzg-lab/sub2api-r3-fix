package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/deployment"

	"golang.org/x/sync/singleflight"
)

var deploymentAsyncTasks atomic.Int64

func beginDeploymentBackground() (func(), bool) { return deployment.BeginBackground() }

// RunDeploymentAsync retains accepted handler work through coordinated shutdown.
func RunDeploymentAsync(task func()) { runDeploymentAsync(task) }

// TrackDeploymentAsync transfers ownership into a queue until its actual flush,
// which may outlive the caller's cancellation. The returned release is idempotent.
func TrackDeploymentAsync() func() { return beginDeploymentAsync() }

// Callers may abandon DoChan results on cancellation. Retain the shared work
// before scheduling it and release even when nobody consumes the result.
func deploymentDoChan(group *singleflight.Group, key string, fn func() (any, error)) <-chan singleflight.Result {
	done := beginDeploymentAsync()
	source := group.DoChan(key, fn)
	result := make(chan singleflight.Result, 1)
	go func() {
		defer done()
		result <- <-source
	}()
	return result
}

func runDeploymentAsync(task func()) {
	done := beginDeploymentAsync()
	go func() { defer done(); task() }()
}

// Includes accepted finite work, queued writes, and their producers. Call only after
// ingress and autonomous producers have stopped.
func WaitDeploymentAsync(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for deploymentAsyncTasks.Load() != 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("asynchronous tasks pending=%d: %w", deploymentAsyncTasks.Load(), ctx.Err())
		case <-ticker.C:
		}
	}
	return ctx.Err()
}

// Register before launching a child so a finishing parent cannot expose a zero gap.
func beginDeploymentAsync() func() {
	deploymentAsyncTasks.Add(1)
	return sync.OnceFunc(func() { deploymentAsyncTasks.Add(-1) })
}
