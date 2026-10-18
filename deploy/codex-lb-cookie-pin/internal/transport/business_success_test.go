package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/cookiestore"
)

func TestForwardBusinessSuccessEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, mime, body, kind string
		probe                           bool
	}{
		{"stream", "/responses", "text/event-stream", completeFixture, "responses_stream", false},
		{"inferred", "/responses", "text/plain", completeFixture, "responses_stream", false},
		{"json", "/responses", "application/json", `{"object":"response","status":"completed"}`, "responses_json", false},
		{"compact", "/responses/compact", "application/json", compactCompleteFixture, "responses_compact", false},
		{"partial", "/responses", "text/event-stream", "data: [DONE]\n\n", "", false},
		{"failed", "/responses", "text/event-stream", "data: {\"type\":\"response.failed\"}\n\n", "", false},
		{"wrong-endpoint", "/responses", "application/json", compactCompleteFixture, "", false},
		{"host-probe", "/responses", "text/event-stream", completeFixture, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.mime)
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
			ctx := t.Context()
			if tc.probe {
				ctx = hostProbeContext(ctx)
			}
			if err := srv.Forward(forwardFixture(ctx, up.URL+tc.endpoint, "fixture")); err != nil {
				t.Fatal(err)
			}
			state, err := journal.Read(42)
			if err != nil {
				t.Fatal(err)
			}
			matches := 0
			for _, line := range strings.Split(logs.String(), "\n") {
				var event map[string]any
				if json.Unmarshal([]byte(line), &event) != nil || event["msg"] != "plugin_business_succeeded" {
					continue
				}
				matches++
				if event["response_kind"] != tc.kind || event["account_id"] != float64(42) ||
					len(event) != 5 || state.BusinessSucceeded != 1 {
					t.Fatal("success diagnostic was not bound to persisted business success")
				}
			}
			if (matches == 1) != (tc.kind != "") || matches > 1 {
				t.Fatalf("unexpected success diagnostic count: %d", matches)
			}
		})
	}
}

func TestForwardBusinessSuccessRequiresPersistence(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	journal, dir := persistentFixture(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		file := filepath.Join(dir, "42.jsonl")
		if err := os.Rename(file, file+".retained"); err != nil {
			t.Error(err)
			return
		}
		if err := os.Mkdir(file, 0700); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, completeFixture)
	}))
	defer up.Close()
	store := cookiestore.New()
	cfg := probeTestConfig()
	cfg.InjectScope = "all"
	store.SetConfig(cfg)
	srv := stoppedProbeServer(t, store)
	srv.evidence = journal
	if err := srv.Forward(forwardFixture(t.Context(), up.URL+"/responses", "fixture")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "plugin_evidence_unavailable") ||
		strings.Contains(logs.String(), "plugin_business_succeeded") {
		t.Fatal("failed durable settlement emitted success evidence")
	}
}
