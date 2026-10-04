package openai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultModelsIncludeBareGPT56Alias(t *testing.T) {
	require.Contains(t, DefaultModelIDs(), "gpt-5.6")
}

func TestDefaultModelsIncludeGPT6Astra(t *testing.T) {
	require.Contains(t, DefaultModelIDs(), "gpt-6-astra")
	require.Contains(t, DefaultModelIDs(), "gpt-6")
}

func TestDefaultModelsPreferConcreteGPT56SolForAccountTests(t *testing.T) {
	require.NotEmpty(t, DefaultModels)
	require.Equal(t, "gpt-5.6-sol", DefaultModels[0].ID)
}

func TestDefaultModelsIncludeGPT6SolLunaAndGPTImage25(t *testing.T) {
	modelIDs := DefaultModelIDs()
	require.Contains(t, modelIDs, "gpt-6.1-sol")
	require.Contains(t, modelIDs, "gpt-6-sol")
	require.Contains(t, modelIDs, "gpt-6-luna")
	require.Contains(t, modelIDs, "gpt-image-2.5-flare")
	require.Contains(t, modelIDs, "gpt-image-2.5-sunburst")
}

func TestDefaultGPT61SolDoesNotGuessReleaseTimestamp(t *testing.T) {
	for _, model := range DefaultModels {
		if model.ID == "gpt-6.1-sol" {
			require.Zero(t, model.Created)
			require.Equal(t, "GPT-6.1 Sol", model.DisplayName)
			return
		}
	}
	t.Fatal("gpt-6.1-sol missing from default models")
}

func TestCanonicalizeOpenAIModelAliasSpelling(t *testing.T) {
	tests := map[string]string{
		" openai/GPT_6_SOL ":   "gpt-6-sol",
		"azure/gpt5.4mini":     "gpt-5.4-mini",
		"gpt-5.3codexspark":    "gpt-5.3-codex-spark",
		"gpt--5.3 codex_spark": "gpt-5.3-codex-spark",
		"not-a-model":          "",
		"":                     "",
	}

	for input, expected := range tests {
		require.Equal(t, expected, CanonicalizeOpenAIModelAliasSpelling(input), input)
	}
}

func TestIsGPT6SolOrLunaModelSpelling(t *testing.T) {
	for _, model := range []string{
		"gpt-6-sol",
		"gpt-6-luna",
		"openai/GPT_6_SOL",
		"gpt-6-sol-none",
		"gpt-6-luna-openai-compact",
	} {
		require.True(t, IsGPT6SolOrLunaModelSpelling(model), model)
	}

	for _, model := range []string{
		"gpt-6-astra",
		"gpt-6-solitude",
		"gpt-6-luna-preview",
		"not-a-model",
	} {
		require.False(t, IsGPT6SolOrLunaModelSpelling(model), model)
	}
}

func TestIsGPT61SolModelSpellingIsExact(t *testing.T) {
	for _, model := range []string{
		"gpt-6.1-sol",
		"openai/GPT_6.1_SOL",
	} {
		require.True(t, IsGPT61SolModelSpelling(model), model)
	}

	for _, model := range []string{
		"gpt-6.1-sol-xhigh",
		"gpt-6.1",
		"gpt-6-sol",
		"gpt-6.1-sol-preview",
	} {
		require.False(t, IsGPT61SolModelSpelling(model), model)
	}
}
