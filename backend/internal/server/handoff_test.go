//go:build !windows

package server

import (
	"context"
	"errors"
	"io"
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

func TestLiveHandoffKeepsOldStreamAndAcceptsNewRequests(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "owner.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0600))
	oldPlugin, nextPlugin := &handoffPluginStub{}, &handoffPluginStub{}
	makeRuntime := func(p *handoffPluginStub) *Handoff {
		h := NewHandoff(lock, "test", p, func(context.Context) bool { return true }, func(context.Context) error { return nil })
		h.background = &deployment.Gate{}
		return h
	}
	old, next := makeRuntime(oldPlugin), makeRuntime(nextPlugin)
	ctx := context.Background()
	require.NoError(t, old.transition(ctx, "serve"))
	require.NoError(t, old.transition(ctx, "resume"))
	end := make(chan struct{})
	server := httptest.NewServer(old.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "before\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
			return
		}
		select {
		case <-end:
			_, _ = io.WriteString(w, "after\n")
		case <-r.Context().Done():
		}
	})))
	defer server.Close()
	defer close(end)
	response, err := server.Client().Get(server.URL)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	first := make([]byte, len("before\n"))
	_, err = io.ReadFull(response.Body, first)
	require.NoError(t, err)
	require.Equal(t, "before\n", string(first))
	require.Equal(t, 1, old.requests.Pending())
	require.NoError(t, old.transition(ctx, "release"))
	require.Equal(t, "released", old.phase.Load())
	require.False(t, oldPlugin.running)
	require.Equal(t, 1, old.requests.Pending(), "releasing plugins must not wait for or cancel the core stream")
	require.NoError(t, next.transition(ctx, "serve"))
	require.NoError(t, next.transition(ctx, "resume"))
	require.NoError(t, old.transition(ctx, "serve"))
	require.Error(t, old.transition(ctx, "resume"), "only one background owner is allowed")
	w := httptest.NewRecorder()
	next.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, httptest.NewRequest("GET", "/v1/models", nil))
	require.Equal(t, 204, w.Code)
	// Simulate automatic rollback while the old request is still open.
	require.NoError(t, next.transition(ctx, "release"))
	require.NoError(t, old.transition(ctx, "serve"))
	require.NoError(t, old.transition(ctx, "resume"))
	end <- struct{}{}
	rest, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, "after\n", string(rest))
	require.NoError(t, old.transition(ctx, "pause"))
	require.NoError(t, next.transition(ctx, "pause"))
}

func TestLiveHandoffBackgroundTimeoutLeavesAdmissionOpen(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "owner.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0600))
	p := &handoffPluginStub{}
	h := NewHandoff(lock, "test", p, func(context.Context) bool { return true }, func(context.Context) error {
		t.Fatal("release must not wait for request settlement")
		return nil
	})
	h.background = &deployment.Gate{}
	ctx := context.Background()
	require.NoError(t, h.transition(ctx, "serve"))
	require.NoError(t, h.transition(ctx, "resume"))
	done, ok := h.background.Enter()
	require.True(t, ok)
	limited, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	require.ErrorIs(t, h.transition(limited, "release"), context.DeadlineExceeded)
	require.Equal(t, "active", h.phase.Load())
	require.True(t, p.running)
	requestDone, ok := h.requests.Enter()
	require.True(t, ok)
	requestDone()
	done()
	require.NoError(t, h.transition(ctx, "release"))
	require.False(t, p.running)
	require.NoError(t, h.transition(ctx, "release"), "release is retryable")
	require.Equal(t, 3, h.status(ctx)["handoff_protocol"])
}

func TestLiveHandoffWaitsForConfigurationWritesButNotCoreRequests(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "owner.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0600))
	p := &handoffPluginStub{}
	h := NewHandoff(lock, "test", p, func(context.Context) bool { return true }, func(context.Context) error { return nil })
	h.background = &deployment.Gate{}
	ctx := context.Background()
	require.NoError(t, h.transition(ctx, "serve"))
	require.NoError(t, h.transition(ctx, "resume"))
	require.NoError(t, h.transition(ctx, "unfreeze"))
	done, ok := h.configuration.Enter()
	require.True(t, ok)
	limited, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	require.ErrorIs(t, h.transition(limited, "release"), context.DeadlineExceeded)
	require.True(t, p.running)
	done()
	coreDone, ok := h.requests.Enter()
	require.True(t, ok)
	require.NoError(t, h.transition(ctx, "release"))
	require.Equal(t, 1, h.requests.Pending())
	coreDone()
	require.NoError(t, h.transition(ctx, "pause"))
}

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
	require.NoError(t, next.transition(ctx, "serve"))
	require.Error(t, next.transition(ctx, "resume"), "only plugin/background startup needs ownership")
	require.True(t, oldPlugin.running)
	_, admitted := next.background.Enter()
	require.False(t, admitted, "failed ownership acquisition cannot start autonomous work")
	require.False(t, newPlugin.running, "traffic switches before plugin restoration")
	response := httptest.NewRecorder()
	next.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(response, httptest.NewRequest("GET", "/api/example", nil))
	require.Equal(t, 204, response.Code)
	require.Equal(t, next.boot, response.Header().Get("X-Sub2api-Instance"))
	require.NoError(t, old.transition(ctx, "pause"))
	require.False(t, oldPlugin.running)
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
