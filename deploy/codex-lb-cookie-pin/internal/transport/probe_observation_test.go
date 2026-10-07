package transport

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
)

func TestProbeObservationCorrelatesRerollWithoutSensitiveContent(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	tmpl := &probeTemplate{
		Generation: 7, CreatedAt: now, CookieVersion: 11,
		URL:            "https://fixture.invalid/private-path",
		Headers:        http.Header{"X-Private-Fixture": {"fixture-header-private"}},
		ProxyURL:       "http://fixture.invalid/private-proxy",
		OriginalCookie: "fixture-cookie-private",
	}
	state := prober.NewState(42)
	cfg := probeTestConfig()
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	for i, verdict := range []prober.Verdict{prober.VerdictFail, prober.VerdictError, prober.VerdictPass} {
		observedAt := now.Add(time.Duration(i) * time.Minute)
		decision := state.Record(verdict, "fixture-question-private", "fixture-answer-private", observedAt, cfg)
		state.LastReasoningTokens = 101 + i
		newProbeObservation(tmpl, state, decision, observedAt).log(logger)
		if decision.ShouldReroll {
			tmpl.CookieVersion++
		}
	}
	decoder := json.NewDecoder(&out)
	for i, action := range []string{"reroll", "scheduled", "scheduled"} {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry["action"] != action || entry["account_id"] != float64(42) ||
			entry["template_generation"] != float64(7) || entry["probe"] != float64(i+1) ||
			entry["quality_rerolls"] != float64(1) || entry["reasoning_tokens"] != float64(101+i) {
			t.Fatalf("incorrect observation metadata: %+v", entry)
		}
		expectedCookieGeneration := float64(12)
		if i == 0 {
			expectedCookieGeneration = 11
		}
		if entry["cookie_generation"] != expectedCookieGeneration {
			t.Fatalf("lost reroll binding: %+v", entry)
		}
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "private") || strings.Contains(string(data), "fixture.invalid") {
			t.Fatal("observation exposed private request or probe content")
		}
		allowed := []string{
			"time", "level", "msg", "account_id", "template_generation", "template_created_at",
			"cookie_generation", "probe", "quality_rerolls", "verdict", "action",
			"reasoning_tokens", "observed_at", "next_probe_at", "retry_not_before",
		}
		if len(entry) != len(allowed) {
			t.Fatalf("unexpected observation fields: got %d, want %d", len(entry), len(allowed))
		}
		for key := range entry {
			if !slices.Contains(allowed, key) {
				t.Fatalf("unapproved observation field: %q", key)
			}
		}
	}
}

func TestProbeObservationReportsEffectiveDeadlineAndIsImmutable(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		decision prober.Decision
		action   string
	}{
		{"upstream_rate_limit", prober.Decision{}, "retry_later"},
		{"quality_backoff", prober.Decision{EnterBackoff: true}, "backoff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := &probeTemplate{Generation: 3, CreatedAt: now, CookieVersion: 5}
			state := prober.NewState(42)
			state.NextProbeAt = now.Add(time.Minute)
			state.BackoffUntil = now.Add(3 * time.Minute)
			state.RetryNotBefore = now.Add(5 * time.Minute)
			observation := newProbeObservation(tmpl, state, tc.decision, now)
			state.RetryNotBefore = time.Time{}
			tmpl.Generation++
			if observation.action != tc.action ||
				!observation.nextProbeAt.Equal(now.Add(5*time.Minute)) ||
				!observation.retryNotBefore.Equal(now.Add(5*time.Minute)) ||
				observation.templateGeneration != 3 {
				t.Fatalf("observation used mutable or premature state: %+v", observation)
			}
		})
	}
}
