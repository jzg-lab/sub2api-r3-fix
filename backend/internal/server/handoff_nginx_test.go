//go:build !windows

package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/deployment"
	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// The local container fixture supplies nginx. No production listener or account
// is used; both targets use the real Handoff middleware with synthetic traffic.
func TestLiveHandoffThroughNginx(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			testLiveHandoffThroughNginx(t, legacy)
		})
	}
}

func testLiveHandoffThroughNginx(t *testing.T, legacy bool) {
	nginx, err := exec.LookPath("nginx")
	if err != nil {
		t.Skip("nginx required; run deploy/tests/local_application.py")
	}
	dir := t.TempDir()
	lock := filepath.Join(dir, "owner.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0600))
	finish := make(chan struct{})
	var finishOnce sync.Once
	end := func() { finishOnce.Do(func() { close(finish) }) }
	makeServer := func() (*Handoff, *httptest.Server) {
		h := NewHandoff(lock, "fixture", &handoffPluginStub{}, func(context.Context) bool { return true }, func(context.Context) error { return nil })
		h.background = &deployment.Gate{}
		s := httptest.NewServer(h.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/stream":
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: before\n\n")
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
					return
				}
				select {
				case <-finish:
					_, _ = io.WriteString(w, "data: after\n\n")
				case <-r.Context().Done():
				}
			case "/ws":
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = ws.CloseNow() }()
				for {
					kind, data, err := ws.Read(r.Context())
					if err != nil || ws.Write(r.Context(), kind, data) != nil {
						return
					}
				}
			default:
				w.WriteHeader(http.StatusNoContent)
			}
		})))
		t.Cleanup(s.Close)
		return h, s
	}
	old, oldServer := makeServer()
	next, newServer := makeServer()
	t.Cleanup(end)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	require.NoError(t, old.transition(ctx, "serve"))
	require.NoError(t, old.transition(ctx, "resume"))
	port, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := port.Addr().String()
	require.NoError(t, port.Close())
	upstream := filepath.Join(dir, "upstream.conf")
	writeTarget := func(url string) {
		require.NoError(t, os.WriteFile(upstream+".next", []byte("server "+strings.TrimPrefix(url, "http://")+";\n"), 0644))
		require.NoError(t, os.Rename(upstream+".next", upstream))
	}
	writeTarget(oldServer.URL)
	config := fmt.Sprintf(`pid %s/nginx.pid;
error_log %s/error.log notice;
events {}
http {
  access_log off;
  map $http_upgrade $connection_upgrade { default upgrade; '' close; }
  upstream app { include %s; }
  server { listen %s; location / {
    proxy_pass http://app; proxy_http_version 1.1; proxy_buffering off;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
  } }
}`, dir, dir, upstream, addr)
	configPath := filepath.Join(dir, "nginx.conf")
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0600))
	process := exec.Command(nginx, "-p", dir, "-c", configPath, "-g", "daemon off;")
	require.NoError(t, process.Start())
	t.Cleanup(func() { _ = process.Process.Signal(syscall.SIGTERM); _ = process.Wait() })
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	check := func(boots ...string) bool {
		res, err := client.Get("http://" + addr + "/core")
		if err != nil {
			return false
		}
		defer func() { _ = res.Body.Close() }()
		for _, boot := range boots {
			if res.StatusCode == 204 && res.Header.Get("X-Sub2api-Instance") == boot {
				return true
			}
		}
		return false
	}
	require.Eventually(t, func() bool { return check(old.boot) }, 5*time.Second, 10*time.Millisecond)
	request, err := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/stream", nil)
	require.NoError(t, err)
	stream, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = stream.Body.Close() }()
	reader := bufio.NewReader(stream.Body)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "data: before\n", line)
	ws, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", nil)
	require.NoError(t, err)
	defer func() { _ = ws.CloseNow() }()
	pairs := []struct {
		from, to *Handoff
		url      string
	}{{old, next, newServer.URL}, {next, old, oldServer.URL}}
	if legacy {
		pairs = pairs[:1]
	}
	for _, pair := range pairs {
		require.NoError(t, pair.to.transition(ctx, "serve"))
		require.Error(t, pair.to.transition(ctx, "resume"), "core is open while the old runtime still owns plugins")
		if !legacy {
			require.NoError(t, pair.from.transition(ctx, "release"))
		}
		writeTarget(pair.url)
		require.NoError(t, process.Process.Signal(syscall.SIGHUP))
		// Every new connection must succeed, including during the reload gap.
		reloadDeadline := time.Now().Add(5 * time.Second)
		for {
			res, err := client.Get("http://" + addr + "/core")
			require.NoError(t, err)
			require.Equal(t, 204, res.StatusCode)
			_ = res.Body.Close()
			if res.Header.Get("X-Sub2api-Instance") == pair.to.boot {
				break
			}
			require.True(t, time.Now().Before(reloadDeadline), "nginx did not switch")
			time.Sleep(5 * time.Millisecond)
		}
		if !legacy {
			require.NoError(t, pair.to.transition(ctx, "resume"))
		}
		require.NoError(t, ws.Write(ctx, websocket.MessageText, []byte("still connected")))
		_, reply, err := ws.Read(ctx)
		require.NoError(t, err)
		require.Equal(t, "still connected", string(reply))
		if pair.to == next {
			// A legacy owner cannot release plugins separately. It must remain
			// alive beyond the former 30-second drain limit without blocking core.
			wait := 9 * time.Second
			if legacy {
				wait = 31 * time.Second
			}
			deadline := time.Now().Add(wait)
			for time.Now().Before(deadline) {
				// New workers can answer before old workers close their listeners.
				if time.Now().Before(reloadDeadline) {
					require.True(t, check(next.boot, old.boot))
				} else {
					require.True(t, check(next.boot))
				}
				time.Sleep(100 * time.Millisecond)
			}
			require.NoError(t, ws.Write(ctx, websocket.MessageText, []byte("after wait")))
			_, reply, err := ws.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "after wait", string(reply))
		}
	}
	end()
	rest, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "\ndata: after\n\n", string(rest))
	require.NoError(t, ws.Close(websocket.StatusNormalClosure, "done"))
	require.NoError(t, old.transition(ctx, "pause"))
	if legacy {
		require.NoError(t, next.transition(ctx, "resume"))
	}
	require.NoError(t, next.transition(ctx, "pause"))
}
