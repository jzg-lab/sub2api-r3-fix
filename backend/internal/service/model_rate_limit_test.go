package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestPreserveModelRateLimitsForAccountEdit(t *testing.T) {
	currentLimits := map[string]any{"current-model": map[string]any{"reason": "current"}}
	staleLimits := map[string]any{"stale-model": map[string]any{"reason": "stale"}}
	for _, tc := range []struct {
		name     string
		current  map[string]any
		incoming map[string]any
		want     map[string]any
	}{
		{
			name:     "cleared limit cannot be resurrected",
			incoming: map[string]any{modelRateLimitsKey: staleLimits, "custom": "edited"},
			want:     map[string]any{"custom": "edited"},
		},
		{
			name:     "current limit wins over stale form",
			current:  map[string]any{modelRateLimitsKey: currentLimits},
			incoming: map[string]any{modelRateLimitsKey: staleLimits, "custom": "edited"},
			want:     map[string]any{modelRateLimitsKey: currentLimits, "custom": "edited"},
		},
		{
			name:     "omitted limit cannot clear runtime limit",
			current:  map[string]any{modelRateLimitsKey: currentLimits},
			incoming: map[string]any{"custom": "edited"},
			want:     map[string]any{modelRateLimitsKey: currentLimits, "custom": "edited"},
		},
		{
			name:     "explicit null cannot clear runtime limit",
			current:  map[string]any{modelRateLimitsKey: currentLimits},
			incoming: map[string]any{modelRateLimitsKey: nil},
			want:     map[string]any{modelRateLimitsKey: currentLimits},
		},
		{name: "nil maps", want: map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeCurrent, err := cloneAccountJSONMap(tc.current)
			require.NoError(t, err)
			beforeIncoming, err := cloneAccountJSONMap(tc.incoming)
			require.NoError(t, err)

			got := PreserveModelRateLimitsForAccountEdit(PlatformOpenAI, tc.current, tc.incoming)

			require.Equal(t, tc.want, got)
			got["result-only"] = true
			require.Equal(t, beforeCurrent, tc.current)
			require.Equal(t, beforeIncoming, tc.incoming)
		})
	}
}

func TestPreserveModelRateLimitsForAccountEditOveragesTransitions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		platform     string
		previous     bool
		next         bool
		wantLimits   bool
		wantCredits  bool
		wantOverages bool
	}{
		{"enable clears limits", PlatformAntigravity, false, true, false, false, false},
		{"disable clears only credit exhaustion", PlatformAntigravity, true, false, true, false, false},
		{"unchanged preserves limits", PlatformAntigravity, true, true, true, true, true},
		{"other platform cannot clear limits", PlatformOpenAI, false, true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := map[string]any{
				creditsExhaustedKey: map[string]any{"reason": "credits"},
				"ordinary-model":    map[string]any{"reason": "rate-limit"},
			}
			current := map[string]any{"allow_overages": tc.previous, modelRateLimitsKey: limits}
			incoming := map[string]any{"allow_overages": tc.next, "antigravity_credits_overages": true}

			got := PreserveModelRateLimitsForAccountEdit(tc.platform, current, incoming)

			_, hasLimits := got[modelRateLimitsKey]
			require.Equal(t, tc.wantLimits, hasLimits)
			if tc.wantLimits {
				retained := got[modelRateLimitsKey].(map[string]any)
				require.Contains(t, retained, "ordinary-model")
				_, hasCredits := retained[creditsExhaustedKey]
				require.Equal(t, tc.wantCredits, hasCredits)
			}
			_, hasOverages := got["antigravity_credits_overages"]
			require.Equal(t, tc.wantOverages, hasOverages)
			require.Equal(t, tc.next, got["allow_overages"])
			require.Len(t, limits, 2, "filtering must not mutate the original nested map")
			require.Equal(t, tc.previous, current["allow_overages"])
			require.Equal(t, true, incoming["antigravity_credits_overages"])
		})
	}
}

func TestIsModelRateLimited(t *testing.T) {
	now := time.Now()
	future := now.Add(10 * time.Minute).Format(time.RFC3339)
	past := now.Add(-10 * time.Minute).Format(time.RFC3339)

	tests := []struct {
		name           string
		account        *Account
		requestedModel string
		expected       bool
	}{
		{
			name: "official model ID hit - claude-sonnet-4-5",
			account: &Account{
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude-sonnet-4-5": map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "claude-sonnet-4-5",
			expected:       true,
		},
		{
			name: "official model ID hit via mapping - request claude-3-5-sonnet, mapped to claude-sonnet-4-5",
			account: &Account{
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"claude-3-5-sonnet": "claude-sonnet-4-5",
					},
				},
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude-sonnet-4-5": map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "claude-3-5-sonnet",
			expected:       true,
		},
		{
			name: "no rate limit - expired",
			account: &Account{
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude-sonnet-4-5": map[string]any{
							"rate_limit_reset_at": past,
						},
					},
				},
			},
			requestedModel: "claude-sonnet-4-5",
			expected:       false,
		},
		{
			name: "no rate limit - no matching key",
			account: &Account{
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"gemini-3-flash": map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "claude-sonnet-4-5",
			expected:       false,
		},
		{
			name:           "no rate limit - unsupported model",
			account:        &Account{},
			requestedModel: "gpt-4",
			expected:       false,
		},
		{
			name:           "no rate limit - empty model",
			account:        &Account{},
			requestedModel: "",
			expected:       false,
		},
		{
			name: "gemini model hit",
			account: &Account{
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"gemini-3-pro-high": map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "gemini-3-pro-high",
			expected:       true,
		},
		{
			name: "antigravity platform - gemini-3-pro-preview mapped to gemini-3-pro-high",
			account: &Account{
				Platform: PlatformAntigravity,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"gemini-3-pro-high": map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "gemini-3-pro-preview",
			expected:       true,
		},
		{
			name: "antigravity platform - gemini family rate limit blocks mapped preview",
			account: &Account{
				Platform: PlatformAntigravity,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						antigravityGeminiModelRateLimitKey: map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "gemini-3-pro-preview",
			expected:       true,
		},
		{
			name: "antigravity platform - gemini family rate limit does not block claude",
			account: &Account{
				Platform: PlatformAntigravity,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						antigravityGeminiModelRateLimitKey: map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "claude-sonnet-4-5",
			expected:       false,
		},
		{
			name: "non-antigravity platform - gemini-3-pro-preview NOT mapped",
			account: &Account{
				Platform: PlatformGemini,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"gemini-3-pro-high": map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "gemini-3-pro-preview",
			expected:       false, // gemini 平台不走 antigravity 映射
		},
		{
			name: "antigravity platform - claude-opus-4-5-thinking mapped to opus-4-6-thinking",
			account: &Account{
				Platform: PlatformAntigravity,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude-opus-4-6-thinking": map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "claude-opus-4-5-thinking",
			expected:       true,
		},
		{
			name: "no scope fallback - claude_sonnet should not match",
			account: &Account{
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude_sonnet": map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "claude-3-5-sonnet-20241022",
			expected:       false,
		},
		{
			name: "openai image generation family key blocks image model",
			account: &Account{
				Platform: PlatformOpenAI,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						openAIImageGenerationRateLimitKey: map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "gpt-image-2",
			expected:       true,
		},
		{
			name: "openai image generation family key does not block text model",
			account: &Account{
				Platform: PlatformOpenAI,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						openAIImageGenerationRateLimitKey: map[string]any{
							"rate_limit_reset_at": future,
						},
					},
				},
			},
			requestedModel: "gpt-5.4",
			expected:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.account.isModelRateLimitedWithContext(context.Background(), tt.requestedModel)
			if result != tt.expected {
				t.Errorf("isModelRateLimited(%q) = %v, want %v", tt.requestedModel, result, tt.expected)
			}
		})
	}
}

func TestIsModelRateLimited_OpenAIImageGenerationIntentBlocksTextModelImageTool(t *testing.T) {
	future := time.Now().Add(10 * time.Minute).Format(time.RFC3339)
	account := &Account{
		Platform: PlatformOpenAI,
		Extra: map[string]any{
			modelRateLimitsKey: map[string]any{
				openAIImageGenerationRateLimitKey: map[string]any{
					"rate_limit_reset_at": future,
				},
			},
		},
	}

	require.False(t, account.isModelRateLimitedWithContext(context.Background(), "gpt-5.4"))
	require.True(t, account.isModelRateLimitedWithContext(WithOpenAIImageGenerationIntent(context.Background()), "gpt-5.4"))
}

func TestIsModelRateLimited_Antigravity_ThinkingAffectsModelKey(t *testing.T) {
	now := time.Now()
	future := now.Add(10 * time.Minute).Format(time.RFC3339)

	account := &Account{
		Platform: PlatformAntigravity,
		Extra: map[string]any{
			modelRateLimitsKey: map[string]any{
				"claude-sonnet-4-5-thinking": map[string]any{
					"rate_limit_reset_at": future,
				},
			},
		},
	}

	ctx := context.WithValue(context.Background(), ctxkey.ThinkingEnabled, true)
	if !account.isModelRateLimitedWithContext(ctx, "claude-sonnet-4-5") {
		t.Errorf("expected model to be rate limited")
	}
}

func TestGetModelRateLimitRemainingTime(t *testing.T) {
	now := time.Now()
	future10m := now.Add(10 * time.Minute).Format(time.RFC3339)
	future5m := now.Add(5 * time.Minute).Format(time.RFC3339)
	past := now.Add(-10 * time.Minute).Format(time.RFC3339)

	tests := []struct {
		name           string
		account        *Account
		requestedModel string
		minExpected    time.Duration
		maxExpected    time.Duration
	}{
		{
			name:           "nil account",
			account:        nil,
			requestedModel: "claude-sonnet-4-5",
			minExpected:    0,
			maxExpected:    0,
		},
		{
			name: "model rate limited - direct hit",
			account: &Account{
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude-sonnet-4-5": map[string]any{
							"rate_limit_reset_at": future10m,
						},
					},
				},
			},
			requestedModel: "claude-sonnet-4-5",
			minExpected:    9 * time.Minute,
			maxExpected:    11 * time.Minute,
		},
		{
			name: "model rate limited - via mapping",
			account: &Account{
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"claude-3-5-sonnet": "claude-sonnet-4-5",
					},
				},
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude-sonnet-4-5": map[string]any{
							"rate_limit_reset_at": future5m,
						},
					},
				},
			},
			requestedModel: "claude-3-5-sonnet",
			minExpected:    4 * time.Minute,
			maxExpected:    6 * time.Minute,
		},
		{
			name: "expired rate limit",
			account: &Account{
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude-sonnet-4-5": map[string]any{
							"rate_limit_reset_at": past,
						},
					},
				},
			},
			requestedModel: "claude-sonnet-4-5",
			minExpected:    0,
			maxExpected:    0,
		},
		{
			name:           "no rate limit data",
			account:        &Account{},
			requestedModel: "claude-sonnet-4-5",
			minExpected:    0,
			maxExpected:    0,
		},
		{
			name: "no scope fallback",
			account: &Account{
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude_sonnet": map[string]any{
							"rate_limit_reset_at": future5m,
						},
					},
				},
			},
			requestedModel: "claude-3-5-sonnet-20241022",
			minExpected:    0,
			maxExpected:    0,
		},
		{
			name: "antigravity platform - claude-opus-4-5-thinking mapped to opus-4-6-thinking",
			account: &Account{
				Platform: PlatformAntigravity,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude-opus-4-6-thinking": map[string]any{
							"rate_limit_reset_at": future5m,
						},
					},
				},
			},
			requestedModel: "claude-opus-4-5-thinking",
			minExpected:    4 * time.Minute,
			maxExpected:    6 * time.Minute,
		},
		{
			name: "antigravity platform - gemini family rate limit remaining",
			account: &Account{
				Platform: PlatformAntigravity,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						antigravityGeminiModelRateLimitKey: map[string]any{
							"rate_limit_reset_at": future10m,
						},
					},
				},
			},
			requestedModel: "gemini-3-pro-preview",
			minExpected:    9 * time.Minute,
			maxExpected:    11 * time.Minute,
		},
		{
			name: "antigravity platform - gemini family remaining ignored for claude",
			account: &Account{
				Platform: PlatformAntigravity,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						antigravityGeminiModelRateLimitKey: map[string]any{
							"rate_limit_reset_at": future10m,
						},
					},
				},
			},
			requestedModel: "claude-sonnet-4-5",
			minExpected:    0,
			maxExpected:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.account.GetModelRateLimitRemainingTimeWithContext(context.Background(), tt.requestedModel)
			if result < tt.minExpected || result > tt.maxExpected {
				t.Errorf("GetModelRateLimitRemainingTime() = %v, want between %v and %v", result, tt.minExpected, tt.maxExpected)
			}
		})
	}
}

func TestGetRateLimitRemainingTime(t *testing.T) {
	now := time.Now()
	future15m := now.Add(15 * time.Minute).Format(time.RFC3339)
	future5m := now.Add(5 * time.Minute).Format(time.RFC3339)

	tests := []struct {
		name           string
		account        *Account
		requestedModel string
		minExpected    time.Duration
		maxExpected    time.Duration
	}{
		{
			name:           "nil account",
			account:        nil,
			requestedModel: "claude-sonnet-4-5",
			minExpected:    0,
			maxExpected:    0,
		},
		{
			name: "model rate limited - 15 minutes",
			account: &Account{
				Platform: PlatformAntigravity,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude-sonnet-4-5": map[string]any{
							"rate_limit_reset_at": future15m,
						},
					},
				},
			},
			requestedModel: "claude-sonnet-4-5",
			minExpected:    14 * time.Minute,
			maxExpected:    16 * time.Minute,
		},
		{
			name: "only model rate limited",
			account: &Account{
				Platform: PlatformAntigravity,
				Extra: map[string]any{
					modelRateLimitsKey: map[string]any{
						"claude-sonnet-4-5": map[string]any{
							"rate_limit_reset_at": future5m,
						},
					},
				},
			},
			requestedModel: "claude-sonnet-4-5",
			minExpected:    4 * time.Minute,
			maxExpected:    6 * time.Minute,
		},
		{
			name: "neither rate limited",
			account: &Account{
				Platform: PlatformAntigravity,
			},
			requestedModel: "claude-sonnet-4-5",
			minExpected:    0,
			maxExpected:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.account.GetRateLimitRemainingTimeWithContext(context.Background(), tt.requestedModel)
			if result < tt.minExpected || result > tt.maxExpected {
				t.Errorf("GetRateLimitRemainingTime() = %v, want between %v and %v", result, tt.minExpected, tt.maxExpected)
			}
		})
	}
}

func TestIsModelRateLimited_AnthropicFableFamilyKey(t *testing.T) {
	now := time.Now()
	future := now.Add(48 * time.Hour).Format(time.RFC3339)

	account := &Account{
		Platform: PlatformAnthropic,
		Extra: map[string]any{
			modelRateLimitsKey: map[string]any{
				anthropicFableRateLimitKey: map[string]any{
					"rate_limit_reset_at": future,
				},
			},
		},
	}

	tests := []struct {
		requestedModel string
		expected       bool
	}{
		{"claude-fable-5", true},
		{"claude-fable-5[1m]", true},      // 家族 key 覆盖变体
		{"Claude-Fable-5-20260601", true}, // 大小写不敏感
		{"claude-sonnet-4-6", false},      // 其他模型不受影响
		{"claude-opus-4-8", false},
	}

	for _, tc := range tests {
		t.Run(tc.requestedModel, func(t *testing.T) {
			got := account.isModelRateLimitedWithContext(context.Background(), tc.requestedModel)
			require.Equal(t, tc.expected, got)
			remaining := account.GetModelRateLimitRemainingTimeWithContext(context.Background(), tc.requestedModel)
			require.Equal(t, tc.expected, remaining > 0)
		})
	}
}

func TestIsAnthropicFableModel(t *testing.T) {
	require.True(t, isAnthropicFableModel("claude-fable-5"))
	require.True(t, isAnthropicFableModel("claude-fable-5[1m]"))
	require.True(t, isAnthropicFableModel("Claude-Fable-5"))
	require.False(t, isAnthropicFableModel("claude-sonnet-4-6"))
	require.False(t, isAnthropicFableModel(""))
}
