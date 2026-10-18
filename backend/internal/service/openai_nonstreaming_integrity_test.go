//go:build unit

package service

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNonStreamingResponsesIntegrityRejectsAmbiguousSuccess(t *testing.T) {
	tests := []struct {
		name   string
		frames []string
	}{
		{
			name: "empty_stream",
		},
		{
			name: "missing_terminal",
			frames: []string{
				`{"type":"response.created","response":{"id":"resp_one"}}`,
				`{"type":"response.output_text.delta","delta":"private-partial-answer"}`,
			},
		},
		{
			name:   "missing_terminal_response",
			frames: []string{`{"type":"response.completed"}`},
		},
		{
			name:   "terminal_response_is_array",
			frames: []string{`{"type":"response.completed","response":[]}`},
		},
		{
			name:   "completed_status_failed",
			frames: []string{`{"type":"response.completed","response":{"id":"resp_one","status":"failed"}}`},
		},
		{
			name:   "completed_status_in_progress",
			frames: []string{`{"type":"response.completed","response":{"id":"resp_one","status":"in_progress"}}`},
		},
		{
			name: "response_identity_changed",
			frames: []string{
				`{"type":"response.created","response":{"id":"resp_one"}}`,
				`{"type":"response.completed","response":{"id":"resp_other","status":"completed"}}`,
			},
		},
		{
			name: "delta_from_other_response",
			frames: []string{
				`{"type":"response.created","response":{"id":"resp_one"}}`,
				`{"type":"response.output_text.delta","response_id":"resp_other","delta":"private-partial-answer"}`,
				`{"type":"response.completed","response":{"id":"resp_one","status":"completed"}}`,
			},
		},
		{
			name: "conflicting_completed_payloads",
			frames: []string{
				`{"type":"response.completed","response":{"id":"resp_one","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"private-first-answer"}]}]}}`,
				`{"type":"response.done","response":{"id":"resp_one","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"private-second-answer"}]}]}}`,
			},
		},
		{
			name: "conflicting_terminal_states",
			frames: []string{
				`{"type":"response.completed","response":{"id":"resp_one","status":"completed"}}`,
				`{"type":"response.incomplete","response":{"id":"resp_one","status":"incomplete"}}`,
			},
		},
		{
			name: "malformed_event_before_terminal",
			frames: []string{
				`{"type":"response.output_text.delta","delta":`,
				`{"type":"response.completed","response":{"id":"resp_one","status":"completed"}}`,
			},
		},
		{
			name: "conflicting_usage_above_float_precision",
			frames: []string{
				`{"type":"response.completed","response":{"id":"resp_one","usage":{"input_tokens":9007199254740992}}}`,
				`{"type":"response.completed","response":{"usage":{"input_tokens":9007199254740993},"id":"resp_one"}}`,
			},
		},
		{
			name: "error_after_completed_must_not_replay",
			frames: []string{
				`{"type":"response.completed","response":{"id":"resp_one","status":"completed"}}`,
				`{"type":"error","error":{"message":"Temporary upstream failure, please retry"}}`,
			},
		},
		{
			name: "output_after_completed",
			frames: []string{
				`{"type":"response.completed","response":{"id":"resp_one","status":"completed","output":[]}}`,
				`{"type":"response.output_text.delta","delta":"private-late-answer"}`,
			},
		},
		{
			name: "output_item_after_completed",
			frames: []string{
				`{"type":"response.completed","response":{"id":"resp_one","status":"completed","output":[]}}`,
				`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"private-late-answer"}]}}`,
			},
		},
	}

	for _, passthrough := range []bool{false, true} {
		mode := "normal"
		if passthrough {
			mode = "passthrough"
		}
		t.Run(mode, func(t *testing.T) {
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					c, rec := newNonStreamingFailoverContext(t)
					svc := newNonStreamingFailoverService()
					c.Writer.Header().Set("Content-Length", "99999")
					body := []byte("data: " + strings.Join(tc.frames, "\n\ndata: ") + "\n\ndata: [DONE]\n\n")
					var err error
					if passthrough {
						result, resultErr := svc.handlePassthroughSSEToJSON(newNonStreamingSSEResponse(), c, newNonStreamingFailoverAccount(), body, "model", "model")
						err = resultErr
						require.Nil(t, result)
					} else {
						result, resultErr := svc.handleSSEToJSON(newNonStreamingSSEResponse(), c, newNonStreamingFailoverAccount(), body, "model", "model")
						err = resultErr
						require.Nil(t, result)
					}
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover), "ambiguous execution must not cause replay")
					require.Equal(t, http.StatusBadGateway, rec.Code)
					require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
					require.Empty(t, rec.Header().Get("Content-Length"))
					require.NotContains(t, rec.Body.String(), "private-")
					require.NotContains(t, err.Error(), "private-")
					_, completed := extractCodexFinalResponse(string(body))
					require.False(t, completed, "probe extraction must not select an ambiguous successful answer")
				})
			}
		})
	}
}

func TestNonStreamingResponsesIntegrityPreservesValidResults(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus string
	}{
		{
			name: "identical_terminal_aliases_with_reordered_fields",
			body: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_one\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n" +
				"data: {\"type\":\"response.done\",\"response\":{\"usage\":{\"output_tokens\":2,\"input_tokens\":3},\"status\":\"completed\",\"id\":\"resp_one\"}}\n\n",
			wantStatus: "completed",
		},
		{
			name: "recoverable_error_before_completed",
			body: "data: {\"type\":\"error\",\"error\":{\"message\":\"recovering\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_one\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n",
			wantStatus: "completed",
		},
		{
			name:       "incomplete_is_not_relabeled_completed",
			body:       "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_one\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"partial\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n",
			wantStatus: "incomplete",
		},
		{
			name:       "cancelled_is_not_relabeled_completed",
			body:       "data: {\"type\":\"response.cancelled\",\"response\":{\"id\":\"resp_one\",\"status\":\"cancelled\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n",
			wantStatus: "cancelled",
		},
		{
			name:       "event_field_and_multiline_json",
			body:       "event: response.completed\r\ndata: {\"response\":{\"id\":\"resp_one\",\"status\":\"completed\",\r\ndata: \"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\r\n\r\n",
			wantStatus: "completed",
		},
		{
			name:       "function_call_without_text",
			body:       "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_one\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_one\",\"name\":\"lookup\",\"arguments\":\"{}\"}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n",
			wantStatus: "completed",
		},
	}
	for _, passthrough := range []bool{false, true} {
		mode := "normal"
		if passthrough {
			mode = "passthrough"
		}
		t.Run(mode, func(t *testing.T) {
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					c, rec := newNonStreamingFailoverContext(t)
					svc := newNonStreamingFailoverService()
					c.Writer.Header().Set("Content-Length", "99999")
					var usage *OpenAIUsage
					if passthrough {
						result, err := svc.handlePassthroughSSEToJSON(newNonStreamingSSEResponse(), c, newNonStreamingFailoverAccount(), []byte(tc.body), "model", "model")
						require.NoError(t, err)
						usage = result.usage
					} else {
						result, err := svc.handleSSEToJSON(newNonStreamingSSEResponse(), c, newNonStreamingFailoverAccount(), []byte(tc.body), "model", "model")
						require.NoError(t, err)
						usage = result.usage
					}
					require.Equal(t, http.StatusOK, rec.Code)
					require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
					require.Empty(t, rec.Header().Get("Content-Length"))
					require.Equal(t, "resp_one", gjson.Get(rec.Body.String(), "id").String())
					require.Equal(t, tc.wantStatus, gjson.Get(rec.Body.String(), "status").String())
					require.Equal(t, 3, usage.InputTokens)
					require.Equal(t, 2, usage.OutputTokens)
				})
			}
		})
	}
}

func TestNonStreamingResponsesIntegrityPreservesLegacyChatSSE(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	for _, passthrough := range []bool{false, true} {
		c, rec := newNonStreamingFailoverContext(t)
		svc := newNonStreamingFailoverService()
		if passthrough {
			_, err := svc.handlePassthroughSSEToJSON(newNonStreamingSSEResponse(), c, newNonStreamingFailoverAccount(), body, "model", "model")
			require.NoError(t, err)
		} else {
			_, err := svc.handleSSEToJSON(newNonStreamingSSEResponse(), c, newNonStreamingFailoverAccount(), body, "model", "model")
			require.NoError(t, err)
		}
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, string(body), rec.Body.String())
		require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	}
}

func TestNonStreamingResponsesIntegrityIngressContentTypes(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, contentType := range []string{"text/event-stream", "application/json", ""} {
			for _, completed := range []bool{false, true} {
				c, rec := newNonStreamingFailoverContext(t)
				svc := newNonStreamingFailoverService()
				body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_one\"}}\n\n"
				if completed {
					body += "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_one\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"
				}
				resp := newNonStreamingSSEResponse()
				resp.Header.Set("Content-Type", contentType)
				resp.Body = io.NopCloser(strings.NewReader(body))
				var err error
				if passthrough {
					_, err = svc.handleNonStreamingResponsePassthrough(c.Request.Context(), resp, c, newNonStreamingFailoverAccount(), "model", "model")
				} else {
					_, err = svc.handleNonStreamingResponse(c.Request.Context(), resp, c, newNonStreamingFailoverAccount(), "model", "model")
				}
				if completed {
					require.NoError(t, err, "passthrough=%v content_type=%q", passthrough, contentType)
					require.Equal(t, http.StatusOK, rec.Code)
					require.Equal(t, "resp_one", gjson.Get(rec.Body.String(), "id").String())
				} else {
					require.ErrorContains(t, err, "missing terminal event", "passthrough=%v content_type=%q", passthrough, contentType)
					require.Equal(t, http.StatusBadGateway, rec.Code)
				}
				require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
			}
		}
	}
}
