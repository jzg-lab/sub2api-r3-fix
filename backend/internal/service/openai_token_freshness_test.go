package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type freshnessAccountRepo struct {
	AccountRepository
	account *Account
	reads   int
}

func (r *freshnessAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	r.reads++
	return snapshotOAuthRefreshAccount(r.account), nil
}

type freshnessTokenCache struct {
	GeminiTokenCache
	value   string
	lease   string
	lockErr error
	sets    int
}

func (c *freshnessTokenCache) GetAccessToken(context.Context, string) (string, error) {
	return c.value, nil
}

func (c *freshnessTokenCache) SetAccessToken(_ context.Context, _ string, value string, _ time.Duration) error {
	c.value = value
	c.sets++
	return nil
}

func (c *freshnessTokenCache) AcquireRefreshLock(context.Context, string, time.Duration) (string, error) {
	return c.lease, c.lockErr
}

func (c *freshnessTokenCache) ReleaseRefreshLock(context.Context, string, string) error {
	return nil
}

type freshnessExecutor struct {
	OAuthRefreshExecutor
	err error
}

func (*freshnessExecutor) CacheKey(account *Account) string { return OpenAITokenCacheKey(account) }
func (*freshnessExecutor) CanRefresh(*Account) bool         { return true }
func (e *freshnessExecutor) NeedsRefresh(account *Account, skew time.Duration) bool {
	expiresAt := account.GetCredentialAsTime("expires_at")
	return expiresAt == nil || time.Until(*expiresAt) <= skew
}
func (e *freshnessExecutor) Refresh(context.Context, *Account) (map[string]any, error) {
	if e.err != nil {
		return nil, e.err
	}
	return nil, errors.New("unexpected fixture refresh")
}

func freshnessAccount(expiry any) *Account {
	account := &Account{
		ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"access_token": "fixture-old", "refresh_token": "fixture-refresh",
		},
	}
	if expiry != nil {
		account.Credentials["expires_at"] = expiry
	}
	return account
}

func TestOpenAITokenFreshnessCacheCannotBypassRefresh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expiry any
	}{
		{"expired", time.Now().Add(-time.Minute).Format(time.RFC3339)},
		{"refresh window", time.Now().Add(time.Second).Format(time.RFC3339)},
		{"missing", nil},
		{"malformed", "not-a-time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := freshnessAccount(tc.expiry)
			current := freshnessAccount(time.Now().Add(time.Hour).Format(time.RFC3339))
			current.Credentials["access_token"] = "fixture-new"
			repo := &freshnessAccountRepo{account: current}
			cache := &freshnessTokenCache{value: old.GetOpenAIAccessToken(), lease: "fixture-lease"}
			provider := NewOpenAITokenProvider(repo, cache, nil)
			provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, cache), &freshnessExecutor{})
			value, err := provider.GetAccessToken(t.Context(), old)
			if err != nil || value != current.GetOpenAIAccessToken() || repo.reads == 0 {
				t.Fatal("stale cache bypassed durable refresh decision")
			}
		})
	}
}

func TestOpenAITokenFreshnessNeverFallsBackToExpired(t *testing.T) {
	for _, failure := range []string{"lock held", "lock unavailable", "refresh failed"} {
		t.Run(failure, func(t *testing.T) {
			account := freshnessAccount(time.Now().Add(-time.Minute).Format(time.RFC3339))
			repo := &freshnessAccountRepo{account: account}
			cache := &freshnessTokenCache{value: account.GetOpenAIAccessToken()}
			executor := &freshnessExecutor{}
			if failure == "lock unavailable" {
				cache.lockErr = errors.New("fixture unavailable")
			}
			if failure == "refresh failed" {
				cache.lease = "fixture-lease"
				executor.err = errors.New("fixture refresh unavailable")
			}
			provider := NewOpenAITokenProvider(repo, cache, nil)
			policy := OpenAIProviderRefreshPolicy()
			policy.OnLockHeld = ProviderLockHeldUseExistingToken
			provider.SetRefreshPolicy(policy)
			provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, cache), executor)
			value, err := provider.GetAccessToken(t.Context(), account)
			if err == nil || value != "" || cache.sets != 0 {
				t.Fatal("expired credential returned or recached after refresh unavailable")
			}
		})
	}
}

func TestOpenAITokenFreshnessFreshCacheKeepsFastPath(t *testing.T) {
	account := freshnessAccount(time.Now().Add(time.Hour).Format(time.RFC3339))
	repo := &freshnessAccountRepo{account: account}
	cache := &freshnessTokenCache{value: account.GetOpenAIAccessToken()}
	provider := NewOpenAITokenProvider(repo, cache, nil)
	value, err := provider.GetAccessToken(t.Context(), account)
	if err != nil || value != account.GetOpenAIAccessToken() || repo.reads != 0 || cache.sets != 0 {
		t.Fatal("fresh cache lost its read-free fast path")
	}
}

func TestOpenAITokenFreshnessRejectsExpiredDurableCacheWinner(t *testing.T) {
	old := freshnessAccount(time.Now().Add(-2 * time.Minute).Format(time.RFC3339))
	current := freshnessAccount(time.Now().Add(-time.Minute).Format(time.RFC3339))
	current.Credentials["access_token"] = "fixture-new"
	repo := &freshnessAccountRepo{account: current}
	cache := &freshnessTokenCache{value: current.GetOpenAIAccessToken()}
	value, err := cachedOAuthAccessToken(t.Context(), cache, OpenAITokenCacheKey(old), old, repo)
	if err != nil || value != "" {
		t.Fatal("cache winner was accepted without checking its own expiry")
	}
}
