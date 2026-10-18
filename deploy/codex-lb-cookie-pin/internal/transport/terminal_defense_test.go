package transport

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/evidence"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
)

func TestTerminalDefenseAcrossForwardAndAutonomousProbe(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		known      bool
	}{
		{"authentication", `{"type":"response.failed","response":{"error":{"code":"token_expired"}}}`, 401, true},
		{"forbidden", `{"type":"response.failed","response":{"error":{"type":"permission_error"}}}`, 403, true},
		{"quota", `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`, 429, true},
		{"overload", `{"type":"response.failed","response":{"error":{"code":"server_overloaded"}}}`, 503, true},
		{"bare-auth", `{"type":"error","code":"invalid_api_key"}`, 401, true},
		{"unknown-error", `{"type":"response.failed","response":{"error":{"message":"response protection is unavailable"}}}`, 0, true},
		{"incomplete", `{"type":"response.incomplete","response":{"status":"incomplete"}}`, 0, true},
		{"completed-error", `{"type":"response.completed","response":{"status":"failed","error":{"code":"token_expired"}}}`, 401, true},
		{"partial", `{"type":"response.output_text.delta","delta":"rate_limit_exceeded"}`, 0, false},
	} {
		for _, consumer := range []string{"business", "host", "autonomous"} {
			t.Run(tc.name+"/"+consumer, func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", tc.body)
				}))
				defer up.Close()
				cfg := probeTestConfig()
				cfg.InjectScope = "all"
				store := cookiestore.New()
				store.SetConfig(cfg)
				srv := stoppedProbeServer(t, store)
				journal, dir := persistentFixture(t)
				srv.evidence = journal
				id := int64(42)
				if consumer == "autonomous" {
					stashFrom(srv, id, up.URL+"/responses", time.Now())
					tmpl := *srv.templates[id]
					verdict, answer, _ := srv.sendProbe(t.Context(), id, &tmpl, prober.QuestionFor(0), cfg)
					if verdict != prober.VerdictError || !strings.HasPrefix(answer, map[bool]string{true: "failed:", false: "unknown:"}[tc.known]) {
						t.Fatalf("unexpected verdict %v / %s", verdict, answer)
					}
				} else {
					ctx := t.Context()
					if consumer == "host" {
						ctx = hostProbeContext(ctx)
					}
					if err := srv.Forward(forwardFixture(ctx, up.URL+"/responses", "fixture")); err != nil {
						t.Fatal(err)
					}
				}
				srv.probeMu.Lock()
				tmpl, state := srv.templates[id], srv.states[id]
				if tc.status == 401 || tc.status == 403 {
					if tmpl != nil {
						t.Error("rejected authentication retained a probe template")
					}
				} else if consumer != "autonomous" && tmpl != nil && tmpl.Confirmed {
					t.Error("HTTP200 without successful Responses terminal admitted a template")
				}
				if (tc.status == 429 || tc.status == 503) && (state == nil || !state.RetryNotBefore.After(time.Now())) {
					t.Error("semantic rejection did not protect autonomous probes")
				}
				srv.probeMu.Unlock()
				reopened, err := evidence.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				st, err := reopened.Read(id)
				if err != nil {
					t.Fatal(err)
				}
				if consumer == "business" {
					if st.BusinessSucceeded != 0 || st.BusinessDispatched != 1 {
						t.Fatalf("failed business accounted as successful: %+v", st)
					}
				} else {
					want := "unknown"
					if tc.known {
						want = "failed"
					}
					if st.Operation.Phase != want || st.ProbePassed != 0 {
						t.Fatalf("persistent phase=%s, want=%s", st.Operation.Phase, want)
					}
				}
			})
		}
	}
}

func TestResponseObserverTypedFailureDoesNotUseProseOrSuccessfulOutput(t *testing.T) {
	for _, tc := range []struct {
		body string
		sse  bool
		want int
	}{
		{`{"error":{"code":"rate_limit_exceeded"}}`, false, 429},
		{`{"error":{"code":"rate_limit_exceeded","type":"server_overloaded"}}`, false, 429},
		{`{"error":{"code":"server_overloaded","type":"rate_limit_exceeded"}}`, false, 429},
		{`{"error":{"code":"rate_limit_exceeded","type":"authentication_error"}}`, false, 401},
		{`{"object":"response","status":"failed","error":{"type":"authentication_error"}}`, false, 401},
		{`{"error":{"message":"invalid_api_key rate_limit_exceeded"}}`, false, 0},
		{"event: response.failed\ndata: {\"response\":{\"error\":{\"code\":\"token_revoked\"}}}\n\n", true, 401},
		{"data: {\"type\":\"error\",\"code\":\"invalid_api_key\"}\n\n" + completeFixture, true, 0},
		{"data: {\"type\":\"response.output_text.delta\",\"delta\":\"invalid_api_key\"}\n\n" + completeFixture, true, 0},
		{"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"token_revoked\"}}}\n\n" + completeFixture, true, 401},
		{"data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\"}\n\ndata: {\"type\":\"error\",\"code\":\"server_overloaded\"}\n\n", true, 429},
	} {
		o := responseObserver{sse: tc.sse}
		o.write([]byte(tc.body))
		o.finish()
		if got := o.failureStatus(); got != tc.want {
			t.Fatalf("status=%d want=%d for %s", got, tc.want, tc.body)
		}
	}
}

const compactCompleteFixture = `{"id":"resp_fixture","object":"response.compaction","output":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fixture"}]},{"type":"compaction","encrypted_content":"fixture"}]}`

func TestForwardCompactCompletionIsEndpointBound(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		success    bool
	}{
		{"valid", compactCompleteFixture, true},
		{"empty-object", `{}`, false},
		{"empty-output", `{"object":"response.compaction","output":[]}`, false},
		{"no-compaction", `{"object":"response.compaction","output":[{"type":"message"}]}`, false},
		{"empty-compaction", `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":""}]}`, false},
		{"retained-after-compaction", `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"a"},{"type":"message","role":"user"}]}`, true},
		{"multiple-compactions", `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"a"},{"type":"compaction","encrypted_content":"b"}]}`, true},
		{"invalid-mixed-compactions", `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":""},{"type":"compaction","encrypted_content":"b"}]}`, false},
		{"truncated", strings.TrimSuffix(compactCompleteFixture, "}"), false},
		{"error", strings.TrimSuffix(compactCompleteFixture, "}") + `,"error":{"code":"token_expired"}}`, false},
		{"incomplete", strings.TrimSuffix(compactCompleteFixture, "}") + `,"status":"incomplete"}`, false},
	} {
		for _, endpoint := range []string{"/responses", "/responses/compact"} {
			t.Run(tc.name+endpoint, func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, tc.body)
				}))
				defer up.Close()
				cfg := probeTestConfig()
				cfg.InjectScope = "all"
				store := cookiestore.New()
				store.SetConfig(cfg)
				srv := stoppedProbeServer(t, store)
				journal, _ := persistentFixture(t)
				srv.evidence = journal
				if err := srv.Forward(forwardFixture(t.Context(), up.URL+endpoint, "fixture")); err != nil {
					t.Fatal(err)
				}
				st, err := journal.Read(42)
				if err != nil {
					t.Fatal(err)
				}
				want := tc.success && endpoint == "/responses/compact"
				if (st.BusinessSucceeded == 1) != want {
					t.Fatalf("success=%d want=%t", st.BusinessSucceeded, want)
				}
				tmpl := srv.templates[42]
				if tmpl != nil && tmpl.Confirmed != want {
					t.Fatalf("template admission=%t want=%t", tmpl.Confirmed, want)
				}
			})
		}
	}
}

func TestForwardResponsesAdmissionRequiresTerminal(t *testing.T) {
	for _, body := range []string{"", "data: [DONE]\n\n", completeFixture, strings.TrimSuffix(completeFixture, "\n")} {
		t.Run(fmt.Sprintf("size=%d", len(body)), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, body)
			}))
			defer up.Close()
			cfg := probeTestConfig()
			cfg.InjectScope = "all"
			store := cookiestore.New()
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			if err := srv.Forward(forwardFixture(t.Context(), up.URL+"/responses", "fixture")); err != nil {
				t.Fatal(err)
			}
			if tmpl := srv.templates[42]; tmpl == nil || tmpl.Confirmed != (body == completeFixture) {
				t.Fatalf("incorrect template admission: %+v", tmpl)
			}
		})
	}
}
