package repository

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestStartupPreservesOtherInstanceQueueCapacity(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		t.Run(strconv.FormatBool(migrated), func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			ctx := context.Background()
			first := NewConcurrencyCache(client, 15, 60)
			restarting := NewConcurrencyCache(client, 15, 60)
			if migrated {
				require.NoError(t, client.Set(ctx, legacyWaitSweepMarkerKey, "1", 0).Err())
			}
			for range 2 {
				ok, err := first.IncrementAccountWaitCount(ctx, 101, 2)
				require.NoError(t, err)
				require.True(t, ok)
				ok, err = first.IncrementWaitCount(ctx, 202, 2)
				require.NoError(t, err)
				require.True(t, ok)
			}

			require.NoError(t, restarting.CleanupStaleProcessSlots(ctx, "new-process"))

			accountCount, err := client.Get(ctx, accountWaitKeyPrefix+"101").Int()
			require.NoError(t, err)
			require.Equal(t, 2, accountCount, "a restart must preserve another instance's waiters")
			userCount, err := client.Get(ctx, waitQueueKeyPrefix+"202").Int()
			require.NoError(t, err)
			require.Equal(t, 2, userCount)
			ok, err := restarting.IncrementAccountWaitCount(ctx, 101, 2)
			require.NoError(t, err)
			require.False(t, ok, "restart must not admit requests beyond the shared queue bound")
			ok, err = restarting.IncrementWaitCount(ctx, 202, 2)
			require.NoError(t, err)
			require.False(t, ok)
			require.NoError(t, first.DecrementAccountWaitCount(ctx, 101))
			require.NoError(t, first.DecrementWaitCount(ctx, 202))
			ok, err = restarting.IncrementAccountWaitCount(ctx, 101, 2)
			require.NoError(t, err)
			require.True(t, ok, "normal departure must release exactly one waiting place")
		})
	}
}

func TestConcurrentStartupCannotOverbookOtherInstanceQueue(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx := context.Background()
	first := NewConcurrencyCache(client, 15, 60)
	for range 10 {
		ok, err := first.IncrementAccountWaitCount(ctx, 101, 10)
		require.NoError(t, err)
		require.True(t, ok)
	}
	var workers sync.WaitGroup
	errs := make(chan error, 24)
	accepted := make(chan bool, 16)
	for n := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errs <- NewConcurrencyCache(client, 15, 60).CleanupStaleProcessSlots(ctx, strconv.Itoa(n))
		}()
	}
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ok, err := first.IncrementAccountWaitCount(ctx, 101, 10)
			errs <- err
			accepted <- ok
		}()
	}
	workers.Wait()
	close(errs)
	close(accepted)
	for err := range errs {
		require.NoError(t, err)
	}
	for ok := range accepted {
		require.False(t, ok)
	}
	count, err := first.GetAccountWaitingCount(ctx, 101)
	require.NoError(t, err)
	require.Equal(t, 10, count)
	ok, err := first.IncrementAccountWaitCount(ctx, 303, 1)
	require.NoError(t, err)
	require.True(t, ok, "one full queue must not prevent another account from accepting work")
}

func TestStartupBoundsLegacyCounterWithoutExtendingActiveExpiry(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx := context.Background()
	legacy := accountWaitKeyPrefix + "101"
	active := waitQueueKeyPrefix + "202"
	require.NoError(t, client.Set(ctx, legacy, 5, 0).Err())
	require.NoError(t, client.Set(ctx, active, 3, 20*time.Second).Err())
	cache := NewConcurrencyCache(client, 15, 60)
	require.NoError(t, cache.CleanupStaleProcessSlots(ctx, "new-process"))
	require.Equal(t, 60*time.Second, server.TTL(legacy))
	require.Equal(t, 20*time.Second, server.TTL(active))
	server.FastForward(21 * time.Second)
	require.False(t, server.Exists(active))
	require.True(t, server.Exists(legacy))
	require.NoError(t, cache.CleanupStaleProcessSlots(ctx, "another-process"))
	require.Equal(t, 39*time.Second, server.TTL(legacy), "restart must not renew stale reservations")
	server.FastForward(40 * time.Second)
	require.False(t, server.Exists(legacy))
}
