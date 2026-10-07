package transport

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

func TestProbeQuotaDeadline(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, used, window, reset, retry string
		want                             time.Duration
	}{
		{"quota", "100", "300", "1800", "120", 30 * time.Minute},
		{"retry", "100", "300", "120", "1800", 30 * time.Minute},
		{"available", "30", "10080", "604800", "", pluginv1.DefaultProbe429Fallback},
		{"unavailable", "100", "0", "604800", "", pluginv1.DefaultProbe429Fallback},
		{"invalid_window", "100", "NaN", "604800", "", pluginv1.DefaultProbe429Fallback},
		{"invalid_used", "NaN", "300", "604800", "", pluginv1.DefaultProbe429Fallback},
		{"negative", "100", "300", "-1", "", pluginv1.DefaultProbe429Fallback},
		{"legacy", "100", "", "1800", "", 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := make(http.Header)
			h.Set("x-codex-primary-used-percent", tc.used)
			h.Set("x-codex-primary-window-minutes", tc.window)
			h.Set("x-codex-primary-reset-after-seconds", tc.reset)
			h.Set("Retry-After", tc.retry)
			if got := probeRetryDeadline(429, h, now); !got.Equal(now.Add(tc.want)) {
				t.Fatalf("deadline %v, expected delay %v", got, tc.want)
			}
			if got := probeRetryDeadline(401, h, now); !got.IsZero() {
				t.Fatal("authentication was misclassified as quota")
			}
		})
	}
	for _, raw := range []string{
		`{"error":{"type":"usage_limit_reached","resets_in_seconds":1800}}`,
		`{"error":{"type":"rate_limit_exceeded","resets_in_seconds":"1800"}}`,
		fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, now.Unix()+1800),
	} {
		if got := probeQuotaBodyDeadline([]byte(raw), now); !got.Equal(now.Add(30 * time.Minute)) {
			t.Fatalf("body reset ignored: %v", got)
		}
	}
	for _, raw := range []string{
		`{"error":{"type":"invalid_request_error","resets_in_seconds":1800}}`,
		`{"error":{"type":"usage_limit_reached","resets_in_seconds":-1}}`,
		`{"error":{"type":"usage_limit_reached","resets_at":"NaN"}}`,
		strings.Repeat(" ", maxProbeErrorBody+1),
	} {
		if got := probeQuotaBodyDeadline([]byte(raw), now); !got.IsZero() {
			t.Fatalf("invalid body established a reset: %v", got)
		}
	}
}
