package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

func TestPluginEvidenceDirectoryStableAndPrivate(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "runtime")
	first, err := pluginEvidenceDir(socket, "fixture/plugin")
	require.NoError(t, err)
	second, err := pluginEvidenceDir(socket, "fixture/plugin")
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, filepath.Join(root, "state"), filepath.Dir(first))
	require.Len(t, filepath.Base(first), 64)
	other, err := pluginEvidenceDir(socket, "another")
	require.NoError(t, err)
	require.NotEqual(t, first, other)
	info, err := os.Stat(first)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	require.NoError(t, os.Remove(first))
	require.NoError(t, os.Symlink(other, first))
	_, err = pluginEvidenceDir(socket, "fixture/plugin")
	require.Error(t, err)
}

func TestPluginEvidenceLogOnlyForwardsFiniteFields(t *testing.T) {
	var out bytes.Buffer
	sink := &pluginEvidenceLog{logger: slog.New(slog.NewJSONHandler(&out, nil))}
	operation := strings.Repeat("a", 32) + "-42-1"
	records := []string{
		`{"msg":"plugin_upstream_admission_rejected","account_id":42,"category":"state_rejection","window_seconds":600,"secret":"fixture-secret"}`,
		`{"msg":"plugin_probe_unknown","account_id":42,"operation_id":"` + operation + `","secret":"fixture-secret"}`,
		`{"msg":"plugin_evidence_unavailable","path":"fixture-secret"}`,
		`{"msg":"unrecognized","secret":"fixture-secret"}`,
		`{"msg":"plugin_probe_unknown","account_id":42,"operation_id":"fixture-secret"}`,
		`{"msg":"plugin_upstream_admission_rejected","account_id":42,"category":"fixture-secret","window_seconds":600}`,
		`{"msg":"plugin_upstream_admission_rejected","account_id":42,"category":"state_rejection","window_seconds":1}`,
		strings.Repeat("x", 3000),
	}
	for _, line := range records {
		for _, b := range []byte(line + "\n") {
			n, err := sink.Write([]byte{b})
			require.NoError(t, err)
			require.Equal(t, 1, n)
		}
	}
	require.NotContains(t, out.String(), "fixture-secret")
	require.Equal(t, 3, strings.Count(out.String(), "\n"))
	require.Contains(t, out.String(), operation)
	// Oversized input must not poison the next complete event.
	_, err := sink.Write([]byte(records[0] + "\n"))
	require.NoError(t, err)
	require.Equal(t, 4, strings.Count(out.String(), "\n"))
}

func TestPluginProbeTrafficMetadataSurvivesRatePolicy(t *testing.T) {
	ctx := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("fixture", "kept"))
	out := pluginv1.WithProbeTraffic(ctx)
	out = pluginv1.WithProbeRateLimitPolicy(out, pluginv1.ProbeRateLimitPolicy{
		NotBefore: time.Now().Add(time.Minute),
	})
	md, ok := metadata.FromOutgoingContext(out)
	require.True(t, ok)
	in := metadata.NewIncomingContext(t.Context(), md)
	require.True(t, pluginv1.IsProbeTraffic(in))
	require.Equal(t, []string{"kept"}, md.Get("fixture"))
	require.False(t, pluginv1.IsProbeTraffic(ctx))
	require.False(t, pluginv1.ReadProbeRateLimitPolicy(in).NotBefore.IsZero())
	original, _ := metadata.FromOutgoingContext(ctx)
	require.Empty(t, original.Get("x-sub2api-probe-traffic"))
	md.Append("x-sub2api-probe-traffic", "true")
	require.False(t, pluginv1.IsProbeTraffic(metadata.NewIncomingContext(t.Context(), md)))
}

// Runs the local unsigned candidate through the existing host process launcher,
// checksum check and gRPC transport. It never installs into the live manager.
func TestPluginEvidenceCandidateProcess(t *testing.T) {
	binary := os.Getenv("SUB2API_TEST_EVIDENCE_BINARY")
	if binary == "" {
		t.Skip("set SUB2API_TEST_EVIDENCE_BINARY to the locally built candidate")
	}
	file, err := os.Open(binary)
	require.NoError(t, err)
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	raw, err := os.ReadFile("../../../deploy/codex-lb-cookie-pin/manifest.source.json")
	require.NoError(t, err)
	var manifest PluginManifest
	require.NoError(t, json.Unmarshal(raw, &manifest))
	// A short socket-only root avoids macOS Unix socket path-length limits.
	root, err := os.MkdirTemp("/tmp", "s2-evidence-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "runtime")
	require.NoError(t, os.Mkdir(socket, 0700))
	installation := &PluginInstallation{
		PluginKey: "lyunlong.codex.lb-cookie-pin", Version: manifest.Version,
		BinaryPath: binary, BinarySHA256: hex.EncodeToString(hash.Sum(nil)),
	}
	var runtime *pluginRuntime
	launch := func() {
		var err error
		runtime, err = startPluginRuntime(t.Context(), installation, 10*time.Second, socket, nil)
		require.NoError(t, err)
	}
	launch()
	t.Cleanup(func() { runtime.kill() })
	var calls atomic.Int64
	var partial atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if partial.Load() {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture\"}\n\n")
		} else {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		}
	}))
	defer upstream.Close()
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1}
	forward := func(ctx context.Context) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/responses",
			strings.NewReader(`{"model":"fixture","reasoning":{"effort":"xhigh"}}`))
		require.NoError(t, err)
		// An HTTP header must never forge trusted host probe metadata.
		request.Header.Set("x-sub2api-probe-traffic", "true")
		require.True(t, runtime.beginRequest())
		response, err := runtime.roundTrip(ctx, request, "", account)
		if err != nil {
			runtime.finishRequest()
			return err
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		require.NoError(t, response.Body.Close())
		return readErr
	}
	require.NoError(t, forward(t.Context()))
	require.NoError(t, forward(pluginv1.WithProbeTraffic(t.Context())))
	partial.Store(true)
	require.NoError(t, forward(pluginv1.WithProbeTraffic(t.Context())))
	type counters struct {
		ProbePassed uint64 `json:"probe_passed"`
		Dispatched  uint64 `json:"business_dispatched"`
		Succeeded   uint64 `json:"business_succeeded"`
		Operation   struct {
			Phase string `json:"phase"`
		} `json:"operation"`
	}
	status := func() counters {
		health, err := runtime.api.Health(t.Context(), &pluginv1.HealthRequest{})
		require.NoError(t, err)
		var state struct {
			Evidence struct {
				Accounts []counters `json:"accounts"`
			} `json:"evidence"`
		}
		require.NoError(t, json.Unmarshal([]byte(health.StatusJson), &state))
		require.Len(t, state.Evidence.Accounts, 1)
		return state.Evidence.Accounts[0]
	}
	require.Eventually(t, func() bool { return status().Operation.Phase == "unknown" }, 3*time.Second, 10*time.Millisecond)
	before := status()
	require.EqualValues(t, 0, before.ProbePassed)
	require.EqualValues(t, 1, before.Dispatched)
	require.EqualValues(t, 1, before.Succeeded)
	runtime.kill()
	launch()
	require.Equal(t, before, status())
	require.Error(t, forward(pluginv1.WithProbeTraffic(t.Context())))
	require.EqualValues(t, 3, calls.Load())
	require.NoError(t, forward(t.Context()))
	require.EqualValues(t, 4, calls.Load())
}
