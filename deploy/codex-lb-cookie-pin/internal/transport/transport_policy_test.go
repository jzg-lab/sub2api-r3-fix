package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"golang.org/x/net/http2"
)

func TestAccountTransportInvalidProxyNeverFallsBack(t *testing.T) {
	srv := stoppedProbeServer(t, cookiestore.New())
	for _, route := range []string{
		"http://", "http://proxy.invalid/path", "http://proxy.invalid/?query=1",
		"http://proxy.invalid/#fragment", "ftp://proxy.invalid", "not-a-proxy",
	} {
		client, err := srv.clientFor(route, 42, 1)
		if err == nil || client != nil || len(srv.proxyClients) != 0 {
			t.Fatal("invalid proxy created a client or fell back to direct")
		}
	}
	client, err := srv.clientFor("", 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("explicit direct transport must not inherit environment proxies")
	}
}

func TestAccountTransportHTTP2Isolation(t *testing.T) {
	for _, proxied := range []bool{false, true} {
		t.Run(fmt.Sprintf("proxy=%t", proxied), func(t *testing.T) {
			origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, r.RemoteAddr)
			}))
			origin.EnableHTTP2 = true
			origin.StartTLS()
			t.Cleanup(origin.Close)
			route := ""
			if proxied {
				route, _ = socksRouteFixture(t, origin.Listener.Addr().String(), "same-proxy", &sync.Map{})
			}
			srv := stoppedProbeServer(t, cookiestore.New())
			roots := x509.NewCertPool()
			roots.AddCert(origin.Certificate())
			srv.Client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
			t.Cleanup(func() {
				for _, client := range srv.proxyClients {
					client.CloseIdleConnections()
				}
			})
			read := func(accountID int64) (string, *http.Client) {
				t.Helper()
				client, err := srv.clientFor(route, accountID, 2)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil || response.ProtoMajor != 2 {
					t.Fatalf("fixture did not complete over HTTP/2: protocol=%s error=%v", response.Proto, err)
				}
				return string(body), client
			}
			first, firstClient := read(42)
			second, _ := read(43)
			reused, reusedClient := read(42)
			if first == second || reused != first || reusedClient != firstClient {
				t.Fatal("HTTP/2 account separation or same-account reuse failed")
			}
			firstClient.CloseIdleConnections()
			secondAfter, _ := read(43)
			firstAfter, _ := read(42)
			if secondAfter != second || firstAfter == first {
				t.Fatal("closing one account's HTTP/2 pool affected another account")
			}
		})
	}
}

func TestAccountTransportIsolation(t *testing.T) {
	for _, proxied := range []bool{false, true} {
		t.Run(fmt.Sprintf("proxy=%t", proxied), func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, r.RemoteAddr)
			}))
			defer origin.Close()
			proxy, target := "", origin.URL
			if proxied {
				proxy, target = origin.URL, "http://example.invalid/"
			}
			srv := stoppedProbeServer(t, cookiestore.New())
			t.Cleanup(func() {
				for _, client := range srv.proxyClients {
					client.CloseIdleConnections()
				}
			})
			first, err := srv.clientFor(proxy, 42, 4)
			if err != nil {
				t.Fatal(err)
			}
			other, err := srv.clientFor(proxy, 43, 4)
			if err != nil {
				t.Fatal(err)
			}
			read := func(client *http.Client) string {
				t.Helper()
				response, err := client.Get(target)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				return string(body)
			}
			a, b := read(first), read(other)
			if first == other || a == b {
				t.Fatal("different accounts shared a connection pool")
			}
			reused, err := srv.clientFor(proxy, 42, 4)
			if err != nil || reused != first || read(reused) != a {
				t.Fatal("same account/route/policy did not reuse its connection")
			}
			changed, err := srv.clientFor(proxy, 42, 1)
			if err != nil || changed == first || read(changed) == a {
				t.Fatal("capacity change reused an obsolete pool")
			}
			tr := changed.Transport.(*http.Transport)
			if tr.MaxConnsPerHost != 1 || tr.MaxIdleConnsPerHost != 1 {
				t.Fatal("account connection capacity was not applied")
			}
			for id := int64(100); id < 130; id++ {
				if _, err := srv.clientFor(proxy, id, 1); err != nil {
					t.Fatal(err)
				}
			}
			if len(srv.proxyClients) != 32 {
				t.Fatal("transport cache is not bounded")
			}
			if _, exists := srv.proxyClients[transportKey{proxyURL: proxy, accountID: 43, concurrency: 4}]; exists {
				t.Fatal("cache did not evict the least recently used policy")
			}
		})
	}
}

func TestAccountTransportDetectsUnansweredHTTP2Ping(t *testing.T) {
	cert := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer cert.Close()
	config := cert.TLS.Clone()
	config.NextProtos = []string{"h2"}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var pings atomic.Int64
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		preface := make([]byte, len(http2.ClientPreface))
		if _, err := io.ReadFull(conn, preface); err != nil {
			done <- err
			return
		}
		framer := http2.NewFramer(conn, conn)
		if string(preface) != http2.ClientPreface {
			done <- fmt.Errorf("not an HTTP/2 connection")
			return
		}
		if err := framer.WriteSettings(); err != nil {
			done <- err
			return
		}
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				// A client close, rather than our fixture timeout, is required.
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					done <- err
				} else {
					done <- nil
				}
				return
			}
			switch f := frame.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					if err := framer.WriteSettingsAck(); err != nil {
						done <- err
						return
					}
				}
			case *http2.PingFrame:
				pings.Add(1) // Deliberately do not acknowledge.
			}
		}
	}()
	srv := stoppedProbeServer(t, cookiestore.New())
	base := srv.Client.Transport.(*http.Transport)
	if base.DialContext == nil || base.TLSHandshakeTimeout != 10*time.Second ||
		base.HTTP2 == nil || base.HTTP2.SendPingTimeout != 15*time.Second || base.HTTP2.PingTimeout != 15*time.Second {
		t.Fatal("production transport lacks bounded dial/TLS and HTTP/2 health checks")
	}
	base.TLSClientConfig = cert.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	base.HTTP2.SendPingTimeout = 100 * time.Millisecond
	base.HTTP2.PingTimeout = 100 * time.Millisecond
	client, err := srv.clientFor("", 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || ctx.Err() != nil || time.Since(started) > 2*time.Second || pings.Load() == 0 {
		t.Fatalf("dead HTTP/2 connection not detected by ping: error=%v pings=%d elapsed=%s", err, pings.Load(), time.Since(started))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAccountTransportPreservesHealthyIdleHTTP2(t *testing.T) {
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(400 * time.Millisecond):
			_, _ = io.WriteString(w, "completed")
		case <-r.Context().Done():
		}
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()
	srv := stoppedProbeServer(t, cookiestore.New())
	base := srv.Client.Transport.(*http.Transport)
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	base.TLSClientConfig = &tls.Config{RootCAs: roots}
	base.HTTP2.SendPingTimeout = 100 * time.Millisecond
	base.HTTP2.PingTimeout = 100 * time.Millisecond
	client, err := srv.clientFor("", 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("healthy idle connection was aborted: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.ProtoMajor != 2 || string(body) != "completed" {
		t.Fatalf("healthy delayed response did not complete: protocol=%s error=%v", response.Proto, err)
	}
}
