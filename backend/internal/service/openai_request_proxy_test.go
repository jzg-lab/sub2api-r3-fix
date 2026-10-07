//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func browserOAuthRouteFixture() *Account {
	id := int64(17)
	return &Account{
		ID: 71, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 2,
		ProxyID: &id,
		Proxy:   &Proxy{ID: id, Protocol: "http", Host: "127.0.0.1", Port: 8080, Status: StatusActive},
	}
}

func TestOpenAIRequestProxyRejectsBeforeTransport(t *testing.T) {
	for _, mode := range []string{"missing_assignment", "missing_snapshot", "wrong_id", "expired", "inactive", "invalid", "direct", "other_route"} {
		t.Run(mode, func(t *testing.T) {
			account := browserOAuthRouteFixture()
			route := account.Proxy.URL()
			switch mode {
			case "missing_assignment":
				account.ProxyID = nil
			case "missing_snapshot":
				account.Proxy = nil
			case "wrong_id":
				account.Proxy.ID++
			case "expired":
				past := time.Now().Add(-time.Second)
				account.Proxy.ExpiresAt = &past
			case "inactive":
				account.Proxy.Status = StatusDisabled
			case "invalid":
				account.Proxy.Port = 0
			case "direct":
				route = ""
			case "other_route":
				route = "http://127.0.0.1:8081"
			}
			upstream := &httpUpstreamRecorder{}
			gateway := &OpenAIGatewayService{httpUpstream: upstream}
			request := httptest.NewRequest(http.MethodPost, "https://example.test/v1/responses", nil)
			response, err := gateway.doOpenAIUpstream(request.Context(), request, route, account)
			require.Error(t, err)
			require.Nil(t, response)
			tester := &AccountTestService{httpUpstream: upstream}
			response, err = tester.doOpenAIAccountTestUpstream(request, route, account, false)
			require.Error(t, err)
			require.Nil(t, response)
			require.Empty(t, upstream.requests)

			pool := newOpenAIWSConnPool(&config.Config{})
			defer pool.Close()
			dialer := &openAIWSCountingDialer{}
			pool.setClientDialerForTest(dialer)
			req := openAIWSAcquireRequest{Account: account, WSURL: "wss://example.test", ProxyURL: route}
			lease, err := pool.Acquire(context.Background(), req)
			require.Error(t, err)
			require.Nil(t, lease)
			conn, err := pool.dialConn(context.Background(), req)
			require.Error(t, err)
			require.Nil(t, conn)
			require.Zero(t, dialer.dialCount)
		})
	}
}

func TestOpenAIRequestProxyAcceptsAssignedRouteAndNonBrowserTraffic(t *testing.T) {
	account := browserOAuthRouteFixture()
	parentID := int64(99)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK}}
	gateway := &OpenAIGatewayService{httpUpstream: upstream}
	request := httptest.NewRequest(http.MethodPost, "https://example.test/v1/responses", nil)
	_, err := gateway.doOpenAIUpstream(request.Context(), request, account.Proxy.URL(), account)
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, account.Proxy.URL(), upstream.lastProxyURL)
	for _, other := range []*Account{
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
		{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken}},
		{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity}},
		{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID},
	} {
		require.NoError(t, validateOpenAIAccountProxyRoute(other, ""))
	}
}

func TestOpenAIRequestProxyPoolCannotReuseAnotherRoute(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 2
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	dialer := &openAIWSCountingDialer{}
	pool.setClientDialerForTest(dialer)
	account := browserOAuthRouteFixture()
	req := openAIWSAcquireRequest{Account: account, WSURL: "wss://example.test", ProxyURL: account.Proxy.URL()}
	first, err := pool.Acquire(context.Background(), req)
	require.NoError(t, err)
	firstID := first.ConnID()
	first.Release()

	// A new snapshot may retain the database ID but have a different endpoint.
	updated := *account
	proxy := *account.Proxy
	proxy.Port++
	updated.Proxy = &proxy
	req.Account, req.ProxyURL = &updated, proxy.URL()
	second, err := pool.Acquire(context.Background(), req)
	require.NoError(t, err)
	require.NotEqual(t, firstID, second.ConnID())
	second.Release()
	require.Equal(t, 2, dialer.dialCount)
	past := time.Now().Add(-time.Second)
	proxy.ExpiresAt = &past
	_, err = pool.Acquire(context.Background(), req)
	require.ErrorIs(t, err, errOpenAIOAuthProxyUnavailable)
	require.Equal(t, 2, dialer.dialCount)
}
