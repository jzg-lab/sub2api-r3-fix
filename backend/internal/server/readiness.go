package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// Readiness is separate from liveness: initialization and required dependencies
// must succeed before a deployment opens ingress. No upstream model is called.
type Readiness struct {
	started atomic.Bool
	mu      sync.Mutex
	checked time.Time
	ready   bool
	checks  []func(context.Context) error
}

func NewReadiness(db *sql.DB, rdb *redis.Client, plugins *service.PluginManager) *Readiness {
	return &Readiness{checks: []func(context.Context) error{
		func(ctx context.Context) error {
			if db == nil {
				return errors.New("database unavailable")
			}
			return db.PingContext(ctx)
		},
		func(ctx context.Context) error {
			if rdb == nil {
				return errors.New("redis unavailable")
			}
			return rdb.Ping(ctx).Err()
		},
		plugins.Ready,
	}}
}

func (r *Readiness) SetReady(ready bool) { r.started.Store(ready) }

func (r *Readiness) check(ctx context.Context) bool {
	if !r.started.Load() || !r.mu.TryLock() {
		return false
	}
	if time.Since(r.checked) < time.Second {
		ready := r.ready
		r.mu.Unlock()
		return ready && r.started.Load()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		// Keep the lock until the driver returns: one stalled dependency must not
		// hold the HTTP response open or accumulate background checks.
		ready := len(r.checks) > 0
		for _, check := range r.checks {
			if err := check(ctx); err != nil {
				ready = false
				break
			}
		}
		ready = ready && ctx.Err() == nil
		r.ready, r.checked = ready, time.Now()
		r.mu.Unlock()
		done <- ready
	}()
	select {
	case ready := <-done:
		return ready && r.started.Load() && ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func (r *Readiness) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/readyz" {
			next.ServeHTTP(w, req)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body := `{"status":"ready"}`
		if !r.check(req.Context()) {
			w.WriteHeader(http.StatusServiceUnavailable)
			body = `{"status":"not_ready"}`
		}
		if req.Method != http.MethodHead {
			_, _ = w.Write([]byte(body))
		}
	})
}
