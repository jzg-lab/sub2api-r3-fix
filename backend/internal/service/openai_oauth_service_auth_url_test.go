package service

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientAuthURLStub struct{}

func (s *openaiOAuthClientAuthURLStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientAuthURLStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientAuthURLStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func TestOpenAIOAuthService_GenerateAuthURL_OpenAIKeepsCodexFlow(t *testing.T) {
	proxyID := int64(7)
	proxyRepo := &mockProxyRepoForOAuth{
		getByIDFunc: func(context.Context, int64) (*Proxy, error) {
			return &Proxy{ID: proxyID, Status: StatusActive, Protocol: "http", Host: "127.0.0.1", Port: 8080}, nil
		},
	}
	svc := NewOpenAIOAuthService(proxyRepo, &openaiOAuthClientAuthURLStub{})
	svc.SetSessionStore(newTestOpenAIOAuthSessionStore())
	defer svc.Stop()

	result, err := svc.GenerateAuthURL(context.Background(), &proxyID, "", PlatformOpenAI)
	require.NoError(t, err)
	require.NotEmpty(t, result.AuthURL)
	require.NotEmpty(t, result.SessionID)
	require.Equal(t, proxyID, result.ProxyID)

	parsed, err := url.Parse(result.AuthURL)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, openai.ClientID, q.Get("client_id"))
	require.Equal(t, "true", q.Get("codex_cli_simplified_flow"))

	session, err := svc.sessionStore.Get(context.Background(), result.SessionID)
	require.NoError(t, err)
	require.Equal(t, openai.ClientID, session.ClientID)
	require.Equal(t, openAIOAuthProxyRouteHash("http://127.0.0.1:8080"), session.ProxyRouteHash)
}
