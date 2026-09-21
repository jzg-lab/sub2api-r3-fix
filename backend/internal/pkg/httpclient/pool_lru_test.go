//go:build unit

package httpclient

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// r17x G 项回归：共享客户端池的上限驱逐与空闲回收。

// 同键重复 getOrBuild 复用同一实例，build 只调一次。
func TestClientPool_SameKeyReusesClient(t *testing.T) {
	p := newClientPool()
	builds := 0
	first, err := p.getOrBuild("k", func() (*http.Client, *http.Transport, error) {
		builds++
		return &http.Client{}, nil, nil
	})
	require.NoError(t, err)
	second, err := p.getOrBuild("k", func() (*http.Client, *http.Transport, error) {
		builds++
		return &http.Client{}, nil, nil
	})
	require.NoError(t, err)
	require.Same(t, first, second)
	require.Equal(t, 1, builds)
}

// 超过 maxSharedClients 时按 LRU 驱逐最久未用条目；被驱逐条目的 idle 连接被关闭。
func TestClientPool_EvictsLRUWhenOverCap(t *testing.T) {
	p := newClientPool()
	base := time.Now().Add(-time.Hour)
	keyAt := func(i int) string { return string(rune('a'+i%26)) + string(rune('0'+i/26)) }
	// 填满到上限(不溢出),并设置严格递增的 lastUsed,避免纳秒并列导致 LRU 顺序不确定
	for i := 0; i < maxSharedClients; i++ {
		_, err := p.getOrBuild(keyAt(i), func() (*http.Client, *http.Transport, error) {
			return &http.Client{}, nil, nil
		})
		require.NoError(t, err)
	}
	p.mu.Lock()
	for i := 0; i < maxSharedClients; i++ {
		p.entries[keyAt(i)].lastUsed.Store(base.Add(time.Duration(i) * time.Second).UnixNano())
	}
	p.mu.Unlock()

	// 再插两个键触发溢出驱逐
	for _, extra := range []string{"zz-1", "zz-2"} {
		_, err := p.getOrBuild(extra, func() (*http.Client, *http.Transport, error) {
			return &http.Client{}, nil, nil
		})
		require.NoError(t, err)
	}

	p.mu.RLock()
	size := len(p.entries)
	p.mu.RUnlock()
	require.Equal(t, maxSharedClients, size, "池内条目必须收敛到上限")
	require.Nil(t, p.get(keyAt(0)), "最久未用条目必须被驱逐")
	require.Nil(t, p.get(keyAt(1)), "次久未用条目必须被驱逐")
	require.NotNil(t, p.get(keyAt(2)), "较新条目必须保留")
}

// 活跃使用会刷新 lastUsed，驱逐时保留近期用过的条目。
func TestClientPool_ActiveUseRefreshesRecency(t *testing.T) {
	p := newClientPool()
	base := time.Now().Add(-time.Hour)
	keyAt := func(i int) string { return string(rune('a'+i%26)) + string(rune('0'+i/26)) }
	// 填满池并设置严格递增时间戳
	for i := 0; i < maxSharedClients; i++ {
		_, err := p.getOrBuild(keyAt(i), func() (*http.Client, *http.Transport, error) {
			return &http.Client{}, nil, nil
		})
		require.NoError(t, err)
	}
	p.mu.Lock()
	for i := 0; i < maxSharedClients; i++ {
		p.entries[keyAt(i)].lastUsed.Store(base.Add(time.Duration(i) * time.Second).UnixNano())
	}
	p.mu.Unlock()

	// 把最早插入的键标记为"刚刚用过"
	require.NotNil(t, p.get(keyAt(0)))
	// 溢出一个键,触发 LRU 驱逐
	_, err := p.getOrBuild("zz-new", func() (*http.Client, *http.Transport, error) {
		return &http.Client{}, nil, nil
	})
	require.NoError(t, err)
	require.NotNil(t, p.get(keyAt(0)), "刚用过的条目不能被 LRU 驱逐")
}

// 空闲超过 TTL 的条目被机会式清扫移除；未过期条目保留。
func TestClientPool_SweepRemovesIdleEntries(t *testing.T) {
	p := newClientPool()
	idleKey := "idle"
	freshKey := "fresh"
	_, err := p.getOrBuild(idleKey, func() (*http.Client, *http.Transport, error) {
		return &http.Client{}, nil, nil
	})
	require.NoError(t, err)
	_, err = p.getOrBuild(freshKey, func() (*http.Client, *http.Transport, error) {
		return &http.Client{}, nil, nil
	})
	require.NoError(t, err)

	// 把 idleKey 的 lastUsed 拨回 TTL 之前
	p.mu.Lock()
	p.entries[idleKey].lastUsed.Store(time.Now().Add(-sharedClientIdleTTL - time.Minute).UnixNano())
	p.sweepIdleLocked(time.Now())
	p.mu.Unlock()

	require.Nil(t, p.get(idleKey), "空闲过期条目必须被清扫")
	require.NotNil(t, p.get(freshKey), "未过期条目必须保留")
}
