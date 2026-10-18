package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestCodexRequestTraceUploadGuardAttribution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
		defer slog.SetDefault(previous)
		req, trace, err := (&httpUpstreamService{}).prepareCodexRequest(
			codexFixtureRequest(t, t.Context(), `{"input":"fixture-private-marker"}`))
		require.NoError(t, err)
		req, guard := guardCodexUpload(req, time.Second)
		defer guard.close()
		trace.upload = guard
		httptrace.ContextClientTrace(req.Context()).GotConn(httptrace.GotConnInfo{})
		_, err = req.Body.Read(make([]byte, 7))
		require.NoError(t, err)
		time.Sleep(2 * time.Second)
		trace.record(42, nil, guard.result(req.Context().Err()))
		var event map[string]any
		require.NoError(t, json.Unmarshal(output.Bytes(), &event))
		require.EqualValues(t, 7, event["upload_body_read_bytes"])
		require.EqualValues(t, 1000, event["upload_idle_ms"])
		require.Equal(t, true, event["upload_guard_expired"])
		require.Equal(t, "upload", event["phase"])
		require.NotContains(t, output.String(), "fixture-private-marker")
	})
}

func TestCodexRequestTraceFiniteResponseMetadata(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	for _, tc := range []struct {
		media, encoding, protocol, wantMedia, wantEncoding, wantProtocol string
	}{
		{"text/event-stream; charset=utf-8", "gzip", "HTTP/1.1", "sse", "gzip", "HTTP/1.1"},
		{"application/json", "zstd", "HTTP/2.0", "json", "zstd", "HTTP/2.0"},
		{"fixture-private-marker", "fixture-private-marker", "fixture-private-marker", "other", "other", "unknown"},
		{"", "", "", "missing", "identity", "unknown"},
	} {
		output.Reset()
		trace := &codexRequestTrace{start: time.Now()}
		trace.record(42, &http.Response{StatusCode: 200, Proto: tc.protocol, Close: true,
			Header: http.Header{"Content-Type": {tc.media}, "Content-Encoding": {tc.encoding},
				"Authorization": {"fixture-private-marker"}}}, nil)
		require.NotContains(t, output.String(), "fixture-private-marker")
		var event map[string]any
		require.NoError(t, json.Unmarshal(output.Bytes(), &event))
		require.Equal(t, tc.wantMedia, event["response_media"])
		require.Equal(t, tc.wantEncoding, event["response_encoding"])
		require.Equal(t, tc.wantProtocol, event["protocol"])
		require.Equal(t, true, event["connection_close"])
	}
}

func TestCodexRequestTraceCorrelatesFailuresAndWaitStage(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	const id = "e2d5e88a-84cb-46ee-83e3-586131ca5745"
	ctx := context.WithValue(t.Context(), ctxkey.RequestID, id)
	ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "fixture-private-marker")
	req, trace, err := (&httpUpstreamService{}).prepareCodexRequest(codexFixtureRequest(t, ctx, "{}"))
	require.NoError(t, err)
	defer req.Body.Close()
	trace.bindClient(42, 7)
	hooks := httptrace.ContextClientTrace(req.Context())
	for _, phase := range []string{"connection", "upload", "response_headers", "headers_received"} {
		switch phase {
		case "upload":
			hooks.GotConn(httptrace.GotConnInfo{})
			hooks.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("fixture-private-marker")})
		case "response_headers":
			hooks.WroteRequest(httptrace.WroteRequestInfo{})
		case "headers_received":
			hooks.GotFirstResponseByte()
		}
		trace.record(42, nil, errors.New("fixture-private-marker"))
		var event map[string]any
		require.NoError(t, json.Unmarshal(output.Bytes(), &event))
		require.Equal(t, id, event["request_id"])
		require.Empty(t, event["client_request_id"])
		require.Equal(t, phase, event["phase"])
		require.GreaterOrEqual(t, event["phase_elapsed_ms"].(float64), float64(0))
		require.Equal(t, "transport_error", event["outcome"])
		if phase != "headers_received" {
			require.EqualValues(t, -1, event["header_wait_ms"])
		}
		require.NotContains(t, output.String(), "fixture-private-marker")
		output.Reset()
	}
	hooks.PutIdleConn(nil)
	trace.recordBodyClose()
	decoder := json.NewDecoder(&output)
	for range 2 {
		var event map[string]any
		require.NoError(t, decoder.Decode(&event))
		require.Equal(t, id, event["request_id"])
		require.Empty(t, event["client_request_id"])
	}
}

func TestCodexRequestTracePoolReturn(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	req, trace, err := (&httpUpstreamService{}).prepareCodexRequest(
		codexFixtureRequest(t, t.Context(), `{"input":"fixture"}`))
	require.NoError(t, err)
	defer req.Body.Close()
	trace.bindClient(42, 7)
	hooks := httptrace.ContextClientTrace(req.Context())
	require.NotNil(t, hooks.PutIdleConn)
	hooks.PutIdleConn(nil)
	hooks.PutIdleConn(errors.New("fixture-private-marker"))
	trace.recordBodyClose()
	require.NotContains(t, output.String(), "fixture-private-marker")
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	require.Len(t, lines, 3)
	for i, line := range lines {
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		require.Equal(t, float64(42), event["account_id"])
		require.Equal(t, float64(7), event["client_id"])
		if i < 2 {
			require.Equal(t, []string{"accepted", "rejected"}[i], event["outcome"])
		} else {
			require.Equal(t, float64(2), event["idle_returns"])
			require.Equal(t, float64(1), event["idle_accepted"])
		}
	}
	var absent *codexRequestTrace
	absent.bindClient(0, 0)
	absent.recordBodyClose()
}

func codexFixtureRequest(t *testing.T, ctx context.Context, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(service.WithHTTPUpstreamProfile(ctx, service.HTTPUpstreamProfileOpenAIPlugin),
		http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func decodeCodexFixture(t *testing.T, req *http.Request) []byte {
	t.Helper()
	require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), req.ContentLength)
	decoder, err := zstd.NewReader(nil)
	require.NoError(t, err)
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(body, nil)
	require.NoError(t, err)
	return decoded
}

func TestCodexRequestCompressionParity(t *testing.T) {
	body := `{"model":"gpt-6-astra","input":"` + strings.Repeat("fixture context and source code ", 20000) + `","reasoning":{"effort":"xhigh"}}`
	for _, endpoint := range []string{"/backend-api/codex/responses", "/backend-api/codex/responses/compact"} {
		for _, profile := range []service.HTTPUpstreamProfile{service.HTTPUpstreamProfileOpenAI, service.HTTPUpstreamProfileOpenAIPlugin} {
			t.Run(endpoint+string(profile), func(t *testing.T) {
				req := codexFixtureRequest(t, t.Context(), body)
				req.URL.Path = endpoint
				req = req.WithContext(service.WithHTTPUpstreamProfile(req.Context(), profile))
				req.Header.Set("Content-Length", "stale")
				req.TransferEncoding = []string{"chunked"}
				originalHeader := req.Header.Clone()
				got, trace, err := (&httpUpstreamService{}).prepareCodexRequest(req)
				require.NoError(t, err)
				require.NotNil(t, trace)
				require.Equal(t, []byte(body), decodeCodexFixture(t, got))
				require.Equal(t, originalHeader, req.Header)
				require.Empty(t, got.Header.Get("Content-Length"))
				require.Nil(t, got.TransferEncoding)
				require.Nil(t, got.GetBody)
				require.Less(t, got.ContentLength, int64(len(body)/5))
				require.Equal(t, int64(len(body)), trace.inputBytes)
				t.Logf("input_bytes=%d wire_bytes=%d prepare_us=%d", trace.inputBytes, trace.wireBytes, trace.ready.Sub(trace.start).Microseconds())
			})
		}
	}
}

func TestCodexRequestCompressionScope(t *testing.T) {
	tests := map[string]func(*http.Request){
		"api-key":      func(r *http.Request) { r.URL.Host = "api.openai.com" },
		"foreign":      func(r *http.Request) { r.URL.Host = "chatgpt.com.example.invalid" },
		"http":         func(r *http.Request) { r.URL.Scheme = "http" },
		"port":         func(r *http.Request) { r.URL.Host = "chatgpt.com:8443" },
		"path":         func(r *http.Request) { r.URL.Path = "/backend-api/accounts" },
		"method":       func(r *http.Request) { r.Method = http.MethodGet },
		"content-type": func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"empty":        func(r *http.Request) { r.Body = http.NoBody },
		"profile": func(r *http.Request) {
			*r = *r.WithContext(t.Context())
		},
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			req := codexFixtureRequest(t, t.Context(), "{}")
			edit(req)
			got, trace, err := (&httpUpstreamService{}).prepareCodexRequest(req)
			require.NoError(t, err)
			require.Same(t, req, got)
			require.Nil(t, trace)
		})
	}
	t.Run("existing-encoding-not-double-compressed", func(t *testing.T) {
		req := codexFixtureRequest(t, t.Context(), "already encoded bytes")
		req.Header.Set("Content-Encoding", "zstd")
		got, trace, err := (&httpUpstreamService{}).prepareCodexRequest(req)
		require.NoError(t, err)
		require.Equal(t, trace.inputBytes, trace.wireBytes)
		require.Nil(t, got.GetBody)
		body, err := io.ReadAll(got.Body)
		require.NoError(t, err)
		require.Equal(t, "already encoded bytes", string(body))
	})
}

type failingCodexBody struct{ closed atomic.Bool }

func (*failingCodexBody) Read([]byte) (int, error) {
	return 0, errors.New("fixture private read detail")
}
func (b *failingCodexBody) Close() error { b.closed.Store(true); return nil }

func TestCodexRequestCompressionFailuresAndCancellation(t *testing.T) {
	t.Run("reader-error-redacted", func(t *testing.T) {
		req := codexFixtureRequest(t, t.Context(), "{}")
		body := &failingCodexBody{}
		req.Body = body
		got, trace, err := (&httpUpstreamService{}).prepareCodexRequest(req)
		require.EqualError(t, err, "codex request body preparation failed")
		require.Nil(t, got)
		require.Nil(t, trace)
		require.True(t, body.closed.Load())
	})
	for _, length := range []int64{-1, 0, 32} {
		t.Run(fmt.Sprintf("limit-%d", length), func(t *testing.T) {
			req := codexFixtureRequest(t, t.Context(), strings.Repeat("x", 32))
			req.ContentLength = length
			cfg := &config.Config{}
			cfg.Gateway.MaxBodySize = 8
			got, _, err := (&httpUpstreamService{cfg: cfg}).prepareCodexRequest(req)
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
	t.Run("truncated-body", func(t *testing.T) {
		req := codexFixtureRequest(t, t.Context(), "{}")
		req.ContentLength = 9
		_, _, err := (&httpUpstreamService{}).prepareCodexRequest(req)
		require.Error(t, err)
	})
	t.Run("unknown-length-and-exact-boundary", func(t *testing.T) {
		req := codexFixtureRequest(t, t.Context(), "{}")
		req.ContentLength = 0
		cfg := &config.Config{}
		cfg.Gateway.MaxBodySize = 2
		got, _, err := (&httpUpstreamService{cfg: cfg}).prepareCodexRequest(req)
		require.NoError(t, err)
		require.Equal(t, []byte("{}"), decodeCodexFixture(t, got))
	})
	t.Run("cancel-blocked-reader", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		req := codexFixtureRequest(t, ctx, "{}")
		reader, writer := io.Pipe()
		defer writer.Close()
		req.Body, req.ContentLength = reader, -1
		done := make(chan error, 1)
		go func() {
			_, _, err := (&httpUpstreamService{}).prepareCodexRequest(req)
			done <- err
		}()
		cancel()
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			t.Fatal("canceled request retained its body reader")
		}
	})
}

type codexFixtureTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r codexFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = r.target.Scheme, r.target.Host
	return r.base.RoundTrip(clone)
}

func TestCodexRequestNativeHTTPConsumers(t *testing.T) {
	body := `{"input":"same prompt","model":"gpt-6-astra"}`
	for _, useTLS := range []bool{false, true} {
		t.Run(map[bool]string{false: "Do", true: "DoWithTLS"}[useTLS], func(t *testing.T) {
			var calls, connections atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				n := calls.Add(1)
				require.Equal(t, []byte(body), decodeCodexFixture(t, req))
				if n == 3 {
					connection, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					_ = connection.Close()
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: fixture\n\n")
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			svc := NewHTTPUpstream(nil).(*httpUpstreamService)
			profile := &tlsfingerprint.Profile{Name: "fixture"}
			var entry *upstreamClientEntry
			if useTLS {
				entry, err = svc.getClientEntryWithTLS("", 42, 1, profile, service.HTTPUpstreamProfileOpenAIPlugin, false, false)
			} else {
				entry, err = svc.getClientEntry("", 42, 1, service.HTTPUpstreamProfileOpenAIPlugin, false, false)
			}
			require.NoError(t, err)
			base := &http.Transport{}
			defer base.CloseIdleConnections()
			entry.client.Transport = codexFixtureTransport{target, base}
			for i := 0; i < 2; i++ {
				req := codexFixtureRequest(t, t.Context(), body)
				req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
					GotConn: func(httptrace.GotConnInfo) { connections.Add(1) },
				}))
				var resp *http.Response
				if useTLS {
					resp, err = svc.DoWithTLS(req, "", 42, 1, profile)
				} else {
					resp, err = svc.Do(req, "", 42, 1)
				}
				require.NoError(t, err)
				got, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				require.True(t, bytes.Contains(got, []byte("data: fixture")))
			}
			require.EqualValues(t, 2, calls.Load())
			require.EqualValues(t, 2, connections.Load(), "existing httptrace must remain composed")
			require.Zero(t, atomic.LoadInt64(&entry.inFlight))
			req := codexFixtureRequest(t, t.Context(), body)
			req.Header.Set("Idempotency-Key", "fixture-do-not-replay")
			if useTLS {
				_, err = svc.DoWithTLS(req, "", 42, 1, profile)
			} else {
				_, err = svc.Do(req, "", 42, 1)
			}
			require.Error(t, err)
			require.EqualValues(t, 3, calls.Load(), "a dispatched request must not be replayed")
			require.Zero(t, atomic.LoadInt64(&entry.inFlight))
		})
	}
}
