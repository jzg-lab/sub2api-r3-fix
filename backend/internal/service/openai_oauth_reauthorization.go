package service

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// This is historical login evidence, not the proxy's most recent test result.
// Generic account edits must never establish or replace this value.
const OpenAIOAuthLoginExitIPExtraKey = "openai_oauth_login_exit_ip"

var (
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
	if err := validateOpenAIInitialLoginBinding(session); err != nil {
		return err
	}
	if session.ReauthorizationAccountID == 0 {
		if session.ReauthorizationRevision != "" || session.ReauthorizationExitIP != "" ||
			session.ReauthorizationCredentialsHash != "" || session.ReauthorizationBrowserSessionID != "" {
			return ErrOpenAIOAuthSessionInvalid
		}
		return nil
	}
	revision, err := time.Parse(time.RFC3339Nano, session.ReauthorizationRevision)
	if err != nil || revision.IsZero() {
		return ErrOpenAIOAuthSessionInvalid
	}
	ip, err := normalizeOpenAIOAuthLoginIP(session.ReauthorizationExitIP)
	if err != nil || ip != session.ReauthorizationExitIP {
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
	ctx context.Context, accountID int64, revision string, proxyID *int64, redirectURI, platform string,
) (*OpenAIAuthURLResult, error) {
	if accountID <= 0 || s.reauthorizationAccountLookup == nil {
		return nil, ErrOAuthReauthorizationStale
	}
	account, err := s.reauthorizationAccountLookup(ctx, accountID)
	if err != nil || account == nil || account.ID != accountID || !IsOpenAIBrowserOAuthAccount(account) {
		return nil, ErrOAuthReauthorizationStale
	}
	expected, err := time.Parse(time.RFC3339Nano, revision)
	if err != nil || !account.UpdatedAt.Equal(expected) {
		return nil, ErrOAuthReauthorizationStale
	}
	if proxyID == nil || account.ProxyID == nil || *proxyID != *account.ProxyID {
		return nil, ErrOpenAIOAuthProxyMismatch
	}
	if qualified, exists := OpenAIOAuthQualifiedProxyID(account.Extra); exists && qualified != *proxyID {
		return nil, ErrOpenAIOAuthProxyMismatch
	}
	if _, present := account.Extra[OpenAIOAuthQualifiedProxyExtraKey]; present {
		if _, valid := OpenAIOAuthQualifiedProxyID(account.Extra); !valid {
			return nil, ErrOpenAIOAuthProxyBindingCorrupt
		}
	}
	ip, err := OpenAIOAuthLoginExitIP(account)
	if err != nil {
		return nil, err
	}
	binding := &OpenAIOAuthSession{
		ReauthorizationAccountID: account.ID,
		ReauthorizationRevision:  account.UpdatedAt.UTC().Format(time.RFC3339Nano),
		ReauthorizationExitIP:    ip,
	}
	return s.generateAuthURL(ctx, proxyID, redirectURI, platform, binding)
}

// Re-read account and route on every boundary. A proxy id alone does not pin
// its public IP; probe through that exact route without environment fallback.
func (s *OpenAIOAuthService) validateReauthorizationSession(ctx context.Context, session *OpenAIOAuthSession, probe bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateOpenAIOAuthReauthorizationBinding(session); err != nil {
		return err
	}
	if session.ReauthorizationAccountID == 0 {
		return s.validateInitialLoginSession(ctx, session, probe)
	}
	if s.reauthorizationAccountLookup == nil {
		return ErrOAuthReauthorizationStale
	}
	account, err := s.reauthorizationAccountLookup(ctx, session.ReauthorizationAccountID)
	if err != nil || account == nil || account.ID != session.ReauthorizationAccountID ||
		!IsOpenAIBrowserOAuthAccount(account) || account.ProxyID == nil || *account.ProxyID != session.ProxyID {
		return ErrOAuthReauthorizationStale
	}
	revision, err := time.Parse(time.RFC3339Nano, session.ReauthorizationRevision)
	if err != nil || !account.UpdatedAt.Equal(revision) {
		return ErrOAuthReauthorizationStale
	}
	if _, exists := account.Extra[OpenAIOAuthQualifiedProxyExtraKey]; exists {
		qualified, valid := OpenAIOAuthQualifiedProxyID(account.Extra)
		if !valid {
			return ErrOpenAIOAuthProxyBindingCorrupt
		}
		if qualified != session.ProxyID {
			return ErrOpenAIOAuthProxyMismatch
		}
	}
	expected, err := OpenAIOAuthLoginExitIP(account)
	if err != nil {
		return err
	}
	if expected != session.ReauthorizationExitIP {
		return ErrOpenAIOAuthLoginIPChanged
	}
	route, err := resolveOpenAIOAuthProxyURL(ctx, s.proxyRepo, account.ProxyID)
	if err != nil || openAIOAuthProxyRouteHash(route) != session.ProxyRouteHash {
		return ErrOpenAIOAuthProxyMismatch
	}
	if err := s.validateFixedReauthorizationEgress(session, route); err != nil {
		return err
	}
	if !probe {
		return nil
	}
	observe := s.observeReauthorizationExitIP
	if observe == nil {
		observe = observeOpenAIOAuthExitIP
	}
	actual, err := observe(ctx, route)
	if err != nil {
		return ErrOpenAIOAuthLoginIPUnavailable
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(actual))
	if err != nil || ip.Zone() != "" || ip.Unmap().String() != expected {
		return ErrOpenAIOAuthLoginIPChanged
	}
	return nil
}

func observeOpenAIOAuthExitIP(ctx context.Context, route string) (string, error) {
	if err := validateOpenAIOAuthProxyURL(route); err != nil {
		return "", ErrOpenAIOAuthLoginIPUnavailable
	}
	proxyURL, err := url.Parse(route)
	if err != nil {
		return "", ErrOpenAIOAuthLoginIPUnavailable
	}
	transport := &http.Transport{
		Proxy:               http.ProxyURL(proxyURL),
		DialContext:         (&net.Dialer{Timeout: 6 * time.Second}).DialContext,
		TLSHandshakeTimeout: 6 * time.Second,
		DisableKeepAlives:   true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		Timeout:       12 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", ErrOpenAIOAuthLoginIPUnavailable
	}
	response, err := client.Do(request)
	if err != nil {
		// Transport errors can contain the authenticated proxy URL.
		return "", ErrOpenAIOAuthLoginIPUnavailable
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 65))
	if err != nil || response.StatusCode != http.StatusOK || len(body) > 64 {
		return "", ErrOpenAIOAuthLoginIPUnavailable
	}
	return strings.TrimSpace(string(body)), nil
}
