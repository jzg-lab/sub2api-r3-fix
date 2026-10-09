package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRecentAccountStatsSharedAttemptsWindowAndExpiry(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	a, b := &gatewayCache{rdb: client}, &gatewayCache{rdb: client}
	ctx, now := context.Background(), time.Unix(1800000000, 0)
	ttft := 1200
	require.NoError(t, a.RecordAccountAttempt(ctx, 7, service.AccountAttemptObservation{Success: true, CacheReadTokens: 80, InputTokens: 100, FirstTokenMs: &ttft}, now))
	require.NoError(t, a.RecordAccountAttempt(ctx, 7, service.AccountAttemptObservation{}, now.Add(time.Second)))
	stats, err := b.GetAccountRecentStats(ctx, []int64{7, 8}, now.Add(time.Second))
	require.NoError(t, err)
	v := stats[7].View()
	require.EqualValues(t, 2, v.Attempts)
	require.InDelta(t, 0.5, *v.SuccessRate, 0.0001)
	require.InDelta(t, 0.8, *v.CacheHitRate, 0.0001)
	require.InDelta(t, 1200, *v.TTFTAvgMs, 0.0001)
	require.EqualValues(t, 1, v.ConsecutiveFailures)
	require.Nil(t, stats[8].View().SuccessRate)
	stats, err = b.GetAccountRecentStats(ctx, []int64{7}, now.Add(10*time.Minute))
	require.NoError(t, err)
	require.Zero(t, stats[7].View().Attempts)
	require.NoError(t, a.RecordAccountAttempt(ctx, 7, service.AccountAttemptObservation{Success: true}, now.Add(10*time.Minute)))
	stats, err = b.GetAccountRecentStats(ctx, []int64{7}, now.Add(10*time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 1, stats[7].View().Attempts)
	require.Zero(t, stats[7].ConsecutiveFailures)
	server.FastForward(11 * time.Minute)
	require.False(t, server.Exists(accountRecentStatsKey(7)))
}

func TestRecentAccountStatsConcurrentAttempts(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &gatewayCache{rdb: client}
	ctx, now := context.Background(), time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(success bool) {
			defer wg.Done()
			errs <- cache.RecordAccountAttempt(ctx, 2, service.AccountAttemptObservation{Success: success}, now)
		}(i%2 == 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	stats, err := cache.GetAccountRecentStats(ctx, []int64{2}, now)
	require.NoError(t, err)
	require.EqualValues(t, 20, stats[2].Successes)
	require.EqualValues(t, 20, stats[2].Failures)
}
