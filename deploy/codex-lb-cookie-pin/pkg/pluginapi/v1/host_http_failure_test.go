package pluginv1

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

func TestHostHTTPFiniteFailureRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, class, phase string
		err, contextErr    error
		body               bool
	}{
		{"dns", "dns_failure", "dns", &net.DNSError{Err: "opaque", Name: "private.invalid"}, nil, false},
		{"dns_timeout", "dns_timeout", "dns", &net.DNSError{Err: "opaque", IsTimeout: true}, nil, false},
		{"proxy_timeout", "network_timeout", "proxy_connect", &net.OpError{Op: "socks connect", Err: context.DeadlineExceeded}, nil, false},
		{"refused", "connection_refused", "connect", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, nil, false},
		{"reset", "connection_reset", "read", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, nil, true},
		{"pipe", "broken_pipe", "write", &net.OpError{Op: "write", Err: syscall.EPIPE}, nil, true},
		{"certificate", "tls_certificate", "tls", x509.UnknownAuthorityError{}, nil, false},
		{"tls_protocol", "tls_protocol", "tls", tls.RecordHeaderError{}, nil, false},
		{"cancel", "request_canceled", "unknown", context.Canceled, context.Canceled, false},
		{"deadline", "request_deadline", "unknown", context.DeadlineExceeded, context.DeadlineExceeded, false},
		{"reset_then_cancel", "connection_reset", "unknown", syscall.ECONNRESET, context.Canceled, false},
		{"eof_then_deadline", "connection_eof", "response_body", io.ErrUnexpectedEOF, context.DeadlineExceeded, true},
		{"unknown", "connection_error", "unknown", errors.New("opaque diagnostic"), nil, false},
		{"unknown_body", "connection_error", "response_body", errors.New("opaque diagnostic"), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := &url.Error{Op: "Post", URL: "https://private.invalid/opaque", Err: tc.err}
			code := HostHTTPFailureCode(wrapped, tc.contextErr, tc.body)
			class, phase, ok := DecodeHostHTTPFailureCode(code)
			if !ok || class != tc.class || phase != tc.phase {
				t.Fatalf("got %q/%q/%v; expected %q/%q", class, phase, ok, tc.class, tc.phase)
			}
			if strings.Contains(code, "private") || strings.Contains(code, "opaque") {
				t.Fatal("non-finite error details escaped into protocol code")
			}
		})
	}
}

func TestHostHTTPFiniteFailureRejectsUnknown(t *testing.T) {
	for _, code := range []string{
		"", "HOST_HTTP_UPSTREAM/opaque", "HOST_HTTP_TRANSPORT/opaque/read",
		"HOST_HTTP_TRANSPORT/connection_error/opaque",
		"HOST_HTTP_TRANSPORT/connection_error/read/extra",
		"HOST_HTTP_TRANSPORT/connection_error/read\n",
		"HOST_HTTP_TRANSPORT//read", strings.Repeat("x", 129),
	} {
		if _, _, ok := DecodeHostHTTPFailureCode(code); ok {
			t.Fatal("unrecognized protocol error accepted")
		}
	}
}

func TestHostHTTPFiniteFailureLegacy(t *testing.T) {
	for _, code := range []string{
		"HOST_HTTP_BINDING", "HOST_HTTP_DRAINING", "HOST_HTTP_REQUEST",
		"HOST_HTTP_UPSTREAM", "HOST_HTTP_STREAM", "HOST_HTTP_RPC_OPEN",
		"HOST_HTTP_RPC_SEND", "HOST_HTTP_RPC_RECV", "HOST_HTTP_PROTOCOL",
	} {
		class, phase, ok := DecodeHostHTTPFailureCode(code)
		if !ok || !ValidTransportFailureClassification(class, phase) {
			t.Fatalf("legacy code %s was not retained", code)
		}
	}
}
