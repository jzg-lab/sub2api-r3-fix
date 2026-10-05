package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
)

type openAIStoredCredentialsKey struct{}

type openAIStoredCredentialsSnapshot struct {
	accountID int64
	proxyID   int64
	hasProxy  bool
	hash      [sha256.Size]byte
}

func snapshotOpenAIStoredCredentials(account *Account) (openAIStoredCredentialsSnapshot, error) {
	snapshot := openAIStoredCredentialsSnapshot{accountID: account.ID}
	if account.ProxyID != nil {
		snapshot.hasProxy, snapshot.proxyID = true, *account.ProxyID
	}
	raw, err := json.Marshal(account.Credentials)
	if err != nil {
		return snapshot, ErrOAuthReauthorizationStale
	}
	snapshot.hash = sha256.Sum256(raw)
	return snapshot, nil
}

// Compare refresh inputs rather than unrelated scheduling/usage revisions.
func bindOpenAIOAuthCredentialSnapshot(ctx context.Context, account *Account) (context.Context, error) {
	if !IsOpenAIBrowserOAuthAccount(account) {
		return ctx, nil
	}
	snapshot, err := snapshotOpenAIStoredCredentials(account)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, openAIStoredCredentialsKey{}, snapshot), nil
}

func ValidateOpenAIOAuthCredentialSnapshot(ctx context.Context, current *Account) error {
	expected, bound := ctx.Value(openAIStoredCredentialsKey{}).(openAIStoredCredentialsSnapshot)
	// Existing import, reauthorization and explicit edit paths keep their rules.
	if !bound {
		return nil
	}
	if !IsOpenAIBrowserOAuthAccount(current) {
		return ErrOAuthReauthorizationStale
	}
	actual, err := snapshotOpenAIStoredCredentials(current)
	if err != nil || actual != expected {
		return ErrOAuthReauthorizationStale
	}
	return nil
}
