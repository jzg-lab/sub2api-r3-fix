package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type rescuePausePluginClient struct {
	pluginv1.TransportPluginClient
	applied [][]byte
	reject  bool
}

func (c *rescuePausePluginClient) ValidateConfig(_ context.Context, req *pluginv1.ValidateConfigRequest, _ ...grpc.CallOption) (*pluginv1.ValidateConfigResponse, error) {
	return &pluginv1.ValidateConfigResponse{Valid: true, NormalizedConfigJson: req.ConfigJson}, nil
}

func (c *rescuePausePluginClient) ApplyConfig(_ context.Context, req *pluginv1.ApplyConfigRequest, _ ...grpc.CallOption) (*pluginv1.ApplyConfigResponse, error) {
	c.applied = append(c.applied, append([]byte(nil), req.ConfigJson...))
	return &pluginv1.ApplyConfigResponse{Applied: !c.reject}, nil
}

func TestPluginRescuePauseSyncAndRestart(t *testing.T) {
	ids := []int64{42, 42, 7}
	source := func(context.Context) ([]int64, error) { return ids, nil }
	for _, runtime := range []*pluginRuntime{
		{api: &rescuePausePluginClient{}, pausedAccountIDsSource: source},
		{api: &rescuePausePluginClient{}, pausedAccountIDsSource: source},
	} {
		base, err := runtime.validateAndApplyNormalizedConfig(t.Context(), []byte(`{"quality_probe_enabled":true,"paused_account_ids":[999],"drop_account_ids":[8]}`))
		require.NoError(t, err)
		require.NotContains(t, string(base), "paused_account_ids", "derived state must not be persisted as administrator config")
		client := runtime.api.(*rescuePausePluginClient)
		var applied map[string]any
		require.NoError(t, json.Unmarshal(client.applied[0], &applied))
		require.Equal(t, []any{float64(7), float64(42)}, applied["paused_account_ids"])
		manager := &PluginManager{runtimes: map[int64]*pluginRuntime{1: runtime}}
		manager.route.Store(&pluginRoute{pluginID: 1, runtime: runtime})
		require.NoError(t, manager.SyncRescueProbePauses(t.Context()))
		require.Len(t, client.applied, 1, "unchanged DB state must not reapply config")
		ids = nil
		require.NoError(t, manager.SyncRescueProbePauses(t.Context()))
		require.Len(t, client.applied, 2)
		require.NotContains(t, string(client.applied[1]), "paused_account_ids")
		require.NotContains(t, string(client.applied[1]), "drop_account_ids", "sync must not replay a one-shot reroll")
		ids = []int64{42, 42, 7}
	}
}

func TestPluginRescuePauseSyncFailsClosed(t *testing.T) {
	for _, mode := range []string{"database", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			client := &rescuePausePluginClient{reject: mode == "unsupported"}
			runtime := &pluginRuntime{api: client, probeBaseConfig: []byte(`{}`), pausedAccountIDsSource: func(context.Context) ([]int64, error) {
				if mode == "database" {
					return nil, errors.New("database unavailable")
				}
				return []int64{42}, nil
			}}
			manager := &PluginManager{runtimes: map[int64]*pluginRuntime{1: runtime}}
			manager.route.Store(&pluginRoute{pluginID: 1, runtime: runtime, rolloutPercent: 100})
			require.Error(t, manager.SyncRescueProbePauses(t.Context()))
			require.Nil(t, manager.route.Load().runtime)
			require.NotEmpty(t, manager.route.Load().unavailable)
			require.Empty(t, manager.runtimes)
		})
	}
}

func TestPluginRescuePauseSyncRespectsLockTimeout(t *testing.T) {
	manager := &PluginManager{}
	manager.operationMu.Lock()
	defer manager.operationMu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, manager.SyncRescueProbePauses(ctx), context.DeadlineExceeded)
}

func TestPluginRescuePauseSurvivesConfigSaveAndRollback(t *testing.T) {
	client := &rescuePausePluginClient{}
	runtime := &pluginRuntime{api: client, installation: &PluginInstallation{ID: 1, BinarySHA256: "fixture"}, pausedAccountIDsSource: func(context.Context) ([]int64, error) { return []int64{42}, nil }}
	repo := &pluginConfigRepository{installation: runtime.installation}
	manager := &PluginManager{repo: repo, encryptor: pluginTokenEncryptor{}, runtimes: map[int64]*pluginRuntime{1: runtime}}
	canonical, err := manager.SaveConfig(t.Context(), 1, json.RawMessage(`{"quality_probe_enabled":true,"paused_account_ids":[]}`))
	require.NoError(t, err)
	require.NotContains(t, string(canonical), "paused_account_ids")
	require.NotContains(t, repo.encrypted, "paused_account_ids")
	require.Contains(t, string(client.applied[0]), `"paused_account_ids":[42]`)
	require.NoError(t, manager.restoreRuntimeConfig(1, runtime, json.RawMessage(`{"quality_probe_enabled":false}`)))
	require.Contains(t, string(client.applied[1]), `"paused_account_ids":[42]`)
}
