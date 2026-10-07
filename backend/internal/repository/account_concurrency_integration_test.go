//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountConcurrencyPersistsPublishesAndLimitsSlots(t *testing.T) {
	ctx := context.Background()
	rdb := testRedis(t)
	cache := NewSchedulerCache(rdb)
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, cache)
	account := &service.Account{
		Name: "concurrency-integration", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Credentials: map[string]any{},
		Extra: map[string]any{}, Status: service.StatusActive, Schedulable: true,
		Concurrency: 3,
	}
	require.NoError(t, repo.Create(ctx, account))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id=$1 OR payload->'account_ids' @> to_jsonb(ARRAY[$1::bigint])", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id=$1", account.ID)
	})
	saved, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, 3, saved.Concurrency)
	require.NoError(t, cache.SetAccount(ctx, saved))
	oldSnapshot := *saved

	slots := NewConcurrencyCache(rdb, 15, 900)
	acquire := func(limit int, requestID string, expected bool) {
		t.Helper()
		acquired, err := slots.AcquireAccountSlot(ctx, account.ID, limit, requestID)
		require.NoError(t, err)
		require.Equal(t, expected, acquired)
	}
	acquire(3, "inflight-1", true)
	acquire(3, "inflight-2", true)
	acquire(3, "inflight-3", true)
	acquire(3, "over-original-limit", false)

	account.Concurrency = 1
	require.NoError(t, repo.UpdateWithAccountBillingSettings(ctx, account, nil, nil, nil, &account.Concurrency))
	saved, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, 1, saved.Concurrency)
	cached, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.NotNil(t, cached)
	require.Equal(t, 1, cached.Concurrency, "post-commit publication must not wait for the outbox poll")
	require.NoError(t, cache.SetAccount(ctx, &oldSnapshot))
	cached, err = cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, 1, cached.Concurrency, "a stale scheduler snapshot cannot undo the edit")
	staleBackground := oldSnapshot
	staleBackground.Name = "background-metadata"
	require.NoError(t, repo.Update(ctx, &staleBackground))
	require.Equal(t, 1, staleBackground.Concurrency, "background writes cannot restore an old limit")
	staleAdminEdit := oldSnapshot
	staleAdminEdit.Name = "unrelated-admin-edit"
	require.NoError(t, repo.UpdateWithAccountBillingSettings(ctx, &staleAdminEdit, nil, nil, nil, nil))
	require.Equal(t, 1, staleAdminEdit.Concurrency, "an omitted limit cannot restore a stale form value")
	count, err := slots.GetAccountConcurrency(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, 3, count, "lowering the limit must not cancel in-flight requests")
	acquire(cached.Concurrency, "after-lowering", false)
	for _, requestID := range []string{"inflight-1", "inflight-2"} {
		require.NoError(t, slots.ReleaseAccountSlot(ctx, account.ID, requestID))
		acquire(cached.Concurrency, "still-full", false)
	}
	require.NoError(t, slots.ReleaseAccountSlot(ctx, account.ID, "inflight-3"))
	acquire(cached.Concurrency, "after-drain", true)

	limit := 4
	rows, err := repo.BulkUpdate(ctx, []int64{account.ID}, service.AccountBulkUpdate{Concurrency: &limit})
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
	cached, err = cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, limit, cached.Concurrency)
	acquire(cached.Concurrency, "after-raising", true)

	// Normal metadata and credential refresh writes must retain the chosen limit.
	saved, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	saved.Name = "concurrency-renamed"
	require.NoError(t, repo.Update(ctx, saved))
	require.NoError(t, repo.UpdateCredentials(ctx, account.ID, map[string]any{"model_mapping": map[string]any{"fixture": "fixture"}}))
	saved, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, limit, saved.Concurrency)
	var persisted, events int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT concurrency FROM accounts WHERE id=$1", account.ID).Scan(&persisted))
	require.Equal(t, limit, persisted)
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM scheduler_outbox WHERE account_id=$1 OR payload->'account_ids' @> to_jsonb(ARRAY[$1::bigint])", account.ID).Scan(&events))
	require.Positive(t, events, "durable publication remains available if the synchronous cache write fails")
}
