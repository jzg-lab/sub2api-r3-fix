package transport

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
)

func TestProbeRetryDeadline(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		status int
		header string
		delay  time.Duration
	}{
		{429, "", time.Minute},
		{429, "0", time.Minute},
		{429, "-1", time.Minute},
		{429, "garbage", time.Minute},
		{429, "120", 2 * time.Minute},
		{429, now.Add(4 * time.Minute).Format(http.TimeFormat), 4 * time.Minute},
		{503, "90", 90 * time.Second},
		{503, "", 0},
		{500, "120", 0},
		{401, "120", 0},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.header), func(t *testing.T) {
			deadline := probeRetryDeadline(tc.status, http.Header{"Retry-After": {tc.header}}, now)
			if tc.delay == 0 {
				if !deadline.IsZero() {
					t.Fatalf("unexpected retry deadline: %v", deadline)
				}
			} else if !deadline.Equal(now.Add(tc.delay)) {
				t.Fatalf("deadline=%v, expected delay=%v", deadline, tc.delay)
			}
		})
	}
	deadline := probeRetryDeadline(429, http.Header{"Retry-After": {"18446744073709551615"}}, now)
	if !deadline.After(now.Add(24 * time.Hour)) {
		t.Fatal("untrusted delay overflowed into an immediate retry")
	}
}

func TestRateLimitedProbeDoesNotRetryRerollOrBypassCooldown(t *testing.T) {
	var requests atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded"}}`)
	}))
	defer up.Close()
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	stashFrom(srv, 42, up.URL, time.Now())
	state := prober.NewState(42)
	state.ConsecPasses = 2
	srv.states[42] = state
	tmpl := *srv.templates[42]
	start := time.Now()
	srv.runProbeCycle(t.Context(), 42, &tmpl, state, cfg)
	if requests.Load() != 1 || state.Probes != 1 || state.Fails != 0 || state.QualityRerolls != 0 ||
		state.ConsecPasses != 0 || state.LastVerdict != prober.VerdictError {
		t.Fatalf("rate limit retried or affected quality: requests=%d state=%+v", requests.Load(), state)
	}
	if state.RetryNotBefore.Before(start.Add(2 * time.Minute)) {
		t.Fatal("server cooldown was lost")
	}
	// A new cookie and adaptive scheduling must not supersede Retry-After.
	pullForFreshSign(state, time.Now(), time.Now())
	state.NextProbeAt = time.Now().Add(-time.Second)
	srv.scanProbeTemplates(t.Context(), time.Now())
	if _, running := srv.probeRunning.Load(42); running {
		t.Fatal("scheduler bypassed Retry-After")
	}
	view := srv.snapshotProber(time.Now(), cfg).Accounts[0]
	if !view.InBackoff || view.NextProbeAt.Before(state.RetryNotBefore) {
		t.Fatal("status hid the effective cooldown")
	}
	srv.probeMu.Lock()
	srv.invalidateProbeLocked(42)
	srv.probeMu.Unlock()
	if state.RetryNotBefore.Before(start.Add(2 * time.Minute)) {
		t.Fatal("template invalidation erased rate limit")
	}
}

func TestCooldownSurvivesCredentialRotationAndTemplateRetirement(t *testing.T) {
	store := cookiestore.New()
	store.SetConfig(probeTestConfig())
	srv := stoppedProbeServer(t, store)
	now := time.Now()
	stashFrom(srv, 42, "https://example.invalid/responses", now)
	state := prober.NewState(42)
	state.RetryNotBefore = now.Add(48 * time.Hour)
	state.ConsecPasses = 2
	srv.states[42] = state
	srv.probeMu.Lock()
	srv.resetProbeStateLocked(42, now)
	srv.probeMu.Unlock()
	if srv.states[42].ConsecPasses != 0 || srv.states[42].RetryNotBefore != state.RetryNotBefore {
		t.Fatal("reset must discard quality evidence but retain rate limit")
	}
	srv.scanProbeTemplates(t.Context(), now.Add(25*time.Hour))
	if srv.templates[42] != nil || srv.states[42].RetryNotBefore != state.RetryNotBefore {
		t.Fatal("retirement lost cooldown")
	}
	stashFrom(srv, 42, "https://example.invalid/responses", now.Add(26*time.Hour))
	if srv.states[42].RetryNotBefore != state.RetryNotBefore {
		t.Fatal("replacement template lost cooldown")
	}
	srv.scanProbeTemplates(t.Context(), now.Add(51*time.Hour))
	if srv.states[42] != nil {
		t.Fatal("expired orphan state was not retired")
	}
}
