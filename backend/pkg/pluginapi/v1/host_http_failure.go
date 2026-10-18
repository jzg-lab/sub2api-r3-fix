package pluginv1

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

const hostHTTPFailurePrefix = "HOST_HTTP_TRANSPORT/"

// HostHTTPFailureCode transports finite diagnostics only. It is not evidence
// that a request was unsent and must never change RequestSent or retry policy.
func HostHTTPFailureCode(err, contextErr error, body bool) string {
	phase := "unknown"
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
	class := "connection_error"
	var dns *net.DNSError
	var certificate *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	var network net.Error
	switch {
	case errors.Is(err, context.Canceled):
		class = "request_canceled"
	case errors.Is(err, context.DeadlineExceeded) && errors.Is(contextErr, context.DeadlineExceeded):
		class = "request_deadline"
	case errors.As(err, &dns):
		class, phase = "dns_failure", "dns"
		if dns.Timeout() {
			class = "dns_timeout"
		}
	case errors.As(err, &certificate), errors.As(err, &authority),
		errors.As(err, &hostname), errors.As(err, &invalid):
		class, phase = "tls_certificate", "tls"
	case errors.As(err, &record):
		class, phase = "tls_protocol", "tls"
	case errors.As(err, &network) && network.Timeout():
		class = "network_timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		class = "connection_refused"
	case errors.Is(err, syscall.ECONNRESET):
		class = "connection_reset"
	case errors.Is(err, syscall.EPIPE):
		class = "broken_pipe"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		class = "connection_eof"
	}
	if body && phase == "unknown" {
		phase = "response_body"
	}
	return hostHTTPFailurePrefix + class + "/" + phase
}

func ValidTransportFailureClassification(class, phase string) bool {
	switch class {
	case "request_canceled", "request_deadline", "dns_timeout", "dns_failure",
		"tls_certificate", "tls_protocol", "network_timeout", "connection_refused",
		"connection_reset", "broken_pipe", "connection_eof", "connection_error",
		"host_binding", "host_draining", "host_request_invalid", "host_rpc_failure":
	default:
		return false
	}
	switch phase {
	case "unknown", "connect", "proxy_connect", "read", "write", "dns", "tls",
		"response_body", "host_rpc", "host_binding", "request":
		return true
	default:
		return false
	}
}

func DecodeHostHTTPFailureCode(code string) (class, phase string, ok bool) {
	switch code {
	case "HOST_HTTP_BINDING":
		return "host_binding", "host_binding", true
	case "HOST_HTTP_DRAINING":
		return "host_draining", "host_rpc", true
	case "HOST_HTTP_REQUEST":
		return "host_request_invalid", "request", true
	case "HOST_HTTP_UPSTREAM":
		return "connection_error", "unknown", true
	case "HOST_HTTP_STREAM":
		return "connection_error", "response_body", true
	case "HOST_HTTP_RPC_OPEN", "HOST_HTTP_RPC_SEND", "HOST_HTTP_RPC_RECV", "HOST_HTTP_PROTOCOL":
		return "host_rpc_failure", "host_rpc", true
	}
	if len(code) > 128 || !strings.HasPrefix(code, hostHTTPFailurePrefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(code, hostHTTPFailurePrefix), "/")
	if len(parts) != 2 || !ValidTransportFailureClassification(parts[0], parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}
