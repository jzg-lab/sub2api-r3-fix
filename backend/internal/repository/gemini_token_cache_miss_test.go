package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOAuthTokenCacheMissLifecycle(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	cache := NewGeminiTokenCache(client)
	for _, key := range []string{"openai:42", "gemini:42", "antigravity:42"} {
		t.Run(key, func(t *testing.T) {
			token, err := cache.GetAccessToken(t.Context(), key)
			require.NoError(t, err)
			require.Empty(t, token)
			require.NoError(t, cache.SetAccessToken(t.Context(), key, "fixture-token", time.Minute))
			token, err = cache.GetAccessToken(t.Context(), key)
			require.NoError(t, err)
			require.Equal(t, "fixture-token", token)
			server.FastForward(time.Minute)
			token, err = cache.GetAccessToken(t.Context(), key)
			require.NoError(t, err)
			require.Empty(t, token)
			require.NoError(t, cache.SetAccessToken(t.Context(), key, "fixture-token", time.Minute))
			require.NoError(t, cache.DeleteAccessToken(t.Context(), key))
			token, err = cache.GetAccessToken(t.Context(), key)
			require.NoError(t, err)
			require.Empty(t, token)
		})
	}
}

func TestOAuthTokenCacheFailuresRemainErrors(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr: server.Addr(), MaxRetries: -1, DialTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })
	cache := NewGeminiTokenCache(client)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := cache.GetAccessToken(ctx, "openai:42")
	require.ErrorIs(t, err, context.Canceled)
	server.SetError("ERR fixture unavailable")
	_, err = cache.GetAccessToken(t.Context(), "openai:42")
	require.Error(t, err)
	server.Close()
	_, err = cache.GetAccessToken(t.Context(), "openai:42")
	require.Error(t, err, "connection failures must not look like cache misses")
}
