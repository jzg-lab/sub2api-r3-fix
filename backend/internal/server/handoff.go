package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/deployment"
)

type handoffPlugin interface {
	Start(context.Context) error
	Stop()
	Ready(context.Context) error
}

// The private Unix socket controls one process. The shared file lock grants
// exactly one process permission to serve requests and own the unchanged plugin.
type Handoff struct {
	operation               sync.Mutex
	phase                   atomic.Value
	requests                deployment.Gate
	configuration           deployment.Gate
	boot, source, ownerPath string
	owner                   *os.File
	plugins                 handoffPlugin
	coreReady               func(context.Context) bool
	settle                  func(context.Context) error
	background              *deployment.Gate
}

func NewHandoff(ownerPath, source string, plugins handoffPlugin, ready func(context.Context) bool, settle func(context.Context) error) *Handoff {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		panic(err)
	}
	h := &Handoff{boot: hex.EncodeToString(nonce[:]), source: source, ownerPath: ownerPath,
		plugins: plugins, coreReady: ready, settle: settle, background: &deployment.Background}
	h.phase.Store("standby")
	return h
}

func (h *Handoff) Listen(path string) (func(), error) {
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0007 != 0 {
		return nil, errors.New("private deployment socket directory required")
	}
	lifecycle, err := privateLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			lifecycle.Close()
		}
	}()
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("deployment path is not a socket")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	srv := &http.Server{Handler: http.HandlerFunc(h.control), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 125 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	failed = false
	return func() { _ = srv.Close(); _ = lifecycle.Close() }, nil
}

func (h *Handoff) status(ctx context.Context) map[string]any {
	return map[string]any{"boot_id": h.boot, "source": h.source, "phase": h.phase.Load(),
		"core_ready": h.coreReady(ctx), "requests": h.requests.Pending(), "background": h.background.Pending()}
}

func (h *Handoff) control(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet && r.URL.Path == "/status" {
		_ = json.NewEncoder(w).Encode(h.status(r.Context()))
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/transition" {
		http.NotFound(w, r)
		return
	}
	var command struct {
		Action  string `json:"action"`
		Boot    string `json:"boot_id"`
		Timeout int    `json:"timeout_seconds"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&command); err != nil || decoder.Decode(new(any)) != io.EOF || command.Boot != h.boot || command.Timeout < 1 || command.Timeout > 120 {
		http.Error(w, "invalid or stale deployment command", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(command.Timeout)*time.Second)
	defer cancel()
	if err := h.transition(ctx, command.Action); err != nil {
		slog.Warn("deployment_transition_failed", "action", command.Action, "phase", h.phase.Load(),
			"requests", h.requests.Pending(), "background", h.background.Pending(), "error", err)
		http.Error(w, "handoff transition failed", http.StatusConflict)
		return
	}
	_ = json.NewEncoder(w).Encode(h.status(ctx))
}

func (h *Handoff) transition(ctx context.Context, action string) error {
	if !h.operation.TryLock() {
		return errors.New("handoff operation in progress")
	}
	defer h.operation.Unlock()
	phase := h.phase.Load().(string)
	switch action {
	case "freeze":
		h.configuration.SetOpen(false)
		return h.configuration.Wait(ctx)
	case "unfreeze":
		if phase != "active" {
			return errors.New("only active instance can accept configuration changes")
		}
		h.configuration.SetOpen(true)
	case "serve":
		if phase == "serving_paused" || phase == "active" {
			return nil
		}
		if phase != "standby" || !h.coreReady(ctx) {
			return errors.New("standby is not ready")
		}
		owner, err := privateLock(h.ownerPath)
		if err != nil {
			return err
		}
		h.owner = owner
		h.phase.Store("serving_paused")
		h.requests.SetOpen(true)
	case "resume":
		if phase == "active" {
			return nil
		}
		if phase != "serving_paused" || h.owner == nil {
			return errors.New("instance is not serving")
		}
		if err := h.plugins.Start(ctx); err != nil {
			h.plugins.Stop()
			return err
		}
		if err := h.plugins.Ready(ctx); err != nil {
			h.plugins.Stop()
			return err
		}
		h.background.SetOpen(true)
		h.phase.Store("active")
	case "pause":
		if phase == "standby" {
			return nil
		}
		if phase != "active" && phase != "serving_paused" {
			return errors.New("invalid handoff state")
		}
		h.phase.Store("draining")
		h.configuration.SetOpen(false)
		h.requests.SetOpen(false)
		h.background.SetOpen(false)
		err := h.requests.Wait(ctx)
		if err == nil {
			err = h.background.Wait(ctx)
		}
		if err == nil {
			err = h.settle(ctx)
		}
		if err != nil {
			h.background.SetOpen(phase == "active")
			h.phase.Store(phase)
			h.requests.SetOpen(true)
			return err
		}
		h.plugins.Stop()
		if h.owner != nil {
			_ = h.owner.Close()
			h.owner = nil
		}
		h.phase.Store("standby")
	default:
		return errors.New("unknown handoff action")
	}
	return nil
}

func (h *Handoff) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sub2api-Instance", h.boot)
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/readyz" {
			phase := h.phase.Load().(string)
			if phase == "active" {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(`{"status":"not_ready"}`))
			}
			return
		}
		done, ok := h.requests.Enter()
		if !ok {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "deployment handoff in progress", http.StatusServiceUnavailable)
			return
		}
		defer done()
		if handoffConfigurationWrite(r) {
			release, allowed := h.configuration.Enter()
			if !allowed {
				w.Header().Set("Retry-After", "2")
				http.Error(w, "configuration is paused during deployment", http.StatusServiceUnavailable)
				return
			}
			defer release()
		}
		next.ServeHTTP(w, r)
	})
}

func handoffConfigurationWrite(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false
	}
	// Customer traffic, payments and emergency account changes remain available.
	for _, prefix := range []string{
		"/api/v1/admin/settings", "/api/v1/admin/plugins", "/api/v1/admin/scheduled-test-plans",
		"/api/v1/admin/backups", "/api/v1/admin/system", "/api/v1/admin/data-management",
		"/api/v1/admin/ops", "/api/v1/admin/prompt-audit/config", "/api/v1/admin/risk-control/config",
		"/api/v1/admin/openai-operations/settings", "/api/v1/admin/channel-monitors",
		"/api/v1/admin/channel-monitor-templates", "/api/v1/admin/channel-monitor-v2/config",
		"/api/v1/admin/accounts/upstream-billing-probe/settings", "/api/v1/admin/accounts/ollama-cloud-usage/settings",
		"/api/v1/admin/accounts/opencode-go-usage/settings",
	} {
		if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
			// The deployment button itself is not a business configuration mutation.
			return !strings.HasPrefix(r.URL.Path, "/api/v1/admin/system/deployment/")
		}
	}
	return false
}
