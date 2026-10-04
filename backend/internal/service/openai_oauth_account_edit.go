package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"time"
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
		snapshot.hasProxy = true
		snapshot.proxyID = *account.ProxyID
	}
	raw, err := json.Marshal(account.Credentials)
	if err != nil {
		return snapshot, ErrOAuthReauthorizationStale
	}
	snapshot.hash = sha256.Sum256(raw)
	return snapshot, nil
}

// Background refreshes compare credentials rather than updated_at so an
// unrelated scheduler/usage update cannot discard a successfully rotated token.
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
	expected, ok := ctx.Value(openAIStoredCredentialsKey{}).(openAIStoredCredentialsSnapshot)
	if !IsOpenAIBrowserOAuthAccount(current) {
		if ok {
			return ErrOAuthReauthorizationStale
		}
		return nil
	}
	actual, err := snapshotOpenAIStoredCredentials(current)
	if !ok || err != nil || actual != expected {
		return ErrOAuthReauthorizationStale
	}
	return nil
}

// Only a successful refresh of stored credentials can create this binding.
// It is not a JSON field or a caller-selected bypass of browser authorization.
type openAIAccountRefreshBinding struct {
	accountID int64
	revision  time.Time
	hash      string
}

func (s *OpenAIOAuthService) RefreshAccountToken(ctx context.Context, account *Account) (*OpenAITokenInfo, error) {
	result, err := s.refreshAccountToken(ctx, account)
	if err != nil || result == nil || !IsOpenAIBrowserOAuthAccount(account) {
		return result, err
	}
	credentials := s.BuildAccountCredentials(result)
	for key, value := range account.Credentials {
		if _, exists := credentials[key]; !exists {
			credentials[key] = value
		}
	}
	hash, err := openAIReauthorizationCredentialsHash(credentials)
	if err != nil {
		return nil, err
	}
	result.accountRefresh = &openAIAccountRefreshBinding{
		accountID: account.ID, revision: account.UpdatedAt, hash: hash,
	}
	return result, nil
}

func (result *OpenAITokenInfo) AccountRefreshUpdate(credentials map[string]any) *UpdateAccountInput {
	return &UpdateAccountInput{Credentials: credentials, openAIRefresh: result.accountRefresh}
}

// Preserve identity fields omitted by ordinary settings forms, just as the
// existing merge preserves redacted tokens. Explicit replacements are checked.
func preserveOpenAIOAuthEditIdentity(current *Account, credentials map[string]any) {
	if !IsOpenAIBrowserOAuthAccount(current) {
		return
	}
	for _, key := range openAIReauthorizationCredentialKeys {
		if _, exists := credentials[key]; exists {
			continue
		}
		if value, exists := current.Credentials[key]; exists {
			credentials[key] = value
		}
	}
}

func validateOpenAIOAuthAccountEdit(current *Account, nextType string, credentials map[string]any, refresh *openAIAccountRefreshBinding) error {
	if !IsOpenAIBrowserOAuthAccount(current) {
		return nil
	}
	if nextType != "" && nextType != current.Type {
		return ErrOpenAIOAuthReauthorizationProofRequired
	}
	if credentials == nil {
		return nil
	}
	changed := false
	for _, key := range openAIReauthorizationCredentialKeys {
		if !reflect.DeepEqual(current.Credentials[key], credentials[key]) {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	next := *current
	next.Credentials = credentials
	if !IsOpenAIBrowserOAuthAccount(&next) || refresh == nil ||
		refresh.accountID != current.ID || !refresh.revision.Equal(current.UpdatedAt) {
		return ErrOpenAIOAuthReauthorizationProofRequired
	}
	hash, err := openAIReauthorizationCredentialsHash(credentials)
	if err != nil || hash != refresh.hash {
		return ErrOpenAIOAuthReauthorizationProofRequired
	}
	return nil
}
