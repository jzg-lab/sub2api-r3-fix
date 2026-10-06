package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	openAIUnauthorizedRecoveryTimeout = 15 * time.Second
	openAIUnauthorizedBodyLimit       = 64 << 10
)

// The original caller context gates credential changes and replay. The outgoing
// request may intentionally be detached for streaming/accounting.
func (s *OpenAIGatewayService) doOpenAIUpstream(ctx context.Context, request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	return doOpenAIUpstreamWithRecovery(ctx, request, proxyURL, account, s.openAITokenProvider, false, s.roundTripOpenAIUpstream)
}

func doOpenAIUpstreamWithRecovery(
	ctx context.Context,
	request *http.Request,
	proxyURL string,
	account *Account,
	provider *OpenAITokenProvider,
	allowUnscheduled bool,
	roundTrip func(*http.Request, string, *Account) (*http.Response, error),
) (*http.Response, error) {
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	var repo AccountRepository
	if provider != nil {
		repo = provider.accountRepo
		if repo == nil && provider.refreshAPI != nil {
			repo = provider.refreshAPI.accountRepo
		}
	}
	account = snapshotOpenAIRequestAccount(request.Context(), account, request.Header.Get("Authorization"), repo)
	authAccount := openAIAuthAttemptAccount(account)
	response, err := roundTrip(request, proxyURL, account)
	bindOpenAIResponseAccount(response, request, account)
	if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized ||
		response.Body == nil || !openAIUnauthorizedAccountEligible(authAccount, allowUnscheduled) ||
		provider == nil || provider.refreshAPI == nil || provider.executor == nil {
		return response, err
	}
	if ctx.Err() != nil {
		_ = response.Body.Close()
		return nil, ctx.Err()
	}
	rejected := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	if rejected == "" || rejected == request.Header.Get("Authorization") ||
		(request.Body != nil && request.Body != http.NoBody && request.GetBody == nil) {
		return response, nil
	}

	// Preserve the complete original error response, including oversized and
	// non-JSON responses, when recovery is inapplicable or unsuccessful.
	originalBody := response.Body
	stopReadCancel := context.AfterFunc(ctx, func() { _ = originalBody.Close() })
	body, readErr := io.ReadAll(io.LimitReader(originalBody, openAIUnauthorizedBodyLimit+1))
	stopReadCancel()
	if ctx.Err() != nil {
		_ = originalBody.Close()
		return nil, ctx.Err()
	}
	response.Body = &openAIUnauthorizedBody{
		Reader: io.MultiReader(bytes.NewReader(body), originalBody), Closer: originalBody,
	}
	if readErr != nil || len(body) > openAIUnauthorizedBodyLimit || !isRecoverableOpenAIUnauthorized(body) {
		return response, nil
	}

	var replayBody io.ReadCloser
	if request.GetBody != nil {
		replayBody, err = request.GetBody()
		if err != nil {
			return response, nil
		}
	}
	replayed := false
	defer func() {
		if replayBody != nil && !replayed {
			_ = replayBody.Close()
		}
	}()
	recoveryCtx, cancel := context.WithTimeout(ctx, openAIUnauthorizedRecoveryTimeout)
	defer cancel()
	executor := &openAIUnauthorizedExecutor{
		OAuthRefreshExecutor: provider.executor,
		expected:             authAccount,
		rejected:             rejected,
		route:                proxyURL,
		allowUnscheduled:     allowUnscheduled,
	}
	result, refreshErr := provider.refreshAPI.RefreshIfNeeded(
		withOAuthRefreshRequestPath(recoveryCtx), authAccount, executor, 0,
	)
	if refreshErr == nil && result != nil && result.LockHeld {
		result, refreshErr = executor.waitForWinner(recoveryCtx, provider.refreshAPI.accountRepo)
	}
	if ctx.Err() != nil {
		_ = response.Body.Close()
		return nil, ctx.Err()
	}
	if errors.Is(refreshErr, errOAuthRefreshAccountStateChanged) || errors.Is(refreshErr, ErrOAuthReauthorizationStale) {
		_ = response.Body.Close()
		// Do not let a stale 401 quarantine a newly edited/reauthorized account.
		return nil, errOAuthRefreshAccountStateChanged
	}
	if refreshErr != nil || result == nil || result.LockHeld || result.Account == nil {
		slog.Debug("openai_401_recovery_unavailable", "account_id", account.ID)
		return response, nil
	}

	// Re-read after persistence so a concurrent disable, credential replacement
	// or proxy change cannot be hidden by the refresh result's older snapshot.
	current, readErr := provider.refreshAPI.accountRepo.GetByID(recoveryCtx, authAccount.ID)
	if ctx.Err() != nil {
		_ = response.Body.Close()
		return nil, ctx.Err()
	}
	if readErr != nil {
		return response, nil
	}
	if !executor.CanRefresh(current) {
		_ = response.Body.Close()
		return nil, errOAuthRefreshAccountStateChanged
	}
	token := current.GetOpenAIAccessToken()
	if token == "" || token == rejected || token != result.Account.GetOpenAIAccessToken() {
		return response, nil
	}
	if result.Refreshed && provider.tokenCache != nil {
		// Never publish a token under a stale scheduler snapshot's cache key.
		if err := provider.tokenCache.DeleteAccessToken(recoveryCtx, OpenAITokenCacheKey(authAccount)); err != nil {
			slog.Warn("openai_401_recovery_cache_invalidation_failed", "account_id", account.ID)
		}
	}
	if recoveryCtx.Err() != nil {
		return response, nil
	}
	retryCtx, cancelRetry := context.WithCancel(request.Context())
	stopCancel := context.AfterFunc(ctx, cancelRetry)
	retry := request.Clone(retryCtx)
	retry.Body = replayBody
	retry.Header.Set("Authorization", "Bearer "+token)
	_ = response.Body.Close()
	slog.Info("openai_401_recovery_retry", "account_id", account.ID, "refreshed", result.Refreshed)
	// One retry only. A second 401 follows the existing quarantine policy.
	replayed = true
	retryAccount := current
	if account.IsShadow() {
		retryAccount = snapshotOAuthRefreshAccount(account)
		retryAccount.openAIAuthAttempt = snapshotOAuthRefreshAccount(current)
	}
	retryResponse, retryErr := roundTrip(retry, proxyURL, retryAccount)
	bindOpenAIResponseAccount(retryResponse, retry, retryAccount)
	cleanup := func() {
		stopCancel()
		cancelRetry()
	}
	if retryErr != nil || retryResponse == nil || retryResponse.Body == nil {
		cleanup()
	} else {
		retryResponse.Body = &openAIRequestContextReadCloser{ReadCloser: retryResponse.Body, cleanup: cleanup}
	}
	return retryResponse, retryErr
}

type openAIUnauthorizedBody struct {
	io.Reader
	io.Closer
}

func isRecoverableOpenAIUnauthorized(body []byte) bool {
	// Only explicit access-token failures are eligible. Generic "Unauthorized",
	// revocation, workspace removal, and security challenges require the normal
	// error/manual reauthorization path, not blind refresh attempts.
	switch strings.ToLower(extractUpstreamErrorCode(body)) {
	case "token_expired", "access_token_expired", "invalid_token", "invalid_api_key":
		message := strings.ToLower(extractUpstreamErrorMessage(body))
		for _, permanent := range []string{"revoked", "invalidated", "deactivated", "disabled", "suspended", "banned", "workspace", "organization", "verification", "challenge"} {
			if strings.Contains(message, permanent) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func openAIUnauthorizedAccountEligible(account *Account, allowUnscheduled bool) bool {
	if !IsOpenAIBrowserOAuthAccount(account) || strings.TrimSpace(account.GetOpenAIRefreshToken()) == "" {
		return false
	}
	// Explicit account/qualification probes can test quarantined candidates.
	// Refreshing their token never graduates them or clears any cooldown.
	snapshot := *account
	if allowUnscheduled {
		snapshot.Schedulable = true
	}
	return snapshot.IsSchedulable()
}

type openAIUnauthorizedExecutor struct {
	OAuthRefreshExecutor
	expected         *Account
	rejected         string
	route            string
	allowUnscheduled bool
}

func (e *openAIUnauthorizedExecutor) CanRefresh(current *Account) bool {
	return openAIUnauthorizedAccountEligible(current, e.allowUnscheduled) &&
		e.expected.Schedulable == current.Schedulable &&
		sameOpenAIUnauthorizedBinding(e.expected, current) &&
		validateOpenAIAccountProxyRoute(current, e.route) == nil &&
		e.OAuthRefreshExecutor.CanRefresh(current)
}

func (e *openAIUnauthorizedExecutor) NeedsRefresh(current *Account, _ time.Duration) bool {
	// Another request may already have rotated the rejected credential while
	// this request waited for the shared local/distributed refresh lock.
	return current.GetOpenAIAccessToken() == e.rejected
}

func (e *openAIUnauthorizedExecutor) waitForWinner(ctx context.Context, repo AccountRepository) (*OAuthRefreshResult, error) {
	wait := openAILockInitialWait
	for i := 0; i < openAILockMaxAttempts; i++ {
		timer := time.NewTimer(jitterLockWait(wait))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		current, err := repo.GetByID(ctx, e.expected.ID)
		if err != nil {
			return nil, err
		}
		if !e.CanRefresh(current) {
			return nil, errOAuthRefreshAccountStateChanged
		}
		if current.GetOpenAIAccessToken() != e.rejected {
			return &OAuthRefreshResult{Account: current}, nil
		}
		wait = min(wait*2, openAILockMaxWait)
	}
	// Do not acquire another lease or rotate a token after this bounded wait.
	return &OAuthRefreshResult{LockHeld: true}, nil
}

func sameOpenAIUnauthorizedBinding(expected, current *Account) bool {
	identity := func(account *Account) string {
		if !IsOpenAIBrowserOAuthAccount(account) {
			return ""
		}
		snapshot := snapshotOAuthRefreshAccount(account)
		for _, key := range []string{"access_token", "refresh_token", "id_token", "expires_at", "_token_version"} {
			delete(snapshot.Credentials, key)
		}
		return OpenAIOAuthAccountRevision(snapshot)
	}
	expectedRevision := identity(expected)
	return expectedRevision != "" && expectedRevision == identity(current)
}
