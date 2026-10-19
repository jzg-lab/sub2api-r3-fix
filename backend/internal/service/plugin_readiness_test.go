package service

import (
	"context"
	"testing"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/stretchr/testify/require"
)

func TestPluginReadinessRequiresStartedAuthoritativeState(t *testing.T) {
	var missing *PluginManager
	require.Error(t, missing.Ready(context.Background()))
	m := NewPluginManager(&bridgeStatusRepo{}, nil, nil, PluginHostInfo{})
	require.Error(t, m.Ready(context.Background()))
	m.started = true
	require.NoError(t, m.Ready(context.Background()), "disabled plugins must not block startup")
	m.repo = &bridgeErrorRepo{}
	require.Error(t, m.Ready(context.Background()))
}

func TestPluginReadinessDoesNotReadDuringReconfiguration(t *testing.T) {
	m := NewPluginManager(&bridgeStatusRepo{}, nil, nil, PluginHostInfo{})
	m.started = true
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	require.Error(t, m.Ready(context.Background()), "configuration writes must not race readiness reads")
}

func TestPluginReadinessChecksServingRuntime(t *testing.T) {
	installation := enabledBridgeInstallation()
	installation.State = PluginStateEnabled
	installation.BinarySHA256 = "expected"
	m := NewPluginManager(&bridgeStatusRepo{installations: []*PluginInstallation{installation}}, nil, nil, PluginHostInfo{})
	m.started = true
	require.Error(t, m.Ready(context.Background()))
	fake := &bridgeHealthClient{health: &pluginv1.HealthResponse{Healthy: true}}
	copy := *installation
	runtime := &pluginRuntime{installation: &copy, client: &hcplugin.Client{}, api: fake}
	route := &pluginRoute{pluginID: installation.ID, runtime: runtime, rolloutPercent: bindingRollout(installation.Bindings)}
	m.route.Store(route)
	require.NoError(t, m.Ready(context.Background()))
	require.Equal(t, 1, fake.calls)
	fake.health.Healthy = false
	require.Error(t, m.Ready(context.Background()))
	fake.health.Healthy = true
	runtime.draining.Store(true)
	require.Error(t, m.Ready(context.Background()))
	runtime.draining.Store(false)
	copy.BinarySHA256 = "old"
	require.Error(t, m.Ready(context.Background()))
	copy.BinarySHA256 = installation.BinarySHA256
	copy.ConfigEncrypted = "stale"
	require.Error(t, m.Ready(context.Background()))
	m.repo = &bridgeStatusRepo{}
	require.Error(t, m.Ready(context.Background()), "a stale enabled route must be reconciled")
}
