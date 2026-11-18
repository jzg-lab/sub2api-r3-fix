//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	capacityBudgetAPIKeyID = int64(912)
)

type machineFailoverUpstream struct {
	service.HTTPUpstream
	mu       sync.Mutex
	hits     []int64
	headers  []http.Header
	bodies   [][]byte
	failByID map[int64]int
	failBody string
}

func (u *machineFailoverUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	u.mu.Lock()
	u.hits = append(u.hits, accountID)
	u.headers = append(u.headers, req.Header.Clone())
	u.bodies = append(u.bodies, append([]byte(nil), body...))
	status := u.failByID[accountID]
	u.mu.Unlock()
	if status > 0 {
		failureBody := u.failBody
		if failureBody == "" {
			failureBody = `{"error":{"message":"upstream unavailable"}}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(failureBody)),
		}, nil
	}
	if bytes.Contains(body, []byte(`"stream":true`)) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(bytes.NewBufferString(
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_machine_failover\",\"model\":\"gpt-5.2\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
			)),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(
			`{"id":"resp_machine_failover","object":"response","model":"gpt-5.2","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
		)),
	}, nil
}

func (u *machineFailoverUpstream) snapshot() ([]int64, []http.Header, [][]byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.hits...), append([]http.Header(nil), u.headers...), append([][]byte(nil), u.bodies...)
}

func (u *machineFailoverUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func newCodexMachineFailoverHandler(t *testing.T) (*machineFailoverUpstream, *gin.Engine, func()) {
	t.Helper()
	groupID := int64(911)
	expires := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	proxyID := int64(55)
	proxySnapshot := &service.Proxy{
		ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 18080, Status: service.StatusActive,
	}
	accounts := []service.Account{
		{
			ID: 811, Name: "codex-machine", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1,
			ProxyID: &proxyID, Proxy: proxySnapshot,
			Credentials: map[string]any{"access_token": "machine-access", "chatgpt_account_id": "chatgpt-machine", "expires_at": expires},
			Extra: map[string]any{
				"codex_fingerprint_mode": "session",
				"codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111",
			},
		},
		{
			ID: 812, Name: "codex-off", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 2,
			ProxyID: &proxyID, Proxy: proxySnapshot,
			Credentials: map[string]any{"access_token": "off-access", "chatgpt_account_id": "chatgpt-off", "expires_at": expires},
			Extra:       map[string]any{"codex_fingerprint_mode": "off"},
		},
	}
	repo := &grokCredentialHandlerRepo{accounts: accounts, missingOnGet: map[int64]bool{}}
	upstream := &machineFailoverUpstream{failByID: map[int64]int{811: http.StatusInternalServerError}}

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.MaxAccountSwitches = 3
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gateway := service.NewOpenAIGatewayService(
		repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCache, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(cache), billingCache, &service.APIKeyService{}, nil, nil, nil, nil, cfg)
	apiKey := &service.APIKey{
		ID: capacityBudgetAPIKeyID, GroupID: &groupID,
		User:  &service.User{ID: 913, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.POST("/openai/v1/responses", h.Responses)
	router.POST("/openai/v1/chat/completions", h.ChatCompletions)
	return upstream, router, func() { billingCache.Stop() }
}

func TestCapacityBudgetThroughRealHTTPHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name  string
		route string
		body  string
	}{
		{"responses_json", "/openai/v1/responses", `{"model":"gpt-5.2","stream":false,"input":"local test"}`},
		{"responses_sse", "/openai/v1/responses", `{"model":"gpt-5.2","stream":true,"input":"local test"}`},
		{"chat_json", "/openai/v1/chat/completions", `{"model":"gpt-5.2","stream":false,"messages":[{"role":"user","content":"local test"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, router, cleanup := newCodexMachineFailoverHandler(t)
			defer cleanup()
			upstream.failByID = map[int64]int{811: 503, 812: 503}
			upstream.failBody = `{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded."}}`
			req := httptest.NewRequest(http.MethodPost, tc.route, bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			hits, _, _ := upstream.snapshot()
			require.Len(t, hits, 3, "capacity request must stop after three upstream failures")
			require.Equal(t, []int64{811, 811, 812}, hits)
			require.Contains(t, recorder.Body.String(), `"code":"server_is_overloaded"`)
			if recorder.Code == http.StatusOK {
				require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.failed"))
			} else {
				require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
			}
		})
	}
}

func (u *machineFailoverUpstream) DoProbeWithTLS(req *http.Request, proxyURL string, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.DoWithTLS(req, proxyURL, 0, accountConcurrency, profile)
}
