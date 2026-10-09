package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

func apiKeyRouteFailureKey(keyID int64, scope string, groupID int64) string {
	return fmt.Sprintf("apikey:route-failure:%d:%x:%d", keyID, sha256.Sum256([]byte(scope)), groupID)
}

func (c *apiKeyCache) FailedAPIKeyRouteGroups(ctx context.Context, keyID int64, scope string, ids []int64) (map[int64]bool, error) {
	failed := make(map[int64]bool)
	if len(ids) == 0 {
		return failed, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = apiKeyRouteFailureKey(keyID, scope, id)
	}
	values, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	for i, value := range values {
		if value != nil {
			failed[ids[i]] = true
		}
	}
	return failed, nil
}

func (c *apiKeyCache) MarkAPIKeyRouteFailed(ctx context.Context, keyID int64, scope string, groupID int64) error {
	return c.rdb.Set(ctx, apiKeyRouteFailureKey(keyID, scope, groupID), "1", time.Minute).Err()
}

func apiKeyRouteSessionKey(keyID int64, scope, session string) string {
	return fmt.Sprintf("apikey:route-session:%d:%x:%x", keyID, sha256.Sum256([]byte(scope)), sha256.Sum256([]byte(session)))
}

func (c *apiKeyCache) APIKeyRouteSession(ctx context.Context, keyID int64, scope, session string) (int64, error) {
	id, err := c.rdb.Get(ctx, apiKeyRouteSessionKey(keyID, scope, session)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return id, err
}

func (c *apiKeyCache) RememberAPIKeyRouteSession(ctx context.Context, keyID int64, scope, session string, groupID int64) error {
	return c.rdb.Set(ctx, apiKeyRouteSessionKey(keyID, scope, session), groupID, time.Hour).Err()
}
