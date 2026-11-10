package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
)

func TestCaptureClaudeHTTPEntryPoints(t *testing.T) {
	for _, entry := range []string{"messages", "messages-passthrough", "responses", "chat/completions"} {
		for _, stream := range []bool{false, true} {
			t.Run(entry+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				manager := NewPluginManager(nil, nil, nil, PluginHostInfo{})
				manager.captureRuntime.Store(&pluginRuntime{})
				response := captureAnthropicResponse(false)
				if stream || !strings.HasPrefix(entry, "messages") {
					response = captureAnthropicResponse(true)
				}
				raw, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				response.Body = io.NopCloser(strings.NewReader(string(raw)))
				upstream := &captureForwardUpstream{resp: response}
				svc := &GatewayService{cfg: captureForwardConfig(), httpUpstream: upstream, rateLimitService: &RateLimitService{}, deferredService: &DeferredService{}}
				svc.cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				svc.SetPluginManager(manager)
				account := &Account{ID: 10, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "test", "base_url": "http://upstream.test"}}
				if entry == "messages-passthrough" {
					account.Extra = map[string]any{"anthropic_passthrough": true}
				}
				request := map[string]any{"model": "claude-3-5-sonnet-latest", "max_tokens": 32, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
				if entry == "responses" {
					delete(request, "messages")
					request["input"] = "hello"
				}
				body, err := json.Marshal(request)
				require.NoError(t, err)
				c := captureForwardContext("/v1/"+strings.TrimSuffix(entry, "-passthrough"), body)
				parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
				require.NoError(t, err)
				switch entry {
				case "messages", "messages-passthrough":
					_, err = svc.Forward(context.Background(), c, account, parsed)
				case "responses":
					_, err = svc.ForwardAsResponses(context.Background(), c, account, body, parsed)
				case "chat/completions":
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, parsed)
				}
				require.NoError(t, err)
				require.Len(t, manager.captureQueue, 1, "successful public forwarding must reach the collector")
				item := <-manager.captureQueue
				defer item.release()
				var record map[string]any
				require.NoError(t, json.Unmarshal(item.request.RecordJson, &record))
				require.Equal(t, string(raw), record["response"])
				require.NotContains(t, record["request"], "test-secret")
			})
		}
	}
}

func TestCaptureOpenAIChatHTTPEntryPoint(t *testing.T) {
	for _, entry := range []string{"chat/completions", "messages", "responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(entry+"/"+map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
				manager := NewPluginManager(nil, nil, nil, PluginHostInfo{})
				manager.captureRuntime.Store(&pluginRuntime{})
				raw := `{"id":"chat_1","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					raw = "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
				}
				upstream := &captureForwardUpstream{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(raw))}}
				svc := &OpenAIGatewayService{cfg: captureForwardConfig(), httpUpstream: upstream}
				svc.SetPluginManager(manager)
				account := &Account{ID: 101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "test", "base_url": "http://upstream.test"}}
				account.Extra = map[string]any{"openai_responses_supported": false}
				body, _ := json.Marshal(map[string]any{"model": "gpt-4", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
				if entry == "responses" {
					body, _ = json.Marshal(map[string]any{"model": "gpt-4", "stream": stream, "input": "hello"})
				}
				c := captureForwardContext("/v1/"+entry, body)
				var err error
				switch entry {
				case "chat/completions":
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
				case "messages":
					_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
				case "responses":
					_, err = svc.Forward(context.Background(), c, account, body)
				}
				require.NoError(t, err)
				require.Len(t, manager.captureQueue, 1)
				item := <-manager.captureQueue
				defer item.release()
				var record map[string]any
				require.NoError(t, json.Unmarshal(item.request.RecordJson, &record))
				require.Equal(t, raw, record["response"])
			})
		}
	}
}

type captureForwardUpstream struct{ resp *http.Response }

func (u *captureForwardUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.resp, nil
}
func (u *captureForwardUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}
func (u *captureForwardUpstream) DoProbeWithTLS(req *http.Request, proxy string, concurrency int, p *tlsfingerprint.Profile) (*http.Response, error) {
	return u.DoWithTLS(req, proxy, 0, concurrency, p)
}
func captureForwardConfig() *config.Config {
	return &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}, Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
}
func captureForwardContext(path string, body []byte) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", path, strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}
func captureAnthropicResponse(stream bool) *http.Response {
	raw := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":93,"output_tokens":16}}`
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
		raw = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest","content":[],"usage":{"input_tokens":93}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":16}}

event: message_stop
data: {"type":"message_stop"}

`
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(raw))}
}

func TestCaptureOpenAIResponsesAndPassthrough(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(map[bool]string{false: "normal", true: "passthrough"}[passthrough]+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				manager := NewPluginManager(nil, nil, nil, PluginHostInfo{})
				manager.captureRuntime.Store(&pluginRuntime{})
				completed := `{"id":"resp_1","object":"response","model":"gpt-4","status":"completed","error":null,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":2,"output_tokens":1}}`
				raw := completed
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					raw = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + completed + "}\n\n"
				}
				upstream := &captureForwardUpstream{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(raw))}}
				svc := &OpenAIGatewayService{cfg: captureForwardConfig(), httpUpstream: upstream}
				svc.SetPluginManager(manager)
				account := &Account{ID: 101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "test", "base_url": "http://upstream.test"}, Extra: map[string]any{"openai_responses_supported": true, "openai_passthrough": passthrough}}
				body, _ := json.Marshal(map[string]any{"model": "gpt-4", "stream": stream, "input": "hello"})
				c := captureForwardContext("/v1/responses", body)
				_, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.Len(t, manager.captureQueue, 1)
				item := <-manager.captureQueue
				defer item.release()
				var record map[string]any
				require.NoError(t, json.Unmarshal(item.request.RecordJson, &record))
				require.Equal(t, raw, record["response"])
			})
		}
	}
}

type captureFailingWriter struct{ gin.ResponseWriter }

func (w captureFailingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestCaptureDiscardsDownstreamWriteFailure(t *testing.T) {
	m := NewPluginManager(nil, nil, nil, PluginHostInfo{})
	m.captureRuntime.Store(&pluginRuntime{})
	c := captureForwardContext("/v1/responses", nil)
	c.Writer = captureFailingWriter{c.Writer}
	original := c.Writer
	ctx, finish := m.beginCapture(context.Background(), c, &Account{ID: 1, Platform: PlatformOpenAI})
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://test/v1/responses", strings.NewReader(`{"model":"gpt-4"}`))
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"completed"}`))}
	captureHTTPResponse(req, resp, nil)
	_, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_, err = c.Writer.Write([]byte("client is gone"))
	require.Error(t, err)
	finish(true)
	require.Empty(t, m.captureQueue)
	require.Equal(t, original, c.Writer)
}
