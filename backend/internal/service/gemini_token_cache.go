package service

import (
	"context"
	"time"
)

// GeminiTokenCache stores short-lived access tokens and coordinates refresh to avoid stampedes.
type GeminiTokenCache interface {
	// cacheKey should be stable for the token scope; for GeminiCli OAuth we primarily use project_id.
	GetAccessToken(ctx context.Context, cacheKey string) (string, error)
	SetAccessToken(ctx context.Context, cacheKey string, token string, ttl time.Duration) error
	DeleteAccessToken(ctx context.Context, cacheKey string) error
}

type GeminiRefreshLeaseCache interface {
	AcquireRefreshLock(ctx context.Context, cacheKey string, ttl time.Duration) (string, error)
	ReleaseRefreshLock(ctx context.Context, cacheKey, lease string) error
}

type legacyGeminiRefreshLockCache interface {
	AcquireRefreshLock(ctx context.Context, cacheKey string, ttl time.Duration) (bool, error)
	ReleaseRefreshLock(ctx context.Context, cacheKey string) error
}

func acquireGeminiRefreshLock(ctx context.Context, cache GeminiTokenCache, key string, ttl time.Duration) (string, bool, error) {
	if leaseCache, ok := cache.(GeminiRefreshLeaseCache); ok {
		lease, err := leaseCache.AcquireRefreshLock(ctx, key, ttl)
		return lease, lease != "", err
	}
	if legacyCache, ok := cache.(legacyGeminiRefreshLockCache); ok {
		acquired, err := legacyCache.AcquireRefreshLock(ctx, key, ttl)
		if acquired {
			return "legacy", true, err
		}
		return "", false, err
	}
	return "", false, nil
}

func releaseGeminiRefreshLock(ctx context.Context, cache GeminiTokenCache, key, lease string) error {
	if leaseCache, ok := cache.(GeminiRefreshLeaseCache); ok {
		return leaseCache.ReleaseRefreshLock(ctx, key, lease)
	}
	if legacyCache, ok := cache.(legacyGeminiRefreshLockCache); ok && lease != "" {
		return legacyCache.ReleaseRefreshLock(ctx, key)
	}
	return nil
}
