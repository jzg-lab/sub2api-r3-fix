package repository

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Codex 0.162.1 defaults to zstd level 3 for ChatGPT backend JSON requests.
// Restrict this to the native endpoint, not third-party or API-key providers.
func isCodexBackendRequest(req *http.Request) bool {
	if req == nil || req.URL == nil || req.Method != http.MethodPost ||
		req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Hostname(), "chatgpt.com") ||
		(req.URL.Port() != "" && req.URL.Port() != "443") || req.Body == nil || req.Body == http.NoBody {
		return false
	}
	profile := service.HTTPUpstreamProfileFromContext(req.Context())
	if profile != service.HTTPUpstreamProfileOpenAI && profile != service.HTTPUpstreamProfileOpenAIPlugin {
		return false
	}
	if req.URL.Path != "/backend-api/codex/responses" && req.URL.Path != "/backend-api/codex/responses/compact" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

type codexRequestTrace struct {
	mu                             sync.Mutex
	start, ready, got, sent, first time.Time
	inputBytes, wireBytes          int64
	reused                         bool
	accountID                      int64
	clientID                       uint64
	requestID, clientRequestID     string
	idleReturns, idleAccepted      int
	idleMilliseconds               int64
	upload                         *codexUploadGuard
}

type codexContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r codexContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *httpUpstreamService) prepareCodexRequest(req *http.Request) (*http.Request, *codexRequestTrace, error) {
	if !isCodexBackendRequest(req) {
		return req, nil, nil
	}
	trace := &codexRequestTrace{start: time.Now(), inputBytes: req.ContentLength, wireBytes: req.ContentLength,
		requestID:       ctxkey.TraceID(req.Context(), ctxkey.RequestID),
		clientRequestID: ctxkey.TraceID(req.Context(), ctxkey.ClientRequestID)}
	req = req.Clone(req.Context())
	req.GetBody = nil
	if req.Header.Get("Content-Encoding") == "" {
		limit := int64(256 << 20)
		if s.cfg != nil && s.cfg.Gateway.MaxBodySize > 0 {
			limit = s.cfg.Gateway.MaxBodySize
		}
		body := req.Body
		var closeOnce sync.Once
		closeBody := func() { closeOnce.Do(func() { _ = body.Close() }) }
		defer closeBody()
		stop := context.AfterFunc(req.Context(), closeBody)
		defer stop()
		if req.ContentLength > limit {
			return nil, nil, errors.New("codex request body exceeds configured limit")
		}
		var encoded bytes.Buffer
		encoder, err := idleCodexEncoders.acquire(&encoded)
		if err != nil {
			return nil, nil, errors.New("codex request compression initialization failed")
		}
		n, readErr := io.Copy(encoder, io.LimitReader(codexContextReader{req.Context(), body}, limit+1))
		closeErr := encoder.Close()
		idleCodexEncoders.release(encoder)
		if err := req.Context().Err(); err != nil {
			return nil, nil, err
		}
		if readErr != nil || closeErr != nil || n > limit || (req.ContentLength > 0 && req.ContentLength != n) {
			return nil, nil, errors.New("codex request body preparation failed")
		}
		req.Body = io.NopCloser(bytes.NewReader(encoded.Bytes()))
		req.ContentLength = int64(encoded.Len())
		req.Header.Set("Content-Encoding", "zstd")
		req.Header.Del("Content-Length")
		req.TransferEncoding = nil
		// An interrupted upload must never be replayed on another connection.
		req.GetBody = nil
		trace.inputBytes, trace.wireBytes = n, req.ContentLength
	}
	trace.ready = time.Now()
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			trace.mu.Lock()
			defer trace.mu.Unlock()
			trace.got, trace.reused = time.Now(), info.Reused
			trace.idleMilliseconds = info.IdleTime.Milliseconds()
		},
		PutIdleConn: func(err error) {
			trace.mu.Lock()
			defer trace.mu.Unlock()
			trace.idleReturns++
			if err == nil {
				trace.idleAccepted++
			}
			slog.Info("codex_upstream_pool_return", "account_id", trace.accountID,
				"client_id", trace.clientID, "request_id", trace.requestID,
				"client_request_id", trace.clientRequestID, "outcome", codexIdleReturnOutcome(err))
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			trace.mu.Lock()
			defer trace.mu.Unlock()
			if info.Err == nil {
				trace.sent = time.Now()
			}
		},
		GotFirstResponseByte: func() {
			trace.mu.Lock()
			defer trace.mu.Unlock()
			trace.first = time.Now()
		},
	}))
	return req, trace, nil
}

func (trace *codexRequestTrace) bindClient(accountID int64, clientID uint64) {
	if trace != nil {
		trace.accountID, trace.clientID = accountID, clientID
	}
}

func codexIdleReturnOutcome(err error) string {
	if err == nil {
		return "accepted"
	}
	switch err.Error() {
	case "http: putIdleConn: keep alives disabled":
		return "disabled"
	case "http: putIdleConn: connection is in bad state":
		return "broken"
	case "http: putIdleConn: too many idle connections":
		return "pool_full"
	case "http: putIdleConn: too many idle connections for host":
		return "host_pool_full"
	case "http: putIdleConn: CloseIdleConnections was called":
		return "pool_closed"
	default:
		return "rejected"
	}
}

func (trace *codexRequestTrace) recordBodyClose() {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	slog.Info("codex_upstream_body_closed", "account_id", trace.accountID,
		"client_id", trace.clientID, "request_id", trace.requestID,
		"client_request_id", trace.clientRequestID, "idle_returns", trace.idleReturns,
		"idle_accepted", trace.idleAccepted)
}

func (trace *codexRequestTrace) record(accountID int64, resp *http.Response, err error) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	millis := func(start, end time.Time) int64 {
		if start.IsZero() || end.IsZero() || end.Before(start) {
			return -1
		}
		return end.Sub(start).Milliseconds()
	}
	status, outcome := 0, "headers"
	media, encoding, protocol, closeConnection := "missing", "identity", "unknown", false
	if resp != nil {
		status = resp.StatusCode
		closeConnection = resp.Close
		switch resp.Proto {
		case "HTTP/1.0", "HTTP/1.1", "HTTP/2.0":
			protocol = resp.Proto
		}
		switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
		case "", "identity":
		case "gzip", "br", "zstd", "deflate":
			encoding = strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
		default:
			encoding = "other"
		}
		kind, _, parseErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		switch {
		case parseErr == nil && kind == "text/event-stream":
			media = "sse"
		case parseErr == nil && kind == "application/json":
			media = "json"
		case resp.Header.Get("Content-Type") != "":
			media = "other"
		}
	}
	if err != nil {
		outcome = "transport_error"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = "canceled"
		}
	}
	now := time.Now()
	uploadReadBytes, uploadIdleMS, uploadExpired := int64(-1), int64(-1), false
	if trace.upload != nil {
		uploadReadBytes, uploadIdleMS, uploadExpired = trace.upload.observation()
	}
	phase, phaseStart := "prepare", trace.start
	switch {
	case !trace.first.IsZero():
		phase, phaseStart = "headers_received", trace.first
	case !trace.sent.IsZero():
		phase, phaseStart = "response_headers", trace.sent
	case !trace.got.IsZero():
		phase, phaseStart = "upload", trace.got
	case !trace.ready.IsZero():
		phase, phaseStart = "connection", trace.ready
	}
	// No URL, headers, prompt, credential, response body or raw error is logged.
	slog.Info("codex_upstream_headers", "account_id", accountID, "outcome", outcome,
		"client_id", trace.clientID, "request_id", trace.requestID, "client_request_id", trace.clientRequestID,
		"phase", phase, "phase_elapsed_ms", millis(phaseStart, now), "idle_ms", trace.idleMilliseconds,
		"status", status, "input_bytes", trace.inputBytes, "wire_bytes", trace.wireBytes,
		"upload_body_read_bytes", uploadReadBytes, "upload_idle_ms", uploadIdleMS,
		"upload_guard_expired", uploadExpired,
		"prepare_ms", millis(trace.start, trace.ready), "connection_ms", millis(trace.ready, trace.got),
		"write_ms", millis(trace.got, trace.sent), "header_wait_ms", millis(trace.sent, trace.first),
		"total_ms", millis(trace.start, now), "connection_reused", trace.reused,
		"response_media", media, "response_encoding", encoding, "protocol", protocol, "connection_close", closeConnection)
}
