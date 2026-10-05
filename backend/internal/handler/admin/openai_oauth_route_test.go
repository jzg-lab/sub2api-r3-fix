package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type oauthRouteProxyRepo struct {
	service.ProxyRepository
	proxy *service.Proxy
	err   error
}

func (r *oauthRouteProxyRepo) GetByID(context.Context, int64) (*service.Proxy, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.proxy != nil {
		copy := *r.proxy
		return &copy, nil
	}
	return &service.Proxy{ID: 7, Status: service.StatusActive, Protocol: "http", Host: "127.0.0.1", Port: 8080}, nil
}

type oauthRouteSessionStore struct {
	session *service.OpenAIOAuthSession
	err     error
}

func (s *oauthRouteSessionStore) Create(_ context.Context, session *service.OpenAIOAuthSession) error {
	s.session = session
	return nil
}

func (s *oauthRouteSessionStore) Get(context.Context, string) (*service.OpenAIOAuthSession, error) {
	if s.err != nil || s.session == nil {
		return nil, s.err
	}
	copy := *s.session
	return &copy, nil
}

func (s *oauthRouteSessionStore) Consume(ctx context.Context, id string) (*service.OpenAIOAuthSession, error) {
	return s.Get(ctx, id)
}

type oauthRouteClient struct {
	service.OpenAIOAuthClient
	route         string
	afterExchange func()
}

func (c *oauthRouteClient) ExchangeCode(_ context.Context, _, _, _, route, _ string) (*openai.TokenResponse, error) {
	c.route = route
	if c.afterExchange != nil {
		c.afterExchange()
	}
	return &openai.TokenResponse{ExpiresIn: 3600}, nil
}

func TestOpenAIOAuthHandlersRejectRouteChangeDuringExchange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"exchange-code", "create-from-oauth"} {
		t.Run(endpoint, func(t *testing.T) {
			proxy := &service.Proxy{ID: 7, Status: service.StatusActive, Protocol: "http", Host: "127.0.0.1", Port: 8080}
			store := &oauthRouteSessionStore{}
			client := &oauthRouteClient{afterExchange: func() { proxy.Port++ }}
			oauth := service.NewOpenAIOAuthService(&oauthRouteProxyRepo{proxy: proxy}, client)
			oauth.SetSessionStore(store)
			t.Cleanup(oauth.Stop)
			result, err := oauth.GenerateAuthURL(t.Context(), &proxy.ID, "", service.PlatformOpenAI)
			require.NoError(t, err)
			body, err := json.Marshal(map[string]any{
				"session_id": result.SessionID, "state": store.session.State, "code": "fixture",
			})
			require.NoError(t, err)
			admin := newStubAdminService()
			handler := NewOpenAIOAuthHandler(oauth, admin, nil, nil)
			router := gin.New()
			router.POST("/admin/openai/exchange-code", handler.ExchangeCode)
			router.POST("/admin/openai/create-from-oauth", handler.CreateAccountFromOAuth)
			request := httptest.NewRequest(http.MethodPost, "/admin/openai/"+endpoint, bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Empty(t, admin.createdAccounts)
			require.Equal(t, "http://127.0.0.1:8080", client.route)
		})
	}
}

func TestOpenAIOAuthGenerateRejectsMalformedReauthorizationInsteadOfCreating(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"invalid account type", `{"proxy_id":7,"account_id":"invalid"}`, http.StatusBadRequest},
		{"missing account", `{"proxy_id":7,"expected_updated_at":"2026-10-03T01:02:03Z"}`, http.StatusBadRequest},
		{"lookup unavailable", `{"proxy_id":7,"account_id":42,"expected_updated_at":"2026-10-03T01:02:03Z"}`, http.StatusServiceUnavailable},
		{"nonexistent account", `{"proxy_id":7,"account_id":0}`, http.StatusNotFound},
		{"malformed JSON", `{"proxy_id":7,`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &oauthRouteSessionStore{}
			oauth := service.NewOpenAIOAuthService(&oauthRouteProxyRepo{}, &oauthRouteClient{})
			oauth.SetSessionStore(store)
			t.Cleanup(oauth.Stop)
			handler := NewOpenAIOAuthHandler(oauth, nil, nil, nil)
			router := gin.New()
			router.POST("/admin/openai/generate-auth-url", handler.GenerateAuthURL)
			request := httptest.NewRequest(http.MethodPost, "/admin/openai/generate-auth-url", bytes.NewBufferString(tc.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, tc.status, response.Code)
			require.Nil(t, store.session, "invalid reauthorization must not create an unbound session")
		})
	}
}

func TestCreateFromOAuthUsesSessionProxyWhenRequestOmitsIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &oauthRouteSessionStore{}
	client := &oauthRouteClient{}
	oauth := service.NewOpenAIOAuthService(&oauthRouteProxyRepo{}, client)
	oauth.SetSessionStore(store)
	defer oauth.Stop()
	id := int64(7)
	result, err := oauth.GenerateAuthURL(t.Context(), &id, "", service.PlatformOpenAI)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"session_id": result.SessionID, "state": store.session.State, "code": "fixture",
	})
	require.NoError(t, err)
	admin := newStubAdminService()
	handler := NewOpenAIOAuthHandler(oauth, admin, nil, nil)
	router := gin.New()
	router.POST("/admin/openai/create-from-oauth", handler.CreateAccountFromOAuth)
	request := httptest.NewRequest(http.MethodPost, "/admin/openai/create-from-oauth", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Len(t, admin.createdAccounts, 1)
	require.NotNil(t, admin.createdAccounts[0].ProxyID)
	require.Equal(t, id, *admin.createdAccounts[0].ProxyID)
	require.Equal(t, "http://127.0.0.1:8080", client.route)
}
