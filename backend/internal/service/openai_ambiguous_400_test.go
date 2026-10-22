package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const ambiguous400Body = `{"error":{"type":"upstream_error","message":"Upstream request failed"}}`

func TestOpenAIAmbiguous400Classification(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"generic", ambiguous400Body, true},
		{"message only", `{"error":{"message":"Upstream request failed"}}`, true},
		{"plain text", `Upstream request failed`, true},
		{"schema", `{"error":{"message":"Upstream request failed","code":"invalid_function_parameters"}}`, false},
		{"parameter", `{"error":{"message":"Upstream request failed","param":"tools[0].parameters"}}`, false},
		{"invalid request", `{"error":{"message":"Upstream request failed","type":"invalid_request_error"}}`, false},
		{"context", `{"error":{"message":"Upstream request failed","code":"context_length_exceeded"}}`, false},
		{"policy", `{"error":{"message":"Upstream request failed","code":"cyber_policy"}}`, false},
		{"conflicting code", `{"code":"unsupported_parameter","error":{"message":"Upstream request failed"}}`, false},
		{"conflicting detail", `{"error":{"message":"Upstream request failed","detail":"unsupported tool"}}`, false},
		{"inner error", `{"error":{"message":"Upstream request failed","inner_error":{"code":"content_policy_violation"}}}`, false},
		{"echoed message", `{"error":{"message":"Invalid schema"},"input":"Upstream request failed"}`, false},
		{"unknown", `{"error":{"message":"Bad Request"}}`, false},
		{"empty", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isOpenAIAmbiguous400(account, http.StatusBadRequest, []byte(tc.body)))
		})
	}
	for _, account := range []*Account{nil, {Platform: PlatformOpenAI, Type: AccountTypeOAuth}, {Platform: PlatformGrok, Type: AccountTypeAPIKey}} {
		require.False(t, isOpenAIAmbiguous400(account, 400, []byte(ambiguous400Body)))
	}
	require.False(t, isOpenAIAmbiguous400(account, 502, []byte(ambiguous400Body)))
}

func TestOpenAIAmbiguous400KeepsAdministratorRules(t *testing.T) {
	for _, passthroughRule := range []bool{false, true} {
		c, _ := newOpenAIUpstreamErrorTestContext(t)
		InitOpenAIAmbiguous400Recovery(c, []byte(`{"model":"gpt-5.1"}`))
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
		if passthroughRule {
			rules := &ErrorPassthroughService{}
			rules.setLocalCache([]*model.ErrorPassthroughRule{newNonFailoverPassthroughRule(400, "Upstream request failed", 400, "custom")})
			BindErrorPassthroughService(c, rules)
		} else {
			account.Credentials = map[string]any{"custom_error_codes_enabled": true, "custom_error_codes": []any{float64(429)}}
		}
		require.Nil(t, (&OpenAIGatewayService{}).openAIAmbiguous400Failure(c, account, &http.Response{StatusCode: 400}, []byte(ambiguous400Body), false))
	}
}

func TestOpenAI400DiagnosticDetailDoesNotCaptureRequestContent(t *testing.T) {
	body := []byte(`{"error":{"code":"invalid_function_parameters","type":"invalid_request_error","param":"tools[0].parameters","message":"prompt contains private-text"},"request":{"input":"private-text"}}`)
	detail := openAI400DiagnosticDetail(body, "no_eligible_failover")
	require.Equal(t, "invalid_function_parameters", gjson.Get(detail, "code").String())
	require.Equal(t, "tools[0].parameters", gjson.Get(detail, "param").String())
	require.NotContains(t, detail, "private-text")
}

func TestOpenAIAmbiguous400ReplayBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body          string
		committed, canceled bool
	}{
		{"previous response", `{"previous_response_id":"resp_original"}`, false, false},
		{"conversation", `{"conversation":"conv_original"}`, false, false},
		{"output", `{}`, true, false},
		{"canceled", `{}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			InitOpenAIAmbiguous400Recovery(c, []byte(tc.body))
			if tc.committed {
				MarkResponseCommitted(c)
			}
			if tc.canceled {
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			}
			svc := &OpenAIGatewayService{}
			err := svc.openAIAmbiguous400Failure(c, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, &http.Response{StatusCode: 400}, []byte(ambiguous400Body), false)
			require.Nil(t, err)
			require.False(t, OpenAIAmbiguous400RecoveryUsed(c, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}))
		})
	}
}

func TestOpenAIAmbiguous400ForwardDoesNotCommitOrPenalize(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "passthrough"}[passthrough], func(t *testing.T) {
			c, rec := newOpenAIUpstreamErrorTestContext(t)
			upstream := &httpUpstreamRecorder{resp: newOpenAIUpstreamErrorResponse(400, ambiguous400Body)}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := openAIFailoverCachedBodyTestAccount(1, "test", nil)
			account.Type = AccountTypeAPIKey
			account.Credentials["api_key"] = "test"
			account.Extra = map[string]any{"openai_passthrough": passthrough}
			_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.1","input":"hi","stream":false}`))
			var failure *UpstreamFailoverError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, OpenAIAmbiguous400Reason, failure.Reason)
			require.False(t, failure.RetryableOnSameAccount)
			require.False(t, failure.ShouldReportAccountScheduleFailure())
			require.False(t, accountAttemptFailure(err))
			require.False(t, IsResponseCommitted(c))
			require.Empty(t, rec.Body.String())
			require.False(t, OpenAIAmbiguous400RecoveryExhausted(c, account, failure))
			require.True(t, OpenAIAmbiguous400RecoveryExhausted(c.Copy(), account, &UpstreamFailoverError{StatusCode: 502}), "a different failure on a copied context must not reset the budget")
		})
	}
}

func TestOpenAIAmbiguous400WSHTTPBridgeFirstTurnOnly(t *testing.T) {
	for _, turn := range []int{1, 2} {
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(ambiguous400Body))}}
		svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
		account := openAIFailoverCachedBodyTestAccount(1, "test", nil)
		account.Type = AccountTypeAPIKey
		account.Credentials["api_key"] = "test"
		c, _ := newOpenAIUpstreamErrorTestContext(t)
		payload := []byte(`{"type":"response.create","model":"gpt-5.1","input":"hi"}`)
		writes := 0
		_, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "sk-test", payload, len(payload), "gpt-5.1", "", "", "", "", turn, func([]byte) error { writes++; return nil })
		var failure *UpstreamFailoverError
		require.Equal(t, turn == 1, errors.As(err, &failure))
		if turn == 1 {
			require.Zero(t, writes)
		} else {
			require.Equal(t, 1, writes)
		}
	}
}

func TestOpenAIAmbiguous400MixedRouteKeepsOAuthRecovery(t *testing.T) {
	for _, transport := range []string{"http", "passthrough", "ws_http_bridge"} {
		t.Run(transport, func(t *testing.T) {
			c, _ := newOpenAIUpstreamErrorTestContext(t)
			body := []byte(`{"model":"gpt-5.1","input":"hello","truncation":"auto","stream":false}`)
			rejection := `{"error":{"code":"unsupported_parameter","param":"truncation","message":"Unsupported parameter: truncation."}}`
			removedField := "truncation"
			if transport == "http" {
				body = []byte(`{"model":"gpt-5.1","input":[{"type":"reasoning","encrypted_content":"test-encrypted","summary":[{"type":"summary_text","text":"keep me"}]},{"role":"user","content":"hello"}],"stream":false}`)
				rejection = `{"error":{"code":"invalid_encrypted_content","message":"bad encrypted content"}}`
				removedField = "input.0.encrypted_content"
			} else if transport == "passthrough" {
				body = []byte(`{"model":"gpt-5.1","input":"hello","prompt_cache_options":{"enabled":true},"stream":false}`)
				rejection = `{"error":{"code":"unsupported_parameter","param":"prompt_cache_options","message":"Unsupported parameter: prompt_cache_options."}}`
				removedField = "prompt_cache_options"
			}
			InitOpenAIAmbiguous400Recovery(c, body)
			apiKey := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			require.False(t, OpenAIAmbiguous400RecoveryExhausted(c, apiKey, &UpstreamFailoverError{Reason: OpenAIAmbiguous400Reason}))
			oauth := openAIFailoverCachedBodyTestAccount(2, "oauth", nil)
			oauth.Extra = map[string]any{"openai_oauth_passthrough": transport == "passthrough"}
			require.False(t, OpenAIAmbiguous400RecoveryUsed(c, oauth))
			require.False(t, OpenAIAmbiguous400RecoveryExhausted(c, oauth, &UpstreamFailoverError{StatusCode: 429}))
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				newOpenAIUpstreamErrorResponse(400, rejection),
				newOpenAIUpstreamErrorResponse(400, ambiguous400Body),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			var err error
			if transport == "ws_http_bridge" {
				_, err = svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, oauth, "test", body, len(body), "gpt-5.1", "", "", "", "", 1, func([]byte) error { return nil })
			} else {
				_, err = svc.Forward(context.Background(), c, oauth, body)
			}
			require.Error(t, err)
			var failure *UpstreamFailoverError
			require.False(t, errors.As(err, &failure), "OAuth generic 400 must not enter the new failover")
			require.Len(t, upstream.requests, 2, "OAuth retains its existing rejected-field retry")
			require.True(t, gjson.GetBytes(upstream.bodies[0], removedField).Exists())
			require.False(t, gjson.GetBytes(upstream.bodies[1], removedField).Exists())
			require.True(t, OpenAIAmbiguous400RecoveryExhausted(c, apiKey, &UpstreamFailoverError{StatusCode: 502}), "OAuth must not reset the API-key recovery budget")
		})
	}
}
