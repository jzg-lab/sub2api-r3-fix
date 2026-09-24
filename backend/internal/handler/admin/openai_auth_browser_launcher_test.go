package admin

import (
	"bytes"
	"encoding/json"
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
			store := &oauthRouteSessionStore{session: &service.OpenAIOAuthSession{
				ID: "session-1", State: strings.Repeat("a", 64),
				CodeVerifier: "test-verifier", ProxyID: 901,
				Platform: service.PlatformOpenAI, CreatedAt: time.Now(),
				RedirectURI: "https://chatgpt.com/api/auth/callback/login-web",
			}}
			repo := &oauthRouteProxyRepo{proxy: &service.Proxy{
				ID: 901, Status: service.StatusActive, Protocol: "socks5h",
				Host: "127.0.0.1", Port: 17913,
			}}
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
			require.Equal(t, "http://127.0.0.1:17923", body.Data.ExitIngress)
		})
	}
}
