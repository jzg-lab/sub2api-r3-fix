package service

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

// r17c 回归:codex-auth-manager 等导入工具的映射模板滞后于新模型发布时,
// 创建/更新必须以恒等映射补齐 gpt-6 家族,且永不覆盖已有映射项。
func TestEnsureOpenAICodexModelMappingFloor(t *testing.T) {
	tests := []struct {
		name        string
		platform    string
		accountType string
		credentials map[string]any
		want        map[string]any
	}{
		{
			name:        "openai_oauth_missing_keys_get_floor",
			platform:    PlatformOpenAI,
			accountType: AccountTypeOAuth,
			credentials: map[string]any{"model_mapping": map[string]any{"gpt-5.6": "gpt-5.6"}},
			want: map[string]any{"model_mapping": map[string]any{
				"gpt-5.6": "gpt-5.6", "gpt-6": "gpt-6", "gpt-6-astra": "gpt-6-astra",
			}},
		},
		{
			name:        "existing_entries_never_overwritten",
			platform:    PlatformOpenAI,
			accountType: AccountTypeOAuth,
			credentials: map[string]any{"model_mapping": map[string]any{"gpt-6": "custom-upstream"}},
			want: map[string]any{"model_mapping": map[string]any{
				"gpt-6": "custom-upstream", "gpt-6-astra": "gpt-6-astra",
			}},
		},
		{
			name:        "non_openai_platform_untouched",
			platform:    "anthropic",
			accountType: AccountTypeOAuth,
			credentials: map[string]any{"model_mapping": map[string]any{"claude-x": "claude-x"}},
			want:        map[string]any{"model_mapping": map[string]any{"claude-x": "claude-x"}},
		},
		{
			name:        "non_oauth_type_untouched",
			platform:    PlatformOpenAI,
			accountType: "api_key",
			credentials: map[string]any{"model_mapping": map[string]any{"gpt-5.6": "gpt-5.6"}},
			want:        map[string]any{"model_mapping": map[string]any{"gpt-5.6": "gpt-5.6"}},
		},
		{
			name:        "empty_mapping_untouched_defaults_apply",
			platform:    PlatformOpenAI,
			accountType: AccountTypeOAuth,
			credentials: map[string]any{"model_mapping": map[string]any{}},
			want:        map[string]any{"model_mapping": map[string]any{}},
		},
		{
			name:        "no_mapping_untouched",
			platform:    PlatformOpenAI,
			accountType: AccountTypeOAuth,
			credentials: map[string]any{"access_token": "t"},
			want:        map[string]any{"access_token": "t"},
		},
		{
			name:        "nil_credentials_noop",
			platform:    PlatformOpenAI,
			accountType: AccountTypeOAuth,
			credentials: nil,
			want:        nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cloned map[string]any
			if tc.credentials != nil {
				cloned = maps.Clone(tc.credentials)
			}
			ensureOpenAICodexModelMappingFloor(tc.platform, tc.accountType, cloned)
			require.Equal(t, tc.want, cloned)
		})
	}
}
