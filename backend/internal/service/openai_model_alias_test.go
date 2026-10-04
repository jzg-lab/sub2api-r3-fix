package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeKnownOpenAICodexModelGPT6Astra(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "openai/gpt-6-astra", "OPENAI/GPT-6_ASTRA", "gpt-6", "openai/gpt-6"} {
		require.Equal(t, "gpt-6-astra", normalizeKnownOpenAICodexModel(model))
	}
}

func TestNormalizeKnownOpenAICodexModelGPT6SolLuna(t *testing.T) {
	tests := map[string]string{
		"gpt-6-sol":                          "gpt-6-sol",
		"openai/GPT_6_SOL":                   "gpt-6-sol",
		"gpt-6-sol-max":                      "gpt-6-sol",
		"gpt-6-luna":                         "gpt-6-luna",
		"provider/gpt-6-luna-openai-compact": "gpt-6-luna",
	}
	for input, expected := range tests {
		require.Equal(t, expected, normalizeKnownOpenAICodexModel(input), input)
	}
}

func TestNormalizeKnownOpenAICodexModelGPT61SolPreservesIdentity(t *testing.T) {
	for _, model := range []string{
		"gpt-6.1-sol",
		"openai/GPT_6.1_SOL",
	} {
		require.Equal(t, "gpt-6.1-sol", normalizeKnownOpenAICodexModel(model), model)
	}

	for _, model := range []string{
		"gpt-6.1-sol-xhigh",
		"gpt-6.1-sol-preview",
	} {
		require.Empty(t, normalizeKnownOpenAICodexModel(model), model)
	}
}

func TestGPT61SolDoesNotInheritGPT6FamilyCapabilities(t *testing.T) {
	require.False(t, isOpenAIGPT6Model("gpt-6.1-sol"))
	require.False(t, isOpenAIGPT6AstraModel("gpt-6.1-sol"))
	require.False(t, openAIModelSupportsPromptCacheOptions("gpt-6.1-sol"))
}

func TestNormalizeKnownOpenAICodexModel_BareGPT56RoutesToSol(t *testing.T) {
	tests := map[string]string{
		"gpt-5.6":            "gpt-5.6-sol",
		"openai/gpt-5.6":     "gpt-5.6-sol",
		"gpt5.6":             "gpt-5.6-sol",
		"gpt-5.6-high":       "gpt-5.6-sol",
		"gpt-5.6-max":        "gpt-5.6-sol",
		"gpt-5.6-2026-07-09": "gpt-5.6-sol",
		"openai/gpt-5.6-max": "gpt-5.6-sol",
	}

	for input, expected := range tests {
		t.Run(input, func(t *testing.T) {
			require.Equal(t, expected, normalizeKnownOpenAICodexModel(input))
		})
	}
}

func TestUsageBillingModelCandidates_BareGPT56IncludesSol(t *testing.T) {
	require.Equal(t,
		[]string{"gpt-5.6", "gpt-5.6-sol"},
		usageBillingModelCandidates("gpt-5.6"),
	)
	require.Equal(t,
		[]string{"openai/gpt-5.6", "gpt-5.6", "gpt-5.6-sol"},
		usageBillingModelCandidates("openai/gpt-5.6"),
	)
}
