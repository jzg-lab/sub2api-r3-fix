package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
)

func TestProbeTerminalDoesNotWaitForTransportEOF(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		phase string
	}{
		{name: "completed", phase: "ok"},
		{
			name:  "rejected",
			body:  "event: response.failed\ndata: {\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"service_unavailable\"}}}\n\n",
			phase: "failed",
		},
		{
			name:  "done_without_terminal",
			body:  "data: [DONE]\n\n",
			phase: "unknown",
		},
		{
			name:  "partial_terminal",
			body:  "event: response.completed\ndata: {\"response\":{\"status\":\"completed\"}}\n",
			phase: "unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			q := prober.QuestionFor(0)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if tc.body == "" {
					writeSSEWithUsage(w, "gpt-6-astra", canaryAnswer(q.Prompt), 1200)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, tc.body)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer up.Close()
			store := cookiestore.New()
			cfg := probeTestConfig()
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			stashFrom(srv, 9, up.URL, time.Now())
			tmpl := *srv.templates[9]
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			verdict, answer, _ := srv.sendProbe(ctx, 9, &tmpl, q, cfg)
			state, err := srv.evidence.Read(9)
			if err != nil || state.Operation.Phase != tc.phase || calls.Load() != 1 {
				t.Fatalf("terminal handling: phase=%s want=%s calls=%d err=%v result=%s",
					state.Operation.Phase, tc.phase, calls.Load(), err, answer)
			}
			if tc.phase == "ok" && verdict != prober.VerdictPass {
				t.Fatalf("completed probe was not graded: %v %s", verdict, answer)
			}
			if tc.phase != "unknown" && ctx.Err() != nil {
				t.Fatal("authoritative terminal waited for the request deadline")
			}
		})
	}
}

func TestReadProbeSSETerminalBoundaries(t *testing.T) {
	completed := "event: response.completed\ndata: {\"response\":{\"status\":\"completed\"}}\n\n"
	for _, tc := range []struct {
		name      string
		body      string
		limit     int
		completed bool
		failed    bool
		oversized bool
	}{
		{name: "header_only_type", body: completed, completed: true},
		{name: "bare_error_then_completed", body: "event: error\ndata: {\"code\":\"service_unavailable\"}\n\n" + completed, completed: true},
		{name: "failed_then_completed", body: "event: response.failed\ndata: {\"response\":{\"status\":\"failed\"}}\n\n" + completed, failed: true},
		{name: "partial_terminal", body: strings.TrimSuffix(completed, "\n")},
		{name: "malformed_before_terminal", body: "data: invalid-json\n\n" + completed},
		{name: "done_without_terminal", body: "data: [DONE]\n\n"},
		{name: "exact_limit", body: completed, limit: len(completed), completed: true},
		{name: "over_limit", body: completed, limit: len(completed) - 1, oversized: true},
		{name: "long_event_line", body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", 8192) + "\"}\n\n" + completed, completed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := tc.limit
			if limit == 0 {
				limit = 2 << 20
			}
			raw, observed, err := readProbeSSE(iotest.OneByteReader(strings.NewReader(tc.body)), limit)
			if err != nil || observed.finish() != tc.completed || observed.failed != tc.failed ||
				(len(raw) > limit) != tc.oversized {
				t.Fatalf("completion=%v failure=%v bytes=%d limit=%d err=%v",
					observed.finish(), observed.failed, len(raw), limit, err)
			}
		})
	}
	t.Run("terminal_before_transport_error", func(t *testing.T) {
		reader := io.MultiReader(strings.NewReader(completed), iotest.ErrReader(io.ErrUnexpectedEOF))
		_, observed, err := readProbeSSE(reader, 2<<20)
		if err != nil || !observed.finish() {
			t.Fatalf("received terminal was lost: %v", err)
		}
	})
	t.Run("transport_error_before_terminal", func(t *testing.T) {
		reader := io.MultiReader(strings.NewReader("data: {}\n\n"), iotest.ErrReader(io.ErrUnexpectedEOF))
		_, observed, err := readProbeSSE(reader, 2<<20)
		if !errors.Is(err, io.ErrUnexpectedEOF) || observed.finish() {
			t.Fatalf("interruption was accepted: %v", err)
		}
	})
}
