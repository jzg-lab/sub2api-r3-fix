package service

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// This is historical login evidence, not the proxy's most recent test result.
// Generic account edits must never establish or replace this value.
const OpenAIOAuthLoginExitIPExtraKey = "openai_oauth_login_exit_ip"

var (
	ErrOpenAIOAuthReauthorizationUnavailable = infraerrors.New(
		http.StatusServiceUnavailable, "OPENAI_OAUTH_REAUTH_ACCOUNT_UNAVAILABLE",
		"account state cannot be verified; retry starting authorization when the service recovers",
	)
	ErrOpenAIOAuthReauthorizationUnsupported = infraerrors.BadRequest(
		"OPENAI_OAUTH_REAUTH_ACCOUNT_UNSUPPORTED",
		"account is not a primary OpenAI browser OAuth account",
	)
	ErrOpenAIOAuthLoginIPUnknown = infraerrors.Conflict(
		"OPENAI_OAUTH_LOGIN_IP_UNKNOWN",
		"original authorization IP is not recorded; recover historical evidence before reauthorizing",
	)
	ErrOpenAIOAuthLoginIPChanged = infraerrors.Conflict(
		"OPENAI_OAUTH_LOGIN_IP_CHANGED",
		"authorization exit does not match the original login IP; no alternate route is permitted",
	)
	ErrOpenAIOAuthLoginIPUnavailable = infraerrors.New(
		http.StatusServiceUnavailable, "OPENAI_OAUTH_LOGIN_IP_UNAVAILABLE",
		"original authorization exit cannot be verified; reauthorization has stopped",
	)
)

func (s *OpenAIOAuthService) SetReauthorizationAccountLookup(lookup func(context.Context, int64) (*Account, error)) {
	s.reauthorizationAccountLookup = lookup
}

func OpenAIOAuthLoginExitIP(account *Account) (string, error) {
	if account == nil {
		return "", ErrOpenAIOAuthLoginIPUnknown
	}
	raw, _ := account.Extra[OpenAIOAuthLoginExitIPExtraKey].(string)
	return normalizeOpenAIOAuthLoginIP(raw)
}

func normalizeOpenAIOAuthLoginIP(raw string) (string, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return "", ErrOpenAIOAuthLoginIPUnknown
	}
	return ip.Unmap().String(), nil
}

func validateOpenAIOAuthReauthorizationBinding(session *OpenAIOAuthSession) error {
	if session == nil || session.ReauthorizationAccountID < 0 {
		return ErrOpenAIOAuthSessionInvalid
	}
	if session.ReauthorizationAccountID == 0 {
		if session.ReauthorizationRevision != "" || session.ReauthorizationExitIP != "" ||
			session.ReauthorizationCredentialsHash != "" || session.ReauthorizationBrowserSessionID != "" ||
			session.ReauthorizationAccountRevision != "" {
			return ErrOpenAIOAuthSessionInvalid
		}
		return nil
	}
	revision, err := time.Parse(time.RFC3339Nano, session.ReauthorizationRevision)
	if err != nil || revision.IsZero() {
		return ErrOpenAIOAuthSessionInvalid
	}
	if session.ReauthorizationAccountRevision != "" &&
		!validOpenAIOAuthAccountRevision(session.ReauthorizationAccountRevision) {
		return ErrOpenAIOAuthSessionInvalid
	}
	if session.ReauthorizationBrowserSessionID != "" &&
		(!validOpenAIAuthBrowserLowerHex(session.ReauthorizationBrowserSessionID, 32) ||
			session.State != "" || session.CodeVerifier != "") {
		return ErrOpenAIOAuthSessionInvalid
	}
	if session.ReauthorizationCredentialsHash != "" &&
		(!validOpenAIAuthBrowserLowerHex(session.ReauthorizationCredentialsHash, 64) ||
			session.ReauthorizationBrowserSessionID == "") {
		return ErrOpenAIOAuthSessionInvalid
	}
	return nil
}

func (s *OpenAIOAuthService) GenerateReauthorizationAuthURL(
	ctx context.Context, accountID int64, revision string, proxyID *int64, redirectURI, platform, accountRevision string,
) (*OpenAIAuthURLResult, error) {
	account, err := s.lookupReauthorizationAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	expected, err := time.Parse(time.RFC3339Nano, revision)
	if err != nil || expected.IsZero() {
		return nil, ErrOAuthReauthorizationStale
	}
	currentRevision := OpenAIOAuthAccountRevision(account)
	if currentRevision == "" {
		return nil, ErrOpenAIOAuthReauthorizationUnavailable
	}
	// Old clients still use strict timestamp matching when starting a flow.
	// New clients bind the identity read from the account DTO, so background
	// health/usage updates between GET and this request do not reject it.
	if (accountRevision == "" && !account.UpdatedAt.Equal(expected)) ||
		(accountRevision != "" && accountRevision != currentRevision) {
		return nil, ErrOAuthReauthorizationStale
	}
	if proxyIDValue(proxyID) != proxyIDValue(account.ProxyID) {
		return nil, ErrOpenAIOAuthProxyMismatch
	}
	binding := &OpenAIOAuthSession{
		ReauthorizationAccountID:       account.ID,
		ReauthorizationRevision:        expected.UTC().Format(time.RFC3339Nano),
		ReauthorizationAccountRevision: currentRevision,
	}
	return s.generateAuthURL(ctx, proxyID, redirectURI, platform, binding)
}

func (s *OpenAIOAuthService) lookupReauthorizationAccount(ctx context.Context, accountID int64) (*Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accountID <= 0 {
		return nil, ErrAccountNotFound
	}
	if s.reauthorizationAccountLookup == nil {
		return nil, ErrOpenAIOAuthReauthorizationUnavailable
	}
	account, err := s.reauthorizationAccountLookup(ctx, accountID)
	if errors.Is(err, ErrAccountNotFound) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		// Repository diagnostics may contain connection credentials.
		return nil, ErrOpenAIOAuthReauthorizationUnavailable
	}
	if account == nil {
		return nil, ErrAccountNotFound
	}
	if account.ID != accountID {
		return nil, ErrOpenAIOAuthReauthorizationUnavailable
	}
	if !IsOpenAIBrowserOAuthAccount(account) {
		return nil, ErrOpenAIOAuthReauthorizationUnsupported
	}
	return account, nil
}

// Re-read the account identity and selected route at every authorization boundary.
func (s *OpenAIOAuthService) validateReauthorizationSession(ctx context.Context, session *OpenAIOAuthSession, probe bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateOpenAIOAuthReauthorizationBinding(session); err != nil {
		return err
	}
	if session.ReauthorizationAccountID == 0 {
		return nil
	}
	account, err := s.lookupReauthorizationAccount(ctx, session.ReauthorizationAccountID)
	if err != nil {
		return err
	}
	if proxyIDValue(account.ProxyID) != session.ProxyID {
		return ErrOpenAIOAuthProxyMismatch
	}
	if session.ReauthorizationAccountRevision != "" {
		if OpenAIOAuthAccountRevision(account) != session.ReauthorizationAccountRevision {
			return ErrOAuthReauthorizationStale
		}
	} else {
		// Sessions created before this revision have no safe identity snapshot.
		revision, err := time.Parse(time.RFC3339Nano, session.ReauthorizationRevision)
		if err != nil || !account.UpdatedAt.Equal(revision) {
			return ErrOAuthReauthorizationStale
		}
	}
	route, err := resolveOpenAIOAuthProxyURL(ctx, s.proxyRepo, account.ProxyID)
	if err != nil || openAIOAuthProxyRouteHash(route) != session.ProxyRouteHash {
		return ErrOpenAIOAuthProxyMismatch
	}
	return nil
}

func proxyIDValue(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}
