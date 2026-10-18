package transport

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

const streamAuthFixture = "event: response.failed\ndata: " +
	`{"response":{"error":{"code":"invalid_api_key"}}}` + "\n\n"

type rejectingEndStream struct{ *fixtureForwardStream }

func (s *rejectingEndStream) Send(frame *pluginv1.ForwardResponse) error {
	if frame.GetEnd() != nil {
		return io.ErrClosedPipe
	}
	return s.fixtureForwardStream.Send(frame)
}

func TestForwardCookieRequiresCompletedResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body, suffix string
		json, accepted     bool
		status             int
		rejectEnd          bool
	}{
		{name: "completed", body: completeFixture, accepted: true},
		{name: "json_completed", body: `{"object":"response","status":"completed"}`, json: true, accepted: true},
		{name: "compact_completed", suffix: "/compact", json: true, accepted: true,
			body: `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"fixture"}]}`},
		{name: "pending_error_superseded", body: "event: error\ndata: {}\n\n" + completeFixture, accepted: true},
		{name: "auth_failure", body: streamAuthFixture},
		{name: "quota_failure", body: streamQuotaFixture},
		{name: "failure_then_completed", body: streamQuotaFixture + completeFixture},
		{name: "completed_then_failure", body: completeFixture + streamAuthFixture},
		{name: "incomplete", body: "event: response.incomplete\ndata: {}\n\n"},
		{name: "truncated", body: strings.TrimSuffix(completeFixture, "\n")},
		{name: "empty"},
		{name: "malformed", body: "data: not-json\n\n"},
		{name: "http_failure", body: completeFixture, status: http.StatusInternalServerError},
		{name: "delivery_failure", body: completeFixture, rejectEnd: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, hostProbe := range []bool{false, true} {
				for _, reroll := range []bool{false, true} {
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						contentType := "text/event-stream"
						if tc.json {
							contentType = "application/json"
						}
						w.Header().Set("Content-Type", contentType)
						w.Header().Set("Set-Cookie", "__cflb=candidate; Max-Age=3500")
						w.Header().Set("Faster-Model", "fixture")
						if tc.status != 0 {
							w.WriteHeader(tc.status)
						}
						_, _ = io.WriteString(w, tc.body)
					}))
					store := cookiestore.New()
					cfg := probeTestConfig()
					cfg.RerollOnFasterModel = reroll
					store.SetConfig(cfg)
					store.Capture(42, []string{"__cflb=original; Max-Age=3500"}, time.Now())
					srv := stoppedProbeServer(t, store)
					ctx := t.Context()
					if hostProbe {
						ctx = hostProbeContext(ctx)
					}
					fixture := forwardFixture(ctx, up.URL+"/responses"+tc.suffix, "fixture")
					var err error
					if tc.rejectEnd {
						err = srv.Forward(&rejectingEndStream{fixture})
					} else {
						err = srv.Forward(fixture)
					}
					up.Close()
					if tc.rejectEnd {
						if !errors.Is(err, io.ErrClosedPipe) {
							t.Fatalf("delivery error changed: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					wantCookie := "__cflb=original"
					if tc.accepted {
						wantCookie = "__cflb=candidate"
						if reroll {
							wantCookie = ""
						}
					}
					if store.MergeHeader(42, "", time.Now()) != wantCookie ||
						(store.Status(time.Now()).Rerolls > 0) != (tc.accepted && reroll) {
						t.Fatalf("cookie adoption crossed terminal boundary: host_probe=%v reroll=%v", hostProbe, reroll)
					}
				}
			}
		})
	}
}

func TestProbeCookieRequiresCompletedResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		accepted   bool
		status     int
	}{
		{name: "complete", body: completeFixture, accepted: true},
		{name: "auth_failure", body: streamAuthFixture},
		{name: "quota_failure", body: streamQuotaFixture},
		{name: "incomplete", body: "event: response.incomplete\ndata: {}\n\n"},
		{name: "truncated", body: strings.TrimSuffix(completeFixture, "\n")},
		{name: "http_failure", body: completeFixture, status: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Set-Cookie", "__cflb=candidate; Max-Age=3500")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer up.Close()
			store := cookiestore.New()
			cfg := probeTestConfig()
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			store.Capture(42, []string{"__cflb=original; Max-Age=3500"}, time.Now())
			stashFrom(srv, 42, up.URL, time.Now())
			tmpl := *srv.templates[42]
			srv.sendProbe(t.Context(), 42, &tmpl, prober.QuestionFor(0), cfg)
			want := "__cflb=original"
			if tc.accepted {
				want = "__cflb=candidate"
			}
			if store.MergeHeader(42, "", time.Now()) != want {
				t.Fatal("probe adopted a cookie before completed Responses evidence")
			}
		})
	}
}

func TestForwardLateStreamOutcomeRespectsIdentity(t *testing.T) {
	for _, quota := range []bool{false, true} {
		for _, rotate := range []string{"same", "replacement", "aba"} {
			t.Run(rotate+map[bool]string{false: "_success", true: "_quota"}[quota], func(t *testing.T) {
				headersSeen, release := make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				var calls atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					first := calls.Add(1) == 1
					w.Header().Set("Content-Type", "text/event-stream")
					cookie := "__cflb=current; Max-Age=3500"
					if first {
						cookie = "__cflb=obsolete; Max-Age=3500"
					}
					w.Header().Set("Set-Cookie", cookie)
					w.(http.Flusher).Flush()
					if first {
						<-release
						if quota {
							_, _ = io.WriteString(w, streamQuotaFixture)
							return
						}
					}
					_, _ = io.WriteString(w, completeFixture)
				}))
				defer up.Close()
				defer unblock()
				store := cookiestore.New()
				cfg := probeTestConfig()
				store.SetConfig(cfg)
				srv := stoppedProbeServer(t, store)
				fixture := &notifyingForwardStream{
					fixtureForwardStream: forwardFixture(t.Context(), up.URL+"/responses", "old"),
					onStart:              func() { close(headersSeen) },
				}
				done := make(chan error, 1)
				go func() { done <- srv.Forward(fixture) }()
				select {
				case <-headersSeen:
				case <-time.After(3 * time.Second):
					t.Fatal("old response headers did not arrive")
				}
				identity := "old"
				if rotate != "same" {
					identity = "new"
				}
				if err := srv.Forward(forwardFixture(t.Context(), up.URL+"/responses", identity)); err != nil {
					t.Fatal(err)
				}
				if rotate == "aba" {
					if err := srv.Forward(forwardFixture(t.Context(), up.URL+"/responses", "old")); err != nil {
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
					t.Fatal("old response did not finish")
				}
				if store.MergeHeader(42, "", time.Now()) != "__cflb=current" {
					t.Fatal("late response replaced the current cookie")
				}
				state := srv.states[42]
				limited := state != nil && state.RetryNotBefore.After(time.Now())
				if limited != (quota && rotate == "same") {
					t.Fatal("late stream quota crossed an identity generation or was lost")
				}
			})
		}
	}
}
