package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
)

func waitProbeIdle(t *testing.T, srv *Server, accountID int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, busy := srv.probeRunning.Load(accountID); !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("probe did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProbeScanFollowsCaptureDeadlineAndBackoff(t *testing.T) {
	store := cookiestore.New()
	store.SetConfig(probeTestConfig())
	srv := stoppedProbeServer(t, store)
	now := time.Now()
	stashFrom(srv, 42, "http://unused.invalid/responses", now)
	store.Capture(42, []string{"__cflb=fixture; Max-Age=300"}, now)
	if delay := srv.scanProbeTemplates(t.Context(), now); delay != postCaptureProbeDelay {
		t.Fatalf("first signature deadline: %s", delay)
	}
	if delay := srv.scanProbeTemplates(t.Context(), now.Add(3*time.Second)); delay != 2*time.Second {
		t.Fatalf("scan postponed the capture-relative deadline: %s", delay)
	}
	state := srv.states[42]
	state.BackoffUntil = now.Add(time.Hour)
	state.NextProbeAt = state.BackoffUntil
	store.Capture(42, []string{"__cflb=replacement; Max-Age=300"}, now.Add(time.Second))
	if delay := srv.scanProbeTemplates(t.Context(), now.Add(4*time.Second)); delay != 10*time.Second {
		t.Fatalf("backoff must retain maintenance cadence: %s", delay)
	}
	if !state.NextProbeAt.Equal(state.BackoffUntil) {
		t.Fatal("fresh sign bypassed account backoff")
	}
	if _, busy := srv.probeRunning.Load(int64(42)); busy {
		t.Fatal("probe dispatched before deadline or during backoff")
	}
}

func TestFreshSignDiscoveryDoesNotAddAnotherDelay(t *testing.T) {
	now := time.Now()
	state := prober.NewState(42)
	state.NextProbeAt = now.Add(time.Minute)
	pullForFreshSign(state, now.Add(-30*time.Second), now)
	if !state.Due(now) {
		t.Fatal("already-settled signature was delayed again at discovery")
	}
}

func TestProbeScanReservesAccountBeforeDispatch(t *testing.T) {
	var requests atomic.Int64
	started, release := make(chan struct{}, 1), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		writePassingScheduledProbe(t, w, r)
	}))
	defer up.Close()
	defer unblock()
	store := cookiestore.New()
	store.SetConfig(probeTestConfig())
	srv := stoppedProbeServer(t, store)
	now := time.Now()
	stashFrom(srv, 42, up.URL, now)
	srv.states[42] = prober.NewState(42)
	srv.scanProbeTemplates(t.Context(), now)
	if _, busy := srv.probeRunning.Load(int64(42)); !busy {
		t.Fatal("dispatch returned without reserving the account")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("reserved probe did not start")
	}
	for range 100 {
		if delay := srv.scanProbeTemplates(t.Context(), now); delay != 10*time.Second {
			t.Fatal("busy account created a zero-delay scheduler loop")
		}
	}
	unblock()
	waitProbeIdle(t, srv, 42)
	srv.scanProbeTemplates(t.Context(), time.Now())
	if requests.Load() != 1 {
		t.Fatalf("duplicate scheduled request: %d", requests.Load())
	}
	if len(srv.probeWake) != 1 {
		t.Fatal("completion must wake the scheduler with a coalesced notification")
	}
}

func TestProbeLoopWakeUsesNearestDeadline(t *testing.T) {
	started := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		writePassingScheduledProbe(t, w, r)
	}))
	defer up.Close()
	store := cookiestore.New()
	store.SetConfig(probeTestConfig())
	srv := stoppedProbeServer(t, store)
	now := time.Now()
	stashFrom(srv, 42, up.URL, now)
	state := prober.NewState(42)
	deadline := now.Add(100 * time.Millisecond)
	state.NextProbeAt = deadline
	srv.states[42] = state
	srv.probeStop = nil
	srv.startProbeLoopLocked()
	defer func() {
		srv.probeStop()
		srv.probeWg.Wait()
		waitProbeIdle(t, srv, 42)
	}()
	for range 100 {
		srv.wakeProbeLoop()
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler waited for the old ten-second polling tick")
	}
	if time.Now().Before(deadline) {
		t.Fatal("notification bypassed the scheduled deadline")
	}
}

func writePassingScheduledProbe(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var body struct {
		Model string `json:"model"`
		Input []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		len(body.Input) != 1 || len(body.Input[0].Content) != 1 {
		t.Error("invalid scheduled probe request")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeSSEWithUsage(w, body.Model, canaryAnswer(body.Input[0].Content[0].Text), 1200)
}

func TestRepeatedCookieRenewalDoesNotAccelerateProbes(t *testing.T) {
	store := cookiestore.New()
	store.SetConfig(probeTestConfig())
	srv := stoppedProbeServer(t, store)
	now := time.Now()
	stashFrom(srv, 42, "http://unused.invalid/responses", now)
	store.Capture(42, []string{"__cflb=fixture; Max-Age=300"}, now)
	state := prober.NewState(42)
	state.KnownSignAt = now
	state.NextProbeAt = now.Add(2 * time.Minute)
	srv.states[42] = state
	store.Capture(42, []string{"__cflb=fixture; Max-Age=300"}, now.Add(time.Minute))
	srv.scanProbeTemplates(t.Context(), now.Add(time.Minute))
	if !state.NextProbeAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatal("unchanged cookie renewal pulled an unnecessary quality probe")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	srv.scanProbeTemplates(ctx, now.Add(3*time.Minute))
	if _, busy := srv.probeRunning.Load(int64(42)); busy {
		t.Fatal("stopped scheduler dispatched a due probe")
	}
}
