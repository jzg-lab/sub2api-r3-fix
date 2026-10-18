package transport

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/evidence"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
)

const streamQuotaFixture = "event: response.failed\ndata: " +
	`{"response":{"status":"failed","error":{"code":"usage_limit_reached","resets_in_seconds":7200}}}` + "\n\n"

func TestResponseObserverQuotaDeadline(t *testing.T) {
	bare := "event: error\ndata: " +
		`{"code":"rate_limit_exceeded","resets_in_seconds":"7200"}` + "\n\n"
	for _, tc := range []struct {
		name, raw string
		sse       bool
		limited   bool
		deadline  bool
	}{
		{"nested", streamQuotaFixture, true, true, true},
		{"bare", bare, true, true, true},
		{"bare_then_complete", bare + completeFixture, true, false, false},
		{"failed_then_complete", streamQuotaFixture + completeFixture, true, true, true},
		{"complete_then_failed", completeFixture + streamQuotaFixture, true, true, true},
		{"json", `{"object":"response","status":"failed","error":{"type":"GoUsageLimitError","resets_in_seconds":7200}}`, false, true, true},
		{"null_error_bare", "event: error\ndata: " +
			`{"code":"rate_limit_exceeded","error":null,"resets_in_seconds":7200}` + "\n\n", true, true, true},
		{"negative", strings.ReplaceAll(streamQuotaFixture, ":7200", ":-1"), true, true, false},
		{"unknown_error", strings.ReplaceAll(streamQuotaFixture, "usage_limit_reached", "unknown_failure"), true, false, false},
		{"prose_only", "data: " +
			`{"type":"response.output_text.delta","delta":"rate_limit_exceeded; resets_in_seconds=7200"}` + "\n\n", true, false, false},
		{"oversized_error", "event: response.failed\ndata: " +
			`{"error":{"code":"usage_limit_reached","resets_in_seconds":7200,"message":"` +
			strings.Repeat("x", maxProbeErrorBody) + `"}}` + "\n\n", true, true, false},
		{"partial_event", strings.TrimSuffix(streamQuotaFixture, "\n"), true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{1, 13, len(tc.raw)} {
				before := time.Now()
				o := responseObserver{sse: tc.sse}
				for raw := []byte(tc.raw); len(raw) > 0; {
					n := min(size, len(raw))
					o.write(raw[:n])
					raw = raw[n:]
				}
				o.finish()
				if (o.failureStatus() == http.StatusTooManyRequests) != tc.limited {
					t.Fatalf("quota classification mismatch for chunk=%d", size)
				}
				deadline := o.failureRetryDeadline()
				if tc.deadline {
					if deadline.Before(before.Add(2*time.Hour)) || deadline.After(time.Now().Add(2*time.Hour)) {
						t.Fatalf("stream reset lost for chunk=%d: %v", size, deadline)
					}
				} else if !deadline.IsZero() {
					t.Fatalf("untrusted or superseded event established a reset: %v", deadline)
				}
			}
		})
	}
}

func TestForwardStreamQuotaDeadlinePersists(t *testing.T) {
	for _, lane := range []string{"business", "host-probe", "host-probe-without-template"} {
		t.Run(lane, func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Retry-After", "120")
				_, _ = io.WriteString(w, streamQuotaFixture)
			}))
			defer up.Close()
			store := cookiestore.New()
			cfg := probeTestConfig()
			if lane == "host-probe-without-template" {
				cfg.Enabled = false
			}
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			journal, dir := persistentFixture(t)
			srv.evidence = journal
			ctx := t.Context()
			if lane != "business" {
				ctx = hostProbeContext(ctx)
			}
			fixture := forwardFixture(ctx, up.URL+"/responses", "fixture")
			before := time.Now()
			if err := srv.Forward(fixture); err != nil {
				t.Fatal(err)
			}
			var delivered strings.Builder
			for _, frame := range fixture.responses {
				delivered.Write(frame.GetBodyChunk())
			}
			if delivered.String() != streamQuotaFixture || calls.Load() != 1 {
				t.Fatal("quota observation changed delivery or replayed the request")
			}
			saved, err := journal.Read(42)
			if err != nil || saved.NotBefore.Before(before.Add(2*time.Hour)) ||
				saved.BusinessSucceeded != 0 || saved.ProbePassed != 0 {
				t.Fatalf("stream quota failed to reach durable protection: %+v %v", saved, err)
			}
			if lane != "host-probe-without-template" {
				state := srv.states[42]
				if state == nil || state.RetryNotBefore.Before(before.Add(2*time.Hour)) ||
					state.Fails != 0 || state.QualityRerolls != 0 {
					t.Fatal("stream quota was ignored or counted as a quality defect")
				}
			}
			reopened, err := evidence.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.Begin(42, evidence.Cohort{}, time.Now()); !errors.Is(err, evidence.ErrBackoff) {
				t.Fatalf("plugin reload lost the upstream reset: %v", err)
			}
		})
	}
}

func TestProbeStreamQuotaDeadlinePersists(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamQuotaFixture)
	}))
	defer up.Close()
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	stashFrom(srv, 42, up.URL, time.Now())
	tmpl := *srv.templates[42]
	before := time.Now()
	verdict, result, _ := srv.sendProbe(t.Context(), 42, &tmpl, prober.QuestionFor(0), cfg)
	if verdict != prober.VerdictError || result != "failed:not-completed" {
		t.Fatalf("quota was not a received rejection: %s %s", verdict, result)
	}
	saved, err := srv.evidence.Read(42)
	if err != nil || saved.NotBefore.Before(before.Add(2*time.Hour)) {
		t.Fatalf("probe SSE reset lost: %+v %v", saved, err)
	}
	if srv.states[42] == nil || srv.states[42].RetryNotBefore.Before(before.Add(2*time.Hour)) {
		t.Fatal("probe scheduler lost the upstream reset")
	}
	_, result, _ = srv.sendProbe(t.Context(), 42, &tmpl, prober.QuestionFor(0), cfg)
	if result != "blocked:backoff" || calls.Load() != 1 {
		t.Fatalf("probe sent before reset: %s calls=%d", result, calls.Load())
	}
}

func TestProbeAuthenticationRetryAfterRetiresTemplate(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "7200")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"type":"authentication_error"}}`)
			}))
			defer up.Close()
			store := cookiestore.New()
			cfg := probeTestConfig()
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			store.Capture(42, []string{"__cflb=original; Max-Age=3500"}, time.Now())
			stashFrom(srv, 42, up.URL, time.Now())
			tmpl := *srv.templates[42]
			before := time.Now()
			verdict, result, _ := srv.sendProbe(t.Context(), 42, &tmpl, prober.QuestionFor(0), cfg)
			if verdict != prober.VerdictError || result != fmt.Sprintf("http:%d+tmpl-dropped", status) ||
				srv.templates[42] != nil || calls.Load() != 1 {
				t.Fatalf("Retry-After retained rejected credentials: %s %s calls=%d", verdict, result, calls.Load())
			}
			saved, err := srv.evidence.Read(42)
			if err != nil || saved.NotBefore.Before(before.Add(2*time.Hour)) {
				t.Fatalf("retiring credentials erased the recovery deadline: %+v %v", saved, err)
			}
			state := srv.states[42]
			if state == nil || state.RetryNotBefore.Before(before.Add(2*time.Hour)) ||
				state.Fails != 0 || state.QualityRerolls != 0 ||
				store.MergeHeader(42, "", time.Now()) != "__cflb=original" {
				t.Fatal("credential retirement lost protection or changed cookie/quality state")
			}
		})
	}
}

func TestForwardAuthenticationRetryAfterRetiresTemplate(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "7200")
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, streamAuthFixture)
			}))
			defer up.Close()
			store := cookiestore.New()
			store.SetConfig(probeTestConfig())
			srv := stoppedProbeServer(t, store)
			before := time.Now()
			if err := srv.Forward(forwardFixture(t.Context(), up.URL+"/responses", "fixture")); err != nil {
				t.Fatal(err)
			}
			state := srv.states[42]
			saved, err := srv.evidence.Read(42)
			if srv.templates[42] != nil || state == nil ||
				state.RetryNotBefore.Before(before.Add(2*time.Hour)) ||
				err != nil || saved.NotBefore.Before(before.Add(2*time.Hour)) {
				t.Fatal("business auth failure retained credentials or lost its recovery deadline")
			}
		})
	}
}
