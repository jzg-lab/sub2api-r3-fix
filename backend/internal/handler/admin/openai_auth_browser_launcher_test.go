package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestLaunchAuthBrowserReturnsProcessOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name     string
		launcher string
		status   int
		message  string
	}{
		{"successful exit", "/usr/bin/true", http.StatusOK, ""},
		{"nonzero exit", "/usr/bin/false", http.StatusInternalServerError, "exit status 1"},
		{"start failure", filepath.Join(t.TempDir(), "missing"), http.StatusInternalServerError, "start auth browser launcher"},
		{"disabled", "", http.StatusServiceUnavailable, "not configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.AuthBrowserLauncher = tc.launcher
			proxy := &service.Proxy{
				ID: 901, Status: service.StatusActive, Protocol: "socks5h",
				Host: "127.0.0.1", Port: 17913,
			}
			store := &oauthRouteSessionStore{session: &service.OpenAIOAuthSession{
				ID: "session-1", State: strings.Repeat("a", 64),
				CodeVerifier: strings.Repeat("b", 128), ProxyID: 901,
				Platform: service.PlatformOpenAI, CreatedAt: time.Now(),
				RedirectURI:    "https://chatgpt.com/api/auth/callback/login-web",
				ProxyRouteHash: fmt.Sprintf("%x", sha256.Sum256([]byte(proxy.URL()))),
			}}
			repo := &oauthRouteProxyRepo{proxy: proxy}
			handler := &OpenAIOAuthHandler{}
			handler.SetAuthBrowserLauncher(service.NewOpenAIAuthBrowserLauncher(cfg, store, repo))
			router := gin.New()
			router.POST("/admin/openai/launch-auth-browser", handler.LaunchAuthBrowser)
			request := httptest.NewRequest(http.MethodPost, "/admin/openai/launch-auth-browser",
				bytes.NewBufferString(`{"session_id":"session-1"}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			require.Equal(t, tc.status, response.Code, response.Body.String())
			var body struct {
				Message string                                 `json:"message"`
				Data    *service.OpenAIAuthBrowserLaunchResult `json:"data"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			if tc.status != http.StatusOK {
				require.Nil(t, body.Data)
				require.Contains(t, body.Message, tc.message)
				return
			}
			require.NotNil(t, body.Data)
			require.True(t, body.Data.Launched)
			require.False(t, body.Data.AlreadyRunning)
			require.Equal(t, "launcher completed", body.Data.Output)
			require.Equal(t, "http://127.0.0.1:17933", body.Data.ExitIngress)
			require.NotContains(t, response.Body.String(), strings.Repeat("a", 64))
			require.NotContains(t, response.Body.String(), `"auth_url"`)
		})
	}
}

func TestLaunchAuthBrowserTransientHTTPBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	marker := strings.Repeat("fixture-input-", 3)
	for _, tc := range []struct {
		name    string
		payload any
		raw     string
	}{
		{name: "malformed JSON", raw: `{"session_id":`},
		{name: "missing session", payload: map[string]any{"login": map[string]string{"password": marker}}},
		{name: "wrong field type", payload: map[string]any{"session_id": "one", "login": marker}},
		{name: "oversized body", payload: map[string]any{"session_id": "one", "login": map[string]string{"password": strings.Repeat(marker, 800)}}},
		{name: "invalid email", payload: map[string]any{"session_id": "one", "login": map[string]string{"email": "not-an-email", "password": marker}}},
		{name: "unbound initial login", payload: map[string]any{"session_id": "one", "login": map[string]string{"email": "fixture@example.invalid", "password": marker}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.AuthBrowserLauncher = "/usr/bin/false"
			store := &oauthRouteSessionStore{session: &service.OpenAIOAuthSession{
				ID: "one", State: strings.Repeat("a", 64),
				CodeVerifier: strings.Repeat("b", 128), CreatedAt: time.Now(),
			}}
			handler := &OpenAIOAuthHandler{}
			handler.SetAuthBrowserLauncher(service.NewOpenAIAuthBrowserLauncher(cfg, store, &oauthRouteProxyRepo{}))
			router := gin.New()
			router.POST("/admin/openai/launch-auth-browser", handler.LaunchAuthBrowser)
			payload := []byte(tc.raw)
			if tc.payload != nil {
				var err error
				payload, err = json.Marshal(tc.payload)
				require.NoError(t, err)
			}
			request := httptest.NewRequest(http.MethodPost, "/admin/openai/launch-auth-browser", bytes.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			require.NotContains(t, response.Body.String(), marker)
			require.NotContains(t, response.Body.String(), "fixture@example.invalid")
			require.Contains(t, response.Body.String(), "AUTH_BROWSER_LAUNCH_INVALID_REQUEST")
		})
	}
}

func TestLaunchAuthBrowserClassifiesPreLaunchFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	validProxy := &service.Proxy{
		ID: 901, Status: service.StatusActive, Protocol: "socks5h",
		Host: "127.0.0.1", Port: 17913,
	}
	validSession := func() *service.OpenAIOAuthSession {
		return &service.OpenAIOAuthSession{
			ID: "session-1", State: strings.Repeat("a", 64),
			CodeVerifier: strings.Repeat("b", 128), ProxyID: validProxy.ID,
			Platform: service.PlatformOpenAI, CreatedAt: time.Now(),
			RedirectURI:    "https://chatgpt.com/api/auth/callback/login-web",
			ProxyRouteHash: fmt.Sprintf("%x", sha256.Sum256([]byte(validProxy.URL()))),
		}
	}

	for _, tc := range []struct {
		name       string
		sessionID  string
		session    func() *service.OpenAIOAuthSession
		storeErr   error
		proxy      func() *service.Proxy
		proxyErr   error
		wantStatus int
		wantReason string
	}{
		{
			name: "missing session", sessionID: "missing",
			session:    func() *service.OpenAIOAuthSession { return nil },
			wantStatus: http.StatusNotFound, wantReason: "AUTH_BROWSER_SESSION_NOT_FOUND",
		},
		{
			name: "blank session", sessionID: " ", session: validSession,
			wantStatus: http.StatusBadRequest, wantReason: "AUTH_BROWSER_LAUNCH_INVALID_REQUEST",
		},
		{
			name: "expired session", sessionID: "session-1",
			session: func() *service.OpenAIOAuthSession {
				session := validSession()
				session.CreatedAt = time.Now().Add(-3 * time.Hour)
				return session
			},
			wantStatus: http.StatusGone, wantReason: "AUTH_BROWSER_SESSION_EXPIRED",
		},
		{
			name: "invalid session state", sessionID: "session-1",
			session: func() *service.OpenAIOAuthSession {
				session := validSession()
				session.State = "corrupted"
				return session
			},
			wantStatus: http.StatusConflict, wantReason: "AUTH_BROWSER_SESSION_INVALID",
		},
		{
			name: "changed proxy route", sessionID: "session-1",
			session: func() *service.OpenAIOAuthSession {
				session := validSession()
				session.ProxyRouteHash = strings.Repeat("0", 64)
				return session
			},
			wantStatus: http.StatusConflict, wantReason: "AUTH_BROWSER_PROXY_ROUTE_STALE",
		},
		{
			name: "inactive proxy", sessionID: "session-1", session: validSession,
			proxy: func() *service.Proxy {
				proxy := *validProxy
				proxy.Status = service.StatusDisabled
				return &proxy
			},
			wantStatus: http.StatusServiceUnavailable, wantReason: "AUTH_BROWSER_PROXY_UNAVAILABLE",
		},
		{
			name: "unsupported proxy ingress", sessionID: "session-1",
			session: func() *service.OpenAIOAuthSession {
				session := validSession()
				proxy := *validProxy
				proxy.Port = 18000
				proxy.Username = "u"
				proxy.Password = "p"
				session.ProxyRouteHash = fmt.Sprintf("%x", sha256.Sum256([]byte(proxy.URL())))
				return session
			},
			proxy: func() *service.Proxy {
				proxy := *validProxy
				proxy.Port = 18000
				proxy.Username = "u"
				proxy.Password = "p"
				return &proxy
			},
			wantStatus: http.StatusServiceUnavailable, wantReason: "AUTH_BROWSER_PROXY_INGRESS_UNAVAILABLE",
		},
		{
			name: "preparation timeout", sessionID: "session-1", session: validSession,
			storeErr:   context.DeadlineExceeded,
			wantStatus: http.StatusGatewayTimeout, wantReason: "AUTH_BROWSER_LAUNCH_TIMEOUT",
		},
		{
			name: "store failure", sessionID: "session-1", session: validSession,
			storeErr:   errors.New("injected store failure"),
			wantStatus: http.StatusInternalServerError, wantReason: "AUTH_BROWSER_LAUNCH_FAILED",
		},
		{
			name: "damaged stored session", sessionID: "session-1", session: validSession,
			storeErr:   fmt.Errorf("%w: proxy is invalid", service.ErrOpenAIOAuthSessionInvalid),
			wantStatus: http.StatusConflict, wantReason: "AUTH_BROWSER_SESSION_INVALID",
		},
		{
			name: "proxy store failure", sessionID: "session-1", session: validSession,
			proxyErr:   errors.New("injected proxy store failure"),
			wantStatus: http.StatusServiceUnavailable, wantReason: "AUTH_BROWSER_PROXY_UNAVAILABLE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.AuthBrowserLauncher = "/usr/bin/true"
			proxy := validProxy
			if tc.proxy != nil {
				proxy = tc.proxy()
			}
			handler := &OpenAIOAuthHandler{}
			handler.SetAuthBrowserLauncher(service.NewOpenAIAuthBrowserLauncher(
				cfg,
				&oauthRouteSessionStore{session: tc.session(), err: tc.storeErr},
				&oauthRouteProxyRepo{proxy: proxy, err: tc.proxyErr},
			))
			router := gin.New()
			router.POST("/admin/openai/launch-auth-browser", handler.LaunchAuthBrowser)
			payload, err := json.Marshal(OpenAILaunchAuthBrowserRequest{SessionID: tc.sessionID})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/admin/openai/launch-auth-browser",
				bytes.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			require.Equal(t, tc.wantStatus, response.Code, response.Body.String())
			var body struct {
				Reason string `json:"reason"`
				Data   any    `json:"data"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			require.Equal(t, tc.wantReason, body.Reason)
			require.Nil(t, body.Data)
			require.NotContains(t, response.Body.String(), `"auth_url"`)
		})
	}
}
