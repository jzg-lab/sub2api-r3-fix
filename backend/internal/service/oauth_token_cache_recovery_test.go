//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type oauthCacheRecoveryRepo struct {
	AccountRepository
	account *Account
	err     error
	reads   int
}

func (r *oauthCacheRecoveryRepo) GetByID(context.Context, int64) (*Account, error) {
	r.reads++
	return r.account, r.err
}

type oauthAccessTokenProvider interface {
	GetAccessToken(context.Context, *Account) (string, error)
}

func TestOAuthTokenProvidersRecoverAfterFailedInvalidation(t *testing.T) {
	cases := []struct {
		platform string
		key      func(*Account) string
		provider func(AccountRepository, GeminiTokenCache) oauthAccessTokenProvider
	}{
		{PlatformOpenAI, OpenAITokenCacheKey, func(r AccountRepository, c GeminiTokenCache) oauthAccessTokenProvider {
			return NewOpenAITokenProvider(r, c, nil)
		}},
		{PlatformAnthropic, ClaudeTokenCacheKey, func(r AccountRepository, c GeminiTokenCache) oauthAccessTokenProvider {
			return NewClaudeTokenProvider(r, c, nil)
		}},
		{PlatformGemini, GeminiTokenCacheKey, func(r AccountRepository, c GeminiTokenCache) oauthAccessTokenProvider {
			return NewGeminiTokenProvider(r, c, nil)
		}},
		{PlatformAntigravity, AntigravityTokenCacheKey, func(r AccountRepository, c GeminiTokenCache) oauthAccessTokenProvider {
			return NewAntigravityTokenProvider(r, c, nil)
		}},
		{PlatformGrok, GrokTokenCacheKey, func(r AccountRepository, c GeminiTokenCache) oauthAccessTokenProvider {
			return NewGrokTokenProvider(r, c)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.platform, func(t *testing.T) {
			account := &Account{ID: 51, Platform: tc.platform, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{
					"access_token":   "after",
					"refresh_token":  "fixture",
					"expires_at":     time.Now().Add(2 * grokTokenRefreshSkew).Format(time.RFC3339),
					"_token_version": int64(2),
				},
			}
			repo := &oauthCacheRecoveryRepo{account: account}
			cache := newOpenAITokenCacheStub()
			cache.tokens[tc.key(account)] = "before"
			cache.deleteErr = errors.New("cache unavailable during credential commit")
			if err := NewCompositeTokenCacheInvalidator(cache).InvalidateToken(context.Background(), account); err == nil {
				t.Fatal("cache deletion failure must remain observable")
			}
			cache.deleteErr = nil
			provider := tc.provider(repo, cache)
			got, err := provider.GetAccessToken(context.Background(), account)
			if err != nil || got != account.GetCredential("access_token") {
				t.Fatalf("recovered cache did not return committed credentials (error: %v)", err)
			}
			if cache.tokens[tc.key(account)] != got {
				t.Fatal("stale cache was not repaired")
			}
			reads := repo.reads
			if _, err := provider.GetAccessToken(context.Background(), account); err != nil {
				t.Fatal(err)
			}
			if repo.reads != reads {
				t.Fatal("valid cache hit introduced a database read")
			}
			repo.err = errors.New("database unavailable")
			cache.tokens[tc.key(account)] = "before"
			got, err = provider.GetAccessToken(context.Background(), account)
			if err != nil || got != account.GetCredential("access_token") {
				t.Fatal("database outage allowed the cache to override the current snapshot")
			}
			repo.err = nil

			// Both cache hits and cache-miss recovery must retain the proxy
			// selected before a concurrent account change.
			latest := *account
			proxyID := int64(99)
			latest.ProxyID = &proxyID
			latest.Credentials = map[string]any{"access_token": "replacement", "_token_version": int64(3)}
			repo.account = &latest
			for _, cached := range []bool{false, true} {
				delete(cache.tokens, tc.key(account))
				if cached {
					cache.tokens[tc.key(account)] = latest.GetCredential("access_token")
				}
				got, err = provider.GetAccessToken(context.Background(), account)
				if err == nil || got != "" {
					t.Fatal("concurrent reauthorization changed credentials across the selected proxy")
				}
			}
		})
	}
}

func TestCachedOAuthAccessTokenRequiresPersistedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*oauthCacheRecoveryRepo)
		wantHit   bool
		wantError bool
	}{
		{"concurrent-refresh", func(*oauthCacheRecoveryRepo) {}, true, false},
		{"stale-cache", func(r *oauthCacheRecoveryRepo) {
			r.account.Credentials["access_token"] = "committed"
		}, false, false},
		{"missing-account", func(r *oauthCacheRecoveryRepo) { r.account = nil }, false, true},
		{"database-unavailable", func(r *oauthCacheRecoveryRepo) { r.err = errors.New("database unavailable") }, false, true},
		{"different-account", func(r *oauthCacheRecoveryRepo) { r.account.ID++ }, false, true},
		{"different-platform", func(r *oauthCacheRecoveryRepo) { r.account.Platform = PlatformGemini }, false, true},
		{"different-type", func(r *oauthCacheRecoveryRepo) { r.account.Type = AccountTypeAPIKey }, false, true},
		{"different-proxy", func(r *oauthCacheRecoveryRepo) { id := int64(99); r.account.ProxyID = &id }, false, true},
		{"missing-proxy", func(r *oauthCacheRecoveryRepo) { r.account.ProxyID = nil }, false, true},
		{"older-version", func(r *oauthCacheRecoveryRepo) { r.account.Credentials["_token_version"] = int64(1) }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxyID := int64(15)
			account := &Account{ID: 51, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ProxyID: &proxyID,
				Credentials: map[string]any{"access_token": "snapshot", "_token_version": int64(2)}}
			latest := *account
			latest.Credentials = map[string]any{"access_token": "cached", "_token_version": int64(3)}
			repo := &oauthCacheRecoveryRepo{account: &latest}
			tc.change(repo)
			cache := newOpenAITokenCacheStub()
			cache.tokens[OpenAITokenCacheKey(account)] = "cached"
			got, err := cachedOAuthAccessToken(context.Background(), cache, OpenAITokenCacheKey(account), account, repo)
			if (got != "") != tc.wantHit || (err != nil) != tc.wantError {
				t.Fatal("cache acceptance did not match persisted account identity")
			}
		})
	}
}

func TestCachedOAuthAccessTokenCannotTrustUnboundCache(t *testing.T) {
	account := &Account{ID: 51, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "snapshot"}}
	cache := newOpenAITokenCacheStub()
	key := OpenAITokenCacheKey(account)
	cache.tokens[key] = "unbound"
	got, err := cachedOAuthAccessToken(context.Background(), cache, key, account, nil)
	if err != nil || got != "" {
		t.Fatal("unbound cache token was accepted without persistence evidence")
	}
}

func TestOpenAITokenProviderLockWaitRejectsStaleCache(t *testing.T) {
	account := &Account{ID: 51, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "after", "_token_version": int64(2)}}
	cache := newOpenAITokenCacheStub()
	key := OpenAITokenCacheKey(account)
	cache.tokens[key] = "before"
	provider := NewOpenAITokenProvider(&oauthCacheRecoveryRepo{account: account}, cache, nil)
	got, err := provider.waitForTokenAfterLockRace(context.Background(), key, account)
	if err != nil || got != "" || provider.SnapshotRuntimeMetrics().LockWaitHit != 0 {
		t.Fatal("refresh lock wait accepted stale credentials")
	}
}
