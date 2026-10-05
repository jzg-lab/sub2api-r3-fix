package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
)

// Local binary/RPC check only; release package trust is verified separately.
func TestPluginRescuePauseLocalRuntime(t *testing.T) {
	binaryPath := os.Getenv("SUB2API_TEST_RESCUE_PLUGIN_BINARY")
	if binaryPath == "" {
		t.Skip("SUB2API_TEST_RESCUE_PLUGIN_BINARY is not set")
	}
	binary, err := os.ReadFile(binaryPath)
	require.NoError(t, err)
	hash := sha256.Sum256(binary)
	root, err := os.MkdirTemp(os.TempDir(), "pause-rpc-")
	require.NoError(t, err)
	defer os.RemoveAll(root)
	manager := NewPluginManager(nil, nil, testPluginConfig(root, false), PluginHostInfo{})
	ids := []int64{42}
	var sourceErr error
	manager.SetRescueTerminatedAccountSource(func(context.Context) ([]int64, error) { return ids, sourceErr })
	installation := &PluginInstallation{ID: 7, PluginKey: rescueCookiePinPluginID, Version: "0.3.9", BinaryPath: binaryPath, BinarySHA256: hex.EncodeToString(hash[:])}
	runtime, err := manager.newRuntime(t.Context(), installation)
	require.NoError(t, err)
	defer runtime.kill()
	require.NotNil(t, runtime.pausedAccountIDsSource, "manager must attach DB suppression to the cookie plugin")
	require.NoError(t, runtime.validateAndApplyConfig(t.Context(), []byte(`{"quality_probe_enabled":true,"inject_scope":"all","persist_kv":false}`)))
	manager.runtimes[installation.ID] = runtime
	manager.route.Store(&pluginRoute{pluginID: installation.ID, runtime: runtime})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "local fixture")
	}))
	defer up.Close()
	forward := func() {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, up.URL, bytes.NewBufferString(`{"model":"fixture-model"}`))
		require.NoError(t, err)
		require.True(t, runtime.beginRequest())
		response, err := runtime.roundTrip(t.Context(), request, "", &Account{ID: 42})
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
	}
	accountCount := func() int {
		health, err := runtime.api.Health(t.Context(), &pluginv1.HealthRequest{})
		require.NoError(t, err)
		var status struct {
			Prober struct {
				Accounts []json.RawMessage `json:"accounts"`
			} `json:"prober"`
		}
		require.NoError(t, json.Unmarshal([]byte(health.StatusJson), &status))
		return len(status.Prober.Accounts)
	}
	forward()
	require.Never(t, func() bool { return accountCount() != 0 }, 11*time.Second, 100*time.Millisecond)
	ids = nil
	require.NoError(t, manager.SyncRescueProbePauses(t.Context()))
	forward()
	require.Eventually(t, func() bool { return accountCount() == 1 }, 12*time.Second, 100*time.Millisecond)
	ids = []int64{42}
	require.NoError(t, manager.SyncRescueProbePauses(t.Context()))
	require.Zero(t, accountCount())
	sourceErr = errors.New("termination state unavailable")
	require.Error(t, manager.SyncRescueProbePauses(t.Context()))
	require.Nil(t, manager.route.Load().runtime)
	require.True(t, runtime.client.Exited(), "failed synchronization must stop the actual plugin process")
}
