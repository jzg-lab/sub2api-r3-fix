package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
	"google.golang.org/grpc"
)

type fixtureForwardStream struct {
	grpc.ServerStream
	ctx       context.Context
	requests  []*pluginv1.ForwardRequest
	responses []*pluginv1.ForwardResponse
}

func (s *fixtureForwardStream) Context() context.Context { return s.ctx }

func (s *fixtureForwardStream) Recv() (*pluginv1.ForwardRequest, error) {
	if len(s.requests) == 0 {
		return nil, io.EOF
	}
	next := s.requests[0]
	s.requests = s.requests[1:]
	return next, nil
}

func (s *fixtureForwardStream) Send(response *pluginv1.ForwardResponse) error {
	s.responses = append(s.responses, response)
	return nil
}

func forwardFixture(ctx context.Context, endpoint, identity string) *fixtureForwardStream {
	return &fixtureForwardStream{
		ctx: ctx,
		requests: []*pluginv1.ForwardRequest{
			{Frame: &pluginv1.ForwardRequest_Start{Start: &pluginv1.ForwardRequestStart{
				AccountId: 42, Method: "POST", Url: endpoint, HasBody: true,
				Headers: map[string]*pluginv1.HeaderValues{
					"Authorization": {Values: []string{"Bearer " + identity}},
				},
			}}},
			{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: []byte(`{"model":"gpt-6-astra"}`)}},
			{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}},
		},
	}
}

func TestLateBusinessResponseCannotOverwriteReplacement(t *testing.T) {
	for _, probeEnabled := range []bool{false, true} {
		name := "passive"
		if probeEnabled {
			name = "probing"
		}
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer old-fixture" {
					close(started)
					<-release
					w.Header().Set("Set-Cookie", "__cflb=obsolete; Max-Age=3500")
					w.Header().Set("Faster-Model", "fixture")
				} else {
					w.Header().Set("Set-Cookie", "__cflb=replacement; Max-Age=3500")
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer up.Close()
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			store := cookiestore.New()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			cfg.QualityProbeEnabled = probeEnabled
			cfg.RerollOnFasterModel = true
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			done := make(chan error, 1)
			go func() {
				done <- srv.Forward(forwardFixture(t.Context(), up.URL, "old-fixture"))
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("old request did not start")
			}
			if err := srv.Forward(forwardFixture(t.Context(), up.URL, "new-fixture")); err != nil {
				t.Fatal(err)
			}
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("old response did not finish")
			}
			if store.MergeHeader(42, "", time.Now()) != "__cflb=replacement" ||
				store.Status(time.Now()).Rerolls != 0 {
				t.Fatal("old business response changed the new authorization's cookie")
			}
		})
	}
}

func TestBusinessCaptureRespectsScopeAndAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scope  string
		status int
	}{
		{"outside scope", "codex", http.StatusOK},
		{"unauthorized", "all", http.StatusUnauthorized},
		{"forbidden", "all", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.scope == "codex" && r.Header.Get("Cookie") != "" {
					t.Error("cookie injected outside configured scope")
				}
				w.Header().Set("Set-Cookie", "__cflb=invalid; Max-Age=3500")
				w.Header().Set("Faster-Model", "fixture")
				w.WriteHeader(tc.status)
			}))
			defer up.Close()
			store := cookiestore.New()
			cfg := probeTestConfig()
			cfg.InjectScope = tc.scope
			cfg.RerollOnFasterModel = true
			store.SetConfig(cfg)
			store.Capture(42, []string{"__cflb=retained; Max-Age=3500"}, time.Now())
			srv := stoppedProbeServer(t, store)
			if err := srv.Forward(forwardFixture(t.Context(), up.URL, "fixture")); err != nil {
				t.Fatal(err)
			}
			if store.MergeHeader(42, "", time.Now()) != "__cflb=retained" {
				t.Fatal("ineligible response mutated account cookies")
			}
		})
	}
}

func TestInvalidProxyCannotFallBackToDirect(t *testing.T) {
	var requests atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	srv := stoppedProbeServer(t, cookiestore.New())
	for _, proxy := range []string{"not-a-proxy", "http://", "ftp://localhost:8888", "http://localhost:8888/unexpected"} {
		stream := forwardFixture(t.Context(), up.URL, "fixture")
		stream.requests[0].GetStart().ProxyUrl = proxy
		if err := srv.Forward(stream); err != nil {
			t.Fatal(err)
		}
		if len(stream.responses) != 1 || stream.responses[0].GetError().GetCode() != "PLUGIN_PROXY" {
			t.Fatal("invalid proxy did not fail closed")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid proxy escaped to direct transport")
	}
}

func TestProxyClientReusesConnectionsAndBoundsCache(t *testing.T) {
	var mu sync.Mutex
	connections := make(map[string]bool)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connections[r.RemoteAddr] = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	srv := stoppedProbeServer(t, cookiestore.New())
	first, err := srv.clientFor(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer first.CloseIdleConnections()
	for i := 0; i < 3; i++ {
		client, err := srv.clientFor(proxy.URL)
		if err != nil || client != first {
			t.Fatal("proxy client was not reused")
		}
		response, err := client.Get("http://example.invalid/probe")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	mu.Lock()
	count := len(connections)
	mu.Unlock()
	if count != 1 {
		t.Fatalf("sequential proxy requests used %d connections, want 1", count)
	}
	for i := 0; i < 40; i++ {
		endpoint := "http://proxy-" + time.Unix(int64(i), 0).Format("150405") + ".invalid:8888"
		if _, err := srv.clientFor(endpoint); err != nil {
			t.Fatal(err)
		}
	}
	if len(srv.proxyClients) > 32 {
		t.Fatal("unbounded proxy transport cache")
	}
}
