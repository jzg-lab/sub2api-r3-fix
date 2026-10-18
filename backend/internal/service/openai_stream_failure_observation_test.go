package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIStreamFailureObservationDoesNotRetainPayload(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"output":[{"text":"private-output"}],"usage":{"input_tokens":10,"output_tokens":2},"error":{"code":"private-code","type":"server_error","message":"response protection is unavailable","extra":"private-extra"}}}`)
	started := true
	got := newOpenAIStreamFailureObservation(payload, "response protection is unavailable", &started)
	require.NotNil(t, got)
	started = false
	require.True(t, *got.ClientOutputStarted, "snapshot must not alias caller state")
	require.Equal(t, "other", got.ErrorCode)
	require.Equal(t, "server_error", got.ErrorType)
	require.Equal(t, 1, *got.ResponseOutputItems)
	require.True(t, got.ProtectionUnavailable)
	require.EqualValues(t, 10, *got.InputTokens)
	require.EqualValues(t, 2, *got.OutputTokens)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "private")

	empty := newOpenAIStreamFailureObservation([]byte(`{"type":"error","error":{}}`), "", nil)
	require.Nil(t, empty.ClientOutputStarted)
	require.Nil(t, empty.ResponseOutputItems)
	require.False(t, empty.UsagePresent)
	require.Nil(t, empty.InputTokens)
	require.Nil(t, empty.OutputTokens)
	require.Nil(t, newOpenAIStreamFailureObservation([]byte(`{"type":"response.output_text.delta","delta":"response protection is unavailable"}`), "", nil))
}

func TestOpenAIStreamFailureExplicitServerClassification(t *testing.T) {
	for _, tc := range []struct {
		name, payload, message string
		want                   bool
	}{
		{"typed_server", `{"type":"error","error":{"type":"server_error"}}`, "response protection is unavailable", true},
		{"coded_server", `{"type":"error","error":{"code":"internal_error"}}`, "", true},
		{"typed_unavailable", `{"type":"error","error":{"code":"service_unavailable"}}`, "", true},
		{"unknown_protection", `{"type":"error","error":{}}`, "response protection is unavailable", false},
		{"policy_over_server", `{"type":"error","error":{"type":"server_error","code":"content_policy"}}`, "", false},
		{"request_over_server", `{"type":"error","error":{"type":"invalid_request_error","code":"server_error"}}`, "", false},
		{"context_over_server", `{"type":"error","error":{"code":"server_error"}}`, "context_length_exceeded", false},
		{"not_output_content", `{"type":"error","error":{},"output":{"code":"server_error"}}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, openAIStreamExplicitServerFailure([]byte(tc.payload), tc.message))
		})
	}
}

func TestOpenAIStreamFailureConsumers(t *testing.T) {
	delta := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
	bare := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"message\":\"response protection is unavailable\"}}\n\n"
	unknown := "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"response protection is unavailable\"}}\n\n"
	failed := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"output\":[],\"error\":{\"type\":\"server_error\",\"message\":\"response protection is unavailable\"},\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n"
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"finished\"}]}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n"
	for _, passthrough := range []bool{false, true} {
		for _, tc := range []struct {
			name, body, event     string
			wantErr, failover     bool
			outputStarted         bool
			wantEvents, outputTok int
			completed             bool
		}{
			{"early_typed_server", bare, "error", true, true, false, 1, 0, false},
			{"early_unknown_no_replay", unknown, "error", true, false, false, 1, 0, false},
			{"post_output_bare", delta + bare, "error", true, false, true, 1, 0, false},
			{"post_output_failed", delta + failed, "response.failed", true, false, true, 1, 2, false},
			{"paired_failure_one_record", delta + bare + failed, "response.failed", true, false, true, 1, 2, false},
			{"completed_after_pending_error", delta + bare + completed, "", false, false, true, 0, 3, true},
			{"completed_after_unknown_early", unknown + completed, "", false, false, false, 0, 3, true},
			{"header_type_only", delta + strings.Replace(failed, `"type":"response.failed",`, "", 1), "response.failed", true, false, true, 1, 2, false},
		} {
			mode := "native/"
			if passthrough {
				mode = "passthrough/"
			}
			t.Run(mode+tc.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
				resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}
				var err error
				var usage *OpenAIUsage
				if passthrough {
					result, failure := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "model", "model")
					err, usage = failure, result.usage
				} else {
					result, failure := svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "model", "model")
					err, usage = failure, result.usage
				}
				require.Equal(t, tc.wantErr, err != nil)
				var failover *UpstreamFailoverError
				require.Equal(t, tc.failover, errors.As(err, &failover))
				require.Equal(t, tc.outputTok, usage.OutputTokens)
				raw, _ := c.Get(OpsUpstreamErrorsKey)
				events, _ := raw.([]*OpsUpstreamErrorEvent)
				require.Len(t, events, tc.wantEvents)
				if tc.wantEvents > 0 {
					observation := events[0].StreamFailure
					require.NotNil(t, observation)
					require.Equal(t, tc.event, observation.EventType)
					require.True(t, observation.ProtectionUnavailable)
					require.Empty(t, events[0].Detail, "raw payload logging stays disabled")
					if !tc.failover {
						require.NotNil(t, observation.ClientOutputStarted)
						require.Equal(t, tc.outputStarted, *observation.ClientOutputStarted)
					}
				}
				if tc.failover {
					require.Empty(t, recorder.Body.String(), "no downstream commit before failover")
				} else if tc.completed {
					require.Contains(t, recorder.Body.String(), `"type":"response.completed"`)
					require.NotContains(t, recorder.Body.String(), `"type":"response.failed"`)
				} else {
					failures := 0
					for _, frame := range strings.Split(recorder.Body.String(), "\n\n") {
						var eventType string
						for _, line := range strings.Split(frame, "\n") {
							if strings.HasPrefix(line, "event:") {
								eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
							}
							if strings.HasPrefix(line, "data:") {
								payload := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
								if effectiveOpenAISSEEventType(payload, eventType) == "response.failed" {
									failures++
								}
							}
						}
					}
					require.Equal(t, 1, failures)
				}
			})
		}
	}
}

func TestOpenAIStreamFailureConsumedUsageStopsReplay(t *testing.T) {
	for _, tc := range []struct {
		name      string
		usage     string
		prefix    string
		wantRetry bool
	}{
		{"input_only", `{"input_tokens":7,"output_tokens":0}`, "", false},
		{"output_only", `{"input_tokens":0,"output_tokens":2}`, "", false},
		{"reasoning_only", `{"output_tokens_details":{"reasoning_tokens":2}}`, "", false},
		{"cached_only", `{"input_tokens_details":{"cached_tokens":7}}`, "", false},
		{"progressive_usage", `null`, "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":7}}}\n\n", false},
		{"progressive_usage_then_zero", `{"input_tokens":0,"output_tokens":0}`, "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":7}}}\n\n", false},
		{"progressive_reasoning_then_zero", `{"input_tokens":0,"output_tokens":0}`, "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n", false},
		{"explicit_zero", `{"input_tokens":0,"output_tokens":0}`, "", true},
		{"absent", `null`, "", true},
	} {
		for _, mode := range []string{
			"native_stream", "passthrough_stream", "native_buffered", "passthrough_buffered",
			"chat_stream", "chat_buffered", "messages_stream", "messages_buffered",
		} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				payload := `{"type":"response.failed","response":{"status":"failed","output":[],"error":{"type":"server_error","message":"temporary failure"},"usage":` + tc.usage + `}}`
				body := tc.prefix + "event: response.failed\ndata: " + payload + "\n\n"
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
				resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
				var err error
				switch mode {
				case "native_stream":
					_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "model", "model")
				case "passthrough_stream":
					_, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "model", "model")
				case "native_buffered":
					_, err = svc.handleSSEToJSON(resp, c, account, []byte(body), "model", "model")
				case "passthrough_buffered":
					_, err = svc.handlePassthroughSSEToJSON(resp, c, account, []byte(body), "model", "model")
				case "chat_stream":
					_, err = svc.handleChatStreamingResponse(resp, c, account, "model", "model", "model", time.Now(), 0)
				case "chat_buffered":
					_, err = svc.handleChatBufferedStreamingResponse(resp, c, account, "model", "model", "model", time.Now())
				case "messages_stream":
					_, err = svc.handleAnthropicStreamingResponse(resp, c, account, "model", "model", "model", time.Now())
				case "messages_buffered":
					_, err = svc.handleAnthropicBufferedStreamingResponse(resp, c, account, "model", "model", "model", time.Now())
				}
				var failure *UpstreamFailoverError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, tc.wantRetry, failure.ShouldRetryNextAccount())
				if !tc.wantRetry {
					require.False(t, failure.RetryableOnSameAccount)
				}
			})
		}
	}
}

func TestOpenAIStreamFailureConsumedUsageBeforeDisconnectStopsReplay(t *testing.T) {
	for _, end := range []string{"eof", "read_error"} {
		for _, mode := range []string{"native", "passthrough"} {
			for _, consumed := range []bool{false, true} {
				name := "unused"
				if consumed {
					name = "consumed"
				}
				t.Run(end+"/"+mode+"/"+name, func(t *testing.T) {
					prefix := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n"
					if consumed {
						prefix += "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":7}}}\n\n"
					}
					var reader io.Reader = strings.NewReader(prefix)
					if end == "read_error" {
						reader = io.MultiReader(reader, iotest.ErrReader(io.ErrUnexpectedEOF))
					}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
					account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
					resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(reader)}
					var err error
					if mode == "native" {
						_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "model", "model")
					} else {
						_, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "model", "model")
					}
					var failure *UpstreamFailoverError
					require.ErrorAs(t, err, &failure)
					require.Equal(t, !consumed, failure.ShouldRetryNextAccount())
					if consumed {
						require.False(t, failure.RetryableOnSameAccount)
					}
				})
			}
		}
	}
}

func TestOpenAIStreamFailureConsumedUsagePreventsCompactFallback(t *testing.T) {
	for _, mode := range []string{"native_stream", "passthrough_stream", "native_buffered", "passthrough_buffered"} {
		for _, consumed := range []bool{false, true} {
			name := "unused"
			if consumed {
				name = "consumed"
			}
			t.Run(mode+"/"+name, func(t *testing.T) {
				body := ""
				if consumed {
					body = "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":7}}}\n\n"
				}
				body += "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"context window exceeded\"}}}\n\n"
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
				svc := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
				resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
				var err error
				switch mode {
				case "native_stream":
					_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "model", "model")
				case "passthrough_stream":
					_, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "model", "model")
				case "native_buffered":
					_, err = svc.handleSSEToJSON(resp, c, account, []byte(body), "model", "model")
				case "passthrough_buffered":
					_, err = svc.handlePassthroughSSEToJSON(resp, c, account, []byte(body), "model", "model")
				}
				require.Error(t, err)
				_, fallback := asOpenAICompactFallbackSignal(err)
				require.Equal(t, !consumed, fallback)
				var failure *UpstreamFailoverError
				if errors.As(err, &failure) && consumed {
					require.False(t, failure.ShouldRetryNextAccount())
				}
			})
		}
	}
}

func TestOpenAIStreamFailureConsumedUsagePreventsCompactHTTPRetry(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.OpenAICompactModel = "compact-fallback"
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	request := []byte(`{"model":"model","input":[]}`)
	for _, tc := range []struct {
		name      string
		usage     string
		wantRetry bool
	}{
		{"consumed", `{"input_tokens":7}`, false},
		{"reasoning", `{"output_tokens_details":{"reasoning_tokens":2}}`, false},
		{"zero", `{"input_tokens":0,"output_tokens":0}`, true},
		{"absent", `null`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"error":{"code":"context_length_exceeded","message":"context window exceeded"},"usage":` + tc.usage + `}`)
			got, _, retry := svc.prepareOpenAICompactFallbackRetry(c, nil, "model", request,
				http.StatusBadRequest, "context window exceeded", body, false)
			require.Equal(t, tc.wantRetry, retry)
			if !tc.wantRetry {
				require.Equal(t, request, got)
			}
		})
	}
}
