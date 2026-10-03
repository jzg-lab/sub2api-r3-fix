package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

type openAIOAuthProxyNoNetworkClient struct {
	calls    int
	proxyURL string
}

func (c *openAIOAuthProxyNoNetworkClient) ExchangeCode(_ context.Context, _, _, _, proxyURL, _ string) (*openai.TokenResponse, error) {
	c.calls++
	c.proxyURL = proxyURL
	return nil, errors.New("unexpected upstream call")
}

func (c *openAIOAuthProxyNoNetworkClient) RefreshToken(context.Context, string, string) (*openai.TokenResponse, error) {
	c.calls++
	return nil, errors.New("unexpected upstream call")
}

func (c *openAIOAuthProxyNoNetworkClient) RefreshTokenWithClientID(_ context.Context, _, proxyURL, _ string) (*openai.TokenResponse, error) {
	c.calls++
	c.proxyURL = proxyURL
	return nil, errors.New("unexpected upstream call")
}

func TestOpenAIOAuthProxyFailuresDoNotReachUpstream(t *testing.T) {
	for _, failure := range []string{"repository unavailable", "deleted", "lookup failure", "disabled", "expired", "invalid endpoint"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			id := int64(7)
			var repo ProxyRepository
			if failure != "repository unavailable" {
				repo = &mockProxyRepoForOAuth{getByIDFunc: func(context.Context, int64) (*Proxy, error) {
					p := &Proxy{ID: id, Status: StatusActive, Protocol: "http", Host: "127.0.0.1", Port: 8080}
					switch failure {
					case "deleted":
						return nil, nil
					case "lookup failure":
						return nil, errors.New("private repository detail")
					case "disabled":
						p.Status = StatusDisabled
					case "expired":
						now := time.Now()
						p.ExpiresAt = &now
						p.FallbackMode = FallbackModeDirect
					case "invalid endpoint":
						p.Host = ""
					}
					return p, nil
				}}
			}
			client := &openAIOAuthProxyNoNetworkClient{}
			svc := NewOpenAIOAuthService(repo, client)
			store := newTestOpenAIOAuthSessionStore()
			svc.SetSessionStore(store)
			defer svc.Stop()
			_, err := svc.GenerateAuthURL(ctx, &id, "", PlatformOpenAI)
			require.Error(t, err)

			require.NoError(t, store.Create(ctx, &OpenAIOAuthSession{
				ID: "proxy-test-session", State: "proxy-test-state", ProxyID: id,
				CodeVerifier: "fixture", CreatedAt: time.Now(), RedirectURI: openai.DefaultRedirectURI,
			}))
			_, err = svc.ExchangeCode(ctx, &OpenAIExchangeCodeInput{
				SessionID: "proxy-test-session", State: "proxy-test-state", Code: "fixture",
			})
			require.Error(t, err)
			_, err = store.Get(ctx, "proxy-test-session")
			require.NoError(t, err, "a proxy failure must not consume the authorization session")

			account := &Account{
				ID: 101, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ProxyID: &id,
				Credentials: map[string]any{"access_token": "fixture", "refresh_token": "fixture"},
			}
			_, err = svc.RefreshAccountToken(ctx, account)
			require.Error(t, err)
			_, err = svc.RefreshTokenWithProxyID(ctx, "fixture", &id, "")
			require.Error(t, err)
			require.Zero(t, client.calls)

			privacyCalls := 0
			factory := func(string) (*req.Client, error) {
				privacyCalls++
				return nil, errors.New("unexpected client creation")
			}
			admin := &adminServiceImpl{proxyRepo: repo, privacyClientFactory: factory}
			require.Equal(t, PrivacyModeFailed, admin.EnsureOpenAIPrivacy(ctx, account))
			require.Equal(t, PrivacyModeFailed, admin.ForceOpenAIPrivacy(ctx, account))
			refresh := &TokenRefreshService{proxyRepo: repo, privacyClientFactory: factory}
			refresh.ensureOpenAIPrivacy(ctx, account)
			require.Zero(t, privacyCalls)
		})
	}
}

func TestOpenAIUnassignedOAuthRefreshesDirectly(t *testing.T) {
	client := &openAIOAuthProxyNoNetworkClient{}
	svc := NewOpenAIOAuthService(nil, client)
	account := &Account{
		ID: 101, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "fixture"},
	}
	_, err := svc.RefreshAccountToken(context.Background(), account)
	require.EqualError(t, err, "unexpected upstream call")
	require.Equal(t, 1, client.calls)
	require.Empty(t, client.proxyURL)
}

func TestOpenAIOAuthDirectAuthorizationReachesExchange(t *testing.T) {
	client := &openAIOAuthProxyNoNetworkClient{}
	svc := NewOpenAIOAuthService(nil, client)
	svc.SetSessionStore(newTestOpenAIOAuthSessionStore())
	defer svc.Stop()
	result, err := svc.GenerateAuthURL(t.Context(), nil, "", PlatformOpenAI)
	require.NoError(t, err)
	require.Zero(t, result.ProxyID)
	session, err := svc.sessionStore.Get(t.Context(), result.SessionID)
	require.NoError(t, err)
	require.Equal(t, openAIOAuthProxyRouteHash(""), session.ProxyRouteHash)
	_, err = svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
		SessionID: result.SessionID, State: session.State, Code: "fixture",
	})
	require.EqualError(t, err, "unexpected upstream call")
	require.Equal(t, 1, client.calls)
	require.Empty(t, client.proxyURL)
	_, err = svc.sessionStore.Get(t.Context(), result.SessionID)
	require.Error(t, err, "direct authorization sessions remain one-shot")
}

func TestOpenAIRawRefreshRejectsInvalidRoutesAndAllowsDirect(t *testing.T) {
	client := &openAIOAuthProxyNoNetworkClient{}
	svc := NewOpenAIOAuthService(nil, client)
	defer svc.Stop()
	for _, route := range []string{" ", "direct://localhost:8080", "http://", "http://localhost:0"} {
		_, err := svc.RefreshToken(context.Background(), "fixture", route)
		require.Error(t, err)
		_, err = svc.RefreshTokenWithClientID(context.Background(), "fixture", route, "fixture")
		require.Error(t, err)
	}
	require.Zero(t, client.calls)
	_, err := svc.RefreshTokenWithProxyID(context.Background(), "fixture", nil, "")
	require.EqualError(t, err, "unexpected upstream call")
	require.Equal(t, 1, client.calls)
}
