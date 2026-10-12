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

func TestDefaultGPT61SolUsesVerifiedReleaseTimestamp(t *testing.T) {
	for _, model := range DefaultModels {
		if model.ID == "gpt-6.1-sol" {
			require.Equal(t, int64(1790640000), model.Created)
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

func TestIsGPT61SolModelSpellingRejectsUnknownAliases(t *testing.T) {
	for _, model := range []string{
		"gpt-6.1-sol",
		"openai/GPT_6.1_SOL",
	} {
		require.True(t, IsGPT61SolModelSpelling(model), model)
	}

	for _, model := range []string{
		"gpt-6.1",
		"gpt-6-sol",
		"gpt-6.1-sol-preview",
	} {
		require.False(t, IsGPT61SolModelSpelling(model), model)
	}
}

func TestGPT61SolIdentityAndEffort(t *testing.T) {
	require.Contains(t, DefaultModelIDs(), "gpt-6.1-sol")
	for _, id := range []string{"gpt-6.1-sol", "openai/gpt-6.1-sol-max", "GPT_6.1_SOL", "gpt-6.1-sol-openai-compact"} {
		require.True(t, IsGPT61SolModelSpelling(id), id)
		require.False(t, IsGPT6SolOrLunaModelSpelling(id), id)
	}
	for _, id := range []string{"gpt-6.1", "gpt-6.1-solitude", "gpt-6.1-sol-preview", "gpt-6-sol"} {
		require.False(t, IsGPT61SolModelSpelling(id), id)
	}
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		require.NoError(t, ValidateGPT61SolReasoningEffort("gpt-6.1-sol", effort))
	}
	for _, effort := range []string{"none", "minimal"} {
		require.Error(t, ValidateGPT61SolReasoningEffort("gpt-6.1-sol", effort))
		require.NoError(t, ValidateGPT61SolReasoningEffort("gpt-6-sol", effort))
	}
	require.Contains(t, CodexBaseInstructionsForModel("gpt-6.1-sol"), "based on GPT-6")
}
