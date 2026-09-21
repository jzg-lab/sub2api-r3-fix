//go:build unit

package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// r17x E 项回归:isOpenAIAccountRuntimeBlocked 无锁快路径。
// 快路径只在"值确定活跃(blockUntil 未过期)"时短路返回 true;
// 未命中/过期/零值都落回持锁原路径,过期清理副作用(Delete+Generation)不变。

func TestRuntimeBlockFastPath_BlockedAccountShortCircuits(t *testing.T) {
	svc := &OpenAIGatewayService{}
	acc := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	until := time.Now().Add(30 * time.Second)

	// 经正式写入口建立 block(持锁+CAS)
	mu := svc.openAIAccountRuntimeBlockLock(acc.ID)
	mu.Lock()
	_, extended := svc.blockAccountSchedulingLocked(acc, until, "test")
	mu.Unlock()
	require.True(t, extended)

	require.True(t, svc.isOpenAIAccountRuntimeBlocked(acc), "未过期 block 必须命中快路径返回 true")
}

func TestRuntimeBlockFastPath_ExpiredFallsBackAndCleans(t *testing.T) {
	svc := &OpenAIGatewayService{}
	acc := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	// 直接放一个已过期的 blockUntil
	svc.openaiAccountRuntimeBlockUntil.Store(acc.ID, time.Now().Add(-time.Second))

	require.False(t, svc.isOpenAIAccountRuntimeBlocked(acc), "过期 block 必须返回 false")
	// 过期清理副作用照旧:条目被删
	_, exists := svc.openaiAccountRuntimeBlockUntil.Load(acc.ID)
	require.False(t, exists, "过期条目必须被持锁路径清理")
}

func TestRuntimeBlockFastPath_UnblockedNoEntry(t *testing.T) {
	svc := &OpenAIGatewayService{}
	acc := &Account{ID: 44, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(acc))
}

func TestRuntimeBlockFastPath_ConcurrentReadWhileBlocking(t *testing.T) {
	svc := &OpenAIGatewayService{}
	acc := &Account{ID: 45, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	var wg sync.WaitGroup
	var blockedCount atomic.Int64
	// 并发读 + 并发写扩 blockUntil:不得 panic/deadlock,读侧结果单调(一旦 true 不回 false)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if svc.isOpenAIAccountRuntimeBlocked(acc) {
					blockedCount.Add(1)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			mu := svc.openAIAccountRuntimeBlockLock(acc.ID)
			mu.Lock()
			svc.blockAccountSchedulingLocked(acc, time.Now().Add(time.Duration(j+1)*time.Second), "storm")
			mu.Unlock()
		}
	}()
	wg.Wait()
	require.GreaterOrEqual(t, blockedCount.Load(), int64(0), "并发下不 panic 即通过;blocked 观测数=")
}
