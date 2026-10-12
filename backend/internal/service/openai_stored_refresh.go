package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Stored-account consumers must hold the shared lease through persistence,
// not merely protect the later database update after a rotating-token request.
func (s *OpenAIOAuthService) RefreshStoredAccount(ctx context.Context, account *Account, force bool) (*Account, error) {
	if s == nil || s.refreshAPI == nil {
		return nil, fmt.Errorf("stored OpenAI refresh coordinator is not configured")
	}
	return s.refreshAPI.refreshStoredOpenAIAccount(ctx, account, NewOpenAITokenRefresher(s, nil), force)
}

type storedOpenAIRefreshExecutor struct {
	OAuthRefreshExecutor
	expected *Account
	force    bool
	api      *OAuthRefreshAPI
}

func (e *storedOpenAIRefreshExecutor) CanRefresh(current *Account) bool {
	return oauthTokenAccountIdentityMatches(e.expected, current) &&
		IsOpenAIBrowserOAuthAccount(current) && !current.IsCredentialShadow() &&
		strings.TrimSpace(current.GetOpenAIRefreshToken()) != "" &&
		current.GetCredential("client_id") == e.expected.GetCredential("client_id") &&
		e.OAuthRefreshExecutor.CanRefresh(current)
}

func (e *storedOpenAIRefreshExecutor) NeedsRefresh(current *Account, window time.Duration) bool {
	if e.force {
		// A concurrent winner already consumed this manual request's generation.
		return current.GetOpenAIAccessToken() == e.expected.GetOpenAIAccessToken() &&
			current.GetOpenAIRefreshToken() == e.expected.GetOpenAIRefreshToken()
	}
	return e.OAuthRefreshExecutor.NeedsRefresh(current, window)
}

func (e *storedOpenAIRefreshExecutor) Refresh(ctx context.Context, current *Account) (map[string]any, error) {
	attempted := snapshotOAuthRefreshAccount(current)
	credentials, err := e.OAuthRefreshExecutor.Refresh(ctx, current)
	if err == nil || ctx.Err() != nil || classifyOpenAIRefreshRecovery(err.Error()) != "permanent" || isSharedProviderRefreshError(err) {
		return credentials, err
	}
	// Persist rejection before releasing the shared lease so queued callers do
	// not consume the same revoked session. CAS protects concurrent reauthorization.
	message := "Token refresh failed (non-retryable): token has been invalidated; browser reauthorization required"
	applied, stateErr := applyOpenAIAuthState(ctx, e.api.accountRepo, attempted, OpenAIAuthStateUpdate{ErrorMessage: &message})
	if stateErr != nil {
		return nil, &providerCycleContainmentRefreshError{err: errors.Join(err,
			fmt.Errorf("failed to conditionally persist OpenAI OAuth refresh failure: %w", stateErr))}
	}
	if applied && e.api.tokenCache != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultRefreshPostPersistCleanupTimeout)
		defer cancel()
		if invalidateErr := NewCompositeTokenCacheInvalidator(e.api.tokenCache).InvalidateToken(cleanupCtx, attempted); invalidateErr != nil {
			slog.Warn("openai_stored_refresh_failed_token_cache_invalidation_failed", "account_id", attempted.ID)
		}
	}
	return nil, err
}

func (api *OAuthRefreshAPI) refreshStoredOpenAIAccount(ctx context.Context, account *Account, executor OAuthRefreshExecutor, force bool) (*Account, error) {
	if api == nil || api.accountRepo == nil || executor == nil {
		return nil, fmt.Errorf("stored OpenAI refresh coordinator is not configured")
	}
	if account == nil || account.ID <= 0 || !IsOpenAIBrowserOAuthAccount(account) ||
		account.IsCredentialShadow() || strings.TrimSpace(account.GetOpenAIRefreshToken()) == "" {
		return nil, infraerrors.BadRequest("OPENAI_OAUTH_NOT_REFRESHABLE", "account requires browser reauthorization")
	}
	if !account.IsActive() && account.Status != StatusError {
		return nil, errOAuthRefreshAccountStateChanged
	}
	if account.Status == StatusError && classifyOpenAIRecovery(account.ErrorMessage) == "permanent" {
		return nil, infraerrors.BadRequest("OPENAI_OAUTH_REAUTHORIZATION_REQUIRED", "authorization was revoked; browser reauthorization is required")
	}
	wrapped := &storedOpenAIRefreshExecutor{
		OAuthRefreshExecutor: executor,
		expected:             snapshotOAuthRefreshAccount(account),
		force:                force,
		api:                  api,
	}
	var result *OAuthRefreshResult
	var err error
	if account.Status == StatusError {
		result, err = api.RecoverOpenAIError(ctx, account, wrapped)
	} else {
		result, err = api.RefreshIfNeeded(withOAuthRefreshRequestPath(ctx), account, wrapped, openAITokenRefreshSkew)
	}
	if err != nil {
		return nil, err
	}
	if result == nil || result.LockHeld {
		return nil, infraerrors.Conflict("OAUTH_REFRESH_IN_PROGRESS", "account refresh is already in progress; retry after it completes")
	}
	// Do not publish a pre-refresh status/proxy snapshot after an admin edit.
	current, err := api.accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	if !wrapped.CanRefresh(current) || !current.IsActive() {
		return nil, errOAuthRefreshAccountStateChanged
	}
	if result.Refreshed && account.Status != StatusError && api.tokenCache != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultRefreshPostPersistCleanupTimeout)
		defer cancel()
		if err := NewCompositeTokenCacheInvalidator(api.tokenCache).InvalidateToken(cleanupCtx, current); err != nil {
			slog.Warn("openai_stored_refresh_token_cache_invalidation_failed", "account_id", current.ID)
		}
	}
	return current, nil
}
