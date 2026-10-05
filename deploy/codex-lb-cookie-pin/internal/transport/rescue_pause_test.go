package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
)

func TestRescuePauseCancelsProbeAndPreservesOtherAccounts(t *testing.T) {
	started, cancelled, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-release:
		}
	}))
	defer up.Close()
	defer close(release)
	cfg := probeTestConfig()
	cfg.InjectScope = "all"
	store := cookiestore.New()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	stashFrom(srv, 42, up.URL, time.Now())
	stashFrom(srv, 7, up.URL, time.Now())
	otherTemplate := srv.templates[7]
	otherState := prober.NewState(7)
	otherState.ConsecPasses = 4
	srv.states[7] = otherState
	oldTemplate := *srv.templates[42]
	state := prober.NewState(42)
	srv.states[42] = state
	go func() {
		defer close(done)
		srv.runProbeCycle(t.Context(), 42, &oldTemplate, state, cfg)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not start")
	}
	cfg.PausedAccountIDs = []int64{42}
	applyTestConfig(t, srv, cfg)
	for _, signal := range []chan struct{}{cancelled, done} {
		select {
		case <-signal:
		case <-time.After(5 * time.Second):
			t.Fatal("paused probe was not cancelled")
		}
	}
	if srv.templates[42] != nil || srv.states[42] != nil || state.Probes != 0 {
		t.Fatal("paused account retained a template or committed an old result")
	}
	stashFrom(srv, 42, up.URL, time.Now())
	if srv.templates[42] != nil {
		t.Fatal("late business request recreated a paused template")
	}
	if srv.templates[7] != otherTemplate || srv.states[7] != otherState || otherState.ConsecPasses != 4 {
		t.Fatal("pausing one account invalidated another account's evidence")
	}
	cfg.PausedAccountIDs = nil
	applyTestConfig(t, srv, cfg)
	if srv.templates[42] != nil {
		t.Fatal("unpause resurrected an old template")
	}
	stashFrom(srv, 42, up.URL, time.Now())
	if srv.templates[42] == nil || srv.templates[42].Generation == oldTemplate.Generation {
		t.Fatal("manual restart did not create a fresh probe generation")
	}
}

func TestLateForwardCannotUndoRescuePause(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Set-Cookie", "__cflb=stale; Max-Age=3500")
		writeSSEWithUsage(w, "gpt-6-astra", "2", 1200)
	}))
	defer up.Close()
	defer unblock()
	cfg := probeTestConfig()
	cfg.InjectScope = "all"
	store := cookiestore.New()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	go func() { done <- srv.Forward(forwardFixture(context.Background(), up.URL, "fixture")) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not start")
	}
	cfg.PausedAccountIDs = []int64{42}
	applyTestConfig(t, srv, cfg)
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not finish")
	}
	if srv.templates[42] != nil || srv.states[42] != nil || store.MergeHeader(42, "", time.Now()) != "" {
		t.Fatal("late forward recreated paused evidence or cookie")
	}
}

func TestPausedForwardCannotContaminateRestart(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Set-Cookie", "__cflb=obsolete; Max-Age=3500")
		writeSSEWithUsage(w, "gpt-6-astra", "2", 1200)
	}))
	defer up.Close()
	defer unblock()
	cfg := probeTestConfig()
	cfg.InjectScope = "all"
	cfg.PausedAccountIDs = []int64{42}
	store := cookiestore.New()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	go func() { done <- srv.Forward(forwardFixture(t.Context(), up.URL, "fixture")) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not start")
	}
	cfg.PausedAccountIDs = nil
	applyTestConfig(t, srv, cfg)
	stashFrom(srv, 42, up.URL, time.Now())
	fresh := srv.templates[42]
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not finish")
	}
	if srv.templates[42] != fresh || store.MergeHeader(42, "", time.Now()) != "" {
		t.Fatal("request started during pause contaminated the restarted rescue")
	}
}
