package dto

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountFromServiceShallow_QuotaRateLimitDoesNotReplace429(t *testing.T) {
	quotaReset := time.Now().UTC().Add(6 * 24 * time.Hour).Truncate(time.Second)
	upstreamReset := time.Now().Add(time.Minute)
	account := &service.Account{
		Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		RateLimitResetAt: &upstreamReset,
		Extra: map[string]any{
			"codex_7d_used_percent": 100.0,
			"codex_7d_reset_at":     quotaReset.Format(time.RFC3339),
		},
	}
	got := AccountFromServiceShallow(account)
	require.Equal(t, &quotaReset, got.QuotaRateLimitResetAt)
	require.Equal(t, &upstreamReset, got.RateLimitResetAt)
	require.Equal(t, &upstreamReset, account.RateLimitResetAt)
	account.Extra["auto_pause_5h_disabled"] = true
	account.Extra["auto_pause_7d_disabled"] = true
	require.Nil(t, AccountFromServiceShallow(account).QuotaRateLimitResetAt)
}
