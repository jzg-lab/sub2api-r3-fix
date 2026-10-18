package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMergeGroupConfiguredCodexModelsUsesSchedulableMappingUnion(t *testing.T) {
	t.Parallel()

	account := func(platform string, mapping map[string]any) Account {
		return Account{Platform: platform, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": mapping}}
	}
	const groupID int64 = 71
	const source = `{"models":[{"slug":"gpt-6-astra","unknown":{"large":9007199254740993}},{"slug":"gpt-5.6-sol"},{"slug":"unsupported-model"},{"unknown":true}],"revision":9007199254740993}`
	svc := &OpenAIGatewayService{accountRepo: codexModelsVisibilityAccountRepo{
		byGroup: map[int64][]Account{groupID: {
			account(PlatformOpenAI, map[string]any{"gpt-6-*": "gpt-6-*"}),
			account(PlatformOpenAI, map[string]any{"gpt-5.6-*": "gpt-5.6-*"}),
			account(PlatformAnthropic, map[string]any{"*": "*"}),
		}},
	}}
	group := &Group{ID: groupID, Platform: PlatformOpenAI}
	_, configured, err := svc.BuildGroupConfiguredCodexModelsManifest(t.Context(), group, "")
	require.NoError(t, err)
	require.False(t, configured, "wildcard-only mappings must reach the remote catalog path")

	manifest := &OpenAIModelsResponse{Body: []byte(source), ETag: `"upstream"`}
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(t.Context(), group, manifest, ""))
	models := decodeCodexManifestModels(t, manifest.Body)
	require.Len(t, models, 2)
	require.Equal(t, "gpt-6-astra", models[0]["slug"])
	require.Equal(t, "gpt-5.6-sol", models[1]["slug"])
	require.Contains(t, string(manifest.Body), `"large":9007199254740993`)
	require.Contains(t, string(manifest.Body), `"revision":9007199254740993`)
	require.Equal(t, codexModelsManifestBodyETag(manifest.Body), manifest.ETag)
	require.NotEqual(t, `"upstream"`, manifest.ETag)

	conditional := &OpenAIModelsResponse{Body: []byte(source)}
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(t.Context(), group, conditional, manifest.ETag))
	require.True(t, conditional.NotModified)
	require.Empty(t, conditional.Body)

	group.ModelAllowlist = GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6-astra", "unsupported-model"}}
	filtered := &OpenAIModelsResponse{Body: []byte(source)}
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(t.Context(), group, filtered, ""))
	models = decodeCodexManifestModels(t, filtered.Body)
	require.Len(t, models, 1, "a display whitelist cannot grant model support")
	require.Equal(t, "gpt-6-astra", models[0]["slug"])
}

func TestMergeGroupConfiguredCodexModelsPreservesUnrestrictedRemoteBytes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		credentials map[string]any
		extra       map[string]any
	}{
		{name: "unmapped"},
		{name: "wildcard", credentials: map[string]any{"model_mapping": map[string]any{"*": "*"}}},
		{name: "passthrough", credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-*": "gpt-*"},
		}, extra: map[string]any{"openai_passthrough": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{accountRepo: codexModelsVisibilityAccountRepo{
				byGroup: map[int64][]Account{71: {{
					Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: tc.credentials, Extra: tc.extra,
				}}},
			}}
			const source = "{ \"models\" : [{\"slug\":\"custom-model\",\"unknown\":true}] }"
			manifest := &OpenAIModelsResponse{Body: []byte(source), ETag: `"source-etag"`}
			require.NoError(t, svc.MergeGroupConfiguredCodexModels(t.Context(), &Group{ID: 71, Platform: PlatformOpenAI}, manifest, ""))
			require.Equal(t, source, string(manifest.Body))
			require.Equal(t, `"source-etag"`, manifest.ETag)
		})
	}
}

func TestMergeGroupConfiguredCodexModelsEmptyPoolDoesNotAdvertiseUnsupportedModels(t *testing.T) {
	t.Parallel()

	svc := &OpenAIGatewayService{accountRepo: codexModelsVisibilityAccountRepo{}}
	manifest := &OpenAIModelsResponse{Body: []byte(`{"models":[{"slug":"gpt-6-astra"}]}`)}
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(t.Context(), &Group{ID: 71, Platform: PlatformOpenAI}, manifest, ""))
	require.Empty(t, decodeCodexManifestModels(t, manifest.Body))
}
