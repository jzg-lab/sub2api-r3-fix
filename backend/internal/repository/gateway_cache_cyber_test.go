package repository

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type cyberRedisCommandHook struct {
	mu            sync.Mutex
	mgetKeyCounts []int
	setBatchSizes []int
	scopeWrites   int
	singleScopes  int
	failSetBatch  int
}

func (h *cyberRedisCommandHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func cyberWriteKeyCounts(cmd redis.Cmder) (exact, scope int) {
	args := cmd.Args()
	first, count := 0, 0
	switch cmd.Name() {
	case "set":
		first, count = 1, 1
	case "eval":
		if len(args) < 3 {
			return 0, 0
		}
		first = 3
		count, _ = args[2].(int)
	default:
		return 0, 0
	}
	for i := first; i < len(args) && i < first+count; i++ {
		key, _ := args[i].(string)
		switch {
		case strings.HasPrefix(key, cyberSessionBlockPrefix):
			exact++
		case strings.HasPrefix(key, cyberSessionScopePrefix):
			scope++
		}
	}
	return exact, scope
}

func (h *cyberRedisCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "mget" {
			h.mu.Lock()
			h.mgetKeyCounts = append(h.mgetKeyCounts, len(cmd.Args())-1)
			h.mu.Unlock()
		}
		if _, scopeCount := cyberWriteKeyCounts(cmd); scopeCount > 0 {
			h.mu.Lock()
			h.singleScopes += scopeCount
			h.mu.Unlock()
		}
		return next(ctx, cmd)
	}
}

func (h *cyberRedisCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		setCount := 0
		scopeCount := 0
		for _, cmd := range cmds {
			exact, scope := cyberWriteKeyCounts(cmd)
			setCount += exact
			scopeCount += scope
		}
		if setCount > 0 {
			h.mu.Lock()
			h.setBatchSizes = append(h.setBatchSizes, setCount)
			h.scopeWrites += scopeCount
			fail := len(h.setBatchSizes) == h.failSetBatch
			h.mu.Unlock()
			if fail {
				return errors.New("injected batch failure")
			}
		}
		return next(ctx, cmds)
	}
}

func TestGatewayCacheCyberBlockWritesScopeAndExactKeysTogether(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewGatewayCache(client).(service.CyberSessionBlockStore)
	require.True(t, ok)

	ctx := context.Background()
	require.NoError(t, store.SetCyberSessionBlocked(ctx, "scope-1", []string{"block-1", "block-2"}, time.Minute))
	active, err := store.IsCyberSessionScopeActive(ctx, "scope-1")
	require.NoError(t, err)
	require.True(t, active)
	matched, err := store.FindCyberSessionBlocked(ctx, []string{"missing", "block-1", "block-2"})
	require.NoError(t, err)
	require.Equal(t, "block-1", matched)
	require.Greater(t, server.TTL(cyberSessionScopePrefix+"scope-1"), time.Duration(0))
	require.Equal(t, server.TTL(cyberSessionBlockPrefix+"block-1"), server.TTL(cyberSessionBlockPrefix+"block-2"))
}

func TestGatewayCacheCyberBlockCommandsAreBoundedAndLookupShortCircuits(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	hook := &cyberRedisCommandHook{}
	client.AddHook(hook)
	store, ok := NewGatewayCache(client).(service.CyberSessionBlockStore)
	require.True(t, ok)

	keys := make([]string, cyberSessionRedisCommandMaxKeys*2+44)
	for i := range keys {
		keys[i] = "block-" + strconv.Itoa(i)
	}
	ctx := context.Background()
	require.NoError(t, store.SetCyberSessionBlocked(ctx, "large-scope", keys, time.Minute))
	require.Equal(t, []int{cyberSessionRedisCommandMaxKeys, cyberSessionRedisCommandMaxKeys, 44}, hook.setBatchSizes)
	require.Equal(t, 3, hook.scopeWrites, "each committed batch carries its scope hint")
	require.Zero(t, hook.singleScopes, "scope publication cannot be a separate command")

	lookup := make([]string, len(keys))
	for i := range lookup {
		lookup[i] = "missing-" + strconv.Itoa(i)
	}
	lookup[cyberSessionRedisCommandMaxKeys+3] = keys[cyberSessionRedisCommandMaxKeys+3]
	matched, err := store.FindCyberSessionBlocked(ctx, lookup)
	require.NoError(t, err)
	require.Equal(t, keys[cyberSessionRedisCommandMaxKeys+3], matched)
	require.Equal(t, []int{cyberSessionRedisCommandMaxKeys, cyberSessionRedisCommandMaxKeys}, hook.mgetKeyCounts)
}

func TestGatewayCacheCyberBlockPartialWriteKeepsCommittedBlocksDiscoverable(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	hook := &cyberRedisCommandHook{failSetBatch: 2}
	client.AddHook(hook)
	store := NewGatewayCache(client).(service.CyberSessionBlockStore)
	keys := make([]string, cyberSessionRedisCommandMaxKeys+1)
	for i := range keys {
		keys[i] = "partial-" + strconv.Itoa(i)
	}
	ctx := context.Background()
	require.Error(t, store.SetCyberSessionBlocked(ctx, "scope", keys, time.Minute))
	active, err := store.IsCyberSessionScopeActive(ctx, "scope")
	require.NoError(t, err)
	require.True(t, active, "a later failed batch cannot hide an earlier committed block")
	found, err := store.FindCyberSessionBlocked(ctx, keys)
	require.NoError(t, err)
	require.Equal(t, keys[0], found)
	require.False(t, server.Exists(cyberSessionBlockPrefix+keys[len(keys)-1]))
	require.Equal(t, server.TTL(cyberSessionBlockPrefix+keys[0]), server.TTL(cyberSessionScopePrefix+"scope"))
	require.Zero(t, hook.singleScopes)
}

func TestGatewayCacheCyberBlockEmptyKeysDoNotActivateScope(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewGatewayCache(client).(service.CyberSessionBlockStore)
	require.NoError(t, store.SetCyberSessionBlocked(context.Background(), "empty", []string{"", ""}, time.Minute))
	require.False(t, server.Exists(cyberSessionScopePrefix+"empty"))
}

func TestGatewayCacheCyberScopeShorterPolicyKeepsEarlierBlocksDiscoverable(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewGatewayCache(client).(service.CyberSessionBlockStore)
	ctx := context.Background()

	require.NoError(t, store.SetCyberSessionBlocked(ctx, "shared", []string{"old"}, 5*time.Minute))
	server.FastForward(time.Minute)
	require.NoError(t, store.SetCyberSessionBlocked(ctx, "shared", []string{"new"}, time.Minute))
	require.Equal(t, 4*time.Minute, server.TTL(cyberSessionScopePrefix+"shared"))

	server.FastForward(90 * time.Second)
	require.False(t, server.Exists(cyberSessionBlockPrefix+"new"))
	active, err := store.IsCyberSessionScopeActive(ctx, "shared")
	require.NoError(t, err)
	require.True(t, active, "short-lived new blocks must not hide older unexpired blocks")
	found, err := store.FindCyberSessionBlocked(ctx, []string{"old"})
	require.NoError(t, err)
	require.Equal(t, "old", found)
	found, err = store.FindCyberSessionBlocked(ctx, []string{"unrelated"})
	require.NoError(t, err)
	require.Empty(t, found, "a retained scope is only a lookup hint")

	server.FastForward(3 * time.Minute)
	active, err = store.IsCyberSessionScopeActive(ctx, "shared")
	require.NoError(t, err)
	require.False(t, active, "bounded hints should expire after all their blocks")
}

func TestGatewayCacheCyberScopeLongerPolicyExtendsHint(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewGatewayCache(client).(service.CyberSessionBlockStore)
	ctx := context.Background()

	require.NoError(t, store.SetCyberSessionBlocked(ctx, "shared", []string{"old"}, time.Minute))
	server.FastForward(30 * time.Second)
	require.NoError(t, store.SetCyberSessionBlocked(ctx, "shared", []string{"new"}, 5*time.Minute))
	require.Equal(t, 5*time.Minute, server.TTL(cyberSessionScopePrefix+"shared"))
	server.FastForward(time.Minute)
	active, err := store.IsCyberSessionScopeActive(ctx, "shared")
	require.NoError(t, err)
	require.True(t, active)
	found, err := store.FindCyberSessionBlocked(ctx, []string{"old", "new"})
	require.NoError(t, err)
	require.Equal(t, "new", found)
}

func TestGatewayCacheCyberRepeatedBlockCannotShortenTTL(t *testing.T) {
	for _, scope := range []string{"original", "new-scope", ""} {
		t.Run("scope="+scope, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			store := NewGatewayCache(client).(service.CyberSessionBlockStore)
			ctx := context.Background()

			require.NoError(t, store.SetCyberSessionBlocked(ctx, "original", []string{"repeated"}, 5*time.Minute))
			server.FastForward(time.Minute)
			require.NoError(t, store.SetCyberSessionBlocked(ctx, scope, []string{"repeated", "short"}, time.Minute))
			require.Equal(t, 4*time.Minute, server.TTL(cyberSessionBlockPrefix+"repeated"))
			if scope != "" {
				require.Equal(t, 4*time.Minute, server.TTL(cyberSessionScopePrefix+scope))
			}
			server.FastForward(2 * time.Minute)
			found, err := store.FindCyberSessionBlocked(ctx, []string{"short", "repeated"})
			require.NoError(t, err)
			require.Equal(t, "repeated", found)
			if scope != "" {
				active, err := store.IsCyberSessionScopeActive(ctx, scope)
				require.NoError(t, err)
				require.True(t, active)
			}
			server.FastForward(3 * time.Minute)
			found, err = store.FindCyberSessionBlocked(ctx, []string{"repeated"})
			require.NoError(t, err)
			require.Empty(t, found, "retained blocks still expire at their original deadline")
			if scope != "" {
				require.False(t, server.Exists(cyberSessionScopePrefix+scope))
			}
		})
	}
}

func TestGatewayCacheCyberRepeatedBlockLongerPolicyExtendsTTL(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewGatewayCache(client).(service.CyberSessionBlockStore)
	ctx := context.Background()

	require.NoError(t, store.SetCyberSessionBlocked(ctx, "scope", []string{"repeated"}, time.Minute))
	server.FastForward(30 * time.Second)
	require.NoError(t, store.SetCyberSessionBlocked(ctx, "scope", []string{"repeated"}, 5*time.Minute))
	require.Equal(t, 5*time.Minute, server.TTL(cyberSessionBlockPrefix+"repeated"))
	require.Equal(t, 5*time.Minute, server.TTL(cyberSessionScopePrefix+"scope"))
	server.FastForward(time.Minute)
	found, err := store.FindCyberSessionBlocked(ctx, []string{"repeated"})
	require.NoError(t, err)
	require.Equal(t, "repeated", found)
}

func TestGatewayCacheCyberRepeatedBlockPreservesPersistentTTL(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewGatewayCache(client).(service.CyberSessionBlockStore)
	require.NoError(t, server.Set(cyberSessionBlockPrefix+"legacy", "1"))
	require.NoError(t, store.SetCyberSessionBlocked(context.Background(), "new-scope", []string{"legacy", "short"}, time.Minute))
	require.Zero(t, server.TTL(cyberSessionBlockPrefix+"legacy"))
	require.Zero(t, server.TTL(cyberSessionScopePrefix+"new-scope"))
	server.FastForward(2 * time.Minute)
	require.True(t, server.Exists(cyberSessionBlockPrefix+"legacy"))
	require.True(t, server.Exists(cyberSessionScopePrefix+"new-scope"))
	require.False(t, server.Exists(cyberSessionBlockPrefix+"short"))
}

func TestGatewayCacheCyberScopeConcurrentWritersRetainLongestTTL(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewGatewayCache(client).(service.CyberSessionBlockStore)
	const count = 16
	errs := make(chan error, count)
	var workers sync.WaitGroup
	for i := 1; i <= count; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errs <- store.SetCyberSessionBlocked(context.Background(), "shared", []string{"repeated", "concurrent-" + strconv.Itoa(i)}, time.Duration(i)*time.Minute)
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, count*time.Minute, server.TTL(cyberSessionScopePrefix+"shared"))
	require.Equal(t, count*time.Minute, server.TTL(cyberSessionBlockPrefix+"repeated"))
	for i := 1; i <= count; i++ {
		require.True(t, server.Exists(cyberSessionBlockPrefix+"concurrent-"+strconv.Itoa(i)))
	}
}

func TestGatewayCacheCyberScopePreservesPersistentLegacyHint(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewGatewayCache(client).(service.CyberSessionBlockStore)
	require.NoError(t, server.Set(cyberSessionScopePrefix+"legacy", "1"))
	require.NoError(t, store.SetCyberSessionBlocked(context.Background(), "legacy", []string{"new"}, time.Minute))
	require.Zero(t, server.TTL(cyberSessionScopePrefix+"legacy"))
	server.FastForward(2 * time.Minute)
	active, err := store.IsCyberSessionScopeActive(context.Background(), "legacy")
	require.NoError(t, err)
	require.True(t, active)
	require.False(t, server.Exists(cyberSessionBlockPrefix+"new"))
}

func TestGatewayCacheCyberBlockRejectsNonpositiveTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Minute} {
		t.Run(ttl.String(), func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			store := NewGatewayCache(client).(service.CyberSessionBlockStore)
			require.Error(t, store.SetCyberSessionBlocked(context.Background(), "scope", []string{"block"}, ttl))
			require.False(t, server.Exists(cyberSessionScopePrefix+"scope"))
			require.False(t, server.Exists(cyberSessionBlockPrefix+"block"))
		})
	}
}

func TestGatewayCacheCyberBlockSubmillisecondTTLRemainsBounded(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewGatewayCache(client).(service.CyberSessionBlockStore)
	require.NoError(t, store.SetCyberSessionBlocked(context.Background(), "scope", []string{"block"}, time.Nanosecond))
	require.Equal(t, time.Millisecond, server.TTL(cyberSessionScopePrefix+"scope"))
	require.Equal(t, time.Millisecond, server.TTL(cyberSessionBlockPrefix+"block"))
	server.FastForward(2 * time.Millisecond)
	require.False(t, server.Exists(cyberSessionScopePrefix+"scope"))
	require.False(t, server.Exists(cyberSessionBlockPrefix+"block"))
}
