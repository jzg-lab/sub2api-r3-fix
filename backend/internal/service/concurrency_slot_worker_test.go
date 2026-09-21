//go:build unit

package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// r17x I 项回归:slot cleanup worker 必须可停止。

// stubConcurrencyCache 只实现 CleanupExpiredAccountSlotKeys 计数,其余方法
// 通过嵌入接口 panic(测试只触达 worker 路径)。
type stubSlotCleanupCache struct {
	ConcurrencyCache
	cleanups atomic.Int64
}

func (c *stubSlotCleanupCache) CleanupExpiredAccountSlotKeys(ctx context.Context) error {
	c.cleanups.Add(1)
	return nil
}

// Stop 后 worker 退出:不再有新的 cleanup 执行。
func TestSlotCleanupWorker_StopsOnSignal(t *testing.T) {
	cache := &stubSlotCleanupCache{}
	svc := NewConcurrencyService(cache)
	svc.StartSlotCleanupWorker(nil, 10*time.Millisecond)

	// 等 worker 至少跑几轮
	require.Eventually(t, func() bool { return cache.cleanups.Load() >= 2 }, 2*time.Second, 10*time.Millisecond)

	svc.StopSlotCleanupWorker()
	// 给 ticker 一点时间确认不再增长
	time.Sleep(80 * time.Millisecond)
	stopped := cache.cleanups.Load()
	time.Sleep(80 * time.Millisecond)
	require.Equal(t, stopped, cache.cleanups.Load(), "Stop 后不得再执行 cleanup")
}

// Stop 幂等;未启动时 Stop 是 no-op 不 panic。
func TestSlotCleanupWorker_StopIdempotentAndSafeWhenNeverStarted(t *testing.T) {
	svc := NewConcurrencyService(&stubSlotCleanupCache{})
	svc.StopSlotCleanupWorker() // 未启动过
	svc.StopSlotCleanupWorker() // 重复

	cache := &stubSlotCleanupCache{}
	svc2 := NewConcurrencyService(cache)
	svc2.StartSlotCleanupWorker(nil, 10*time.Millisecond)
	require.Eventually(t, func() bool { return cache.cleanups.Load() >= 1 }, 2*time.Second, 10*time.Millisecond)
	svc2.StopSlotCleanupWorker()
	svc2.StopSlotCleanupWorker() // 重复 Stop 不 panic、不 double-close
}

// 并发 Stop + 重启不竞态(-race 下验证)。
func TestSlotCleanupWorker_RestartUnderConcurrency(t *testing.T) {
	cache := &stubSlotCleanupCache{}
	svc := NewConcurrencyService(cache)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.StartSlotCleanupWorker(nil, 5*time.Millisecond)
			time.Sleep(20 * time.Millisecond)
			svc.StopSlotCleanupWorker()
		}()
	}
	wg.Wait()
	svc.StopSlotCleanupWorker()
}
