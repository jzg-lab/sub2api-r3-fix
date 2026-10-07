package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

type notifyingForwardStream struct {
	*fixtureForwardStream
	onStart func()
}

func (s *notifyingForwardStream) Send(response *pluginv1.ForwardResponse) error {
	if err := s.fixtureForwardStream.Send(response); err != nil {
		return err
	}
	if response.GetStart() != nil {
		s.onStart()
	}
	return nil
}

func TestForwardConcurrentQuotaDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name           string
		delayHeaders   bool
		rotateIdentity bool
		restoreOld     bool
	}{
		{name: "late_headers_extend_same_identity", delayHeaders: true},
		{name: "late_body_extends_same_identity"},
		{name: "late_body_cannot_affect_new_identity", rotateIdentity: true},
		{name: "late_body_cannot_cross_identity_aba", rotateIdentity: true, restoreOld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headersSeen, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					if tc.delayHeaders {
						close(headersSeen)
						<-release
					}
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(http.StatusTooManyRequests)
					w.(http.Flusher).Flush()
					if !tc.delayHeaders {
						<-release
					}
					fmt.Fprint(w, `{"error":{"type":"usage_limit_reached","resets_in_seconds":1800}}`)
					return
				}
				if tc.rotateIdentity {
					w.WriteHeader(http.StatusOK)
				} else {
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(http.StatusTooManyRequests)
				}
			}))
			defer up.Close()
			defer unblock()
			store := cookiestore.New()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			fixture := &notifyingForwardStream{
				fixtureForwardStream: forwardFixture(t.Context(), up.URL, "fixture-old"),
				onStart: func() {
					if !tc.delayHeaders {
						close(headersSeen)
					}
				},
			}
			before := time.Now()
			done := make(chan error, 1)
			go func() { done <- srv.Forward(fixture) }()
			select {
			case <-headersSeen:
			case <-time.After(3 * time.Second):
				t.Fatal("first response did not reach the required boundary")
			}
			identity := "fixture-old"
			if tc.rotateIdentity {
				identity = "fixture-new"
			}
			if err := srv.Forward(forwardFixture(t.Context(), up.URL, identity)); err != nil {
				t.Fatal(err)
			}
			if tc.restoreOld {
				if err := srv.Forward(forwardFixture(t.Context(), up.URL, "fixture-old")); err != nil {
					t.Fatal(err)
				}
			}
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("first response did not terminate")
			}
			state := srv.states[42]
			if state == nil {
				t.Fatal("quota deadline missing")
			}
			if tc.rotateIdentity {
				if state.RetryNotBefore.After(before.Add(5 * time.Minute)) {
					t.Fatal("obsolete response extended a replacement identity")
				}
			} else if state.RetryNotBefore.Before(before.Add(30 * time.Minute)) {
				t.Fatal("a shorter concurrent 429 discarded the longer quota deadline")
			}
			if state.Fails != 0 || state.QualityRerolls != 0 {
				t.Fatal("rate limiting was counted as a quality failure")
			}
		})
	}
}

func TestForwardAuthenticationFailureRetiresProbeTemplate(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer up.Close()
			store := cookiestore.New()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			state := prober.NewState(42)
			state.RetryNotBefore = time.Now().Add(time.Hour)
			srv.states[42] = state
			if err := srv.Forward(forwardFixture(t.Context(), up.URL, "fixture")); err != nil {
				t.Fatal(err)
			}
			if srv.templates[42] != nil {
				t.Fatal("rejected business credentials remain eligible for autonomous probes")
			}
			if srv.states[42] == nil || srv.states[42].RetryNotBefore != state.RetryNotBefore {
				t.Fatal("authentication failure erased an existing cooldown")
			}
		})
	}
}

func TestForwardQuotaBodyExtendsCooldownWithoutChangingResponse(t *testing.T) {
	body := `{"error":{"type":"usage_limit_reached","resets_in_seconds":1800}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("Set-Cookie", "__cflb=rejected; Max-Age=3500")
		w.Header().Set("Faster-Model", "fixture")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, body)
	}))
	defer up.Close()
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.InjectScope, cfg.RerollOnFasterModel = "all", true
	store.SetConfig(cfg)
	store.Capture(42, []string{"__cflb=original; Max-Age=3500"}, time.Now())
	srv := stoppedProbeServer(t, store)
	fixture := forwardFixture(t.Context(), up.URL, "fixture")
	before := time.Now()
	if err := srv.Forward(fixture); err != nil {
		t.Fatal(err)
	}
	state := srv.states[42]
	if state == nil || state.RetryNotBefore.Before(before.Add(30*time.Minute)) ||
		state.Fails != 0 || state.QualityRerolls != 0 {
		t.Fatal("quota body was ignored or counted as a quality failure")
	}
	if store.MergeHeader(42, "", time.Now()) != "__cflb=original" {
		t.Fatal("429 changed the pinned cookie")
	}
	var forwarded string
	for _, frame := range fixture.responses {
		forwarded += string(frame.GetBodyChunk())
	}
	if forwarded != body {
		t.Fatal("reading quota metadata altered the forwarded response")
	}
}

func TestForwardFailureFencesLateProbeSuccess(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			w.Header().Set("Set-Cookie", "__cflb=late; Max-Age=3500")
			writeSSEWithUsage(w, "gpt-6-astra", "2", 1200)
			return
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer up.Close()
	defer unblock()
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.InjectScope = "all"
	store.SetConfig(cfg)
	store.Capture(42, []string{"__cflb=original; Max-Age=3500"}, time.Now())
	srv := stoppedProbeServer(t, store)
	fixture := forwardFixture(t.Context(), up.URL, "fixture")
	srv.stashTemplate(fixture.requests[0].GetStart(), fixture.requests[1].GetBodyChunk(), time.Now())
	tmpl := *srv.templates[42]
	state := prober.NewState(42)
	state.ConsecPasses = 2
	srv.states[42] = state
	go func() {
		defer close(done)
		srv.runProbeCycle(t.Context(), 42, &tmpl, state, cfg)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe not started")
	}
	before := time.Now()
	if err := srv.Forward(fixture); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("late probe did not terminate")
	}
	if state.Probes != 0 || state.ConsecPasses != 0 ||
		state.RetryNotBefore.Before(before.Add(2*time.Minute)) {
		t.Fatal("late success bypassed a business cooldown")
	}
	if store.MergeHeader(42, "", time.Now()) != "__cflb=original" {
		t.Fatal("late success replaced the cookie")
	}
}

func TestForwardRateLimitConstrainsProbeScheduler(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
			}))
			defer up.Close()
			store := cookiestore.New()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			start := time.Now()
			if err := srv.Forward(forwardFixture(t.Context(), up.URL, "fixture")); err != nil {
				t.Fatal(err)
			}
			state := srv.states[42]
			if state == nil || state.RetryNotBefore.Before(start.Add(2*time.Minute)) {
				t.Fatal("business Retry-After did not reach the probe scheduler")
			}
			state.NextProbeAt = time.Now().Add(-time.Second)
			srv.scanProbeTemplates(t.Context(), time.Now())
			if _, running := srv.probeRunning.Load(int64(42)); running {
				t.Fatal("probe dispatched during business cooldown")
			}
		})
	}
}

func TestForwardLateFailureCannotRetireNewAuthorization(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer fixture-old" {
					close(started)
					<-release
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer up.Close()
			defer unblock()
			store := cookiestore.New()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			done := make(chan error, 1)
			go func() { done <- srv.Forward(forwardFixture(t.Context(), up.URL, "fixture-old")) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("request not started")
			}
			if err := srv.Forward(forwardFixture(t.Context(), up.URL, "fixture-new")); err != nil {
				t.Fatal(err)
			}
			current := srv.templates[42]
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if srv.templates[42] != current ||
				(srv.states[42] != nil && !srv.states[42].RetryNotBefore.IsZero()) {
				t.Fatal("late failure mutated a newer authorization")
			}
		})
	}
}

func TestForwardFailureCancelsInFlightProbe(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
			}))
			defer up.Close()
			store := cookiestore.New()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			fixture := forwardFixture(t.Context(), up.URL, "fixture")
			srv.stashTemplate(fixture.requests[0].GetStart(), fixture.requests[1].GetBodyChunk(), time.Now())
			srv.states[42] = prober.NewState(42)
			probeCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			srv.probeRunning.Store(int64(42), cancel)
			defer srv.probeRunning.Delete(int64(42))
			if err := srv.Forward(fixture); err != nil {
				t.Fatal(err)
			}
			if probeCtx.Err() == nil {
				t.Fatal("in-flight probe survived a current business failure")
			}
		})
	}
}
