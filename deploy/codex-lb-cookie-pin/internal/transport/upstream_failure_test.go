package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
)

func TestUpstreamFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name, class, phase string
		err                error
		contextErr         error
	}{
		{"proxy_timeout", "network_timeout", "proxy_connect", &net.OpError{Op: "socks connect", Err: context.DeadlineExceeded}, nil},
		{"dns_timeout", "dns_timeout", "dns", &net.DNSError{Err: "private", Name: "private.invalid", IsTimeout: true}, nil},
		{"dns_failure", "dns_failure", "dns", &net.DNSError{Err: "private", Name: "private.invalid"}, nil},
		{"refused", "connection_refused", "connect", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, nil},
		{"reset", "connection_reset", "read", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, nil},
		{"pipe", "broken_pipe", "write", &net.OpError{Op: "write", Err: syscall.EPIPE}, nil},
		{"eof", "connection_eof", "unknown", io.EOF, nil},
		{"partial", "connection_eof", "unknown", io.ErrUnexpectedEOF, nil},
		{"certificate", "tls_certificate", "tls", x509.UnknownAuthorityError{}, nil},
		{"tls_protocol", "tls_protocol", "tls", tls.RecordHeaderError{}, nil},
		{"request_deadline", "request_deadline", "unknown", context.DeadlineExceeded, context.DeadlineExceeded},
		{"canceled", "request_canceled", "unknown", context.Canceled, context.Canceled},
		{"wrapped_cancel", "request_canceled", "unknown", context.Canceled, nil},
		{"reset_before_cancel", "connection_reset", "read", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, context.Canceled},
		{"eof_before_deadline", "connection_eof", "unknown", io.ErrUnexpectedEOF, context.DeadlineExceeded},
		{"unknown", "connection_error", "unknown", errors.New("private-token:private.invalid"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &url.Error{Op: "Post", URL: "https://private.invalid/private-query", Err: tc.err}
			class, phase := classifyUpstreamFailure(err, tc.contextErr)
			if class != tc.class || phase != tc.phase {
				t.Fatalf("got %s/%s, want %s/%s", class, phase, tc.class, tc.phase)
			}
		})
	}
}

func TestUpstreamFailureLogHasOnlySafeMetadata(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	err := &url.Error{
		Op: "Post", URL: "https://private.invalid/private-query",
		Err: &net.OpError{Op: "socks connect", Err: context.DeadlineExceeded},
	}
	logUpstreamFailure(logger, 42, "forward_connect",
		"socks5h://private-user:private-password@private.invalid:1234",
		time.Now().Add(-5*time.Second), err, nil, nil)
	var entry map[string]any
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "private") {
		t.Fatal("transport diagnostic exposed private request or proxy data")
	}
	allowed := []string{"time", "level", "msg", "account_id", "operation",
		"error_class", "phase", "context_state", "proxy_scheme", "elapsed_ms"}
	if len(entry) != len(allowed) {
		t.Fatalf("unexpected diagnostic fields: %d", len(entry))
	}
	for key := range entry {
		if !slices.Contains(allowed, key) {
			t.Fatalf("unapproved diagnostic field: %q", key)
		}
	}
	if entry["account_id"] != float64(42) || entry["operation"] != "forward_connect" ||
		entry["error_class"] != "network_timeout" || entry["phase"] != "proxy_connect" ||
		entry["proxy_scheme"] != "socks5h" || entry["elapsed_ms"].(float64) < 5000 {
		t.Fatal("transport diagnostic lost its safe correlation fields")
	}
	out.Reset()
	suffix := logUpstreamFailure(logger, 42, "private-operation", "private-proxy", time.Now(), context.Canceled, context.Canceled, nil)
	if strings.Contains(out.String(), "private") || !strings.Contains(out.String(), `"level":"INFO"`) {
		t.Fatal("cancellation or invalid diagnostic vocabulary was not handled safely")
	}
	if suffix != " [error_class=request_canceled phase=unknown context_state=canceled]" {
		t.Fatal("RPC diagnostic contains more than the finite classification")
	}
}

type failingDiagnosticTransport struct {
	calls      int
	body       bool
	statusCode int
	cancel     context.CancelFunc
}

func (r *failingDiagnosticTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	if r.cancel != nil {
		r.cancel()
	}
	if r.body {
		code := r.statusCode
		if code == 0 {
			code = http.StatusOK
		}
		return &http.Response{
			StatusCode: code, Header: make(http.Header), ContentLength: -1,
			Body: io.NopCloser(failingDiagnosticReader{}),
		}, nil
	}
	return nil, &net.OpError{Op: "socks connect", Err: context.DeadlineExceeded}
}

func TestProbeFailureDiagnosticPrecedesCleanupCancellation(t *testing.T) {
	for _, body := range []bool{false, true} {
		name, operation, class := "connect", "probe_connect", "network_timeout"
		if body {
			name, operation, class = "body", "probe_body", "connection_eof"
		}
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
			defer slog.SetDefault(previous)
			store := cookiestore.New()
			cfg := probeTestConfig()
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			stashFrom(srv, 42, "https://private.invalid/private-query", time.Now())
			tmpl := *srv.templates[42]
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			transport := &failingDiagnosticTransport{body: body}
			if !body {
				transport.cancel = cancel
			}
			client, err := srv.clientFor(tmpl.ProxyURL, 42, tmpl.AccountConcurrency)
			if err != nil {
				t.Fatal(err)
			}
			client.Transport = transport
			verdict, _, _ := srv.sendProbe(ctx, 42, &tmpl, prober.Question{Prompt: "private-question"}, cfg)
			if verdict != prober.VerdictError || transport.calls != 1 {
				t.Fatal("diagnostics changed probe cancellation or result semantics")
			}
			var entry map[string]any
			decoder := json.NewDecoder(&out)
			rawLogs := out.String()
			diagnostics := 0
			for decoder.More() {
				var event map[string]any
				if err := decoder.Decode(&event); err != nil {
					t.Fatal(err)
				}
				if event["msg"] == "upstream_transport_failure" {
					entry = event
					diagnostics++
				}
			}
			if diagnostics != 1 {
				t.Fatalf("expected one transport diagnostic, got %d", diagnostics)
			}
			if entry["operation"] != operation || entry["error_class"] != class ||
				strings.Contains(rawLogs, "private") {
				t.Fatal("cleanup hid the original error or diagnostic exposed private data")
			}
			if !body && entry["context_state"] != "canceled" {
				t.Fatal("cancellation state was lost or replaced the transport error")
			}
		})
	}
}

type failingDiagnosticReader struct{}

func (failingDiagnosticReader) Read(out []byte) (int, error) {
	return copy(out, "private-partial-body"), io.ErrUnexpectedEOF
}

func TestForwardFailureLogsWithoutRetryOrAdmission(t *testing.T) {
	for _, body := range []bool{false, true} {
		name, operation, code := "connect", "forward_connect", "UPSTREAM"
		if body {
			name, operation, code = "body", "forward_body", "UPSTREAM_BODY"
		}
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
			defer slog.SetDefault(previous)
			store := cookiestore.New()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			transport := &failingDiagnosticTransport{body: body}
			stream := forwardFixture(t.Context(), "https://private.invalid/private-query", "private-auth")
			start := stream.requests[0].GetStart()
			client, err := srv.clientFor(start.GetProxyUrl(), start.GetAccountId(), start.GetAccountConcurrency())
			if err != nil {
				t.Fatal(err)
			}
			client.Transport = transport
			if err := srv.Forward(stream); err != nil {
				t.Fatal(err)
			}
			last := stream.responses[len(stream.responses)-1].GetError()
			if last == nil || last.Code != code || !last.RequestSent || transport.calls != 1 {
				t.Fatal("diagnostics changed error framing, replay safety or attempt count")
			}
			wantSuffix := " [error_class=network_timeout phase=proxy_connect context_state=active]"
			if body {
				wantSuffix = " [error_class=connection_eof phase=response_body context_state=active]"
				if len(stream.responses) != 3 || string(stream.responses[1].GetBodyChunk()) != "private-partial-body" {
					t.Fatal("partial body or single terminal error semantics changed")
				}
			}
			if !strings.HasSuffix(last.Message, wantSuffix) || strings.Contains(last.Message, "private") {
				t.Fatal("host's original request log cannot safely classify the plugin failure")
			}
			if srv.templates[42] != nil && srv.templates[42].Confirmed {
				t.Fatal("failed request admitted a quality probe")
			}
			var entry map[string]any
			if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
				t.Fatalf("missing single diagnostic: %v", err)
			}
			if entry["operation"] != operation || strings.Contains(out.String(), "private") {
				t.Fatal("wrong failure phase or private diagnostic data")
			}
		})
	}
}

func TestProbeHTTPFailureDiagnosticKeepsBodyErrorBeforeCancel(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(previous)
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	stashFrom(srv, 42, "https://private.invalid/private-query", time.Now())
	tmpl := *srv.templates[42]
	transport := &failingDiagnosticTransport{body: true, statusCode: http.StatusUnauthorized}
	client, err := srv.clientFor(tmpl.ProxyURL, 42, tmpl.AccountConcurrency)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = transport
	verdict, reason, _ := srv.sendProbe(t.Context(), 42, &tmpl, prober.Question{Prompt: "private-question"}, cfg)
	if verdict != prober.VerdictError || reason != "http:401+tmpl-dropped" || transport.calls != 1 {
		t.Fatal("diagnostics changed the existing non-retryable HTTP failure")
	}
	logged := out.String()
	decoder := json.NewDecoder(strings.NewReader(logged))
	var entry map[string]any
	if err := decoder.Decode(&entry); err != nil {
		t.Fatal(err)
	}
	if entry["operation"] != "probe_body" || entry["error_class"] != "connection_eof" ||
		entry["phase"] != "response_body" {
		t.Fatal("cleanup cancellation replaced the failed HTTP response-body diagnosis")
	}
	if strings.Contains(logged, "private") {
		t.Fatal("HTTP failure diagnostic exposed response or request data")
	}
}

func TestUpstreamFailureTraceBindsOnlyMatchingTLSFailure(t *testing.T) {
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://private.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	request, trace := traceUpstreamRequest(request)
	callback := httptrace.ContextClientTrace(request.Context()).TLSHandshakeDone
	tlsFailure := errors.New("private-tls-failure")
	callback(tls.ConnectionState{}, tlsFailure)
	wrapped := &url.Error{Op: "Get", URL: "https://private.invalid", Err: tlsFailure}
	if class, phase := trace.classify(wrapped, nil); class != "connection_error" || phase != "tls" {
		t.Fatal("matching TLS callback was not retained")
	}
	if _, phase := trace.classify(io.ErrUnexpectedEOF, nil); phase != "unknown" {
		t.Fatal("unrelated TLS callback relabeled a body failure")
	}
	callback(tls.ConnectionState{}, nil)
	if _, phase := trace.classify(wrapped, nil); phase != "unknown" {
		t.Fatal("successful handshake retained a stale failure")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			callback(tls.ConnectionState{}, tlsFailure)
		}
	}()
	for i := 0; i < 100; i++ {
		trace.classify(wrapped, nil)
	}
	wg.Wait()
}

func TestUpstreamFailureRealTLSHandshakeTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			<-stop
		}
	}()
	t.Cleanup(func() {
		close(stop)
		listener.Close()
		<-finished
	})
	transport := &http.Transport{TLSHandshakeTimeout: 100 * time.Millisecond}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	callbacks := 0
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		TLSHandshakeDone: func(tls.ConnectionState, error) { callbacks++ },
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request, trace := traceUpstreamRequest(request)
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("unresponsive local TLS peer unexpectedly succeeded")
	}
	class, phase := trace.classify(err, ctx.Err())
	if class != "network_timeout" || phase != "tls" || callbacks != 1 {
		t.Fatalf("real TLS timeout got %s/%s with %d original callbacks", class, phase, callbacks)
	}
	var out bytes.Buffer
	suffix := logUpstreamFailure(slog.New(slog.NewJSONHandler(&out, nil)), 42,
		"forward_connect", "", time.Now(), err, ctx.Err(), trace)
	if suffix != " [error_class=network_timeout phase=tls context_state=active]" ||
		strings.Contains(out.String(), listener.Addr().String()) {
		t.Fatal("real TLS diagnostic lost its phase or exposed its endpoint")
	}
}

func TestUpstreamFailureRealSOCKSRejection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			finished <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		greeting := make([]byte, 3)
		if _, err = io.ReadFull(conn, greeting); err == nil {
			_, err = conn.Write([]byte{5, 0})
		}
		if err == nil {
			header := make([]byte, 5)
			_, err = io.ReadFull(conn, header)
			if err == nil && header[3] == 3 {
				_, err = io.CopyN(io.Discard, conn, int64(header[4])+2)
			}
		}
		if err == nil {
			_, err = conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		}
		finished <- err
	}()
	t.Cleanup(func() { listener.Close() })
	proxy := &url.URL{Scheme: "socks5h", Host: listener.Addr().String()}
	transport := &http.Transport{Proxy: http.ProxyURL(proxy)}
	t.Cleanup(transport.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://private.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	request, trace := traceUpstreamRequest(request)
	response, err := (&http.Client{Transport: transport}).Do(request)
	if response != nil {
		response.Body.Close()
	}
	listener.Close()
	if serverErr := <-finished; serverErr != nil {
		t.Fatalf("local SOCKS fixture failed: %v", serverErr)
	}
	if err == nil {
		t.Fatal("rejecting SOCKS peer unexpectedly succeeded")
	}
	class, phase := trace.classify(err, ctx.Err())
	if class != "connection_error" || phase != "proxy_connect" {
		t.Fatalf("real SOCKS rejection got %s/%s", class, phase)
	}
}
