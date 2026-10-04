package service

import (
	"context"
	"errors"
	"strings"
)

// A failed eviction must not let a recovered cache override committed OAuth
// credentials. Matching snapshots retain the normal cache-only fast path.
func cachedOAuthAccessToken(ctx context.Context, cache GeminiTokenCache, key string, account *Account, repo AccountRepository) (string, error) {
	token, err := cache.GetAccessToken(ctx, key)
	if err != nil || strings.TrimSpace(token) == "" || account == nil {
		return "", err
	}
	if token == account.GetCredential("access_token") {
		return token, nil
	}
	if repo == nil {
		return "", nil
	}

	// A concurrent refresh can legitimately populate a newer token. Only the
	// persisted account, not the cache value alone, may establish that fact.
	latest, err := repo.GetByID(ctx, account.ID)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			return "", errOAuthRefreshAccountStateChanged
		}
		return "", err
	}
	if !oauthTokenAccountIdentityMatches(account, latest) {
		return "", errOAuthRefreshAccountStateChanged
	}
	if latest.GetCredentialAsInt64("_token_version") < account.GetCredentialAsInt64("_token_version") {
		return "", nil
	}
	if token != latest.GetCredential("access_token") {
		return "", nil
	}
	return token, nil
}

func oauthTokenAccountIdentityMatches(account, latest *Account) bool {
	if account == nil || latest == nil || latest.ID != account.ID ||
		latest.Platform != account.Platform || latest.Type != account.Type ||
		(latest.ProxyID == nil) != (account.ProxyID == nil) {
		return false
	}
	return latest.ProxyID == nil || *latest.ProxyID == *account.ProxyID
}
