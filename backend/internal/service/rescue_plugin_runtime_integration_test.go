package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Exercise the shipped plugin through the host installer, RPC and gateway,
// without using production accounts or making any external upstream requests.
func TestRescuePluginRuntimeReauthorizationIsolation(t *testing.T) {
	packagePath := os.Getenv("SUB2API_TEST_RESCUE_PLUGIN_PACKAGE")
	if packagePath == "" {
		t.Skip("SUB2API_TEST_RESCUE_PLUGIN_PACKAGE is not set")
	}
	packageFile, err := os.Open(packagePath)
	require.NoError(t, err)
	defer packageFile.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host := PluginHostInfo{Version: "0.3.0+r17bd-reauth-candidate", BuildType: "test"}
	cfg := testPluginConfig(t.TempDir(), true)
	installation, err := NewPluginPackageInstaller(cfg, host).Install(ctx, packageFile, nil)
	require.NoError(t, err)
	require.Equal(t, "lyunlong.codex.lb-cookie-pin", installation.PluginKey)
	require.Equal(t, "0.3.7", installation.Version)
	installation.ID = 7

	// macOS Unix socket paths have a short limit; keep RPC outside the long
	// test name while retaining the caller's SSD-backed TMPDIR.
	socketDir, err := os.MkdirTemp(os.TempDir(), "rescue-rpc-")
	require.NoError(t, err)
	defer os.RemoveAll(socketDir)
	runtime, err := startPluginRuntime(ctx, installation, 10*time.Second, socketDir)
	require.NoError(t, err)
	defer runtime.kill()
	require.NoError(t, runtime.validateAndApplyConfig(ctx, []byte(`{
		"enabled":true,"quality_probe_enabled":false,"persist_kv":false
	}`)))

	manager := NewPluginManager(nil, nil, cfg, host)
	manager.route.Store(&pluginRoute{pluginID: installation.ID, runtime: runtime, rolloutPercent: 100})
	legacy := &pluginRoutingHTTPUpstream{}
	gateway := &OpenAIGatewayService{pluginManager: manager, httpUpstream: legacy}

	type observation struct {
		cookie string
		host   string
	}
	var mu sync.Mutex
	seen := make(map[string]observation)
	slowStarted := make(chan struct{})
	slowRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(slowRelease) }) }
	defer release()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step := r.Header.Get("X-Test-Step")
		mu.Lock()
		seen[step] = observation{cookie: r.Header.Get("Cookie"), host: r.URL.Host}
		mu.Unlock()
		if step == "old-inflight" {
			close(slowStarted)
			select {
			case <-slowRelease:
			case <-r.Context().Done():
				return
			}
		}
		switch step {
		case "seed-a":
			w.Header().Set("Set-Cookie", "__cflb=old-a; Path=/")
		case "seed-b":
			w.Header().Set("Set-Cookie", "__cflb=other-b; Path=/")
		case "new-credential":
			w.Header().Set("Set-Cookie", "__cflb=new-a; Path=/")
		case "old-inflight":
			w.Header().Set("Set-Cookie", "__cflb=stale-a; Path=/")
		case "rejected":
			w.Header().Set("Set-Cookie", "__cflb=rejected-a; Path=/")
			w.WriteHeader(http.StatusUnauthorized)
		}
		_, _ = io.WriteString(w, `{"fixture":true}`)
	}))
	defer proxy.Close()
	// Release pending requests before httptest waits for its handlers.
	defer release()

	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	proxyPort, err := strconv.Atoi(proxyURL.Port())
	require.NoError(t, err)
	proxyID := int64(21)
	proxyRecord := &Proxy{ID: proxyID, Protocol: "http", Host: proxyURL.Hostname(), Port: proxyPort, Status: StatusActive}
	account := func(id int64) *Account {
		return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Concurrency: 1, ProxyID: &proxyID, Proxy: proxyRecord}
	}
	request := func(acc *Account, credential, step, route string) (int, error) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost,
			"http://chatgpt.com/backend-api/codex/responses",
			strings.NewReader(`{"model":"fixture-model","stream":false}`))
		if reqErr != nil {
			return 0, reqErr
		}
		req.Header.Set("Authorization", "Bearer fixture-"+credential)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Step", step)
		response, requestErr := gateway.doOpenAIUpstream(req, route, acc)
		if requestErr != nil {
			return 0, requestErr
		}
		defer response.Body.Close()
		_, readErr := io.ReadAll(response.Body)
		return response.StatusCode, readErr
	}
	send := func(id int64, credential, step string, wantStatus int) {
		t.Helper()
		status, sendErr := request(account(id), credential, step, proxyRecord.URL())
		require.NoError(t, sendErr)
		require.Equal(t, wantStatus, status)
	}
	cookie := func(step, want string) {
		t.Helper()
		mu.Lock()
		value, ok := seen[step]
		mu.Unlock()
		require.True(t, ok, "request did not reach the local proxy: %s", step)
		assert.Equal(t, "chatgpt.com", value.host)
		assert.Equal(t, want, value.cookie, "step=%s", step)
	}

	send(41, "old", "seed-a", http.StatusOK)
	send(42, "other", "seed-b", http.StatusOK)
	cookie("seed-a", "")
	cookie("seed-b", "")

	slowDone := make(chan error, 1)
	go func() {
		_, slowErr := request(account(41), "old", "old-inflight", proxyRecord.URL())
		slowDone <- slowErr
	}()
	select {
	case <-slowStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	send(41, "new", "new-credential", http.StatusOK)
	cookie("new-credential", "")
	release()
	select {
	case slowErr := <-slowDone:
		require.NoError(t, slowErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	send(41, "new", "after-old-response", http.StatusOK)
	cookie("old-inflight", "__cflb=old-a")
	cookie("after-old-response", "__cflb=new-a")
	send(42, "other", "other-account", http.StatusOK)
	cookie("other-account", "__cflb=other-b")

	send(41, "new", "rejected", http.StatusUnauthorized)
	send(41, "new", "after-rejection", http.StatusOK)
	cookie("after-rejection", "__cflb=new-a")

	require.NoError(t, runtime.validateAndApplyConfig(ctx, []byte(`{
		"enabled":true,"quality_probe_enabled":false,"persist_kv":false,"drop_account_ids":[41]
	}`)))
	send(41, "new", "after-reroll", http.StatusOK)
	cookie("after-reroll", "")
	send(42, "other", "other-after-reroll", http.StatusOK)
	cookie("other-after-reroll", "__cflb=other-b")

	_, err = request(account(41), "new", "wrong-proxy", "")
	require.Error(t, err)
	mu.Lock()
	_, reachedProxy := seen["wrong-proxy"]
	mu.Unlock()
	assert.False(t, reachedProxy)
	assert.Equal(t, 0, legacy.doCalls, "no direct/legacy fallback on route errors")

	inactiveAccount := account(41)
	inactiveProxy := *proxyRecord
	inactiveProxy.Status = StatusDisabled
	inactiveAccount.Proxy = &inactiveProxy
	_, err = request(inactiveAccount, "new", "inactive-proxy", proxyRecord.URL())
	require.ErrorIs(t, err, errOpenAIOAuthProxyUnavailable)
	mu.Lock()
	_, reachedProxy = seen["inactive-proxy"]
	mu.Unlock()
	assert.False(t, reachedProxy)
	assert.Equal(t, 0, legacy.doCalls, "no fallback for an inactive assigned proxy")

	require.Eventually(t, func() bool { return runtime.inFlight.Load() == 0 },
		time.Second, 10*time.Millisecond)
	runtime.draining.Store(true)
	_, err = request(account(41), "new", "draining", proxyRecord.URL())
	require.Error(t, err)
	assert.Equal(t, 0, legacy.doCalls, "no direct/legacy fallback while plugin drains")
	assert.Zero(t, runtime.inFlight.Load())
}
