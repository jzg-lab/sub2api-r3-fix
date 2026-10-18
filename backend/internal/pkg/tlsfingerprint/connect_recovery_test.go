package tlsfingerprint

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestConnectBeforeRequestBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"eof", io.EOF, 2},
		{"partial", io.ErrUnexpectedEOF, 2},
		{"reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, 2},
		{"deadline", context.DeadlineExceeded, 1},
		{"canceled", context.Canceled, 1},
		{"certificate", x509.UnknownAuthorityError{}, 1},
		{"authentication", errors.New("proxy authentication failed"), 1},
		{"refused", syscall.ECONNREFUSED, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int
			_, err := connectBeforeRequest(context.Background(), func() (net.Conn, error) {
				attempts++
				return nil, tc.err
			})
			if attempts != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("attempts=%d error=%v", attempts, err)
			}
		})
	}
	t.Run("cancellation_between_attempts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var attempts int
		_, _ = connectBeforeRequest(ctx, func() (net.Conn, error) {
			attempts++
			cancel()
			return nil, io.EOF
		})
		if attempts != 1 {
			t.Fatalf("canceled request dialed %d times", attempts)
		}
	})
}

func TestConnectRecoveryDoesNotReplayApplicationRequest(t *testing.T) {
	for _, mode := range []string{"connect_eof_then_success", "response_headers_eof", "partial_response_body"} {
		t.Run(mode, func(t *testing.T) {
			var requests, dials, accepted atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "one business request" {
					t.Errorf("business body changed")
				}
				if mode == "response_headers_eof" {
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				}
				if mode == "partial_response_body" {
					w.Header().Set("Content-Length", "100")
					_, _ = io.WriteString(w, "partial")
					return
				}
				_, _ = io.WriteString(w, "ok")
			}))
			server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
				if state == http.StateNew && accepted.Add(1) == 1 && mode == "connect_eof_then_success" {
					_ = conn.Close()
				}
			}
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: time.Second}, Config: &tls.Config{RootCAs: roots}}
			transport := &http.Transport{DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return connectBeforeRequest(ctx, func() (net.Conn, error) {
					dials.Add(1)
					return dialer.DialContext(ctx, network, addr)
				})
			}}
			defer transport.CloseIdleConnections()
			req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("one business request"))
			req.GetBody = nil
			resp, err := (&http.Client{Transport: transport, Timeout: 3 * time.Second}).Do(req)
			if mode == "response_headers_eof" {
				if err == nil {
					resp.Body.Close()
					t.Fatal("expected response failure")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if mode == "partial_response_body" && !errors.Is(readErr, io.ErrUnexpectedEOF) {
					t.Fatalf("partial response lost its failure: %v", readErr)
				}
				if mode == "connect_eof_then_success" && (readErr != nil || string(body) != "ok") {
					t.Fatalf("recovery failed: %v", readErr)
				}
			}
			wantDials := int32(1)
			if mode == "connect_eof_then_success" {
				wantDials = 2
			}
			if requests.Load() != 1 || dials.Load() != wantDials {
				t.Fatalf("requests=%d dials=%d", requests.Load(), dials.Load())
			}
		})
	}
}
