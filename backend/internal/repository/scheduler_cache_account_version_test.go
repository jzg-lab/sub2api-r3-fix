//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newSchedulerPublicationTestCache(t *testing.T) *schedulerCache {
	t.Helper()
	socket := os.Getenv("SUB2API_CACHE_TEST_SOCKET")
	if socket == "" {
		return newSchedulerCacheUnit(t)
	}
	require.True(t, filepath.IsAbs(socket))
	require.Equal(t, "scheduler-redis.sock", filepath.Base(socket))
	rdb := redis.NewClient(&redis.Options{Network: "unix", Addr: socket})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(context.Background()).Err())
	require.NoError(t, rdb.FlushDB(context.Background()).Err())
	return &schedulerCache{rdb: rdb, mgetChunkSize: 2, writeChunkSize: 2}
}

func publicationAccount(id int64, revision time.Time, name string) service.Account {
	return service.Account{
		ID: id, Name: name, UpdatedAt: revision, Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
	}
}

func TestSchedulerAccountPublicationRejectsOlderSingleAndSnapshotWrites(t *testing.T) {
	cache := newSchedulerPublicationTestCache(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 16, 0, 0, 0, 900, time.UTC)
	newer := publicationAccount(8101, now, "current")
	newer.Schedulable = false
	older := publicationAccount(newer.ID, now.Add(-time.Nanosecond), "obsolete")
	require.NoError(t, cache.SetAccount(ctx, &newer))
	require.NoError(t, cache.SetAccount(ctx, &older))
	bucket := service.SchedulerBucket{Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{older}))
	got, err := cache.GetAccount(ctx, newer.ID)
	require.NoError(t, err)
	require.Equal(t, "current", got.Name)
	require.False(t, got.Schedulable)
	meta, err := cache.rdb.Get(ctx, schedulerAccountMetaKey("8101")).Result()
	require.NoError(t, err)
	projected, err := decodeCachedAccount(meta)
	require.NoError(t, err)
	require.False(t, projected.Schedulable)
}

func TestSchedulerAccountPublicationDeletionFencesAllReplays(t *testing.T) {
	cache := newSchedulerPublicationTestCache(t)
	ctx := context.Background()
	account := publicationAccount(8102, time.Now(), "deleted")
	require.NoError(t, cache.SetAccount(ctx, &account))
	require.NoError(t, cache.DeleteAccount(ctx, account.ID))
	// Even an arbitrarily later publication cannot reuse a deleted database ID.
	account.UpdatedAt = account.UpdatedAt.Add(time.Hour)
	require.NoError(t, cache.SetAccount(ctx, &account))
	ids, err := cache.writeAccountIDs(ctx, []service.Account{account})
	require.NoError(t, err)
	require.Empty(t, ids)
	require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{account.ID: time.Now()}))
	got, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Nil(t, got)
	for _, key := range schedulerAccountPublicationKeys(account.ID)[:3] {
		require.Zero(t, cache.rdb.Exists(ctx, key).Val())
	}
	require.Equal(t, "deleted", cache.rdb.Get(ctx, schedulerAccountPublicationKeys(account.ID)[3]).Val())
	account.ID++
	require.NoError(t, cache.SetAccount(ctx, &account))
	got, err = cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.NotNil(t, got, "re-adding creates a distinct database ID, not a tombstone reuse")
}

func TestSchedulerAccountPublicationInvalidPayloadRetainsRevisionNotTombstone(t *testing.T) {
	cache := newSchedulerPublicationTestCache(t)
	ctx := context.Background()
	now := time.Now()
	account := publicationAccount(8103, now, "current")
	require.NoError(t, cache.SetAccount(ctx, &account))
	invalid := account
	badTime := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	invalid.ExpiresAt = &badTime
	invalid.UpdatedAt = now.Add(-time.Second)
	require.NoError(t, cache.SetAccount(ctx, &invalid))
	got, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.NotNil(t, got, "an old invalid response cannot evict a newer healthy snapshot")
	invalid.UpdatedAt = now.Add(time.Second)
	require.NoError(t, cache.SetAccount(ctx, &invalid))
	got, err = cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Nil(t, got)
	require.NoError(t, cache.SetAccount(ctx, &account))
	got, err = cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Nil(t, got, "eviction must not discard the revision fence")
	account.UpdatedAt = invalid.UpdatedAt
	require.NoError(t, cache.SetAccount(ctx, &account))
	got, err = cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.NotNil(t, got, "corrected data is allowed after transient encoding failure")
}

func TestSchedulerAccountPublicationFullAndMetadataAreAtomic(t *testing.T) {
	cache := newSchedulerPublicationTestCache(t)
	ctx := context.Background()
	initial := publicationAccount(8104, time.Now(), "first")
	require.NoError(t, cache.SetAccount(ctx, &initial))
	var writers sync.WaitGroup
	failures := make(chan error, 40)
	for i := 1; i <= 40; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			account := initial
			account.UpdatedAt = initial.UpdatedAt.Add(time.Duration(i) * time.Microsecond)
			account.Schedulable = i%2 == 0
			failures <- cache.SetAccount(ctx, &account)
		}(i)
	}
	for i := 0; i < 40; i++ {
		values, err := cache.rdb.MGet(ctx, schedulerAccountKey("8104"), schedulerAccountMetaKey("8104")).Result()
		require.NoError(t, err)
		full, err := decodeCachedAccount(values[0])
		require.NoError(t, err)
		meta, err := decodeCachedAccount(values[1])
		require.NoError(t, err)
		require.Equal(t, full.Schedulable, meta.Schedulable)
	}
	writers.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	got, err := cache.GetAccount(ctx, initial.ID)
	require.NoError(t, err)
	require.True(t, got.UpdatedAt.Equal(initial.UpdatedAt.Add(40*time.Microsecond)))
}

func TestSchedulerAccountPublicationConcurrentDeleteAndChunking(t *testing.T) {
	cache := newSchedulerPublicationTestCache(t)
	cache.writeChunkSize = 2
	ctx := context.Background()
	var accounts []service.Account
	for i := int64(1); i <= 7; i++ {
		accounts = append(accounts, publicationAccount(8200+i, time.Now(), "batch"))
	}
	require.NoError(t, cache.DeleteAccount(ctx, accounts[1].ID))
	var writers sync.WaitGroup
	failures := make(chan error, 2)
	writers.Add(2)
	go func() {
		defer writers.Done()
		_, err := cache.writeAccountIDs(ctx, accounts)
		failures <- err
	}()
	go func() {
		defer writers.Done()
		failures <- cache.DeleteAccount(ctx, accounts[3].ID)
	}()
	writers.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	ids, err := cache.writeAccountIDs(ctx, accounts)
	require.NoError(t, err)
	require.Equal(t, []int64{8201, 8203, 8205, 8206, 8207}, ids)
	for _, id := range []int64{8202, 8204} {
		got, err := cache.GetAccount(ctx, id)
		require.NoError(t, err)
		require.Nil(t, got)
	}
}

func TestSchedulerAccountPublicationIgnoresLegacyNamespace(t *testing.T) {
	cache := newSchedulerPublicationTestCache(t)
	ctx := context.Background()
	account := publicationAccount(8105, time.Now(), "obsolete")
	payload, err := json.Marshal(account)
	require.NoError(t, err)
	require.NoError(t, cache.rdb.Set(ctx, "sched:acc:8105", payload, 0).Err())
	require.NoError(t, cache.rdb.Set(ctx, "sched:meta:8105", payload, 0).Err())
	got, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Nil(t, got)
	account.Name = "new-namespace"
	require.NoError(t, cache.SetAccount(ctx, &account))
	require.NoError(t, cache.rdb.Set(ctx, "sched:acc:8105", payload, 0).Err())
	got, err = cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, "new-namespace", got.Name)
}

func TestSchedulerAccountRevisionOrdering(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 1, time.UTC)
	first, err := schedulerAccountRevision(now)
	require.NoError(t, err)
	second, err := schedulerAccountRevision(now.Add(time.Nanosecond))
	require.NoError(t, err)
	require.Less(t, first, second)
	same, err := schedulerAccountRevision(now.In(time.FixedZone("offset", 8*3600)))
	require.NoError(t, err)
	require.Equal(t, first, same)
	_, err = schedulerAccountRevision(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))
	require.Error(t, err)
}
