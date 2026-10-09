package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyRouteFailureSharedScopeAndExpiry(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	first, second := &apiKeyCache{rdb: client}, &apiKeyCache{rdb: client}
	ctx := context.Background()
	scope := "responses\x00gpt-test"
	require.NoError(t, first.MarkAPIKeyRouteFailed(ctx, 7, scope, 10))
	failed, err := second.FailedAPIKeyRouteGroups(ctx, 7, scope, []int64{10, 20})
	require.NoError(t, err)
	require.Equal(t, map[int64]bool{10: true}, failed)
	for _, other := range []struct {
		id    int64
		scope string
	}{{8, scope}, {7, "messages\x00gpt-test"}, {7, "responses\x00other"}} {
		failed, err = second.FailedAPIKeyRouteGroups(ctx, other.id, other.scope, []int64{10, 20})
		require.NoError(t, err)
		require.Empty(t, failed)
	}
	server.FastForward(time.Minute)
	failed, err = second.FailedAPIKeyRouteGroups(ctx, 7, scope, []int64{10, 20})
	require.NoError(t, err)
	require.Empty(t, failed)
}

func TestAPIKeyRouteSessionScopeAndExpiry(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := &apiKeyCache{rdb: client}
	ctx := context.Background()
	require.NoError(t, cache.RememberAPIKeyRouteSession(ctx, 7, "responses\x00model", "conversation", 2))
	id, err := cache.APIKeyRouteSession(ctx, 7, "responses\x00model", "conversation")
	require.NoError(t, err)
	require.EqualValues(t, 2, id)
	for _, args := range [][3]string{{"7", "responses\x00other-model", "conversation"}, {"7", "responses\x00model", "new-conversation"}} {
		id, err = cache.APIKeyRouteSession(ctx, 7, args[1], args[2])
		require.NoError(t, err)
		require.Zero(t, id)
	}
	id, err = cache.APIKeyRouteSession(ctx, 8, "responses\x00model", "conversation")
	require.NoError(t, err)
	require.Zero(t, id)
	server.FastForward(time.Hour)
	id, err = cache.APIKeyRouteSession(ctx, 7, "responses\x00model", "conversation")
	require.NoError(t, err)
	require.Zero(t, id)
}
