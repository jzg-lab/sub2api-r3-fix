package transport

import (
	"context"
	"errors"
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

func TestForwardNewTemplateWaitsForSuccessfulResponse(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				w.WriteHeader(status)
			}))
			defer up.Close()
			defer unblock()
			store := cookiestore.New()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			done := make(chan error, 1)
			go func() { done <- srv.Forward(forwardFixture(t.Context(), up.URL, "fixture")) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("business request did not start")
			}
			// Advance scheduler time without sleeps or any real provider request.
			srv.scanProbeTemplates(t.Context(), time.Now())
			srv.probeMu.Lock()
			prematureState := srv.states[42] != nil
			srv.probeMu.Unlock()
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("business request did not finish")
			}
			if prematureState {
				t.Fatal("unconfirmed business credentials entered autonomous scheduling")
			}
			now := time.Now()
			srv.scanProbeTemplates(t.Context(), now)
			srv.probeMu.Lock()
			state := srv.states[42]
			// An explicit quota hold is retained, but is not probe admission.
			admitted := state != nil && !state.RetryNotBefore.After(now)
			srv.probeMu.Unlock()
			if admitted != (status == http.StatusOK) {
				t.Fatalf("probe admission=%v after business status=%d", admitted, status)
			}
			if calls.Load() != 1 {
				t.Fatal("request admission generated extra upstream traffic")
			}
		})
	}
}

func TestForwardSameIdentityCannotConfirmNewerModelTemplate(t *testing.T) {
	for _, changeModel := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_model=%v", changeModel), func(t *testing.T) {
			testForwardLateAdmission(t, changeModel)
		})
	}
}

func testForwardLateAdmission(t *testing.T, changeModel bool) {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer up.Close()
	defer unblock()
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.InjectScope = "all"
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	done := make(chan error, 1)
	go func() { done <- srv.Forward(forwardFixture(t.Context(), up.URL, "fixture")) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("old-model request did not start")
	}
	newer := forwardFixture(t.Context(), up.URL, "fixture")
	if changeModel {
		newer.requests[1] = &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{
			BodyChunk: []byte(`{"model":"fixture-new-model","input":"fixture"}`),
		}}
	}
	if err := srv.Forward(newer); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old-model request did not finish")
	}
	srv.scanProbeTemplates(t.Context(), time.Now())
	if srv.states[42] != nil {
		t.Fatal("late success admitted a failed replacement generation")
	}
}

func TestForwardAdmissionWaitsForCompleteBody(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "complete")
	}))
	defer up.Close()
	defer unblock()
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.InjectScope = "all"
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	stream := &notifyingForwardStream{
		fixtureForwardStream: forwardFixture(t.Context(), up.URL, "fixture"),
		onStart:              func() { close(started) },
	}
	done := make(chan error, 1)
	go func() { done <- srv.Forward(stream) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("successful response headers did not arrive")
	}
	srv.scanProbeTemplates(t.Context(), time.Now())
	srv.probeMu.Lock()
	premature := srv.templates[42].Confirmed || srv.states[42] != nil
	srv.probeMu.Unlock()
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("response did not complete")
	}
	if premature {
		t.Fatal("headers alone admitted an unfinished stream")
	}
	if !srv.templates[42].Confirmed {
		t.Fatal("completed successful response did not admit its template")
	}
}

type endFailureForwardStream struct{ *fixtureForwardStream }

func (s *endFailureForwardStream) Send(response *pluginv1.ForwardResponse) error {
	if response.GetEnd() != nil {
		return errors.New("fixture consumer disconnected")
	}
	return s.fixtureForwardStream.Send(response)
}

func TestForwardIncompleteAttemptNeverAdmitsTemplate(t *testing.T) {
	for _, kind := range []string{"invalid-proxy", "cancelled", "truncated-body", "consumer-disconnected"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if kind == "truncated-body" {
					w.Header().Set("Content-Length", "100")
				}
				fmt.Fprint(w, "short")
			}))
			defer up.Close()
			store := cookiestore.New()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fixture := forwardFixture(ctx, up.URL, "fixture")
			switch kind {
			case "invalid-proxy":
				fixture.requests[0].GetStart().ProxyUrl = "unsupported://localhost"
			case "cancelled":
				cancel()
			}
			var err error
			if kind == "consumer-disconnected" {
				err = srv.Forward(&endFailureForwardStream{fixture})
			} else {
				err = srv.Forward(fixture)
			}
			if kind == "consumer-disconnected" && err == nil {
				t.Fatal("expected failed downstream completion")
			}
			if kind != "consumer-disconnected" && (err != nil || len(fixture.responses) == 0 ||
				fixture.responses[len(fixture.responses)-1].GetError() == nil) {
				t.Fatal("failed attempt did not retain its transport error")
			}
			srv.scanProbeTemplates(t.Context(), time.Now())
			if srv.templates[42].Confirmed || srv.states[42] != nil {
				t.Fatal("incomplete attempt admitted an autonomous probe")
			}
			if (kind == "invalid-proxy" || kind == "cancelled") && calls.Load() != 0 {
				t.Fatal("invalid or cancelled request reached the upstream")
			}
		})
	}
}

func TestForwardConfirmedTemplateSurvivesOrdinaryTraffic(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 2 {
			close(started)
			<-release
			w.WriteHeader(http.StatusBadRequest)
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
	if err := srv.Forward(forwardFixture(t.Context(), up.URL, "fixture")); err != nil {
		t.Fatal(err)
	}
	srv.scanProbeTemplates(t.Context(), time.Now())
	state := srv.states[42]
	if state == nil {
		t.Fatal("first successful request did not create a probe schedule")
	}
	state.NextProbeAt = time.Now().Add(time.Hour)
	state.ConsecPasses = 2
	done := make(chan error, 1)
	go func() { done <- srv.Forward(forwardFixture(t.Context(), up.URL, "fixture")) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("second request did not start")
	}
	srv.probeMu.Lock()
	retained := srv.templates[42].Confirmed && srv.states[42] == state && state.ConsecPasses == 2
	srv.probeMu.Unlock()
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second request did not terminate")
	}
	if !retained || !srv.templates[42].Confirmed || srv.states[42] != state || state.ConsecPasses != 2 {
		t.Fatal("ordinary same-identity traffic reset an already confirmed template")
	}
}

func TestForwardSuccessfulRetryAdmitsWithoutForgivingCooldown(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.InjectScope = "all"
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	if err := srv.Forward(forwardFixture(t.Context(), up.URL, "fixture")); err != nil {
		t.Fatal(err)
	}
	deadline := srv.states[42].RetryNotBefore
	if srv.templates[42].Confirmed || !deadline.After(time.Now()) {
		t.Fatal("rejected first request admitted a probe or lost its cooldown")
	}
	// Even after the hold expires, a failed template must not launch probes.
	srv.scanProbeTemplates(t.Context(), deadline.Add(time.Second))
	if _, running := srv.probeRunning.Load(int64(42)); running {
		t.Fatal("cooldown expiry admitted rejected credentials")
	}
	if err := srv.Forward(forwardFixture(t.Context(), up.URL, "fixture")); err != nil {
		t.Fatal(err)
	}
	if !srv.templates[42].Confirmed || !srv.states[42].RetryNotBefore.Equal(deadline) {
		t.Fatal("fresh success failed to admit or erased an upstream cooldown")
	}
	srv.scanProbeTemplates(t.Context(), time.Now())
	if _, running := srv.probeRunning.Load(int64(42)); running {
		t.Fatal("successful request bypassed the retained cooldown")
	}
	if calls.Load() != 2 {
		t.Fatal("admission generated extra upstream requests")
	}
}

func TestUnconfirmedTemplateExpiresWithoutErasingCooldown(t *testing.T) {
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.InjectScope = "all"
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	now := time.Now()
	fixture := forwardFixture(t.Context(), "http://unused.invalid/responses", "fixture")
	srv.stashTemplate(fixture.requests[0].GetStart(), fixture.requests[1].GetBodyChunk(), now.Add(-time.Hour))
	state := prober.NewState(42)
	state.RetryNotBefore = now.Add(time.Hour)
	srv.states[42] = state
	srv.scanProbeTemplates(t.Context(), now)
	if srv.templates[42] != nil {
		t.Fatal("cooldown retained an expired unconfirmed credential snapshot")
	}
	if srv.states[42] == nil || !srv.states[42].RetryNotBefore.Equal(state.RetryNotBefore) {
		t.Fatal("template retirement discarded an upstream cooldown")
	}
}
