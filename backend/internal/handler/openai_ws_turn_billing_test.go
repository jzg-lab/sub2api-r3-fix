package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type wsTurnBillingLookup struct {
	key   *service.APIKey
	err   error
	calls int
}

func (l *wsTurnBillingLookup) GetByKey(context.Context, string) (*service.APIKey, error) {
	l.calls++
	return l.key, l.err
}

func wsTurnBillingFixture() *service.APIKey {
	groupID := int64(7)
	return &service.APIKey{
		ID: 11, UserID: 13, Key: "fixture", GroupID: &groupID,
		User: &service.User{ID: 13},
		Group: &service.Group{
			ID: groupID, Platform: service.PlatformOpenAI, Hydrated: true, Status: service.StatusActive,
			SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1,
		},
	}
}

func TestOpenAIWSTurnBillingSnapshots(t *testing.T) {
	conn := wsTurnBillingFixture()
	fresh := wsTurnBillingFixture()
	fresh.Group.RateMultiplier = 2
	fresh.User = &service.User{ID: conn.UserID}
	lookup := &wsTurnBillingLookup{key: fresh}
	var turns openAIWSTurnBillingAPIKeys
	require.Same(t, conn, turns.begin(t.Context(), lookup, 1, conn))
	require.Zero(t, lookup.calls)
	second := turns.begin(t.Context(), lookup, 2, conn)
	require.NotSame(t, conn, second)
	require.Same(t, fresh.Group, second.Group)
	require.Same(t, conn.User, second.User)
	require.Equal(t, float64(1), conn.Group.RateMultiplier)
	require.Same(t, second, turns.begin(t.Context(), lookup, 2, conn))
	require.Equal(t, 1, lookup.calls, "BeforeRequest and BeforeTurn must share one snapshot")
	require.Same(t, conn, turns.forTurn(1, conn))
	require.Same(t, second, turns.forTurn(2, conn))

	latest := wsTurnBillingFixture()
	latest.Group.RateMultiplier = 3
	lookup.key = latest
	third := turns.begin(t.Context(), lookup, 3, conn)
	require.Same(t, latest.Group, third.Group)
	require.Same(t, second, turns.forTurn(2, conn), "late completion retains its own price")
	require.Len(t, turns.keys, 2)
	require.NotContains(t, turns.keys, 1)
	require.Same(t, conn, turns.forTurn(0, conn))
	require.Same(t, conn, turns.begin(t.Context(), lookup, 0, conn))
}

func TestOpenAIWSTurnBillingRejectsIdentityChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*service.APIKey)
	}{
		{"key id", func(k *service.APIKey) { k.ID++ }},
		{"user", func(k *service.APIKey) { k.UserID++ }},
		{"missing group id", func(k *service.APIKey) { k.GroupID = nil }},
		{"changed group", func(k *service.APIKey) { *k.GroupID++ }},
		{"missing group", func(k *service.APIKey) { k.Group = nil }},
		{"incomplete group", func(k *service.APIKey) { k.Group.Hydrated = false }},
		{"inconsistent group", func(k *service.APIKey) { k.Group.ID++ }},
		{"platform", func(k *service.APIKey) { k.Group.Platform = service.PlatformAnthropic }},
		{"subscription", func(k *service.APIKey) { k.Group.SubscriptionType = service.SubscriptionTypeSubscription }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, fresh := wsTurnBillingFixture(), wsTurnBillingFixture()
			tt.mutate(fresh)
			require.Same(t, conn, refreshOpenAIWSTurnBillingAPIKey(t.Context(), &wsTurnBillingLookup{key: fresh}, conn))
		})
	}
	conn := wsTurnBillingFixture()
	require.Same(t, conn, refreshOpenAIWSTurnBillingAPIKey(t.Context(), nil, conn))
	require.Same(t, conn, refreshOpenAIWSTurnBillingAPIKey(t.Context(), &wsTurnBillingLookup{}, conn))
	require.Same(t, conn, refreshOpenAIWSTurnBillingAPIKey(t.Context(), &wsTurnBillingLookup{err: errors.New("unavailable")}, conn))
	require.Nil(t, refreshOpenAIWSTurnBillingAPIKey(t.Context(), &wsTurnBillingLookup{}, nil))
}

func TestOpenAIWSTurnBillingContextPreservesRouting(t *testing.T) {
	key := wsTurnBillingFixture()
	old := *key.Group
	old.RateMultiplier = 0.5
	ctx := context.WithValue(t.Context(), ctxkey.Group, &old)
	refreshed := withOpenAIWSTurnBillingGroup(ctx, key)
	require.Same(t, key.Group, refreshed.Value(ctxkey.Group))
	require.Same(t, &old, ctx.Value(ctxkey.Group))
	selected := *key.Group
	selected.ID++
	routed := context.WithValue(t.Context(), ctxkey.Group, &selected)
	require.Same(t, routed, withOpenAIWSTurnBillingGroup(routed, key), "composite child routing is not pricing")
	require.Same(t, ctx, withOpenAIWSTurnBillingGroup(ctx, nil))
}
