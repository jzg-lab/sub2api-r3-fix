//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOAuthRefreshLockLeaseOwnership(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	first := NewGeminiTokenCache(client)
	second := NewGeminiTokenCache(client)
	ctx := context.Background()
	const cacheKey = "openai:account:lease-test"
	key := oauthRefreshLockKeyPrefix + cacheKey

	oldLease, err := first.AcquireRefreshLock(ctx, cacheKey, time.Second)
	require.NoError(t, err)
	require.NotEmpty(t, oldLease)
	busy, err := second.AcquireRefreshLock(ctx, cacheKey, time.Second)
	require.NoError(t, err)
	require.Empty(t, busy)

	mr.FastForward(2 * time.Second)
	newLease, err := second.AcquireRefreshLock(ctx, cacheKey, time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, newLease)
	require.True(t, oldLease != newLease)
	require.NoError(t, first.ReleaseRefreshLock(ctx, cacheKey, oldLease))
	require.NoError(t, first.ReleaseRefreshLock(ctx, cacheKey, ""))
	require.True(t, mr.Exists(key), "expired or empty lease must not delete a successor")
	current, err := mr.Get(key)
	require.NoError(t, err)
	require.True(t, current == newLease)
	require.NoError(t, second.ReleaseRefreshLock(ctx, cacheKey, newLease))
	require.False(t, mr.Exists(key))
	require.NoError(t, second.ReleaseRefreshLock(ctx, cacheKey, newLease))
}

func TestOAuthRefreshLockRejectsInvalidLeaseAndCanceledAcquisition(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := NewGeminiTokenCache(client)
	for _, ttl := range []time.Duration{0, -time.Second} {
		lease, err := cache.AcquireRefreshLock(context.Background(), "invalid", ttl)
		require.Error(t, err)
		require.Empty(t, lease)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lease, err := cache.AcquireRefreshLock(ctx, "canceled", time.Minute)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, lease)
	require.Empty(t, mr.Keys())
}
