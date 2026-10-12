//go:build unit

package service

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TestForward_OAuthWebSearchHistoryDeclaresTool drives the Codex compaction
// shape (web_search_call history, tools:[]) through Forward and asserts the
// body reaching chatgpt.com declares web_search (#7927): top-level for the
// standard wire, input additional_tools for Responses Lite.
func TestForward_OAuthWebSearchHistoryDeclaresTool(t *testing.T) {
	for _, tc := range []struct {
		name        string
		passthrough bool
		lite        bool
	}{
		{"transform", false, false},
		{"passthrough", true, false},
		{"transform lite", false, true},
		{"passthrough lite", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			s := newAstraOAuthSetup(t, tc.passthrough)
			if tc.lite {
				s.c.Request.Header.Set(responsesLiteHeader, "true")
			}
			inner := `{"id":"resp_test","model":"gpt-5.5","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
			s.upstream.resp = &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(codexCompletedSSE(inner))),
			}
			body, err := sjson.SetRawBytes([]byte(openAIWebSearchHistoryCompactionBody), "input.-1", []byte(`{"type":"compaction_trigger"}`))
			require.NoError(t, err)

			result, err := s.svc.Forward(context.Background(), s.c, s.account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, s.upstream.lastReq)
			require.Equal(t, astraProCodexResponsesURL, s.upstream.lastReq.URL.String())

			forwarded := s.upstream.lastBody
			require.Equal(t, "web_search_call", gjson.GetBytes(forwarded, "input.1.type").String())
			require.Equal(t, "none", gjson.GetBytes(forwarded, "tool_choice").String())
			items := gjson.GetBytes(forwarded, "input").Array()
			require.Equal(t, "compaction_trigger", items[len(items)-1].Get("type").String())
			if !tc.lite {
				tools := gjson.GetBytes(forwarded, "tools").Array()
				require.Len(t, tools, 1)
				require.Equal(t, "web_search", tools[0].Get("type").String())
				return
			}
			require.False(t, gjsonToolsContainWebSearch(gjson.GetBytes(forwarded, "tools")))
			additional := items[len(items)-2]
			require.Equal(t, "additional_tools", additional.Get("type").String())
			require.Equal(t, "web_search", additional.Get("tools.0.type").String())
		})
	}
}

const astraProCodexResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

type astraForwardSetup struct {
	upstream *httpUpstreamRecorder
	svc      *OpenAIGatewayService
	c        *gin.Context
	rec      *httptest.ResponseRecorder
	account  *Account
}

// newAstraOAuthSetup builds an OpenAI OAuth account using the minimal
// svc+account harness pattern (mirrors openai_oauth_passthrough_test.go). When
// passthrough is true the account routes through forwardOpenAIPassthrough.
func newAstraOAuthSetup(t *testing.T, passthrough bool) *astraForwardSetup {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:             888,
		Name:           "oauth-astra",
		Platform:       PlatformOpenAI,
		Type:           AccountTypeOAuth,
		Concurrency:    1,
		Credentials:    map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-acc"},
		Extra:          map[string]any{"openai_passthrough": passthrough},
		Status:         StatusActive,
		Schedulable:    true,
		RateMultiplier: f64p(1),
	}
	return &astraForwardSetup{upstream: upstream, svc: svc, c: c, rec: rec, account: account}
}

func codexCompletedSSE(inner string) string {
	return "data: {\"type\":\"response.completed\",\"response\":" + inner + "}\n\ndata: [DONE]\n\n"
}
