package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/evidence"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
	"google.golang.org/grpc/metadata"
)

const completeFixture = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

func TestEvidenceCohortPreservesEffortShapeWithoutProxySecrets(t *testing.T) {
	for _, tc := range []struct{ body, effort, form string }{
		{`{"model":"fixture"}`, "", "omitted"},
		{`{"model":"fixture","reasoning":{}}`, "", "omitted"},
		{`{"model":"fixture","reasoning":{"effort":"medium"}}`, "medium", "explicit-medium"},
		{`{"model":"fixture","reasoning":{"effort":"xhigh"}}`, "xhigh", "explicit-xhigh"},
		{`{"model":"fixture","reasoning":{"effort":null}}`, "", "explicit-null"},
		{`{"model":"fixture","reasoning":{"effort":""}}`, "", "explicit-empty"},
		{`{"model":"fixture","reasoning":{"effort":123}}`, "unrecognized", "explicit-unrecognized"},
	} {
		c := bodyCohort([]byte(tc.body), "socks5://fixture-user:fixture-secret@127.0.0.1:17921")
		if c.Model != "fixture" || c.Effort != tc.effort || c.EffortForm != tc.form ||
			strings.Contains(c.Bucket, "fixture") || len(c.Bucket) != 24 {
			t.Fatalf("incorrect non-secret cohort: %+v", c)
		}
		other := bodyCohort([]byte(tc.body), "socks5://replacement:changed@127.0.0.1:17921")
		if c.Bucket != other.Bucket {
			t.Fatal("credential rotation changed egress identity")
		}
	}
}

func TestResponseObserverRequiresCompleteTerminalAcrossChunks(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		sse, ok   bool
	}{
		{"complete", completeFixture, true, true},
		{"crlf", strings.ReplaceAll(completeFixture, "\n", "\r\n"), true, true},
		{"multiline", "data: {\"type\":\"response.completed\",\ndata: \"response\":{\"status\":\"completed\"}}\n\n", true, true},
		{"event-header", "event: response.completed\ndata: {\"response\":{\"status\":\"completed\"}}\n\n", true, true},
		{"done-header", "event: response.done\ndata: {\"response\":{\"status\":\"completed\"}}\n\n", true, true},
		{"payload-type-wins", "event: response.failed\n" + completeFixture, true, true},
		{"pending-error-completed", "event: error\ndata: {\"message\":\"temporary\"}\n\n" + completeFixture, true, true},
		{"failed-completed", "event: response.failed\ndata: {}\n\n" + completeFixture, true, false},
		{"incomplete-completed", "event: response.incomplete\ndata: {}\n\n" + completeFixture, true, false},
		{"completed-error", completeFixture + "event: error\ndata: {\"message\":\"failure\"}\n\n", true, false},
		{"completed-cancelled", completeFixture + "event: response.cancelled\ndata: {}\n\n", true, false},
		{"completed-canceled", completeFixture + "data: {\"type\":\"response.canceled\"}\n\n", true, false},
		{"header-reset", "event: response.completed\n\ndata: {\"response\":{\"status\":\"completed\"}}\n\n", true, false},
		{"error-invalid-completion", "data: {\"type\":\"error\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\"}}\n\n", true, false},
		{"delta", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture\"}\n\n", true, false},
		{"partial-terminal", strings.TrimSuffix(completeFixture, "\n"), true, false},
		{"failed-after-complete", completeFixture + "data: {\"type\":\"response.failed\"}\n\n", true, false},
		{"invalid-after-complete", completeFixture + "data: not-json\n\n", true, false},
		{"error", "data: {\"type\":\"error\",\"message\":\"failure\"}\n\n", true, false},
		{"incomplete", "data: {\"type\":\"response.incomplete\"}\n\n", true, false},
		{"done-only", "data: [DONE]\n\n", true, false},
		{"json", `{"object":"response","status":"completed"}`, false, true},
		{"json-error", `{"object":"response","status":"completed","error":{"code":"failure"}}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{1, 7, len(tc.raw)} {
				o := responseObserver{sse: tc.sse}
				for raw := []byte(tc.raw); len(raw) > 0; {
					n := min(size, len(raw))
					o.write(raw[:n])
					raw = raw[n:]
				}
				if o.finish() != tc.ok {
					t.Fatalf("chunk=%d observer=%+v", size, o)
				}
			}
		})
	}
	o := responseObserver{sse: true}
	o.write([]byte("data: " + strings.Repeat("x", 2<<20)))
	o.write([]byte(completeFixture))
	if o.finish() || len(o.line) != 0 || len(o.data) != 0 {
		t.Fatal("oversized event was not bounded")
	}
}

func TestResponseObserverIncompleteReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    responseObserver
		want string
	}{
		{"invalid", responseObserver{invalid: true, completed: true}, "invalid_event"},
		{"failed", responseObserver{failed: true, completed: true}, "failed_event"},
		{"bare error", responseObserver{bareError: true}, "failed_event"},
		{"partial line", responseObserver{sse: true, line: []byte("data:")}, "partial_event"},
		{"partial event", responseObserver{sse: true, data: []byte("{}")}, "partial_event"},
		{"delivered terminal without settlement", responseObserver{sse: true, completed: true}, "delivery_interrupted"},
		{"unrecognized json", responseObserver{}, "unrecognized_json"},
		{"missing terminal", responseObserver{sse: true}, "missing_terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.o.incompleteReason(); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResponseObserverInvalidCauseIsBoundedAndSticky(t *testing.T) {
	const limit = 2 << 20
	for _, tc := range []struct {
		name, raw, want string
		sse             bool
	}{
		{"json body limit", strings.Repeat(" ", limit+1), "json_body_limit", false},
		{"sse line limit", "data: " + strings.Repeat(" ", limit), "sse_line_limit", true},
		{"sse event limit", "data: " + strings.Repeat(" ", limit/2) + "\n" +
			"data: " + strings.Repeat(" ", limit/2) + "\n", "sse_event_limit", true},
		{"json syntax", `{"private_fixture":`, "json_syntax", false},
		{"sse syntax", "data: private_fixture\n\n", "json_syntax", true},
		{"json schema", `{"type":{"private_fixture":true}}`, "json_schema", false},
		{"sse schema", "data: {\"response\":\"private_fixture\"}\n\n", "json_schema", true},
		{"mislabelled sse", completeFixture, "json_syntax", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{7, len(tc.raw)} {
				o := responseObserver{sse: tc.sse}
				for raw := []byte(tc.raw); len(raw) > 0; {
					n := min(size, len(raw))
					o.write(raw[:n])
					raw = raw[n:]
				}
				if o.finish() || !o.invalid || o.knownFailure() || o.settledSSE() {
					t.Fatal("invalid response was certified or classified as an upstream rejection")
				}
				if got := o.incompleteReason(); got != tc.want {
					t.Fatalf("chunk=%d reason=%q want=%q", size, got, tc.want)
				}
				if len(o.data) != 0 || len(o.line) != 0 {
					t.Fatal("invalid payload retained")
				}
				o.write([]byte(completeFixture))
				if o.finish() || o.incompleteReason() != tc.want {
					t.Fatal("later input erased the original failure")
				}
			}
		})
	}
	t.Run("same chunk later terminal", func(t *testing.T) {
		calls := 0
		o := responseObserver{sse: true, onEvent: func(string, []byte) { calls++ }}
		o.write([]byte("data: not-json\n\n" + completeFixture))
		if o.finish() || o.completed || calls != 0 || o.incompleteReason() != "json_syntax" {
			t.Fatal("a terminal after invalid input reached evidence consumers")
		}
	})
}

func TestResponseObserverInfersFramingWithoutInspectingJSONText(t *testing.T) {
	for _, tc := range []struct {
		name, raw        string
		compact, sse, ok bool
	}{
		{"data", completeFixture, false, true, true},
		{"event", "event: response.completed\n" + completeFixture, false, true, true},
		{"blank lines", "\r\n\n" + completeFixture, false, true, true},
		{"comment", ": keepalive\n\n" + completeFixture, false, true, true},
		{"id", "id: fixture\n" + completeFixture, false, true, true},
		{"retry", "retry: 1000\n" + completeFixture, false, true, true},
		{"json text", `{"object":"response","status":"completed","output":[{"type":"message","text":"data: fixture\nevent: response.completed"}]}`, false, false, true},
		{"json whitespace", "\r\n " + `{"object":"response","status":"completed"}`, false, false, true},
		{"json padded", strings.Repeat("\n", 65) + `{"object":"response","status":"completed"}`, false, false, true},
		{"json then sse", "{}\n" + completeFixture, false, false, false},
		{"unexpected preamble", "not-sse\n" + completeFixture, false, false, false},
		{"partial prefix", "data", false, false, false},
		{"partial terminal", strings.TrimSuffix(completeFixture, "\n"), false, true, false},
		{"done only", "data: [DONE]\n\n", false, true, false},
		{"malformed", "data: not-json\n\n" + completeFixture, false, true, false},
		{"failed", "event: response.failed\ndata: {}\n\n" + completeFixture, false, true, false},
		{"compact json", `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"fixture"}]}`, true, false, true},
		{"compact sse", completeFixture, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{1, 3, 7, len(tc.raw)} {
				o := responseObserver{sniffSSE: !tc.compact, compact: tc.compact}
				for raw := []byte(tc.raw); len(raw) > 0; {
					n := min(size, len(raw))
					o.write(raw[:n])
					raw = raw[n:]
				}
				if got := o.finish(); got != tc.ok || o.sse != tc.sse || o.inferredSSE != tc.sse {
					t.Fatalf("chunk=%d completed=%v sse=%v inferred=%v reason=%s",
						size, got, o.sse, o.inferredSSE, o.incompleteReason())
				}
			}
		})
	}
	t.Run("stream bound remains per event", func(t *testing.T) {
		o := responseObserver{sniffSSE: true}
		o.write([]byte(strings.Repeat("data: {}\n\n", (2<<20)/10+1) + completeFixture))
		if !o.finish() {
			t.Fatalf("valid stream treated as one JSON body: %s", o.incompleteReason())
		}
	})
	t.Run("JSON limit unchanged", func(t *testing.T) {
		o := responseObserver{sniffSSE: true}
		o.write([]byte(strings.Repeat("\n", 2<<20) + "{}"))
		if o.finish() || o.incompleteReason() != "json_body_limit" {
			t.Fatal("framing detection bypassed JSON body limit")
		}
	})
}

func TestProbeEvidenceConsumersShareSSEFraming(t *testing.T) {
	raw := "event: error\ndata: {\"message\":\"temporary\"}\n\n" +
		"event: response.done\ndata: {\"response\":{\"status\":\"completed\",\n" +
		"data: \"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"42\"}]}],\n" +
		"data: \"usage\":{\"output_tokens_details\":{\"reasoning_tokens\":901}}}}\n\n"
	for _, fixture := range []string{raw, strings.ReplaceAll(raw, "\n", "\r\n")} {
		if !probeCompleted([]byte(fixture)) || extractAnswerFromSSE([]byte(fixture)) != "42" {
			t.Fatal("terminal and answer consumers disagree on framed events")
		}
		if got, ok := extractUsageFromSSE([]byte(fixture)); !ok || got != 901 {
			t.Fatalf("terminal and usage consumers disagree: %d %v", got, ok)
		}
	}
	o := responseObserver{sse: true}
	o.write([]byte("event: error\ndata: {\"code\":\"admission_denied\"}\n\n"))
	if o.category != "admission_rejected" || o.finish() {
		t.Fatal("event-only rejection lost its category")
	}
}

func TestForwardEvidenceConsumesAuthoritativeTerminal(t *testing.T) {
	for _, probe := range []bool{false, true} {
		t.Run(map[bool]string{false: "business", true: "host-probe"}[probe], func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: error\ndata: {\"message\":\"temporary\"}\n\n"+
					"event: response.completed\ndata: {\"response\":{\"status\":\"completed\"}}\n\n")
			}))
			defer up.Close()
			srv := stoppedProbeServer(t, cookiestore.New())
			ctx := t.Context()
			if probe {
				ctx = hostProbeContext(ctx)
			}
			if err := srv.Forward(forwardFixture(ctx, up.URL+"/responses", "fixture")); err != nil {
				t.Fatal(err)
			}
			st, err := srv.evidence.Read(42)
			if err != nil {
				t.Fatal(err)
			}
			if probe {
				if st.Operation.Phase != "ok" || st.ProbePassed != 0 || st.BusinessDispatched != 0 || st.BusinessSucceeded != 0 {
					t.Fatalf("host terminal observation misclassified: %+v", st)
				}
			} else if st.BusinessDispatched != 1 || st.BusinessSucceeded != 1 || st.ProbePassed != 0 {
				t.Fatalf("business terminal observation misclassified: %+v", st)
			}
		})
	}
}

func TestForwardRecognizesSSEWithoutContentType(t *testing.T) {
	for _, contentType := range []string{"", "text/plain", "application/json"} {
		for _, probe := range []bool{false, true} {
			t.Run(contentType+"/"+map[bool]string{false: "business", true: "host-probe"}[probe], func(t *testing.T) {
				var calls atomic.Int64
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header()["Content-Type"] = nil
					if contentType != "" {
						w.Header().Set("Content-Type", contentType)
					}
					_, _ = io.WriteString(w, completeFixture)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				defer up.Close()
				srv := stoppedProbeServer(t, cookiestore.New())
				ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
				defer cancel()
				streamCtx := ctx
				if probe {
					streamCtx = hostProbeContext(ctx)
				}
				stream := forwardFixture(streamCtx, up.URL+"/responses", "fixture")
				if err := srv.Forward(stream); err != nil {
					t.Fatal(err)
				}
				state, err := srv.evidence.Read(42)
				if err != nil {
					t.Fatal(err)
				}
				var body strings.Builder
				var ends, transportErrors int
				for _, frame := range stream.responses {
					body.Write(frame.GetBodyChunk())
					if frame.GetEnd() != nil {
						ends++
					}
					if frame.GetError() != nil {
						transportErrors++
					}
				}
				if calls.Load() != 1 || body.String() != completeFixture {
					t.Fatal("request replayed or forwarded body changed")
				}
				if ctx.Err() != nil || ends != 1 || transportErrors != 0 {
					t.Fatalf("framed terminal lost: context=%v ends=%d transport_errors=%d",
						ctx.Err(), ends, transportErrors)
				}
				if probe {
					if state.Operation.Phase != "ok" || state.BusinessDispatched != 0 {
						t.Fatalf("host probe terminal misclassified: phase=%s", state.Operation.Phase)
					}
				} else if state.BusinessDispatched != 1 || state.BusinessSucceeded != 1 {
					t.Fatalf("business terminal misclassified: dispatched=%d succeeded=%d",
						state.BusinessDispatched, state.BusinessSucceeded)
				}
			})
		}
	}
}

func TestForwardTerminalDoesNotWaitForTransportEOF(t *testing.T) {
	for _, tc := range []struct {
		name, body, phase string
	}{
		{"completed", completeFixture, "ok"},
		{"header_terminal", "event: response.completed\ndata: {\"response\":{\"status\":\"completed\"}}\n\n", "ok"},
		{"pending_error_completed", "event: error\ndata: {\"code\":\"service_unavailable\"}\n\n" + completeFixture, "ok"},
		{"failed", "event: response.failed\ndata: {\"response\":{\"status\":\"failed\"}}\n\n", "failed"},
		{"failed_then_completed", "event: response.failed\ndata: {\"response\":{\"status\":\"failed\"}}\n\n" + completeFixture, "failed"},
		{"partial_terminal", strings.TrimSuffix(completeFixture, "\n"), "unknown"},
		{"done_only", "data: [DONE]\n\n", "unknown"},
		{"bare_error", "event: error\ndata: {\"code\":\"service_unavailable\"}\n\n", "failed"},
		{"malformed", "data: not-json\n\n" + completeFixture, "unknown"},
	} {
		for _, probe := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "business", true: "host-probe"}[probe], func(t *testing.T) {
				var calls atomic.Int64
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, tc.body)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				defer up.Close()
				srv := stoppedProbeServer(t, cookiestore.New())
				ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
				defer cancel()
				streamCtx := ctx
				if probe {
					streamCtx = hostProbeContext(ctx)
				}
				stream := forwardFixture(streamCtx, up.URL+"/responses", "fixture")
				if err := srv.Forward(stream); err != nil {
					t.Fatal(err)
				}
				state, err := srv.evidence.Read(42)
				if err != nil {
					t.Fatal(err)
				}
				terminal := tc.phase != "unknown" && tc.name != "bare_error"
				var ends, transportErrors int
				var body strings.Builder
				for _, frame := range stream.responses {
					if frame.GetEnd() != nil {
						ends++
					}
					if frame.GetError() != nil {
						transportErrors++
					}
					body.Write(frame.GetBodyChunk())
				}
				if calls.Load() != 1 || body.String() != tc.body {
					t.Fatalf("request replay or changed response: calls=%d body_equal=%v", calls.Load(), body.String() == tc.body)
				}
				if terminal && (ctx.Err() != nil || ends != 1 || transportErrors != 0) {
					t.Fatalf("terminal waited for EOF: context=%v ends=%d transport_errors=%d", ctx.Err(), ends, transportErrors)
				}
				if !terminal && (ctx.Err() == nil || ends != 0 || transportErrors != 1) {
					t.Fatalf("unfinished stream settled early: context=%v ends=%d transport_errors=%d", ctx.Err(), ends, transportErrors)
				}
				if probe {
					if state.Operation.Phase != tc.phase {
						t.Fatalf("operation=%s want=%s", state.Operation.Phase, tc.phase)
					}
				} else {
					wantSuccess := uint64(0)
					if tc.phase == "ok" {
						wantSuccess = 1
					}
					if state.BusinessDispatched != 1 || state.BusinessSucceeded != wantSuccess {
						t.Fatalf("business evidence: dispatched=%d succeeded=%d", state.BusinessDispatched, state.BusinessSucceeded)
					}
				}
			})
		}
	}
}

func TestAdmissionOnlyRecognizesUpstreamErrorFields(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"error":{"message":"Missing TURN-State Ticket"}}`, "turn_state_ticket"},
		{`{"error":{"message":"requires a Freshly Validated Ticket"}}`, "turn_state_ticket"},
		{`{"error":{"code":"upstream_STATE_REJECTION"}}`, "state_rejection"},
		{`{"type":"error","code":"admission_denied"}`, "admission_rejected"},
		{`{"type":"response.failed","response":{"error":{"message":"session admission refused"}}}`, "admission_rejected"},
		{`{"error":{"code":"invalid_api_key","message":"unauthorized"}}`, ""},
		{`{"error":{"code":"rate_limit_exceeded"}}`, ""},
		{`{"error":{"code":"forbidden"}}`, ""},
		{`{"error":{"message":"state_rejection is just a word"}}`, ""},
		{`{"type":"response.output_text.delta","delta":"turn-state ticket"}`, ""},
		{`{"object":"response","output":[{"text":"freshly validated ticket"}]}`, ""},
	} {
		if got := admissionCategory([]byte(tc.raw)); got != tc.want {
			t.Fatalf("category=%q want=%q", got, tc.want)
		}
	}
}

func hostProbeContext(ctx context.Context) context.Context {
	out := pluginv1.WithProbeTraffic(ctx)
	md, _ := metadata.FromOutgoingContext(out)
	return metadata.NewIncomingContext(ctx, md)
}

func persistentFixture(t *testing.T) (*evidence.Store, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func TestProbePassAndBusinessFailureRemainSeparate(t *testing.T) {
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.InjectScope = "all"
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	journal, dir := persistentFixture(t)
	srv.evidence = journal
	up := newFakeUpstream(t, "pass")
	stashFrom(srv, 9, up.server.URL, time.Now())
	tmpl := *srv.templates[9]
	verdict, _, _ := srv.sendProbe(t.Context(), 9, &tmpl, prober.QuestionFor(0), cfg)
	if verdict != prober.VerdictPass {
		t.Fatal("probe fixture did not pass")
	}
	up.forceState = http.StatusInternalServerError
	stream := forwardFixture(t.Context(), up.server.URL+"/responses", "fixture-no-leak")
	stream.requests[0].GetStart().AccountId = 9
	if err := srv.Forward(stream); err != nil {
		t.Fatal(err)
	}
	st, err := journal.Read(9)
	if err != nil || st.ProbePassed != 1 || st.BusinessDispatched != 1 || st.BusinessSucceeded != 0 {
		t.Fatalf("counter conflation: %+v %v", st, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "9.jsonl"))
	if err != nil || bytes.Contains(raw, []byte("fixture-no-leak")) || bytes.Contains(raw, []byte("Authorization")) {
		t.Fatal("journal leaked request authentication", err)
	}
	var status struct {
		Evidence struct {
			Accounts []evidence.State `json:"accounts"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(srv.mergeStatusJSON(time.Now(), store.Status(time.Now()), cfg), &status); err != nil ||
		len(status.Evidence.Accounts) != 1 || status.Evidence.Accounts[0].BusinessSucceeded != 0 {
		t.Fatal("UI status conflated probe success", err)
	}
}

func TestHostProbeUnknownBlocksAllProbeEntrypointsAcrossReload(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	defer up.Close()
	journal, dir := persistentFixture(t)
	cfg := probeTestConfig()
	cfg.InjectScope = "all"
	store := cookiestore.New()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	srv.evidence = journal
	if err := srv.Forward(forwardFixture(hostProbeContext(t.Context()), up.URL+"/responses", "fixture")); err != nil {
		t.Fatal(err)
	}
	st, err := journal.Read(42)
	if err != nil || st.Operation.Phase != "unknown" || st.BusinessDispatched != 0 || st.ProbePassed != 0 {
		t.Fatalf("host probe was misclassified: %+v %v", st, err)
	}
	reopened, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv.evidence = reopened
	stream := forwardFixture(hostProbeContext(t.Context()), up.URL+"/responses", "replacement-fixture")
	if err := srv.Forward(stream); err != nil || stream.responses[0].GetError().GetCode() != "PLUGIN_PROBE_BLOCKED" {
		t.Fatal("new host operation bypassed unknown", err)
	}
	stashFrom(srv, 42, up.URL+"/responses", time.Now())
	tmpl := *srv.templates[42]
	_, result, _ := srv.sendProbe(t.Context(), 42, &tmpl, prober.QuestionFor(0), cfg)
	if result != "blocked:unknown" || calls.Load() != 1 {
		t.Fatalf("automatic replay: calls=%d result=%s", calls.Load(), result)
	}
	// Pure business observation must not turn the probe fence into a route ban.
	if err := srv.Forward(forwardFixture(t.Context(), up.URL+"/responses", "fixture")); err != nil || calls.Load() != 2 {
		t.Fatal("probe protection changed business admission", err)
	}
	st, _ = journal.Read(42)
	if st.BusinessSucceeded != 0 || st.LastEvent.Result != "unknown" {
		t.Fatal("incomplete business stream became a completed success")
	}
}

func TestHostProbeLong429BodyDeadlineWithoutTemplate(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","resets_in_seconds":7200}}`)
	}))
	defer up.Close()
	srv := stoppedProbeServer(t, cookiestore.New())
	start := time.Now()
	if err := srv.Forward(forwardFixture(hostProbeContext(t.Context()), up.URL+"/responses", "fixture")); err != nil {
		t.Fatal(err)
	}
	st, err := srv.evidence.Read(42)
	if err != nil || st.Operation.Phase != "failed" || st.NotBefore.Before(start.Add(2*time.Hour)) ||
		st.BusinessDispatched != 0 {
		t.Fatalf("longer upstream deadline lost: %+v %v", st, err)
	}
}

func TestAdmissionSignalIsDurableDeduplicatedAndObservationOnly(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	var calls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"Missing freshly validated ticket; fixture-secret"}}`)
	}))
	defer up.Close()
	journal, dir := persistentFixture(t)
	srv := stoppedProbeServer(t, cookiestore.New())
	srv.evidence = journal
	for i := 0; i < 2; i++ {
		stream := forwardFixture(t.Context(), up.URL+"/responses", "fixture-secret")
		if err := srv.Forward(stream); err != nil || stream.responses[0].GetStart().GetStatusCode() != 400 {
			t.Fatal("observation changed response", err)
		}
		reopened, err := evidence.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		srv.evidence = reopened
	}
	if calls != 2 || strings.Count(logs.String(), `"msg":"plugin_upstream_admission_rejected"`) != 1 ||
		strings.Contains(logs.String(), "fixture-secret") {
		t.Fatal("alert was duplicated or leaked upstream text")
	}
	st, err := srv.evidence.Read(42)
	if err != nil || st.Step != 0 || st.BusinessDispatched != 2 || st.BusinessSucceeded != 0 ||
		len(st.Alerts) != 1 {
		t.Fatalf("observation changed retry policy: %+v %v", st, err)
	}
}

func TestProbePersistenceFailureNeverSends(t *testing.T) {
	up := newFakeUpstream(t, "pass")
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	journal, dir := persistentFixture(t)
	srv.evidence = journal
	if err := os.Mkdir(filepath.Join(dir, "9.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	stashFrom(srv, 9, up.server.URL, time.Now())
	tmpl := *srv.templates[9]
	_, result, _ := srv.sendProbe(t.Context(), 9, &tmpl, prober.QuestionFor(0), cfg)
	if result != "blocked:persistence" || atomic.LoadInt64(&up.requests) != 0 {
		t.Fatal("persistence failure permitted outbound traffic")
	}
	if _, err := journal.Begin(9, evidence.Cohort{}, time.Now()); err == nil || errors.Is(err, evidence.ErrBackoff) {
		t.Fatal("storage corruption was disguised as ordinary cooldown")
	}
}

func TestProbeMidBodyDisconnectNeverReplaysAfterIdentityRotation(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 4096\r\n\r\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		_ = buffer.Flush()
	}))
	defer up.Close()
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	journal, dir := persistentFixture(t)
	srv.evidence = journal
	stashFrom(srv, 9, up.URL, time.Now())
	tmpl := *srv.templates[9]
	_, result, _ := srv.sendProbe(t.Context(), 9, &tmpl, prober.QuestionFor(0), cfg)
	if result != "unknown:read" || calls.Load() != 1 {
		t.Fatalf("interrupted response: %s calls=%d", result, calls.Load())
	}
	reopened, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv.evidence = reopened
	stashFrom(srv, 9, up.URL, time.Now())
	store.Capture(9, []string{"__cflb=replacement; Max-Age=300"}, time.Now())
	srv.probeMu.Lock()
	srv.resetProbeStateLocked(9, time.Now())
	srv.probeMu.Unlock()
	tmpl = *srv.templates[9]
	_, result, _ = srv.sendProbe(t.Context(), 9, &tmpl, prober.QuestionFor(0), cfg)
	if result != "blocked:unknown" || calls.Load() != 1 {
		t.Fatal("new cookie, state or plugin generation bypassed unknown", result)
	}
}

func TestProbeConnectionRefusalRetainsBackoffWithoutUnknown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	stashFrom(srv, 9, "http://"+address+"/responses", time.Now())
	tmpl := *srv.templates[9]
	_, result, _ := srv.sendProbe(t.Context(), 9, &tmpl, prober.QuestionFor(0), cfg)
	st, err := srv.evidence.Read(9)
	if err != nil || result != "failed:connect" || st.Operation.Phase != "failed" ||
		st.Step != 1 || !st.NotBefore.After(time.Now()) {
		t.Fatalf("known unsent failure was misclassified: %s %+v %v", result, st, err)
	}
	_, result, _ = srv.sendProbe(t.Context(), 9, &tmpl, prober.QuestionFor(0), cfg)
	if result != "blocked:backoff" {
		t.Fatal("connection failure bypassed backoff", result)
	}
}
