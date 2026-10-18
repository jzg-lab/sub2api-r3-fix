package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/tidwall/gjson"
)

const (
	pluginHTTPTerminalFrameLimit = 256 << 10
	pluginHTTPTerminalTailLimit  = 64 << 10
	pluginHTTPTerminalDrainTime  = 200 * time.Millisecond
)

var errPluginHTTPTerminalTailLimit = errors.New("terminal HTTP tail exceeds limit")

// Hold only a complete successful terminal while consuming the final HTTP chunk.
// Otherwise the plugin cancels its RPC before net/http can return the connection
// to the account/proxy pool. Ordinary output is never buffered by this observer.
type pluginHTTPTerminalObserver struct {
	frame                                                        []byte
	lineBytes                                                    int
	lineLast                                                     byte
	discard                                                      bool
	settled                                                      bool
	frames                                                       int
	completed                                                    int
	missingStatus                                                int
	oversized                                                    int
	started, headersSent, firstBody, firstSemantic, firstVisible time.Time
	semanticSent, visibleSent                                    time.Time
	requestID, clientRequestID                                   string
}

func newPluginHTTPTerminalObserver(req *http.Request, resp *http.Response) *pluginHTTPTerminalObserver {
	if req == nil || req.URL == nil || req.Method != http.MethodPost ||
		req.URL.Path != "/backend-api/codex/responses" || resp == nil ||
		resp.StatusCode != http.StatusOK {
		return nil
	}
	// Codex can omit Content-Type. Still require a complete successful SSE
	// frame before draining; explicit non-SSE media never enters this path.
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		media, _, err := mime.ParseMediaType(contentType)
		if err != nil || media != "text/event-stream" {
			return nil
		}
	}
	return &pluginHTTPTerminalObserver{}
}

func (o *pluginHTTPTerminalObserver) bind(req *http.Request, started time.Time) {
	if o == nil {
		return
	}
	o.started = started
	o.headersSent = time.Now()
	o.requestID = ctxkey.TraceID(req.Context(), ctxkey.RequestID)
	o.clientRequestID = ctxkey.TraceID(req.Context(), ctxkey.ClientRequestID)
}

func (o *pluginHTTPTerminalObserver) write(chunk []byte) bool {
	if o == nil || o.settled {
		return false
	}
	if len(chunk) > 0 && o.firstBody.IsZero() {
		o.firstBody = time.Now()
	}
	for _, b := range chunk {
		if !o.discard {
			if len(o.frame) == pluginHTTPTerminalFrameLimit {
				o.frame = nil
				o.discard = true
				o.oversized++
			} else {
				o.frame = append(o.frame, b)
			}
		}
		if b != '\n' {
			o.lineBytes++
			o.lineLast = b
			continue
		}
		blank := o.lineBytes == 0 || o.lineBytes == 1 && o.lineLast == '\r'
		o.lineBytes = 0
		if !blank {
			continue
		}
		if !o.discard {
			o.frames++
			o.observeProgress(o.frame)
			completed, hasStatus, successful := pluginHTTPFrameCompletion(o.frame)
			if completed {
				o.completed++
				if !hasStatus {
					o.missingStatus++
				}
			}
			if successful {
				o.settled = true
				o.frame = nil
				return true
			}
		}
		o.frame = o.frame[:0]
		o.discard = false
	}
	return false
}

func (o *pluginHTTPTerminalObserver) observeProgress(raw []byte) {
	if !o.firstSemantic.IsZero() && !o.firstVisible.IsZero() {
		return
	}
	var parser openAICompatSSEFrameParser
	for _, line := range strings.Split(string(raw), "\n") {
		frame, ok := parser.AddLine(strings.TrimSuffix(line, "\r"))
		if !ok || !gjson.Valid(frame.Data) {
			continue
		}
		kind := gjson.Get(frame.Data, "type").String()
		if frame.EventType != "" && frame.EventType != kind {
			continue
		}
		now := time.Now()
		if o.firstSemantic.IsZero() && openAIStreamDataStartsSemanticTTFT(frame.Data, kind) {
			o.firstSemantic = now
		}
		if o.firstVisible.IsZero() && openAIStreamDataStartsVisibleOutput(frame.Data, kind) {
			o.firstVisible = now
		}
	}
}

func (o *pluginHTTPTerminalObserver) forwarded() {
	if o == nil {
		return
	}
	if !o.firstSemantic.IsZero() && o.semanticSent.IsZero() {
		o.semanticSent = time.Now()
	}
	if !o.firstVisible.IsZero() && o.visibleSent.IsZero() {
		o.visibleSent = time.Now()
	}
}

func pluginHTTPFrameCompletion(raw []byte) (completed, hasStatus, successful bool) {
	var parser openAICompatSSEFrameParser
	for _, line := range strings.Split(string(raw), "\n") {
		frame, ok := parser.AddLine(strings.TrimSuffix(line, "\r"))
		if !ok || !gjson.Valid(frame.Data) {
			continue
		}
		kind := gjson.Get(frame.Data, "type").String()
		if frame.EventType != "" && frame.EventType != kind {
			continue
		}
		if kind == "response.completed" || kind == "response.done" {
			status := gjson.Get(frame.Data, "response.status")
			return true, status.Exists(), status.String() == "completed"
		}
	}
	return false, false, false
}

func (o *pluginHTTPTerminalObserver) record(accountID int64, response *http.Response, eof bool, received int64) {
	fields := []any{"account_id", accountID, "eligible", o != nil, "eof", eof,
		"bytes", received, "status", response.StatusCode, "connection_close", response.Close}
	if o != nil {
		millis := func(end time.Time) int64 {
			if o.started.IsZero() || end.IsZero() || end.Before(o.started) {
				return -1
			}
			return end.Sub(o.started).Milliseconds()
		}
		fields = append(fields, "settled", o.settled, "frames", o.frames,
			"completed_frames", o.completed, "missing_status", o.missingStatus, "oversized_frames", o.oversized,
			"request_id", o.requestID, "client_request_id", o.clientRequestID,
			"headers_plugin_send_ms", millis(o.headersSent),
			"first_body_ms", millis(o.firstBody), "first_semantic_ms", millis(o.firstSemantic),
			"first_visible_ms", millis(o.firstVisible), "semantic_plugin_send_ms", millis(o.semanticSent),
			"visible_plugin_send_ms", millis(o.visibleSent))
	}
	slog.Info("plugin_http_stream_summary", fields...)
}

func readPluginHTTPTerminalTail(body io.ReadCloser, budget time.Duration, accountID int64, cancelRequest context.CancelFunc) ([]byte, error) {
	started := time.Now()
	var timedOut atomic.Bool
	canceled := make(chan struct{})
	timer := time.AfterFunc(budget, func() {
		timedOut.Store(true)
		// A decoding wrapper's Close may not interrupt a concurrent Read.
		// Cancel only this request, then close the body after the reader exits.
		cancelRequest()
		close(canceled)
	})
	var tail bytes.Buffer
	_, err := io.CopyN(&tail, body, pluginHTTPTerminalTailLimit+1)
	if !timer.Stop() {
		<-canceled
	}
	outcome := "eof"
	if timedOut.Load() {
		outcome = "deadline"
		err = context.DeadlineExceeded
		_ = body.Close()
	} else if err == nil {
		outcome = "limit"
		err = errPluginHTTPTerminalTailLimit
		cancelRequest()
		_ = body.Close()
	} else if !errors.Is(err, io.EOF) {
		outcome = "read_error"
		cancelRequest()
		_ = body.Close()
	}
	slog.Info("plugin_http_terminal_drain", "account_id", accountID, "outcome", outcome,
		"bytes", tail.Len(), "elapsed_ms", time.Since(started).Milliseconds())
	return tail.Bytes(), err
}
