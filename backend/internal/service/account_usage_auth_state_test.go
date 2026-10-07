package service

import (
	"context"
	"testing"
	"time"
)

type usageAuthStateRepo struct {
	accountUsageCodexProbeRepo
	clearCalls int
}

func (r *usageAuthStateRepo) ClearError(context.Context, int64) error {
	r.clearCalls++
	return nil
}

func TestAccountUsageService_UsageDoesNotRecoverAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		force    bool
	}{
		{"openai_cached", PlatformOpenAI, false},
		{"openai_forced_without_auth_proof", PlatformOpenAI, true},
		{"gemini_local_quota", PlatformGemini, false},
		{"antigravity_unavailable", PlatformAntigravity, false},
		{"grok_unavailable", PlatformGrok, false},
		{"anthropic_cached", PlatformAnthropic, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			account := &Account{
				ID: 71, Platform: tc.platform, Type: AccountTypeOAuth,
				Status: StatusError, ErrorMessage: "unauthenticated",
				Extra: map[string]any{
					"codex_5h_used_percent": 25.0,
					"codex_5h_reset_at":     now.Add(time.Hour).Format(time.RFC3339),
					"codex_7d_used_percent": 30.0,
					"codex_7d_reset_at":     now.Add(24 * time.Hour).Format(time.RFC3339),
				},
			}
			repo := &usageAuthStateRepo{}
			cache := &UsageCache{}
			cache.apiCache.Store(account.ID, &apiUsageCache{
				response: &ClaudeUsageResponse{}, timestamp: now,
			})
			cache.windowStatsCache.Store(account.ID, &windowStatsCache{
				stats: &WindowStats{}, timestamp: now,
			})
			svc := &AccountUsageService{accountRepo: repo, cache: cache}

			usage, err := svc.getUsageForAccount(context.Background(), account, tc.force)
			if err != nil || usage == nil {
				t.Fatalf("usage display must remain available: err=%v", err)
			}
			if repo.clearCalls != 0 || account.Status != StatusError || account.ErrorMessage != "unauthenticated" {
				t.Fatalf("usage is not authentication proof: clears=%d status=%s", repo.clearCalls, account.Status)
			}
		})
	}
}
