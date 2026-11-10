package service

import (
	"context"
	"encoding/json"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCaptureReadCloserRecordsOnlyCompleteReads(t *testing.T) {
	reader := &captureReadCloser{ReadCloser: io.NopCloser(strings.NewReader("raw upstream body"))}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.complete || reader.tooLarge || string(data) != reader.buffer.String() {
		t.Fatalf("capture = complete:%v tooLarge:%v body:%q", reader.complete, reader.tooLarge, reader.buffer.String())
	}

	partial := &captureReadCloser{ReadCloser: &failingReadCloser{Reader: strings.NewReader("partial")}}
	if _, err := io.ReadAll(partial); err == nil {
		t.Fatal("ReadAll unexpectedly succeeded")
	}
	if partial.complete {
		t.Fatal("capture marked a failed read as complete")
	}
}

type failingReadCloser struct{ io.Reader }

func (*failingReadCloser) Close() error { return nil }
func (r *failingReadCloser) Read(p []byte) (int, error) {
	n, _ := r.Reader.Read(p)
	if n > 0 {
		return n, nil
	}
	return 0, io.ErrUnexpectedEOF
}

func TestCaptureRequiresSuccessfulCompleteResponse(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		success, eof, want      bool
	}{
		{"json", "application/json", `{"output":"ok"}`, true, true, true},
		{"json-error-null", "application/json", `{"error":null,"status":"completed","output":[]}`, true, true, true},
		{"json-truncated", "application/json", `{"output":`, true, true, false},
		{"json-error", "application/json", `{"error":{"message":"no"}}`, true, true, false},
		{"json-no-eof", "application/json", `{"output":"ok"}`, true, false, false},
		{"forward-failed", "application/json", `{"output":"ok"}`, false, true, false},
		{"claude", "text/event-stream", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", true, false, true},
		{"responses", "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", true, false, true},
		{"chat", "text/event-stream", "data: [DONE]\n\n", true, false, true},
		{"sse-incomplete", "text/event-stream", "data: {\"type\":\"response.incomplete\"}\n\n", true, true, false},
		{"sse-no-terminal", "text/event-stream", "data: {\"type\":\"content_block_delta\"}\n\n", true, true, false},
		{"sse-error", "text/event-stream", "event: error\ndata: {\"error\":{}}\n\ndata: [DONE]\n\n", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewPluginManager(nil, nil, nil, PluginHostInfo{})
			runtime := &pluginRuntime{}
			m.captureRuntime.Store(runtime)
			ctx, finish := m.beginCapture(context.Background(), nil, &Account{ID: 7, Platform: PlatformOpenAI})
			req, err := http.NewRequestWithContext(ctx, "POST", "http://example.test/v1/responses", strings.NewReader(`{ "model":"upstream-model" }`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer must-not-save")
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body))}
			captureHTTPResponse(req, resp, nil)
			if tc.eof {
				_, err = io.ReadAll(resp.Body)
			} else {
				_, err = io.ReadFull(resp.Body, make([]byte, len(tc.body)))
			}
			require.NoError(t, err)
			finish(tc.success)
			require.Equal(t, tc.want, len(m.captureQueue) == 1)
			if tc.want {
				item := <-m.captureQueue
				defer item.release()
				var record map[string]any
				require.NoError(t, json.Unmarshal(item.request.RecordJson, &record))
				require.Equal(t, tc.body, record["response"])
				require.Equal(t, `{ "model":"upstream-model" }`, record["request"])
				require.NotContains(t, string(item.request.RecordJson), "must-not-save")
			}
		})
	}
}

func TestCaptureSkipsCancelledProbeAndBudgetExhaustion(t *testing.T) {
	for _, name := range []string{"cancelled", "probe", "budget", "replaced", "disabled", "failed-status"} {
		t.Run(name, func(t *testing.T) {
			m := NewPluginManager(nil, nil, nil, PluginHostInfo{})
			runtime := &pluginRuntime{}
			m.captureRuntime.Store(runtime)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "probe" {
				base = pluginv1.WithProbeTraffic(base)
			}
			if name == "budget" {
				var release func()
				base, release = pkghttputil.WithRequestBodyMemoryBudget(base, pkghttputil.NewBodyMemoryBudget(1))
				defer release()
			}
			ctx, finish := m.beginCapture(base, nil, &Account{ID: 1, Platform: PlatformAnthropic})
			req, _ := http.NewRequestWithContext(ctx, "POST", "http://test/v1/messages", strings.NewReader(`{"model":"claude"}`))
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"content":[]}`))}
			if name == "failed-status" {
				resp.StatusCode = 500
			}
			captureHTTPResponse(req, resp, nil)
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, `{"content":[]}`, string(got))
			if name == "cancelled" {
				cancel()
			}
			if name == "replaced" {
				m.captureRuntime.Store(&pluginRuntime{})
			}
			if name == "disabled" {
				m.captureRuntime.Store(nil)
			}
			finish(true)
			require.Empty(t, m.captureQueue)
			require.Empty(t, m.captureSlots)
		})
	}
}

func TestCaptureWorkerShutdownAndQueueBounds(t *testing.T) {
	m := NewPluginManager(nil, nil, nil, PluginHostInfo{})
	runtime := &pluginRuntime{}
	m.captureRuntime.Store(runtime)
	released := 0
	for i := 0; i < 128; i++ {
		require.True(t, m.enqueueCapture(runtime, &pluginv1.CaptureRecordRequest{RecordJson: []byte(`{}`)}, func() { released++ }))
	}
	require.False(t, m.enqueueCapture(runtime, &pluginv1.CaptureRecordRequest{RecordJson: []byte(`{}`)}, func() { t.Error("rejected record release belongs to caller") }))
	require.EqualValues(t, 256, m.captureQueuedBytes.Load())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	m.captureLoop(ctx, done)
	<-done
	require.Equal(t, 128, released)
	require.Zero(t, m.captureQueuedBytes.Load())
	require.Empty(t, m.captureQueue)
}

func TestCapturePublisherCannotSignAnotherPlugin(t *testing.T) {
	require.NotEmpty(t, trustedPluginPublisherKey(nil, "jzg-lab-sub-data-save-v1", "jzg-lab.sub-data-save"))
	require.Empty(t, trustedPluginPublisherKey(nil, "jzg-lab-sub-data-save-v1", "other.plugin"))
}
