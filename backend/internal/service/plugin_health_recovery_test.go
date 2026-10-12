package service

import (
	"context"
	"errors"
	"testing"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	hcplugin "github.com/hashicorp/go-plugin"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestPluginHealthTimeoutPreservesInflightRequests(t *testing.T) {
	fake := &bridgeHealthClient{err: status.Error(codes.DeadlineExceeded, "health deadline")}
	manager, runtime, route := newPluginHealthFixture(t, fake)
	require.True(t, runtime.beginRequest())
	defer runtime.finishRequest()

	require.Error(t, manager.reconcileOnce(context.Background()))
	require.Same(t, route, manager.route.Load(), "one health timeout must not destroy an active transport")
	require.Same(t, runtime, manager.runtimes[route.pluginID])
	require.False(t, runtime.client.Exited())
	require.EqualValues(t, 1, runtime.inFlight.Load())
	require.False(t, runtime.beginRequest(), "unknown health must reject new requests, not fall back")
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.True(t, manager.ShouldRouteOpenAIOAuth(account))
	_, handled, err := manager.RoundTripOpenAIOAuth(context.Background(), nil, "", account)
	require.True(t, handled, "quarantine must not bypass the enabled plugin")
	require.ErrorIs(t, err, errPluginRuntimeUnavailable)

	fake.err = nil
	fake.health = &pluginv1.HealthResponse{Healthy: true}
	require.NoError(t, manager.reconcileOnce(context.Background()))
	require.Same(t, route, manager.route.Load())
	require.True(t, runtime.healthFailureSince.IsZero())
	require.True(t, runtime.beginRequest(), "the same process must resume after health recovers")
	runtime.finishRequest()
}

func TestPluginHealthTimeoutRetiresOnlyAfterInflightFinishes(t *testing.T) {
	manager, runtime, route := newPluginHealthFixture(t, &bridgeHealthClient{err: context.DeadlineExceeded})
	require.True(t, runtime.beginRequest())
	require.Error(t, manager.reconcileOnce(context.Background()))
	require.Same(t, route, manager.route.Load())
	runtime.finishRequest()
	require.Error(t, manager.reconcileOnce(context.Background()))
	require.Nil(t, manager.route.Load().runtime)
	require.Empty(t, manager.runtimes)
}

func newPluginHealthFixture(t *testing.T, api pluginv1.TransportPluginClient) (*PluginManager, *pluginRuntime, *pluginRoute) {
	t.Helper()
	installation := enabledBridgeInstallation()
	runtime := &pluginRuntime{
		installation: installation,
		client:       hcplugin.NewClient(&hcplugin.ClientConfig{}),
		api:          api,
		done:         make(chan struct{}),
	}
	t.Cleanup(runtime.kill)
	manager := NewPluginManager(&bridgeStatusRepo{installations: []*PluginInstallation{installation}}, nil, nil, PluginHostInfo{})
	route := &pluginRoute{pluginID: installation.ID, runtime: runtime, rolloutPercent: bindingRollout(installation.Bindings)}
	manager.route.Store(route)
	manager.runtimes[installation.ID] = runtime
	return manager, runtime, route
}

func TestPluginHealthFailureRetirementBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		health   *pluginv1.HealthResponse
		inFlight bool
		expired  bool
	}{
		{name: "deadline-idle", err: context.DeadlineExceeded},
		{name: "deadline-grace-expired", err: status.Error(codes.DeadlineExceeded, "timeout"), inFlight: true, expired: true},
		{name: "explicit-unhealthy", health: &pluginv1.HealthResponse{Healthy: false}, inFlight: true},
		{name: "nil-response", inFlight: true},
		{name: "transport-lost", err: status.Error(codes.Unavailable, "connection closed"), inFlight: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, runtime, _ := newPluginHealthFixture(t, &bridgeHealthClient{err: tc.err, health: tc.health})
			if tc.inFlight {
				require.True(t, runtime.beginRequest())
				defer runtime.finishRequest()
			}
			if tc.expired {
				runtime.healthFailureSince = time.Now().Add(-pluginHealthDrainGrace)
			}
			require.Error(t, manager.reconcileOnce(context.Background()))
			require.NotNil(t, manager.route.Load())
			require.Nil(t, manager.route.Load().runtime)
			require.Empty(t, manager.runtimes)
			require.True(t, runtime.draining.Load())
			require.False(t, runtime.beginRequest())
		})
	}
}

func TestPluginHealthParentCancellationDoesNotInvalidateRoute(t *testing.T) {
	manager, runtime, route := newPluginHealthFixture(t, &bridgeHealthClient{err: context.Canceled})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, manager.reconcileOnce(ctx), context.Canceled)
	require.Same(t, route, manager.route.Load())
	require.False(t, runtime.healthUnavailable.Load())
	require.True(t, runtime.beginRequest())
	runtime.finishRequest()
}

func TestPluginHealthStaleFailureCannotRemoveNewRoute(t *testing.T) {
	manager, oldRuntime, oldRoute := newPluginHealthFixture(t, &bridgeHealthClient{})
	newRuntime := &pluginRuntime{done: make(chan struct{})}
	newRoute := &pluginRoute{pluginID: oldRoute.pluginID, runtime: newRuntime, rolloutPercent: 100}
	manager.runtimes[oldRoute.pluginID] = newRuntime
	manager.route.Store(newRoute)
	require.NoError(t, manager.markRuntimeUnavailable(oldRoute, "old health failed"))
	require.Same(t, newRoute, manager.route.Load())
	require.Same(t, newRuntime, manager.runtimes[oldRoute.pluginID])
	require.False(t, oldRuntime.draining.Load())
	require.False(t, newRuntime.draining.Load())
}

type inspectingHealthClient struct {
	pluginv1.TransportPluginClient
	liveness bool
}

func (c *inspectingHealthClient) Health(ctx context.Context, _ *pluginv1.HealthRequest, _ ...grpc.CallOption) (*pluginv1.HealthResponse, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	c.liveness = pluginv1.IsLivenessHealth(metadata.NewIncomingContext(context.Background(), md))
	return &pluginv1.HealthResponse{Healthy: true, StatusJson: `{"evidence":{"available":true}}`}, nil
}

func TestPluginHealthRuntimeUsesLivenessButStatusDoesNot(t *testing.T) {
	client := &inspectingHealthClient{}
	manager, runtime, route := newPluginHealthFixture(t, client)
	require.NoError(t, runtime.checkHealth(context.Background()))
	require.True(t, client.liveness)
	got, err := manager.probeStatus(context.Background(), route.pluginID)
	require.NoError(t, err)
	require.False(t, client.liveness)
	require.JSONEq(t, `{"evidence":{"available":true}}`, got.StatusJSON)
	runtime.api = &bridgeHealthClient{health: nil}
	require.Error(t, runtime.checkHealth(context.Background()), "nil Health response must not panic")
	runtime.api = &bridgeHealthClient{err: context.DeadlineExceeded}
	require.True(t, errors.Is(runtime.checkHealth(context.Background()), context.DeadlineExceeded))
}
