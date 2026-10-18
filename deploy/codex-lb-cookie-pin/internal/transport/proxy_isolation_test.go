package transport

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
)

// Record the proxy's actual outgoing TCP connection, not the requested route.
func socksRouteFixture(t *testing.T, target, label string, routes *sync.Map) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var connections atomic.Int64
	var handlers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				if err := acceptSOCKSRoute(conn, target, label, routes, &connections); err != nil && ctx.Err() == nil {
					t.Errorf("local SOCKS fixture: %v", err)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		<-done
		handlers.Wait()
	})
	return "socks5://" + listener.Addr().String(), &connections
}

func acceptSOCKSRoute(conn net.Conn, target, label string, routes *sync.Map, connections *atomic.Int64) error {
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return err
	}
	if greeting[0] != 5 {
		return fmt.Errorf("unexpected SOCKS version %d", greeting[0])
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return err
	}
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return err
	}
	if header[0] != 5 || header[1] != 1 || header[2] != 0 {
		return fmt.Errorf("unexpected SOCKS connect header %v", header)
	}
	var host string
	switch header[3] {
	case 1, 4:
		size := net.IPv4len
		if header[3] == 4 {
			size = net.IPv6len
		}
		address := make([]byte, size)
		if _, err := io.ReadFull(conn, address); err != nil {
			return err
		}
		host = net.IP(address).String()
	case 3:
		var size [1]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return err
		}
		address := make([]byte, int(size[0]))
		if _, err := io.ReadFull(conn, address); err != nil {
			return err
		}
		host = string(address)
	default:
		return fmt.Errorf("unexpected SOCKS address type %d", header[3])
	}
	var port [2]byte
	if _, err := io.ReadFull(conn, port[:]); err != nil {
		return err
	}
	if net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(port[:]))) != target {
		return fmt.Errorf("SOCKS fixture refused non-fixture target")
	}
	upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		return err
	}
	defer upstream.Close()
	routes.Store(upstream.LocalAddr().String(), label)
	connections.Add(1)
	if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(upstream, conn)
		upstream.Close()
	}()
	_, _ = io.Copy(conn, upstream)
	conn.Close()
	<-done
	return nil
}

func TestProxyClientHTTP2RouteIsolation(t *testing.T) {
	for _, tc := range []struct {
		scheme      string
		h2          bool
		directFirst bool
	}{
		{"socks5", true, false},
		{"socks5h", true, false},
		{"socks5", true, true},
		{"socks5h", true, true},
		{"socks5", false, false},
		{"socks5h", false, false},
	} {
		t.Run(fmt.Sprintf("%s/h2=%t/direct-first=%t", tc.scheme, tc.h2, tc.directFirst), func(t *testing.T) {
			var routes sync.Map
			origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				label, ok := routes.Load(r.RemoteAddr)
				if !ok {
					label = "direct"
				}
				w.Header().Set("X-Fixture-Route", label.(string))
				_, _ = io.WriteString(w, `{"status":"completed"}`)
			}))
			origin.EnableHTTP2 = tc.h2
			origin.StartTLS()
			t.Cleanup(origin.Close)
			a, aConnections := socksRouteFixture(t, origin.Listener.Addr().String(), "A", &routes)
			b, bConnections := socksRouteFixture(t, origin.Listener.Addr().String(), "B", &routes)
			a = strings.Replace(a, "socks5://", tc.scheme+"://", 1)
			b = strings.Replace(b, "socks5://", tc.scheme+"://", 1)
			srv := stoppedProbeServer(t, cookiestore.New())
			base := srv.Client.Transport.(*http.Transport)
			// Keep the real production transport configuration and trust only our local TLS fixture.
			if base.TLSClientConfig == nil {
				base.TLSClientConfig = origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			}
			base.TLSClientConfig.RootCAs = x509.NewCertPool()
			base.TLSClientConfig.RootCAs.AddCert(origin.Certificate())
			t.Cleanup(func() {
				srv.Client.CloseIdleConnections()
				for _, client := range srv.proxyClients {
					client.CloseIdleConnections()
				}
			})
			wantProtocol := 1
			if tc.h2 {
				wantProtocol = 2
			}
			check := func(proxy, want string) {
				t.Helper()
				client, err := srv.clientFor(proxy, 42, 4)
				if err != nil {
					t.Error(err)
					return
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
				if err != nil {
					t.Error(err)
					return
				}
				response, err := client.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if readErr != nil {
					t.Error(readErr)
				}
				if got := response.Header.Get("X-Fixture-Route"); got != want {
					t.Errorf("actual outgoing route = %s, requested %s", got, want)
				}
				if response.ProtoMajor != wantProtocol {
					t.Errorf("protocol = %s, want HTTP/%d", response.Proto, wantProtocol)
				}
			}
			if tc.directFirst {
				check("", "direct")
			}
			for _, route := range []struct{ proxy, label string }{{a, "A"}, {b, "B"}, {a, "A"}, {b, "B"}, {"", "direct"}} {
				check(route.proxy, route.label)
			}
			if aConnections.Load() != 1 || bConnections.Load() != 1 {
				t.Errorf("sequential connections A=%d B=%d, want one reused connection per proxy",
					aConnections.Load(), bConnections.Load())
			}
			if t.Failed() {
				return
			}
			var requests sync.WaitGroup
			for i := 0; i < 12; i++ {
				requests.Add(1)
				go func(i int) {
					defer requests.Done()
					if i%2 == 0 {
						check(a, "A")
					} else {
						check(b, "B")
					}
				}(i)
			}
			requests.Wait()
			srv.proxyClients[transportKey{proxyURL: a, accountID: 42, concurrency: 4}].CloseIdleConnections()
			check(b, "B")
			check(a, "A")
			if tc.h2 && (aConnections.Load() != 2 || bConnections.Load() != 1) {
				t.Errorf("closing A affected another pool: connections A=%d B=%d", aConnections.Load(), bConnections.Load())
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			stream := forwardFixture(ctx, origin.URL, "route-fixture")
			stream.requests[0].GetStart().ProxyUrl = b
			if err := srv.Forward(stream); err != nil {
				t.Fatal(err)
			}
			if len(stream.responses) < 2 || stream.responses[0].GetStart() == nil {
				t.Fatal("forward did not return a response start")
			}
			start := stream.responses[0].GetStart()
			if got := start.GetHeaders()["X-Fixture-Route"].GetValues(); len(got) != 1 || got[0] != "B" {
				t.Fatalf("forward used the wrong outgoing route: %v", got)
			}
			if start.GetProtocolMajor() != int32(wantProtocol) || stream.responses[len(stream.responses)-1].GetEnd() == nil {
				t.Fatal("forward protocol or completion changed")
			}
		})
	}
}
