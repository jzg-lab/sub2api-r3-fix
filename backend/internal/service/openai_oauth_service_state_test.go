package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientStateStub struct {
	exchangeCalled int32
	lastClientID   string
}

func (s *openaiOAuthClientStateStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	atomic.AddInt32(&s.exchangeCalled, 1)
	s.lastClientID = clientID
	return &openai.TokenResponse{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresIn:    3600,
	}, nil
}

func (s *openaiOAuthClientStateStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientStateStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	return s.RefreshToken(ctx, refreshToken, proxyURL)
}

func TestOpenAIOAuthService_ExchangeCode_StateRequired(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	proxyID := int64(7)
	svc := NewOpenAIOAuthService(&mockProxyRepoForOAuth{
		getByIDFunc: func(context.Context, int64) (*Proxy, error) {
			return &Proxy{ID: proxyID, Status: StatusActive, Protocol: "http", Host: "127.0.0.1", Port: 8080}, nil
		},
	}, client)
	store := newTestOpenAIOAuthSessionStore()
	svc.SetSessionStore(store)
	defer svc.Stop()

	_ = store.Create(context.Background(), &OpenAIOAuthSession{
		ID:             "sid",
		State:          "expected-state",
		CodeVerifier:   "verifier",
		ClientID:       openai.ClientID,
		RedirectURI:    openai.DefaultRedirectURI,
		ProxyID:        proxyID,
		ProxyRouteHash: openAIOAuthProxyRouteHash("http://127.0.0.1:8080"),
		CreatedAt:      time.Now(),
	})

	_, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "oauth state is required")
	require.Equal(t, int32(0), atomic.LoadInt32(&client.exchangeCalled))
}

func TestOpenAIOAuthService_ExchangeCode_StateMismatch(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	proxyID := int64(7)
	svc := NewOpenAIOAuthService(&mockProxyRepoForOAuth{
		getByIDFunc: func(context.Context, int64) (*Proxy, error) {
			return &Proxy{ID: proxyID, Status: StatusActive, Protocol: "http", Host: "127.0.0.1", Port: 8080}, nil
		},
	}, client)
	store := newTestOpenAIOAuthSessionStore()
	svc.SetSessionStore(store)
	defer svc.Stop()

	_ = store.Create(context.Background(), &OpenAIOAuthSession{
		ID:             "sid",
		State:          "expected-state",
		CodeVerifier:   "verifier",
		ClientID:       openai.ClientID,
		RedirectURI:    openai.DefaultRedirectURI,
		ProxyID:        proxyID,
		ProxyRouteHash: openAIOAuthProxyRouteHash("http://127.0.0.1:8080"),
		CreatedAt:      time.Now(),
	})

	_, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
		State:     "wrong-state",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid oauth state")
	require.Equal(t, int32(0), atomic.LoadInt32(&client.exchangeCalled))
}

func TestOpenAIOAuthService_ExchangeCode_StateMatch(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	proxyID := int64(7)
	svc := NewOpenAIOAuthService(&mockProxyRepoForOAuth{
		getByIDFunc: func(context.Context, int64) (*Proxy, error) {
			return &Proxy{ID: proxyID, Status: StatusActive, Protocol: "http", Host: "127.0.0.1", Port: 8080}, nil
		},
	}, client)
	store := newTestOpenAIOAuthSessionStore()
	svc.SetSessionStore(store)
	defer svc.Stop()

	_ = store.Create(context.Background(), &OpenAIOAuthSession{
		ID:             "sid",
		State:          "expected-state",
		CodeVerifier:   "verifier",
		ClientID:       openai.ClientID,
		RedirectURI:    openai.DefaultRedirectURI,
		ProxyID:        proxyID,
		ProxyRouteHash: openAIOAuthProxyRouteHash("http://127.0.0.1:8080"),
		CreatedAt:      time.Now(),
	})

	info, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
		State:     "expected-state",
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, proxyID, info.ProxyID)
	require.Equal(t, "at", info.AccessToken)
	require.Equal(t, openai.ClientID, info.ClientID)
	require.Equal(t, openai.ClientID, client.lastClientID)
	require.Equal(t, int32(1), atomic.LoadInt32(&client.exchangeCalled))

	_, err = svc.sessionStore.Get(context.Background(), "sid")
	require.Error(t, err)
}
