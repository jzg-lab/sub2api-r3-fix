//go:build !windows

package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/deployment"
	"github.com/stretchr/testify/require"
)

type handoffPluginStub struct {
	running bool
	fail    bool
}

func (p *handoffPluginStub) Start(context.Context) error { p.running = true; return nil }
func (p *handoffPluginStub) Stop()                       { p.running = false }
func (p *handoffPluginStub) Ready(context.Context) error {
	if p.fail {
		return errors.New("failed")
	}
	return nil
}

func TestHandoffSwitchBeforePluginResume(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "owner.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0600))
	oldPlugin, newPlugin := &handoffPluginStub{}, &handoffPluginStub{}
	newRuntime := func(plugin *handoffPluginStub) *Handoff {
		h := NewHandoff(lock, "test", plugin, func(context.Context) bool { return true }, func(context.Context) error { return nil })
		h.background = &deployment.Gate{}
		return h
	}
	old, next := newRuntime(oldPlugin), newRuntime(newPlugin)
	ctx := context.Background()
	require.NoError(t, old.transition(ctx, "serve"))
	require.False(t, oldPlugin.running)
	require.NoError(t, old.transition(ctx, "resume"))
	require.True(t, oldPlugin.running)
	require.Error(t, next.transition(ctx, "serve"), "standby cannot own the same app/plugin lock")
	require.NoError(t, old.transition(ctx, "pause"))
	require.False(t, oldPlugin.running)
	require.NoError(t, next.transition(ctx, "serve"))
	require.False(t, newPlugin.running, "traffic switches before plugin restoration")
	response := httptest.NewRecorder()
	next.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(response, httptest.NewRequest("GET", "/api/example", nil))
	require.Equal(t, 204, response.Code)
	require.Equal(t, next.boot, response.Header().Get("X-Sub2api-Instance"))
	require.NoError(t, next.transition(ctx, "resume"))
	require.True(t, newPlugin.running)
	require.NoError(t, next.transition(ctx, "pause"))
	require.NoError(t, old.transition(ctx, "serve"))
	require.NoError(t, old.transition(ctx, "resume"), "old instance can resume without restarting")
	require.NoError(t, old.transition(ctx, "pause"))
}

func TestHandoffDrainTimeoutPreservesOldService(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "owner.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0600))
	p := &handoffPluginStub{}
	h := NewHandoff(lock, "test", p, func(context.Context) bool { return true }, func(context.Context) error { return nil })
	h.background = &deployment.Gate{}
	require.NoError(t, h.transition(context.Background(), "serve"))
	require.NoError(t, h.transition(context.Background(), "resume"))
	done, ok := h.requests.Enter()
	require.True(t, ok)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	require.ErrorIs(t, h.transition(ctx, "pause"), context.DeadlineExceeded)
	require.Equal(t, "active", h.phase.Load())
	require.True(t, p.running)
	done()
	require.NoError(t, h.transition(context.Background(), "pause"))
}

func TestHandoffPluginFailureRemainsExplicitlyPaused(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "owner.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0600))
	p := &handoffPluginStub{fail: true}
	h := NewHandoff(lock, "test", p, func(context.Context) bool { return true }, func(context.Context) error { return nil })
	h.background = &deployment.Gate{}
	require.NoError(t, h.transition(context.Background(), "serve"))
	require.Error(t, h.transition(context.Background(), "resume"))
	require.Equal(t, "serving_paused", h.phase.Load())
	require.False(t, p.running)
	_, ok := h.background.Enter()
	require.False(t, ok)
	handler := h.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for attempt := 0; attempt < 2; attempt++ {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(method, "/readyz", nil))
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.NotContains(t, response.Body.String(), `"status":"ready"`)
		}
		require.Equal(t, true, h.status(context.Background())["core_ready"], "private core check remains usable")
		require.Error(t, h.transition(context.Background(), "resume"))
	}
	p.fail = false
	require.NoError(t, h.transition(context.Background(), "resume"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.True(t, p.running)
	require.NoError(t, h.transition(context.Background(), "pause"))
}

func TestHandoffConfigurationFreezeKeepsCustomerAndEmergencyPaths(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "owner.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0600))
	h := NewHandoff(lock, "test", &handoffPluginStub{}, func(context.Context) bool { return true }, func(context.Context) error { return nil })
	h.background = &deployment.Gate{}
	ctx := context.Background()
	require.NoError(t, h.transition(ctx, "serve"))
	require.NoError(t, h.transition(ctx, "resume"))
	defer h.transition(ctx, "pause")
	handler := h.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for _, item := range []struct {
		method, path string
		status       int
	}{
		{"PUT", "/api/v1/admin/plugins/1/config", 503},
		{"PUT", "/api/v1/admin/backups/schedule", 503},
		{"POST", "/api/v1/admin/scheduled-test-plans", 503},
		{"GET", "/api/v1/admin/settings", 204},
		{"PATCH", "/api/v1/admin/accounts/1", 204},
		{"POST", "/api/v1/admin/system/deployment/rollback", 204},
		{"POST", "/v1/responses", 204},
		{"POST", "/api/v1/payment/callback", 204},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(item.method, item.path, nil))
		require.Equal(t, item.status, w.Code, item.path)
	}
	require.NoError(t, h.transition(ctx, "unfreeze"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/admin/settings", nil))
	require.Equal(t, 204, w.Code)
	require.NoError(t, h.transition(ctx, "freeze"))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("PUT", "/api/v1/admin/settings", nil))
	require.Equal(t, 503, w.Code)
}

func TestHandoffControlRejectsStaleOrTrailingCommand(t *testing.T) {
	h := NewHandoff("unused", "test", &handoffPluginStub{}, func(context.Context) bool { return true }, func(context.Context) error { return nil })
	for _, body := range []string{
		`{"action":"serve","boot_id":"stale","timeout_seconds":1}`,
		`{"action":"serve","boot_id":"` + h.boot + `","timeout_seconds":1} {}`,
	} {
		w := httptest.NewRecorder()
		h.control(w, httptest.NewRequest("POST", "/transition", strings.NewReader(body)))
		require.Equal(t, 400, w.Code)
		require.Equal(t, "standby", h.phase.Load())
	}
}
