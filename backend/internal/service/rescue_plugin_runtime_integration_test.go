package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/spf13/viper"
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
	hostVersion := os.Getenv("SUB2API_TEST_RESCUE_HOST_VERSION")
	require.NotEmpty(t, hostVersion, "use the exact candidate host version")
	host := PluginHostInfo{Version: hostVersion, BuildType: "test"}
	cfg := testPluginConfig(t.TempDir(), false)
	cfg.Plugins.MaxUploadBytes = 128 * 1024 * 1024
	cfg.Plugins.MaxUncompressedBytes = 256 * 1024 * 1024
	trustConfig := os.Getenv("SUB2API_TEST_RESCUE_TRUST_CONFIG")
	require.NotEmpty(t, trustConfig, "a release package must be tested with the target host trust configuration")
	loader := viper.New()
	loader.SetConfigFile(trustConfig)
	require.NoError(t, loader.ReadInConfig())
	// Copy only publisher trust and archive limits; never use the live data root.
	require.False(t, loader.GetBool("plugins.allow_unsigned"), "release verification must reject unsigned packages")
	cfg.Plugins.TrustedPublishers = loader.GetStringMapString("plugins.trusted_publishers")
	for key, target := range map[string]*int64{
		"plugins.max_upload_bytes":       &cfg.Plugins.MaxUploadBytes,
		"plugins.max_uncompressed_bytes": &cfg.Plugins.MaxUncompressedBytes,
	} {
		if loader.IsSet(key) {
			*target = loader.GetInt64(key)
		}
	}
	installation, err := NewPluginPackageInstaller(cfg, host).Install(ctx, packageFile, nil)
	require.NoError(t, err)
	require.Equal(t, PluginSignatureTrusted, installation.SignatureStatus)
	require.True(t, installation.Compatibility.Compatible)
	require.Equal(t, "lyunlong.codex.lb-cookie-pin", installation.PluginKey)
	sourceManifest, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy",
		"codex-lb-cookie-pin", "manifest.source.json"))
	require.NoError(t, err)
	var expectedManifest struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	require.NoError(t, json.Unmarshal(sourceManifest, &expectedManifest))
	require.Equal(t, installation.PluginKey, expectedManifest.ID)
	require.NotEmpty(t, expectedManifest.Version)
	require.Equal(t, expectedManifest.Version, installation.Version,
		"the signed package must match the committed source manifest")
	installation.ID = 7

	// macOS Unix socket paths have a short limit; keep RPC outside the long
	// test name while retaining the caller's SSD-backed TMPDIR.
	runtimeDir, err := os.MkdirTemp(os.TempDir(), "rr-")
	require.NoError(t, err)
	defer os.RemoveAll(runtimeDir)
	socketDir := filepath.Join(runtimeDir, "rpc")
	require.NoError(t, os.Mkdir(socketDir, 0700))
	evidenceDir, err := pluginEvidenceDir(socketDir, installation.PluginKey)
	require.NoError(t, err)
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
	settlementConnections := make(map[string]bool)
	slowStarted := make(chan struct{})
	slowRelease := make(chan struct{})
	stalledTerminalReleased := make(chan struct{})
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
		if step == "settlement-sse" || step == "settlement-no-media" || step == "settlement-stalled-tail" {
			if step != "settlement-stalled-tail" {
				mu.Lock()
				settlementConnections[r.RemoteAddr] = true
				mu.Unlock()
			}
			if step == "settlement-no-media" {
				// The real upstream omits this header. Suppress net/http sniffing.
				w.Header()["Content-Type"] = nil
			} else {
				w.Header().Set("Content-Type", "text/event-stream")
			}
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
			w.(http.Flusher).Flush()
			if step == "settlement-stalled-tail" {
				<-r.Context().Done()
				close(stalledTerminalReleased)
				return
			}
			// A terminal frame can arrive before the final HTTP chunk.
			time.Sleep(10 * time.Millisecond)
			return
		}
		_, _ = io.WriteString(w, `{"object":"response","status":"completed","output":[]}`)
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
	// Native broker/lifecycle acceptance uses only the loopback proxy. Assert
	// the host-resolved profile at the transport seam; repository tests cover
	// TLS dialing and pool isolation, not this plain-HTTP fixture.
	localTransport := http.DefaultTransport.(*http.Transport).Clone()
	localTransport.Proxy = http.ProxyURL(proxyURL)
	defer localTransport.CloseIdleConnections()
	localClient := &http.Client{Transport: localTransport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	fingerprint := &tlsfingerprint.Profile{CipherSuites: []uint16{0x1301}}
	boundHost := &pluginHostHTTP{
		brokerID: runtime.broker.NextId(), business: make(map[string]*pluginHTTPGrant),
		probes: make(map[int64]*pluginHTTPGrant), probeRunning: make(map[int64]bool),
		resolve: func(*Account) *tlsfingerprint.Profile { return fingerprint },
		runtime: runtime,
		upstream: hostHTTPUpstreamFixture{call: func(req *http.Request, route string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
			if route != proxyRecord.URL() || (id != 41 && id != 42) || concurrency != 1 ||
				pluginHTTPProfileKey(profile) != pluginHTTPProfileKey(fingerprint) ||
				!HTTPUpstreamRedirectsDisabled(req.Context()) || req.GetBody != nil {
				return nil, errors.New("native host transport binding mismatch")
			}
			return localClient.Do(req)
		}},
	}
	require.NoError(t, boundHost.start(runtime))
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
		response, requestErr := gateway.doOpenAIUpstream(req.Context(), req, route, acc)
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
	// Reading body EOF must include the signed plugin's durable settlement.
	// This catches RPC cancellation races that unit transport fixtures cannot.
	for n := uint64(2); n <= 25; n++ {
		step := "settlement-sse"
		if n >= 14 {
			step = "settlement-no-media"
		}
		send(42, "other", step, http.StatusOK)
		raw, readErr := os.ReadFile(filepath.Join(evidenceDir, "42.jsonl"))
		require.NoError(t, readErr)
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		var record struct {
			State struct {
				Dispatched uint64 `json:"business_dispatched"`
				Succeeded  uint64 `json:"business_succeeded"`
				LastEvent  struct {
					Result string `json:"result"`
				} `json:"last_event"`
			} `json:"state"`
		}
		require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &record))
		require.Equal(t, n, record.State.Dispatched)
		require.Equal(t, n, record.State.Succeeded)
		require.Equal(t, "succeeded", record.State.LastEvent.Result)
	}
	mu.Lock()
	settlementConnectionCount := len(settlementConnections)
	mu.Unlock()
	require.Equal(t, 1, settlementConnectionCount,
		"settled SSE must retain the HTTP connection when its final chunk follows promptly")
	stalledStart := time.Now()
	send(42, "other", "settlement-stalled-tail", http.StatusOK)
	require.Less(t, time.Since(stalledStart), 2*time.Second,
		"connection reuse must not leave a completed model response waiting for EOF")
	select {
	case <-stalledTerminalReleased:
	case <-time.After(time.Second):
		t.Fatal("stalled terminal HTTP connection was not released")
	}
	send(42, "other", "after-stalled-tail", http.StatusOK)

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

	// Bind native acceptance to the exact bytes and trust policy, not a fixed
	// package hash or a synthetic installer with allow_unsigned enabled.
	if receiptPath := os.Getenv("SUB2API_TEST_RESCUE_PLUGIN_RECEIPT"); receiptPath != "" {
		require.False(t, t.Failed(), "never publish a passing receipt after an assertion failed")
		commit, commitErr := exec.Command("git", "rev-parse", "HEAD").Output()
		require.NoError(t, commitErr)
		require.NoError(t, exec.Command("git", "diff", "--quiet", "HEAD", "--").Run(),
			"native release acceptance requires committed source")
		source, readErr := os.ReadFile("rescue_plugin_runtime_integration_test.go")
		require.NoError(t, readErr)
		sum := func(data []byte) string {
			value := sha256.Sum256(data)
			return hex.EncodeToString(value[:])
		}
		policy, marshalErr := json.Marshal(cfg.Plugins.TrustedPublishers)
		require.NoError(t, marshalErr)
		trustSource, readErr := os.ReadFile(trustConfig)
		require.NoError(t, readErr)
		receipt, marshalErr := json.MarshalIndent(map[string]any{
			"schema":                    "sub2api-rescue-native-acceptance.v1",
			"status":                    "passed",
			"recorded_at":               time.Now().UTC().Format(time.RFC3339),
			"plugin_sha256":             sum(installation.ArtifactData),
			"plugin_key":                installation.PluginKey,
			"plugin_version":            installation.Version,
			"host_version":              host.Version,
			"host_commit":               strings.TrimSpace(string(commit)),
			"signature_status":          installation.SignatureStatus,
			"compatible":                installation.Compatibility.Compatible,
			"allow_unsigned":            cfg.Plugins.AllowUnsigned,
			"max_upload_bytes":          cfg.Plugins.MaxUploadBytes,
			"max_uncompressed_bytes":    cfg.Plugins.MaxUncompressedBytes,
			"trusted_publishers_sha256": sum(policy),
			"trust_config_sha256":       sum(trustSource),
			"test_source_sha256":        sum(source),
			"source_manifest_sha256":    sum(sourceManifest),
			"checks": []string{"native_installer", "rpc", "gateway", "credential_rotation",
				"stale_response", "account_isolation", "401", "reroll", "proxy_fail_closed", "drain",
				"host_http_broker", "host_resolved_profile_binding", "host_route_binding",
				"no_redirect", "no_request_body_replay"},
			"production_adoption": false,
		}, "", "  ")
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(receiptPath, append(receipt, '\n'), 0o600))
	}
}
