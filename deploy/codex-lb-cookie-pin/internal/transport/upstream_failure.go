package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

type upstreamFailureTrace struct {
	mu        sync.Mutex
	tlsError  error
	connected bool
}

func traceUpstreamRequest(request *http.Request) (*http.Request, *upstreamFailureTrace) {
	trace := &upstreamFailureTrace{}
	ctx := httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				trace.mu.Lock()
				trace.connected = true
				trace.mu.Unlock()
			}
		},
		GotConn: func(httptrace.GotConnInfo) {
			trace.mu.Lock()
			trace.connected = true
			trace.mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			trace.mu.Lock()
			trace.tlsError = err
			trace.mu.Unlock()
		},
	})
	return request.WithContext(ctx), trace
}

func (trace *upstreamFailureTrace) definitelyNotConnected(err error) bool {
	if trace == nil {
		return false
	}
	trace.mu.Lock()
	connected := trace.connected
	trace.mu.Unlock()
	if connected {
		return false
	}
	_, phase := classifyUpstreamFailure(err, nil)
	return phase == "dns" || phase == "connect" || phase == "proxy_connect"
}

func (trace *upstreamFailureTrace) classify(err, contextErr error) (class, phase string) {
	class, phase = classifyUpstreamFailure(err, contextErr)
	if trace != nil && phase == "unknown" {
		trace.mu.Lock()
		tlsError := trace.tlsError
		trace.mu.Unlock()
		// A callback from another connection must not label this failure.
		if tlsError != nil && errors.Is(err, tlsError) {
			phase = "tls"
		}
	}
	return class, phase
}

// Preserve only finite classifications. Raw transport errors can contain
// credential-bearing proxy URLs, upstream URLs and private certificate names.
func classifyUpstreamFailure(err, contextErr error) (class, phase string) {
	phase = "unknown"
	var op *net.OpError
	if errors.As(err, &op) {
		switch op.Op {
		case "dial":
			phase = "connect"
		case "socks connect":
			phase = "proxy_connect"
		case "read", "write":
			phase = op.Op
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "request_canceled", phase
	case errors.Is(err, context.DeadlineExceeded) && errors.Is(contextErr, context.DeadlineExceeded):
		return "request_deadline", phase
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.Timeout() {
			return "dns_timeout", "dns"
		}
		return "dns_failure", "dns"
	}
	var certificate *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &certificate) || errors.As(err, &authority) ||
		errors.As(err, &hostname) || errors.As(err, &invalid) {
		return "tls_certificate", "tls"
	}
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return "tls_protocol", "tls"
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "network_timeout", phase
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused", phase
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset", phase
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe", phase
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection_eof", phase
	default:
		return "connection_error", phase
	}
}

func upstreamContextState(err error) string {
	switch {
	case err == nil:
		return "active"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "unknown"
	}
}

func logUpstreamFailure(logger *slog.Logger, accountID int64, operation, proxyRaw string, started time.Time, err, contextErr error, trace *upstreamFailureTrace) string {
	class, phase := trace.classify(err, contextErr)
	contextState := upstreamContextState(contextErr)
	switch operation {
	case "forward_connect", "forward_body", "probe_connect", "probe_body":
	default:
		operation = "unknown"
	}
	if phase == "unknown" && (operation == "forward_body" || operation == "probe_body") {
		phase = "response_body"
	}
	scheme := "invalid"
	if strings.TrimSpace(proxyRaw) == "" {
		scheme = "direct"
	} else if parsed, parseErr := url.Parse(proxyRaw); parseErr == nil {
		switch parsed.Scheme {
		case "http", "https", "socks5", "socks5h":
			scheme = parsed.Scheme
		}
	}
	level := slog.LevelWarn
	if class == "request_canceled" {
		level = slog.LevelInfo
	}
	logger.Log(context.Background(), level, "upstream_transport_failure",
		"account_id", accountID,
		"operation", operation,
		"error_class", class,
		"phase", phase,
		"context_state", contextState,
		"proxy_scheme", scheme,
		"elapsed_ms", max(time.Since(started).Milliseconds(), 0))
	// The host retains this finite suffix under its original request ID.
	// ForwardRequestStart.RequestId is a different, plugin-local identifier.
	return " [error_class=" + class + " phase=" + phase + " context_state=" + contextState + "]"
}
