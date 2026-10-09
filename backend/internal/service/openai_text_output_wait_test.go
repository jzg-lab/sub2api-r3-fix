package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type textWaitUpstream struct {
	response *http.Response
	send     func(*http.Request) (*http.Response, error)
}

func (u textWaitUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if u.send != nil {
		return u.send(req)
	}
	return u.response, nil
}
func (u textWaitUpstream) DoWithTLS(r *http.Request, p string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, p, id, concurrency)
}

func TestTextFirstOutputDeadlineIgnoresBlankStreams(t *testing.T) {
	for _, tc := range []struct{ name, prefix string }{
		{"responses", "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\" \\n\"}\n\n"},
		{"chat", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\" \"}}]}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			written := make(chan struct{})
			go func() {
				defer close(written)
				_, _ = io.WriteString(writer, tc.prefix)
				for {
					if _, err := io.WriteString(writer, ": heartbeat\n\n"); err != nil {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			u := textWaitUpstream{response: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}}
			svc := &OpenAIGatewayService{httpUpstream: u, cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 1}}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			req := httptest.NewRequest("POST", "https://provider.example/v1/responses", nil)
			_, err := svc.doOpenAITextUpstream(c.Request.Context(), c, req, "", &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, []byte(`{"model":"gpt-test","stream":true}`), time.Now().Add(-950*time.Millisecond), true)
			var failure *UpstreamFailoverError
			require.ErrorAs(t, err, &failure)
			require.Contains(t, string(failure.ResponseBody), "first_output_timeout")
			require.False(t, IsResponseCommitted(c))
			select {
			case <-written:
			case <-time.After(time.Second):
				t.Fatal("upstream reader not released")
			}
		})
	}
}

func TestTextPrefixPreservesBytesAndReleasesSpool(t *testing.T) {
	for _, event := range []string{
		`{"type":"response.output_text.delta","delta":"Hello"}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"tool","arguments":""}}`,
		`{"type":"response.reasoning_text.delta","delta":"Thinking"}`,
	} {
		prefix := ": " + strings.Repeat("x", 70*1024) + "\r\n\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\" \"}\r\n\r\ndata: " + event + "\r\n\r\n"
		tail := "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4}}}\n\n"
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(prefix + tail))}
		require.NoError(t, stageOpenAITextPrefix(resp))
		staged := resp.Body.(*openAIStagedTextBody)
		require.NotNil(t, staged.stage.tempFile)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, prefix+tail, string(data))
		require.NoError(t, resp.Body.Close())
		require.True(t, staged.stage.closed)
	}
}

func TestTextPrefixIsBounded(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat(" ", openAIFirstOutputStageMaxBytes+1)))}
	defer resp.Body.Close()
	require.ErrorIs(t, stageOpenAITextPrefix(resp), errOpenAIFirstOutputStageLimit)
}

func TestUsageDrainOnlyStartsWhenClientDisconnects(t *testing.T) {
	client, cancel := context.WithCancel(context.Background())
	upstream, release := withUpstreamDrain(client, context.Background(), 30*time.Millisecond)
	defer release()
	time.Sleep(40 * time.Millisecond)
	require.NoError(t, upstream.Err(), "live client must not get a total request deadline")
	cancel()
	require.NoError(t, upstream.Err(), "cancellation must leave a usage collection grace period")
	select {
	case <-upstream.Done():
	case <-time.After(time.Second):
		t.Fatal("drain did not end")
	}
	require.Contains(t, context.Cause(upstream).Error(), "usage incomplete")
}

func TestTextDeadlineCoversHeadersAndUnaryBody(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			u := textWaitUpstream{send: func(req *http.Request) (*http.Response, error) {
				if phase == "headers" {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				reader, writer := io.Pipe()
				t.Cleanup(func() { _ = writer.Close() })
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: reader}, nil
			}}
			svc := &OpenAIGatewayService{httpUpstream: u, cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 1}}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			req := httptest.NewRequest("POST", "https://provider.example/v1/responses", nil)
			_, err := svc.doOpenAITextUpstream(c.Request.Context(), c, req, "", &Account{ID: 1, Platform: PlatformOpenAI}, []byte(`{"model":"gpt-test"}`), time.Now().Add(-900*time.Millisecond), false)
			var failure *UpstreamFailoverError
			require.ErrorAs(t, err, &failure)
			require.Contains(t, string(failure.ResponseBody), "first_output_timeout")
			require.False(t, c.Writer.Written())
		})
	}
}

func TestTextPrefixKeepalivePreservesHeadersAndRetry(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	go func() {
		time.Sleep(1100 * time.Millisecond)
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"unavailable\"}}}\n\n")
		_ = writer.Close()
	}()
	svc := &OpenAIGatewayService{httpUpstream: textWaitUpstream{response: &http.Response{
		StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Codex-Turn-State": {"original-turn"}}, Body: reader,
	}}, cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 3, StreamKeepaliveInterval: 1}}}
	c, rec := newPassthroughKeepaliveTestContext(t)
	c.Set("openai_passthrough", true)
	req := httptest.NewRequest("POST", "https://provider.example/v1/responses", nil)
	account := &Account{ID: 1, Platform: PlatformOpenAI}
	resp, err := svc.doOpenAITextUpstream(c.Request.Context(), c, req, "", account, []byte(`{"model":"gpt-test","stream":true}`), time.Now(), true)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-test", "gpt-test")
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.Contains(t, rec.Body.String(), ": keepalive\n\n")
	require.Equal(t, "original-turn", rec.Result().Header.Get("x-codex-turn-state"))
	require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
	latency, ok := c.Get(OpsUpstreamLatencyMsKey)
	require.True(t, ok)
	require.Less(t, latency, int64(500), "header latency must not include prefix waiting")
}

func TestUsageDrainCancelsBlockedHTTPRead(t *testing.T) {
	ended := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": waiting\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(ended)
	}))
	defer server.Close()
	client, disconnect := context.WithCancel(context.Background())
	upstream, release := detachUpstreamContextWithDrain(client, 30*time.Millisecond)
	defer release()
	req, err := http.NewRequestWithContext(upstream, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	disconnect()
	_, err = io.ReadAll(resp.Body)
	require.Error(t, err)
	require.Contains(t, context.Cause(upstream).Error(), "usage incomplete")
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("upstream connection was not released")
	}
}

func TestTextOutputDisarmsDeadlineEvenWithoutSSEContentType(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	prefix := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
	tail := "data: {\"type\":\"response.completed\"}\n\n"
	go func() {
		_, _ = io.WriteString(writer, prefix)
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(writer, tail)
		_ = writer.Close()
	}()
	svc := &OpenAIGatewayService{httpUpstream: textWaitUpstream{response: &http.Response{StatusCode: 200, Header: http.Header{}, Body: reader}}, cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 1}}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	req := httptest.NewRequest("POST", "https://provider.example/v1/responses", nil)
	resp, err := svc.doOpenAITextUpstream(c.Request.Context(), c, req, "", &Account{ID: 1, Platform: PlatformOpenAI}, []byte(`{"model":"gpt-test","stream":true}`), time.Now().Add(-900*time.Millisecond), true)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, prefix+tail, string(data))
}

func TestKeepaliveRestartsDoNotCommitTextOutput(t *testing.T) {
	c, rec := newPassthroughKeepaliveTestContext(t)
	for range 3 {
		stop := startOpenAISSEKeepalive(c, time.Hour)
		value, _ := c.Get(openAICompactSSEKeepaliveKey)
		require.True(t, value.(*openAICompactSSEKeepalive).beat())
		stop()
		require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
	}
	require.Equal(t, strings.Repeat(": keepalive\n\n", 3), rec.Body.String())
	_, err := c.Writer.WriteString("data: real\n\n")
	require.NoError(t, err)
	require.True(t, openAIStreamClientOutputStarted(c, false))
}

func TestWhitespaceBeforeFailureRemainsReplayable(t *testing.T) {
	for _, protocol := range []string{"responses", "messages", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			c, rec := newPassthroughKeepaliveTestContext(t)
			stop := startOpenAISSEKeepalive(c, time.Hour)
			value, _ := c.Get(openAICompactSSEKeepaliveKey)
			require.True(t, value.(*openAICompactSSEKeepalive).beat())
			stop()
			body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\" \"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"upstream unavailable\"}}}\n\n"
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
			svc := &OpenAIGatewayService{}
			account := &Account{ID: 1, Platform: PlatformOpenAI}
			var err error
			switch protocol {
			case "responses":
				_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-test", "gpt-test")
			case "messages":
				_, err = svc.handleAnthropicStreamingResponse(resp, c, account, "gpt-test", "gpt-test", "gpt-test", time.Now())
			case "chat":
				_, err = svc.handleChatStreamingResponse(resp, c, account, "gpt-test", "gpt-test", "gpt-test", time.Now(), 0)
			}
			var failure *UpstreamFailoverError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, ": keepalive\n\n", rec.Body.String())
			require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
		})
	}
}

func TestResponsesTerminalDoesNotRequireEOF(t *testing.T) {
	for _, mode := range []string{"native", "passthrough", "buffered"} {
		t.Run(mode, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\n"
			go func() { _, _ = io.Copy(writer, bytes.NewBufferString(body)) }()
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}
			svc := &OpenAIGatewayService{}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			done := make(chan error, 1)
			go func() {
				var err error
				switch mode {
				case "native":
					_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "m", "m")
				case "passthrough":
					_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "m", "m")
				case "buffered":
					var data []byte
					data, err = readOpenAIResponseBodyThroughTerminal(resp, 1024)
					if err == nil && string(data) != body {
						err = io.ErrUnexpectedEOF
					}
				}
				done <- err
			}()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(time.Second):
				reader.Close()
				<-done
				t.Fatal("terminal event waited for EOF")
			}
		})
	}
}

func (u textWaitUpstream) DoProbeWithTLS(req *http.Request, proxy string, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.DoWithTLS(req, proxy, 0, concurrency, profile)
}

func TestDeterministicUpstreamErrorsSkipSameAccountRetries(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_count": 3}}
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		headers http.Header
		want    bool
	}{
		{"deleted provider group", 403, `{"error":{"code":"token_group_required","message":"group deleted"}}`, nil, true},
		{"cloudflare block", 403, `<!DOCTYPE html><html>Cloudflare Error 1010</html>`, http.Header{"Content-Type": []string{"text/html"}}, true},
		{"unsupported model", 404, `{"error":{"message":"Model gpt-test is not supported by any configured account in this group"}}`, nil, true},
		{"string model error", 404, `{"error":"model not found"}`, nil, true},
		{"ordinary forbidden", 403, `{"error":{"message":"forbidden"}}`, nil, false},
		{"echoed error code", 403, `{"error":{"message":"forbidden"},"request":{"code":"token_group_required"}}`, nil, false},
		{"endpoint missing", 404, `{"error":{"message":"endpoint not found"},"request":{"model":"gpt-test"}}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			err := svc.newOpenAIAccountFailoverError(account, tc.status, tc.headers, body, "", false, true)
			require.Equal(t, !tc.want, err.RetryableOnSameAccount)
			if tc.want {
				require.True(t, shouldFailoverOpenAIPassthroughResponse(account, tc.status, body, tc.headers))
				require.Equal(t, NextAccountRetry, err.NextAccountAction)
			}
			if tc.status == 404 {
				require.Equal(t, tc.want, svc.shouldFailoverOpenAIUpstreamResponse(tc.status, "", body))
			}
		})
	}
}

func TestResponsesFailedUsageStopsBeforeEOF(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, terminal := range []string{"failed", "done"} {
			t.Run(fmt.Sprintf("passthrough=%v/%s", passthrough, terminal), func(t *testing.T) {
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"unavailable\"}\n\n"
				if terminal == "failed" {
					body += "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"},\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\n"
				} else {
					body += "data: [DONE]\n\n"
				}
				go func() { _, _ = io.WriteString(writer, body) }()
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}
				svc := &OpenAIGatewayService{}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				done := make(chan error, 1)
				var usage *OpenAIUsage
				go func() {
					if passthrough {
						result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-test", "gpt-test")
						usage = result.usage
						done <- err
					} else {
						result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-test", "gpt-test")
						usage = result.usage
						done <- err
					}
				}()
				select {
				case err := <-done:
					require.ErrorContains(t, err, "upstream response failed")
					if terminal == "failed" {
						require.Equal(t, 4, usage.InputTokens)
					}
				case <-time.After(time.Second):
					_ = reader.Close()
					<-done
					t.Fatal("terminal event waited for EOF")
				}
			})
		}
	}
}
