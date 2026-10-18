package handler

import (
	"context"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type openAIWSTurnAPIKeyLookup interface {
	GetByKey(context.Context, string) (*service.APIKey, error)
}

// Retain the previous turn until its asynchronous usage submission has captured
// its snapshot. Refresh pricing only, never the connection's routing identity.
type openAIWSTurnBillingAPIKeys struct {
	mu   sync.Mutex
	keys map[int]*service.APIKey
}

func (k *openAIWSTurnBillingAPIKeys) begin(ctx context.Context, lookup openAIWSTurnAPIKeyLookup, turn int, conn *service.APIKey) *service.APIKey {
	if turn <= 0 {
		return conn
	}
	k.mu.Lock()
	if key, exists := k.keys[turn]; exists {
		k.mu.Unlock()
		return key
	}
	k.mu.Unlock()
	key := conn
	if turn > 1 {
		key = refreshOpenAIWSTurnBillingAPIKey(ctx, lookup, conn)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if existing, exists := k.keys[turn]; exists {
		return existing
	}
	if k.keys == nil {
		k.keys = make(map[int]*service.APIKey, 2)
	}
	for previous := range k.keys {
		if previous < turn-1 {
			delete(k.keys, previous)
		}
	}
	k.keys[turn] = key
	return key
}

func (k *openAIWSTurnBillingAPIKeys) forTurn(turn int, conn *service.APIKey) *service.APIKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key, exists := k.keys[turn]; exists {
		return key
	}
	return conn
}

func refreshOpenAIWSTurnBillingAPIKey(ctx context.Context, lookup openAIWSTurnAPIKeyLookup, conn *service.APIKey) *service.APIKey {
	if lookup == nil || conn == nil || conn.Key == "" || conn.GroupID == nil || !service.IsGroupContextValid(conn.Group) {
		return conn
	}
	latest, err := lookup.GetByKey(ctx, conn.Key)
	if err != nil || latest == nil || latest.ID != conn.ID || latest.UserID != conn.UserID || latest.GroupID == nil {
		return conn
	}
	if *latest.GroupID != *conn.GroupID {
		// A smart-routed connection keeps billing its bound customer package.
		routes, ok := lookup.(interface {
			APIKeyForRoute(context.Context, *service.APIKey, int64) (*service.APIKey, error)
		})
		if !ok || !latest.HasSmartRoutes() {
			return conn
		}
		latest, err = routes.APIKeyForRoute(ctx, latest, *conn.GroupID)
		if err != nil || latest == nil {
			return conn
		}
	}
	group := latest.Group
	if !service.IsGroupContextValid(group) || group.ID != conn.Group.ID || conn.Group.ID != *conn.GroupID ||
		group.Platform != conn.Group.Platform ||
		group.SubscriptionType != conn.Group.SubscriptionType {
		return conn
	}
	turnKey := *conn
	turnKey.Group = group
	return &turnKey
}

func withOpenAIWSTurnBillingGroup(ctx context.Context, key *service.APIKey) context.Context {
	if key == nil || !service.IsGroupContextValid(key.Group) {
		return ctx
	}
	if selected, ok := ctx.Value(ctxkey.Group).(*service.Group); ok && selected != nil && selected.ID != key.Group.ID {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.Group, key.Group)
}
