package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

func TestProbeRequestCompatibility(t *testing.T) {
	for _, name := range []string{"none effort", "compact endpoint", "inherited body headers"} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if _, ok := body["instructions"]; !ok {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"message":"Instructions are required","param":"instructions"}}`)
					return
				}
				if r.URL.Path != "/backend-api/codex/responses" {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"message":"Unsupported parameter: stream","param":"stream"}}`)
					return
				}
				for _, header := range []string{"Content-Encoding", "Content-Md5", "Digest", "Idempotency-Key",
					"X-Codex-Turn-State", "X-Codex-Turn-Metadata"} {
					if r.Header.Get(header) != "" {
						w.WriteHeader(http.StatusBadRequest)
						fmt.Fprint(w, `{"error":{"message":"Invalid Content-Encoding","code":"invalid_header"}}`)
						return
					}
				}
				if r.Header.Get("Authorization") != "Bearer fixture" ||
					r.Header.Get("Chatgpt-Account-Id") != "account-fixture" ||
					r.Header.Get("User-Agent") != "fixture-client" ||
					r.Header.Get("Session-Id") != "session-fixture" {
					t.Error("request normalization changed account or client identity")
				}
				if r.Header.Get("Accept") != "text/event-stream" ||
					r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("probe must request uncompressed SSE")
				}
				writeSSEWithUsage(w, "gpt-6-astra", "21", 1200)
			}))
			defer up.Close()
			store := cookiestore.New()
			cfg := probeTestConfig()
			if name == "none effort" {
				cfg.ProbeReasoningEffort = ""
			}
			store.SetConfig(cfg)
			srv := stoppedProbeServer(t, store)
			endpoint := up.URL + "/backend-api/codex/responses"
			headers := map[string]*pluginv1.HeaderValues{
				"Authorization":      {Values: []string{"Bearer fixture"}},
				"Chatgpt-Account-Id": {Values: []string{"account-fixture"}},
				"User-Agent":         {Values: []string{"fixture-client"}},
				"Session-Id":         {Values: []string{"session-fixture"}},
			}
			if name == "compact endpoint" {
				endpoint += "/compact"
			}
			if name == "inherited body headers" {
				for _, header := range []string{"Content-Encoding", "Content-Md5", "Digest", "Idempotency-Key",
					"X-Codex-Turn-State", "X-Codex-Turn-Metadata"} {
					headers[header] = &pluginv1.HeaderValues{Values: []string{"stale-fixture"}}
				}
				headers["Content-Type"] = &pluginv1.HeaderValues{Values: []string{"application/octet-stream"}}
				headers["Accept-Encoding"] = &pluginv1.HeaderValues{Values: []string{"gzip"}}
			}
			srv.stashTemplate(&pluginv1.ForwardRequestStart{
				AccountId: 42, Method: http.MethodPost, Url: endpoint, Headers: headers,
			}, []byte(`{"model":"gpt-6-astra"}`), time.Now())
			tmpl := *srv.templates[42]
			verdict, answer, _ := srv.sendProbe(t.Context(), 42, &tmpl, prober.Question{
				Prompt: "fixture", Grade: func(answer string) bool { return answer == "21" },
			}, cfg)
			if verdict != prober.VerdictPass || requests != 1 {
				t.Fatalf("compatible probe must pass without retries: %s %q requests=%d", verdict, answer, requests)
			}
		})
	}
}

func TestProbeHTTP400RetainsSafeCauseWithoutQualityPenalty(t *testing.T) {
	requests := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"reasoning.effort","message":"Unsupported value with this model; Bearer private-fixture; __cflb=private-cookie"}}`)
	}))
	defer up.Close()
	store := cookiestore.New()
	cfg := probeTestConfig()
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	store.Capture(42, []string{"__cflb=retained; Max-Age=3500"}, time.Now())
	stashFrom(srv, 42, up.URL, time.Now())
	tmpl := *srv.templates[42]
	state := prober.NewState(42)
	state.ConsecPasses = 2
	srv.runProbeCycle(t.Context(), 42, &tmpl, state, cfg)
	if state.LastVerdict != prober.VerdictError ||
		!strings.HasPrefix(state.LastAnswer, "http:400 unsupported:reasoning.effort") {
		t.Fatalf("lost safe failure cause: %s %q", state.LastVerdict, state.LastAnswer)
	}
	if strings.Contains(state.LastAnswer, "private") || len([]rune(state.LastAnswer)) > 80 {
		t.Fatal("diagnostic leaked response contents or exceeded status limit")
	}
	if requests != 1 || state.Probes != 1 || state.Fails != 0 || state.ConsecFails != 0 ||
		state.QualityRerolls != 0 || state.ConsecPasses != 0 ||
		store.MergeHeader(42, "", time.Now()) != "__cflb=retained" {
		t.Fatal("HTTP failure must not reroll, certify recovery or punish answer quality")
	}
}

func TestProbeHTTPDiagnosticsAreBoundedAndSecretSafe(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind, parameter string
		readErr                     error
	}{
		{"missing instructions", `{"detail":"Instructions are required"}`, "missing", "instructions", nil},
		{"unsupported tokens", `{"error":"Unsupported parameter: max_output_tokens"}`, "unsupported", "max_output_tokens", nil},
		{"stream constraint", `{"error":{"message":"stream must be true"}}`, "invalid", "stream", nil},
		{"model unavailable", `{"error":{"code":"model_not_found","message":"private-fixture"}}`, "model-unavailable", "model", nil},
		{"invalid compression", `{"error":{"code":"invalid_header","message":"Invalid Content-Encoding"}}`, "invalid", "content-encoding", nil},
		{"unknown data", `{"error":{"code":"private-fixture","param":"private-fixture","message":"private-fixture"}}`, "rejected", "", nil},
		{"word boundaries", `{"error":{"message":"private-streamkey private-modelname private-instructionskey"}}`, "rejected", "", nil},
		{"html", `<html>private-fixture</html>`, "non-json", "", nil},
		{"malformed", `{"error":"private-fixture"`, "non-json", "", nil},
		{"empty", "", "empty-body", "", nil},
		{"body read failure", `{"error":"private-fixture"}`, "body-unreadable", "", errors.New("private-reader-fixture")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := classifyProbeHTTPError(400, []byte(tc.body), tc.readErr)
			if failure.Kind != tc.kind || failure.Parameter != tc.parameter {
				t.Fatalf("unexpected classification: %+v", failure)
			}
			if strings.Contains(failure.Summary(), "private") || len(failure.Summary()) > 80 ||
				len(failure.Fingerprint) != 12 {
				t.Fatal("diagnostic must not copy upstream fields or exceed status budget")
			}
		})
	}
	raw := []byte(strings.Repeat("x", maxProbeErrorBody) + "private-tail")
	short := classifyProbeHTTPError(400, raw[:maxProbeErrorBody], nil)
	long := classifyProbeHTTPError(400, raw, nil)
	if short != long {
		t.Fatal("response diagnostics must use at most the bounded prefix")
	}
}

func TestProbeHeadersDoNotMutateBusinessTemplate(t *testing.T) {
	original := http.Header{
		"Content-Encoding":   []string{"gzip"},
		"Content-Length":     []string{"999"},
		"Idempotency-Key":    []string{"business"},
		"X-Codex-Turn-State": []string{"business-state"},
		"Authorization":      []string{"Bearer fixture"},
	}
	next := probeHeaders(original)
	if next.Get("Content-Length") != "" || next.Get("Content-Encoding") != "" {
		t.Fatal("stale body framing survived")
	}
	if original.Get("Content-Encoding") != "gzip" || original.Get("Idempotency-Key") != "business" ||
		original.Get("X-Codex-Turn-State") != "business-state" {
		t.Fatal("probe headers aliased the business template")
	}
}

func TestCompactForwardUnchangedAndProbeIdentityStable(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses/compact" ||
			r.Header.Get("X-Codex-Turn-State") != "business-state" {
			t.Error("probe normalization must not alter the business request")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"model":"gpt-6-astra"}` {
			t.Error("business body changed")
		}
		fmt.Fprint(w, `{}`)
	}))
	defer up.Close()
	store := cookiestore.New()
	store.SetConfig(probeTestConfig())
	srv := stoppedProbeServer(t, store)
	endpoint := up.URL + "/backend-api/codex/responses"
	first := forwardFixture(t.Context(), endpoint, "identity")
	srv.stashTemplate(first.requests[0].GetStart(), []byte(`{"model":"gpt-6-astra"}`), time.Now())
	generation := srv.templates[42].Generation
	compact := forwardFixture(t.Context(), endpoint+"/compact", "identity")
	compact.requests[0].GetStart().Headers["X-Codex-Turn-State"] = &pluginv1.HeaderValues{Values: []string{"business-state"}}
	if err := srv.Forward(compact); err != nil {
		t.Fatal(err)
	}
	if srv.templates[42].URL != endpoint || srv.templates[42].Generation != generation {
		t.Fatal("compaction must not redirect probes or replace account identity")
	}
}
