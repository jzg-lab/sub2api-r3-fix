package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/pluginconfig"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

func applyTestConfig(t *testing.T, srv *Server, cfg pluginconfig.Config) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.ApplyConfig(t.Context(), &pluginv1.ApplyConfigRequest{ConfigJson: raw})
	if err != nil || !resp.GetApplied() {
		t.Fatalf("apply config failed: %v, %v", resp, err)
	}
}

func TestApplyConfigInvalidatesQualification(t *testing.T) {
	changes := map[string]func(*pluginconfig.Config){
		"disabled":  func(c *pluginconfig.Config) { c.Enabled = false },
		"probe-off": func(c *pluginconfig.Config) { c.QualityProbeEnabled = false },
		"grading":   func(c *pluginconfig.Config) { c.ProbeMinReasoningTokens++ },
		"model":     func(c *pluginconfig.Config) { c.ProbeModel = "fixture-model" },
		"scope":     func(c *pluginconfig.Config) { c.InjectScope = "codex" },
		"drop":      func(c *pluginconfig.Config) { c.DropAccountIDs = []int64{42} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				w.Header().Set("Set-Cookie", "__cflb=old-result; Max-Age=3500")
				writeSSEWithUsage(w, "gpt-6-astra", "2", 1200)
			}))
			defer up.Close()
			defer unblock()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			store := cookiestore.New()
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			stashFrom(srv, 42, up.URL, time.Now())
			store.Capture(42, []string{"__cflb=current; Max-Age=3500"}, time.Now())
			state := prober.NewState(42)
			state.ConsecPasses = 3
			state.ConsecFails = 2
			state.BurstProbes = 7
			state.BackoffUntil = time.Now().Add(time.Hour)
			state.NextProbeAt = state.BackoffUntil
			srv.states[42] = state
			before := *state
			tmpl := *srv.templates[42]
			go func() {
				defer close(done)
				srv.runProbeCycle(context.Background(), 42, &tmpl, state, cfg)
			}()
			<-started
			change(&cfg)
			applyTestConfig(t, srv, cfg)
			view := srv.snapshotProber(time.Now(), cfg)
			for _, account := range view.Accounts {
				if account.AccountID == 42 && account.ConsecPasses != 0 {
					t.Error("old qualification remains visible to the host")
				}
			}
			unblock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("invalidated probe did not finish")
			}
			if state.Probes != before.Probes {
				t.Error("old probe committed after config transition")
			}
			if state.ConsecFails != before.ConsecFails || state.BurstProbes != before.BurstProbes ||
				!state.BackoffUntil.Equal(before.BackoffUntil) {
				t.Error("configuration invalidation lost failure/backoff protection")
			}
			if name == "drop" {
				if store.MergeHeader(42, "", time.Now()) != "" {
					t.Error("late response restored a manually dropped cookie")
				}
			} else if store.MergeHeader(42, "", time.Now()) == "__cflb=old-result" {
				t.Error("late response changed the cookie after config transition")
			}
		})
	}
}

func TestApplyConfigReplayKeepsEvidence(t *testing.T) {
	cfg := probeTestConfig()
	store := cookiestore.New()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	stashFrom(srv, 42, "https://chatgpt.com/backend-api/codex/responses", time.Now())
	state := prober.NewState(42)
	state.ConsecPasses = 3
	srv.states[42] = state
	generation := srv.templates[42].Generation
	applyTestConfig(t, srv, cfg)
	if srv.templates[42].Generation != generation || state.ConsecPasses != 3 {
		t.Fatal("unchanged configuration invalidated valid evidence")
	}
}

func TestProbeCleanupAlsoRunsWhenDisabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := probeTestConfig()
		cfg.QualityProbeEnabled = enabled
		store := cookiestore.New()
		store.SetConfig(cfg)
		srv := stoppedProbeServer(t, store)
		now := time.Now()
		stashFrom(srv, 42, "https://chatgpt.com/backend-api/codex/responses", now.Add(-25*time.Hour))
		state := prober.NewState(42)
		state.ConsecPasses = 3
		srv.states[42] = state
		// Active probing must not extend the absolute lifetime indefinitely.
		srv.templates[42].SeenAt = now
		stashFrom(srv, 43, "https://chatgpt.com/backend-api/codex/responses", now.Add(-25*time.Hour))
		srv.scanProbeTemplates(t.Context(), now)
		if len(srv.templates) != 0 || len(srv.states) != 0 {
			t.Fatalf("disabled=%v: expired templates or qualification survived", !enabled)
		}
		if got := srv.snapshotProber(now, cfg); len(got.Accounts) != 0 || got.Probes != 0 {
			t.Fatal("retired evidence is still exported")
		}
	}
}

func TestTemplateIdentityBindsModelAndCreationTime(t *testing.T) {
	cfg := probeTestConfig()
	store := cookiestore.New()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	now := time.Now()
	start := &pluginv1.ForwardRequestStart{
		AccountId: 42, Method: "POST", Url: "https://chatgpt.com/backend-api/codex/responses",
	}
	srv.stashTemplate(start, []byte(`{"model":"fixture-a"}`), now)
	first := *srv.templates[42]
	srv.stashTemplate(start, []byte(`{"model":"fixture-a"}`), now.Add(time.Minute))
	if srv.templates[42].Generation != first.Generation || !srv.templates[42].CreatedAt.Equal(now) {
		t.Fatal("ordinary business refresh renewed the evidence lifetime")
	}
	state := prober.NewState(42)
	state.ConsecPasses = 3
	state.BackoffUntil = now.Add(time.Hour)
	srv.states[42] = state
	store.Capture(42, []string{"__cflb=retained; Max-Age=3500"}, now)
	srv.stashTemplate(start, []byte(`{"model":"fixture-b"}`), now.Add(2*time.Minute))
	if srv.templates[42].Generation == first.Generation || srv.states[42].ConsecPasses != 0 {
		t.Fatal("different model inherited qualification")
	}
	if !srv.templates[42].CreatedAt.Equal(now) || !state.BackoffUntil.Equal(now.Add(time.Hour)) ||
		store.MergeHeader(42, "", now) != "__cflb=retained" {
		t.Fatal("model change reset account backoff, cookie or template lifetime")
	}
}

func TestLateBusinessResponseAfterConfigChange(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				w.Header().Set("Set-Cookie", "__cflb=obsolete; Max-Age=3500")
			}))
			defer up.Close()
			defer unblock()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			store := cookiestore.New()
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			stream := forwardFixture(t.Context(), up.URL, "fixture")
			stream.requests[0].GetStart().Method = method
			done := make(chan error, 1)
			go func() { done <- srv.Forward(stream) }()
			<-started
			cfg.InjectScope = "codex"
			applyTestConfig(t, srv, cfg)
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("business request did not finish")
			}
			if store.MergeHeader(42, "", time.Now()) != "" {
				t.Fatal("out-of-scope response populated the cookie jar")
			}
		})
	}
}
