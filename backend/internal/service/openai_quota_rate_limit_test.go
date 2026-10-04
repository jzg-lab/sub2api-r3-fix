package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAICodexQuotaRateLimitResetAt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(6 * 24 * time.Hour)
	for _, tc := range []struct {
		name       string
		used       float64
		reset      any
		disabled5h any
		disabled7d any
		limited    bool
	}{
		{name: "exhausted", used: 100, reset: reset.Format(time.RFC3339), limited: true},
		{name: "rounded below boundary", used: 99.6, reset: reset.Format(time.RFC3339)},
		{name: "missing reset", used: 100},
		{name: "invalid reset", used: 100, reset: "invalid"},
		{name: "reset reached", used: 100, reset: now.Format(time.RFC3339)},
		{name: "overdraft", used: 100, reset: reset.Format(time.RFC3339), disabled5h: true, disabled7d: true},
		{name: "legacy overdraft", used: 100, reset: reset.Format(time.RFC3339), disabled5h: "true", disabled7d: 1},
		{name: "single disabled window", used: 100, reset: reset.Format(time.RFC3339), disabled7d: true, limited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{
				Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Extra: map[string]any{
					"codex_7d_used_percent":  tc.used,
					"codex_7d_reset_at":      tc.reset,
					"auto_pause_5h_disabled": tc.disabled5h,
					"auto_pause_7d_disabled": tc.disabled7d,
				},
			}
			got := OpenAICodexQuotaRateLimitResetAt(account, now)
			if tc.limited {
				require.NotNil(t, got)
				require.True(t, got.Equal(reset))
			} else {
				require.Nil(t, got)
			}
			require.Nil(t, account.RateLimitResetAt)
		})
	}
	require.Nil(t, OpenAICodexQuotaRateLimitResetAt(nil, now))
	require.Nil(t, OpenAICodexQuotaRateLimitResetAt(&Account{
		Platform: PlatformAnthropic, Extra: map[string]any{
			"codex_7d_used_percent": 100.0, "codex_7d_reset_at": reset.Format(time.RFC3339),
		},
	}, now))
}
