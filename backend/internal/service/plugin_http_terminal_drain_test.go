package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestPluginHTTPProgressSeparatesArrivalAndForwarding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
		defer slog.SetDefault(previous)
		const id = "E2D5E88A-84CB-46EE-83E3-586131CA5745"
		ctx := context.WithValue(t.Context(), ctxkey.RequestID, id)
		ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "fixture-private-marker")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://fixture.invalid/backend-api/codex/responses", nil)
		require.NoError(t, err)
		response := &http.Response{StatusCode: 200}
		observer := newPluginHTTPTerminalObserver(req, response)
		observer.bind(req, time.Now())
		time.Sleep(time.Second)
		require.False(t, observer.write([]byte("data: {\"type\":\"response.created\"}\n\n")))
		observer.forwarded()
		time.Sleep(time.Second)
		require.False(t, observer.write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture-private-marker\"}\n")))
		require.True(t, observer.firstSemantic.IsZero(), "incomplete frames do not start a semantic timer")
		time.Sleep(5 * time.Second)
		require.False(t, observer.write([]byte("\n")))
		time.Sleep(10 * time.Millisecond)
		observer.forwarded()
		time.Sleep(3 * time.Second)
		require.True(t, observer.write([]byte(pluginHTTPCompletedFixture)))
		observer.forwarded()
		observer.record(42, response, true, 123)
		var event map[string]any
		require.NoError(t, json.Unmarshal(output.Bytes(), &event))
		require.Equal(t, id, event["request_id"])
		require.Empty(t, event["client_request_id"])
		require.EqualValues(t, 0, event["headers_plugin_send_ms"])
		require.EqualValues(t, 1000, event["first_body_ms"])
		require.EqualValues(t, 7000, event["first_semantic_ms"])
		require.EqualValues(t, 7000, event["first_visible_ms"])
		require.EqualValues(t, 7010, event["semantic_plugin_send_ms"])
		require.EqualValues(t, 7010, event["visible_plugin_send_ms"])
		require.NotContains(t, output.String(), "fixture-private-marker")
	})
}

func TestPluginHTTPProgressSemanticDoesNotInventVisibleOrSent(t *testing.T) {
	o := &pluginHTTPTerminalObserver{started: time.Now()}
	require.False(t, o.write([]byte(
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"arguments\":\"\"}}\n\n")))
	require.False(t, o.firstSemantic.IsZero())
	require.True(t, o.firstVisible.IsZero())
	require.True(t, o.semanticSent.IsZero(), "arrival does not prove Send succeeded")
	require.True(t, o.visibleSent.IsZero())
	o.forwarded()
	require.False(t, o.semanticSent.IsZero())
	require.True(t, o.visibleSent.IsZero())
}

func TestPluginHTTPProgressRejectsIncompleteOrUntrustedFrames(t *testing.T) {
	for _, data := range []string{
		": keepalive\n\n",
		"data: {broken}\n\n",
		"event: response.created\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n",
		"data: " + strings.Repeat("x", pluginHTTPTerminalFrameLimit) + "\n\n",
		"data: {\"type\":\"response.created\"}\n\n",
	} {
		o := &pluginHTTPTerminalObserver{started: time.Now()}
		require.False(t, o.write([]byte(data)))
		o.forwarded()
		require.True(t, o.firstSemantic.IsZero())
		require.True(t, o.firstVisible.IsZero())
		require.True(t, o.semanticSent.IsZero())
		require.True(t, o.visibleSent.IsZero())
	}
	var absent *pluginHTTPTerminalObserver
	absent.bind(nil, time.Now())
	absent.forwarded()
}

func TestPluginHTTPTerminalFiniteSummary(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	observer := &pluginHTTPTerminalObserver{}
	require.False(t, observer.write([]byte("data: "+strings.Repeat("x", pluginHTTPTerminalFrameLimit)+"\n\n")))
	require.False(t, observer.write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"private\":\"fixture-private-marker\"}}\n\n")))
	require.True(t, observer.write([]byte(pluginHTTPCompletedFixture)))
	observer.record(42, &http.Response{StatusCode: 200, Close: true}, false, 123)
	require.NotContains(t, output.String(), "fixture-private-marker")
	var event map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &event))
	require.Equal(t, true, event["eligible"])
	require.Equal(t, true, event["settled"])
	require.EqualValues(t, 2, event["completed_frames"])
	require.EqualValues(t, 1, event["missing_status"])
	require.EqualValues(t, 1, event["oversized_frames"])
	require.Equal(t, true, event["connection_close"])
	output.Reset()
	var absent *pluginHTTPTerminalObserver
	absent.record(42, &http.Response{StatusCode: 200}, true, 123)
	require.NoError(t, json.Unmarshal(output.Bytes(), &event))
	require.Equal(t, false, event["eligible"])
	require.Equal(t, true, event["eof"])
}

const pluginHTTPCompletedFixture = "event: response.completed\ndata: " +
	`{"type":"response.completed","response":{"status":"completed"}}` + "\n\n"

func TestPluginHTTPTerminalObserver(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		want       bool
	}{
		{"completed", pluginHTTPCompletedFixture, true},
		{"crlf", strings.ReplaceAll(pluginHTTPCompletedFixture, "\n", "\r\n"), true},
		{"data_only", strings.TrimPrefix(pluginHTTPCompletedFixture, "event: response.completed\n"), true},
		{"truncated", strings.TrimSuffix(pluginHTTPCompletedFixture, "\n"), false},
		{"wrong_event", strings.Replace(pluginHTTPCompletedFixture, "event: response.completed", "event: response.failed", 1), false},
		{"not_completed", strings.Replace(pluginHTTPCompletedFixture, `"status":"completed"`, `"status":"in_progress"`, 1), false},
		{"bad_json", "data: {\"type\":\"response.completed\" broken}\n\n", false},
		{"comment", ": response.completed\n\n", false},
		{"done_marker", "data: [DONE]\n\n", false},
		{"oversized", "data: " + strings.Repeat("x", pluginHTTPTerminalFrameLimit) + "\n\n", false},
		{"after_oversized", "data: " + strings.Repeat("x", pluginHTTPTerminalFrameLimit) + "\n\n" + pluginHTTPCompletedFixture, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{1, 7, 32768} {
				observer := &pluginHTTPTerminalObserver{}
				found := false
				for i := 0; i < len(tc.data); i += size {
					found = observer.write([]byte(tc.data[i:min(i+size, len(tc.data))])) || found
					require.LessOrEqual(t, len(observer.frame), pluginHTTPTerminalFrameLimit)
				}
				require.Equal(t, tc.want, found, "chunk size %d", size)
				if found {
					require.False(t, observer.write([]byte(pluginHTTPCompletedFixture)), "drain only once")
				}
			}
		})
	}
	var absent *pluginHTTPTerminalObserver
	require.False(t, absent.write([]byte(pluginHTTPCompletedFixture)))
}

func TestPluginHTTPTerminalObserverScope(t *testing.T) {
	for _, tc := range []struct {
		path, method, media string
		status              int
		want                bool
	}{
		{"/backend-api/codex/responses", "POST", "text/event-stream; charset=utf-8", 200, true},
		{"/backend-api/codex/responses", "POST", "", 200, true},
		{"/backend-api/codex/responses", "POST", "text/event-stream; broken", 200, false},
		{"/backend-api/codex/responses/compact", "POST", "text/event-stream", 200, false},
		{"/backend-api/codex/responses", "GET", "text/event-stream", 200, false},
		{"/backend-api/codex/responses", "POST", "application/json", 200, false},
		{"/backend-api/codex/responses", "POST", "text/event-stream", 429, false},
		{"/backend-api/codex/responses", "POST", "", 429, false},
	} {
		req, err := http.NewRequest(http.MethodPost, "https://fixture.invalid"+tc.path, nil)
		require.NoError(t, err)
		req.Method = tc.method
		resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.media}}}
		require.Equal(t, tc.want, newPluginHTTPTerminalObserver(req, resp) != nil)
	}
	require.Nil(t, newPluginHTTPTerminalObserver(nil, nil))
}

func TestPluginHTTPTerminalTailBounds(t *testing.T) {
	t.Run("byte_exact_eof", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		tail, err := readPluginHTTPTerminalTail(io.NopCloser(strings.NewReader(": tail\n\n")), time.Second, 42, cancel)
		require.ErrorIs(t, err, io.EOF)
		require.Equal(t, ": tail\n\n", string(tail))
		require.NoError(t, ctx.Err(), "normal EOF must preserve the reusable connection")
	})
	t.Run("size_limit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		tail, err := readPluginHTTPTerminalTail(io.NopCloser(strings.NewReader(strings.Repeat("x", pluginHTTPTerminalTailLimit+2))), time.Second, 42, cancel)
		require.ErrorIs(t, err, errPluginHTTPTerminalTailLimit)
		require.Len(t, tail, pluginHTTPTerminalTailLimit+1)
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	})
	t.Run("deadline", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer writer.Close()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stop := context.AfterFunc(ctx, func() { _ = writer.CloseWithError(ctx.Err()) })
		defer stop()
		start := time.Now()
		_, err := readPluginHTTPTerminalTail(reader, 10*time.Millisecond, 42, cancel)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Less(t, time.Since(start), time.Second)
	})
	t.Run("cancel", func(t *testing.T) {
		reader, writer := io.Pipe()
		_, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, writer.CloseWithError(context.Canceled))
		_, err := readPluginHTTPTerminalTail(reader, time.Second, 42, cancel)
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("upstream_error", func(t *testing.T) {
		reader, writer := io.Pipe()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		failure := errors.New("fixture read failure")
		require.NoError(t, writer.CloseWithError(failure))
		_, err := readPluginHTTPTerminalTail(reader, time.Second, 42, cancel)
		require.ErrorIs(t, err, failure)
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	})
}

// Closing a decoding wrapper need not interrupt its active transport read.
// Only cancellation of the request reliably releases this reader.
type pluginHTTPCancelOnlyBody struct {
	ctx context.Context
}

func (b *pluginHTTPCancelOnlyBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *pluginHTTPCancelOnlyBody) Close() error { return nil }

func TestPluginHTTPTerminalTailCancelsBlockedTransportRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := readPluginHTTPTerminalTail(&pluginHTTPCancelOnlyBody{ctx: ctx}, 20*time.Millisecond, 42, cancel)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 300*time.Millisecond,
		"the terminal drain must cancel its request, not wait for the parent deadline")
}
