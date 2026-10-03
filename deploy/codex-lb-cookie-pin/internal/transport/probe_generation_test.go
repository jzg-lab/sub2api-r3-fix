package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

func stoppedProbeServer(t *testing.T, store *cookiestore.Store) *Server {
	t.Helper()
	srv := New(store)
	srv.probeStop()
	srv.probeWg.Wait()
	return srv
}

func TestLateProbeCannotOverwriteReauthorization(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				w.Header().Set("Set-Cookie", "__cflb=obsolete; Max-Age=3500")
				if status != http.StatusOK {
					w.WriteHeader(status)
					return
				}
				writeSSEWithUsage(w, "gpt-6-astra", "2", 1200)
			}))
			defer up.Close()
			cfg := probeTestConfig()
			store := cookiestore.New()
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			stashFrom(srv, 42, up.URL, time.Now())
			tmpl := *srv.templates[42]
			oldState := prober.NewState(42)
			srv.states[42] = oldState
			go func() {
				defer close(done)
				srv.runProbeCycle(t.Context(), 42, &tmpl, oldState, cfg)
			}()
			<-started
			srv.stashTemplate(&pluginv1.ForwardRequestStart{
				Method: "POST", Url: up.URL, AccountId: 42,
				Headers: map[string]*pluginv1.HeaderValues{
					"Authorization": {Values: []string{"Bearer replacement-fixture"}},
				},
			}, []byte(`{"model":"gpt-6-astra"}`), time.Now())
			newState := prober.NewState(42)
			srv.probeMu.Lock()
			srv.states[42] = newState
			srv.probeMu.Unlock()
			store.Capture(42, []string{"__cflb=new-generation; Max-Age=3500"}, time.Now())
			close(release)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("stale cycle did not stop")
			}
			if srv.templates[42] == nil || srv.templates[42].Generation == tmpl.Generation {
				t.Fatal("replacement template lost")
			}
			if oldState.Probes != 0 || newState.Probes != 0 {
				t.Fatal("stale probe committed a verdict")
			}
			if got := store.MergeHeader(42, "", time.Now()); got != "__cflb=new-generation" {
				t.Fatal("stale probe modified the replacement cookie")
			}
		})
	}
}

func TestLateProbeCannotOverwriteNewBusinessCookie(t *testing.T) {
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store.Capture(42, []string{"__cflb=business; Max-Age=3500"}, time.Now())
		w.Header().Set("Set-Cookie", "__cflb=probe; Max-Age=3500")
		writeSSEWithUsage(w, "gpt-6-astra", "2", 1200)
	}))
	defer up.Close()
	stashFrom(srv, 42, up.URL, time.Now())
	state := prober.NewState(42)
	srv.runProbeCycle(t.Context(), 42, srv.templates[42], state, cfg)
	if state.Probes != 0 || store.MergeHeader(42, "", time.Now()) != "__cflb=business" {
		t.Fatal("late probe accepted or replaced a newer business cookie")
	}
}

func TestProbeRejectsUncertifiedStream(t *testing.T) {
	for _, mode := range []string{"delta-only", "incomplete", "missing-usage", "negative-usage", "oversized", "failed"} {
		t.Run(mode, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "delta-only":
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n")
				case "incomplete", "failed":
					fmt.Fprintf(w, "data: {\"type\":\"response.%s\",\"response\":{\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}]}}\n\n", mode)
				case "missing-usage":
					writeSSE(w, "gpt-6-astra", "21")
				case "negative-usage":
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":-1}}}\n\n")
				case "oversized":
					fmt.Fprint(w, strings.Repeat(" ", (2<<20)+1))
				}
			}))
			defer up.Close()
			cfg := probeTestConfig()
			store := cookiestore.New()
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			stashFrom(srv, 42, up.URL, time.Now())
			state := prober.NewState(42)
			state.ConsecPasses = 2
			srv.runProbeCycle(context.Background(), 42, srv.templates[42], state, cfg)
			if state.LastVerdict != prober.VerdictError || state.ConsecPasses != 0 || state.QualityRerolls != 0 {
				t.Fatalf("uncertified stream must not certify or reroll: %+v", state)
			}
		})
	}
}
