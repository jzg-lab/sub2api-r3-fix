package service

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type injectedHealthTimeoutClient struct {
	pluginv1.TransportPluginClient
	timeout atomic.Bool
}

func (c *injectedHealthTimeoutClient) Health(ctx context.Context, req *pluginv1.HealthRequest, opts ...grpc.CallOption) (*pluginv1.HealthResponse, error) {
	if c.timeout.Load() {
		return nil, status.Error(codes.DeadlineExceeded, "injected liveness timeout")
	}
	return c.TransportPluginClient.Health(ctx, req, opts...)
}

func TestPluginHealthNativeStreamSurvivesTimeout(t *testing.T) {
	binaryPath := os.Getenv("SUB2API_TEST_HEALTH_PLUGIN_BINARY")
	if binaryPath == "" {
		t.Skip("SUB2API_TEST_HEALTH_PLUGIN_BINARY is required for the isolated native process test")
	}
	require.True(t, filepath.IsAbs(binaryPath))
	binary, err := os.ReadFile(binaryPath)
	require.NoError(t, err)
	sum := sha256.Sum256(binary)
	installation := enabledBridgeInstallation()
	installation.PluginKey = "lyunlong.codex.lb-cookie-pin"
	installation.Version = "0.3.14"
	installation.BinaryPath = binaryPath
	installation.BinarySHA256 = hex.EncodeToString(sum[:])

	// Keep Unix socket paths short enough for macOS. This root never touches
	// the installed plugin, its state directory, or production accounts.
	root, err := os.MkdirTemp("", "s2-health-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socketDir := filepath.Join(root, "runtime")
	require.NoError(t, os.Mkdir(socketDir, 0700))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime, err := startPluginRuntime(ctx, installation, 10*time.Second, socketDir)
	require.NoError(t, err)
	t.Cleanup(runtime.kill)
	api := &injectedHealthTimeoutClient{TransportPluginClient: runtime.api}
	runtime.api = api
	manager := NewPluginManager(&bridgeStatusRepo{installations: []*PluginInstallation{installation}}, nil, nil, PluginHostInfo{})
	route := &pluginRoute{pluginID: installation.ID, runtime: runtime, rolloutPercent: 100}
	manager.route.Store(route)
	manager.runtimes[installation.ID] = runtime

	var requests atomic.Int64
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		unblock()
		upstream.Close()
	})
	newRequest := func() *http.Request {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/v1/responses", strings.NewReader(`{"stream":true}`))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		return request
	}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1}
	response, handled, err := manager.RoundTripOpenAIOAuth(ctx, newRequest(), "", account)
	require.True(t, handled)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusOK, response.StatusCode)
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "event: response.created\n", first)
	require.EqualValues(t, 1, runtime.inFlight.Load())

	api.timeout.Store(true)
	require.Error(t, manager.reconcileOnce(ctx))
	require.Same(t, route, manager.route.Load())
	require.False(t, runtime.client.Exited(), "liveness timeout must not kill an active native plugin stream")
	_, handled, err = manager.RoundTripOpenAIOAuth(ctx, newRequest(), "", account)
	require.True(t, handled)
	require.ErrorIs(t, err, errPluginRuntimeUnavailable)
	require.EqualValues(t, 1, requests.Load(), "quarantine must not replay or bypass the plugin")

	api.timeout.Store(false)
	require.NoError(t, manager.reconcileOnce(ctx))
	require.Same(t, route, manager.route.Load())
	unblock()
	remainder, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(remainder), "event: response.completed\n")
	require.NoError(t, response.Body.Close())
	require.Zero(t, runtime.inFlight.Load())

	resumed, handled, err := manager.RoundTripOpenAIOAuth(ctx, newRequest(), "", account)
	require.True(t, handled)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resumed.Body.Close() })
	body, err := io.ReadAll(resumed.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "event: response.completed\n")
	require.NoError(t, resumed.Body.Close())
	require.EqualValues(t, 2, requests.Load())
	require.Zero(t, runtime.inFlight.Load())
	require.False(t, runtime.client.Exited(), "recovery must use the original native process")
}
