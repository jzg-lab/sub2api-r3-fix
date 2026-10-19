package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadinessLifecycleAndLiveness(t *testing.T) {
	calls := 0
	r := &Readiness{checks: []func(context.Context) error{func(ctx context.Context) error {
		_, bounded := ctx.Deadline()
		require.True(t, bounded)
		calls++
		return nil
	}}}
	h := r.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	require.Equal(t, 200, get("/health").Code)
	require.Equal(t, 503, get("/readyz").Code)
	require.Zero(t, calls)
	r.SetReady(true)
	require.Equal(t, 200, get("/readyz").Code)
	require.Equal(t, "no-store", get("/readyz").Header().Get("Cache-Control"))
	require.Equal(t, 1, calls, "cache bounds dependency polling")
	r.SetReady(false)
	require.Equal(t, 503, get("/readyz").Code, "shutdown must override cached success")
}

func TestReadinessDependencyFailuresArePrivate(t *testing.T) {
	for failure := 0; failure < 3; failure++ {
		r := &Readiness{}
		for i := 0; i < 3; i++ {
			r.checks = append(r.checks, func(context.Context) error {
				if i == failure {
					return errors.New("private connection details")
				}
				return nil
			})
		}
		r.SetReady(true)
		w := httptest.NewRecorder()
		r.Handler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
		require.Equal(t, 503, w.Code)
		require.JSONEq(t, `{"status":"not_ready"}`, w.Body.String())
	}
}

func TestReadinessDoesNotQueueConcurrentChecks(t *testing.T) {
	r := &Readiness{}
	r.SetReady(true)
	r.mu.Lock()
	require.False(t, r.check(context.Background()))
	r.mu.Unlock()
	require.False(t, r.check(context.Background()), "no checks is not ready")
}

func TestReadinessBoundsUnresponsiveDependency(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	r := &Readiness{checks: []func(context.Context) error{func(context.Context) error {
		calls.Add(1)
		<-release // Some database drivers do not return promptly when their context expires.
		return nil
	}}}
	r.SetReady(true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- r.check(ctx) }()
	select {
	case ready := <-done:
		require.False(t, ready)
	case <-time.After(time.Second):
		t.Fatal("readiness HTTP deadline was held by a stalled dependency")
	}
	require.False(t, r.check(context.Background()), "do not start another blocked check")
	require.Equal(t, int32(1), calls.Load())
}

func TestReadinessCanceledCheckDoesNotCacheSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Readiness{checks: []func(context.Context) error{func(context.Context) error {
		cancel()
		return nil
	}}}
	r.SetReady(true)
	require.False(t, r.check(ctx))
	require.False(t, r.check(context.Background()))
}

func TestReadinessMethods(t *testing.T) {
	r := &Readiness{checks: []func(context.Context) error{func(context.Context) error { return nil }}}
	r.SetReady(true)
	for _, method := range []string{"HEAD", "POST"} {
		w := httptest.NewRecorder()
		r.Handler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest(method, "/readyz", nil))
		if method == "POST" {
			require.Equal(t, 405, w.Code)
		} else {
			require.Equal(t, 200, w.Code)
		}
		require.Empty(t, w.Body.String())
	}
}
