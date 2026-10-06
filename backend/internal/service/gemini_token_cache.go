package service

import (
	"context"
	"time"
)

// GeminiTokenCache stores short-lived access tokens and coordinates refresh to avoid stampedes.
type GeminiTokenCache interface {
	// cacheKey should be stable for the token scope; for GeminiCli OAuth we primarily use project_id.
	// A missing or expired entry returns an empty token without an error.
	GetAccessToken(ctx context.Context, cacheKey string) (string, error)
	SetAccessToken(ctx context.Context, cacheKey string, token string, ttl time.Duration) error
	DeleteAccessToken(ctx context.Context, cacheKey string) error

	// An empty lease means another worker holds the lock. Release must compare
	// the lease atomically so an expired holder cannot unlock its successor.
	AcquireRefreshLock(ctx context.Context, cacheKey string, ttl time.Duration) (string, error)
	ReleaseRefreshLock(ctx context.Context, cacheKey, lease string) error
}
