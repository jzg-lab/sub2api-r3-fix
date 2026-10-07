package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// OpenAIAuthStateUpdate changes one authentication-related state, never
// credentials. The repository must match the actual attempted account.
type OpenAIAuthStateUpdate struct {
	ErrorMessage   *string
	CooldownUntil  *time.Time
	CooldownReason string
}

type OpenAIAuthStateRepository interface {
	ApplyOpenAIAuthStateIfUnchanged(context.Context, *Account, OpenAIAuthStateUpdate) (bool, error)
}

func applyOpenAIAuthState(ctx context.Context, repo AccountRepository, account *Account, change OpenAIAuthStateUpdate) (bool, error) {
	conditional, ok := repo.(OpenAIAuthStateRepository)
	if !ok {
		return false, errors.New("OpenAI authentication state CAS repository unavailable")
	}
	return conditional.ApplyOpenAIAuthStateIfUnchanged(ctx, account, change)
}

func setAccountTestAuthError(ctx context.Context, repo AccountRepository, account *Account, message string) {
	if repo == nil {
		return
	}
	if !IsOpenAIBrowserOAuthAccount(account) {
		_ = repo.SetError(ctx, account.ID, message)
		return
	}
	if _, err := applyOpenAIAuthState(ctx, repo, account, OpenAIAuthStateUpdate{ErrorMessage: &message}); err != nil {
		slog.Warn("openai_test_auth_state_update_failed", "account_id", account.ID)
	}
}

func (s *RateLimitService) handleOpenAIOAuthUnauthorized(ctx context.Context, account *Account, body []byte, upstreamMessage string) bool {
	code := extractUpstreamErrorCode(body)
	missingRefresh := strings.TrimSpace(account.GetOpenAIRefreshToken()) == ""
	permanent := code == "token_invalidated" || code == "token_revoked" ||
		gjson.GetBytes(body, "detail").String() == "Unauthorized" ||
		missingRefresh
	message := "Authentication failed (401): invalid or expired credentials"
	if upstreamMessage != "" {
		message = "OAuth 401: " + upstreamMessage
	}
	switch {
	case code == "token_invalidated" || code == "token_revoked":
		message = "Token revoked (401): account authentication permanently revoked"
		if upstreamMessage != "" {
			message = "Token revoked (401): " + upstreamMessage
		}
	case gjson.GetBytes(body, "detail").String() == "Unauthorized":
		message = "Unauthorized (401): account authentication failed permanently"
		if upstreamMessage != "" {
			message = "Unauthorized (401): " + upstreamMessage
		}
	case missingRefresh:
		message = "Authentication failed (401): refresh_token missing, cannot recover"
		if upstreamMessage != "" {
			message = "OAuth 401 (no refresh_token): " + upstreamMessage
		}
	}
	change := OpenAIAuthStateUpdate{}
	until := time.Time{}
	if permanent {
		change.ErrorMessage = &message
	} else {
		cooldownMinutes := 10
		if s.cfg != nil && s.cfg.RateLimit.OAuth401CooldownMinutes > 0 {
			cooldownMinutes = s.cfg.RateLimit.OAuth401CooldownMinutes
		}
		until = time.Now().Add(time.Duration(cooldownMinutes) * time.Minute)
		change.CooldownUntil = &until
		change.CooldownReason = message
	}
	applied, err := applyOpenAIAuthState(ctx, s.accountRepo, account, change)
	if err != nil {
		slog.Warn("openai_401_state_update_failed", "account_id", account.ID)
		return false
	}
	if !applied {
		slog.Debug("openai_401_stale_result_ignored", "account_id", account.ID)
		return false
	}
	if s.tokenCacheInvalidator != nil {
		if err := s.tokenCacheInvalidator.InvalidateToken(ctx, account); err != nil {
			slog.Warn("oauth_401_invalidate_cache_failed", "account_id", account.ID)
		}
	}
	s.notifyAccountSchedulingBlocked(account, until, "oauth_401")
	return true
}

type openAIResponseAccountKey struct{}

func isOpenAIAuthAttemptAccount(account *Account) bool {
	return IsOpenAIBrowserOAuthAccount(account) ||
		(account != nil && account.IsOpenAIOAuth() && account.IsShadow())
}

// Capture before network I/O, not while handling its response. A provider can
// rotate the full credential set before returning a token to an older scheduler
// snapshot; replacing only access_token would bind that attempt to a mixed set.
func snapshotOpenAIRequestAccount(ctx context.Context, account *Account, authorization string, repo AccountRepository) *Account {
	if !isOpenAIAuthAttemptAccount(account) {
		return account
	}
	snapshot := snapshotOAuthRefreshAccount(account)
	snapshot.openAIAuthAttempt = nil
	owner := account
	if account.IsShadow() {
		if repo == nil {
			return snapshot
		}
		var err error
		owner, err = resolveCredentialAccount(ctx, repo, account)
		if err != nil || !IsOpenAIBrowserOAuthAccount(owner) ||
			!sameOpenAIProbeProxy(owner.ProxyID, account.ProxyID) {
			return snapshot
		}
	}
	token := ""
	if strings.HasPrefix(authorization, "Bearer ") {
		token = strings.TrimPrefix(authorization, "Bearer ")
	}
	if token != "" && token != owner.GetOpenAIAccessToken() && repo != nil {
		current, err := repo.GetByID(ctx, owner.ID)
		if err == nil && current != nil && token == current.GetOpenAIAccessToken() &&
			current.Status == owner.Status && current.Schedulable == owner.Schedulable &&
			sameOpenAIUnauthorizedBinding(owner, current) {
			owner = current
		}
	}
	attempt := snapshotOAuthRefreshAccount(owner)
	attempt.openAIAuthAttempt = nil
	if attempt.Credentials == nil {
		attempt.Credentials = make(map[string]any)
	}
	attempt.Credentials["access_token"] = token
	if account.IsShadow() {
		snapshot.openAIAuthAttempt = attempt
		return snapshot
	}
	return attempt
}

func openAIAuthAttemptAccount(account *Account) *Account {
	if account == nil || !account.IsShadow() {
		return account
	}
	attempt := account.openAIAuthAttempt
	if attempt == nil || attempt.ID != *account.ParentAccountID || attempt.IsShadow() {
		return nil
	}
	return attempt
}

func bindOpenAIResponseAccount(response *http.Response, request *http.Request, account *Account) {
	if response == nil || request == nil || !isOpenAIAuthAttemptAccount(account) {
		return
	}
	snapshot := snapshotOAuthRefreshAccount(account)
	// The provider may have returned a fresher token than the scheduler
	// snapshot, or header overrides may have changed the attempted token.
	// A mismatching token must never quarantine the newer durable credentials.
	const prefix = "Bearer "
	authorization := request.Header.Get("Authorization")
	if snapshot.Credentials == nil {
		snapshot.Credentials = make(map[string]any)
	}
	snapshot.Credentials["access_token"] = ""
	if len(authorization) >= len(prefix) && authorization[:len(prefix)] == prefix {
		snapshot.Credentials["access_token"] = authorization[len(prefix):]
	}
	response.Request = request.WithContext(context.WithValue(request.Context(), openAIResponseAccountKey{}, snapshot))
}

func openAIResponseAccount(response *http.Response, fallback *Account) *Account {
	if response != nil && response.Request != nil && fallback != nil {
		if account, ok := response.Request.Context().Value(openAIResponseAccountKey{}).(*Account); ok && account.ID == fallback.ID {
			return account
		}
	}
	return fallback
}
