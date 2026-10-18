package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeCodexModelsManifestServiceTiers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		want    string
		literal string
		changed bool
	}{
		{
			name:    "null becomes empty without adding capabilities",
			body:    `{"models":[{"slug":"custom","service_tiers": null,"unknown":{"number":9007199254740993}}],"metadata":{"kept":true}}`,
			want:    `{"models":[{"slug":"custom","service_tiers":[],"unknown":{"number":9007199254740993}}],"metadata":{"kept":true}}`,
			literal: "9007199254740993",
			changed: true,
		},
		{
			name: "missing field is not synthesized",
			body: ` {"models":[{"slug":"custom"}]} `,
		},
		{
			name: "existing empty array stays byte identical",
			body: ` {"models":[{"slug":"custom","service_tiers": []}]} `,
		},
		{
			name: "provider tier metadata stays byte identical",
			body: `{"models":[{"slug":"custom","service_tiers":[{"id":"provider","unknown":true}]}]}`,
		},
		{
			name:    "only explicit null fields change in mixed entries",
			body:    `{"models":[{"slug":"one","service_tiers":null},{"slug":"two","service_tiers":[{"id":"provider"}]},{"slug":"three"},null,42]}`,
			want:    `{"models":[{"slug":"one","service_tiers":[]},{"slug":"two","service_tiers":[{"id":"provider"}]},{"slug":"three"},null,42]}`,
			changed: true,
		},
		{
			name: "empty models stays byte identical",
			body: ` {"models":[]} `,
		},
		{
			name: "other invalid field types are not silently rewritten",
			body: `{"models":[{"slug":"one","service_tiers":false}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := []byte(tt.body)
			normalized, err := normalizeCodexModelsManifestServiceTiers(original)
			require.NoError(t, err)
			require.Equal(t, tt.body, string(original), "must not mutate the input")
			if tt.changed {
				require.JSONEq(t, tt.want, string(normalized))
				require.Contains(t, string(normalized), `"service_tiers":[]`)
				require.NotContains(t, string(normalized), `"service_tiers":null`)
			} else {
				require.Equal(t, tt.body, string(normalized))
			}
			if tt.literal != "" {
				require.Contains(t, string(normalized), tt.literal)
			}
			again, err := normalizeCodexModelsManifestServiceTiers(normalized)
			require.NoError(t, err)
			require.Equal(t, normalized, again)
		})
	}
}

func TestNormalizeCodexModelsManifestServiceTiersRejectsInvalidEnvelope(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{`, `[]`, `null`, `{}`, `{"models":null}`, `{"models":"wrong"}`} {
		t.Run(body, func(t *testing.T) {
			_, err := normalizeCodexModelsManifestServiceTiers([]byte(body))
			require.Error(t, err)
		})
	}
}

func TestFetchCodexModelsManifestOAuthNormalizesNullServiceTiers(t *testing.T) {
	const upstreamBody = `{"models":[{"slug":"custom","service_tiers":null,"unknown":{"kept":true}}],"metadata":{"source":"upstream"}}`
	for _, upstreamETag := range []string{`W/"upstream-only"`, ""} {
		t.Run("upstream_etag="+upstreamETag, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if upstreamETag != "" {
					w.Header().Set("ETag", upstreamETag)
				}
				_, _ = w.Write([]byte(upstreamBody))
			}))
			t.Cleanup(server.Close)
			original := chatgptCodexModelsURL
			chatgptCodexModelsURL = server.URL
			t.Cleanup(func() { chatgptCodexModelsURL = original })

			s := &OpenAIGatewayService{}
			manifest, err := s.FetchCodexModelsManifest(context.Background(), newCodexModelsTestAccount(), "0.145.0", "")
			require.NoError(t, err)
			models := decodeCodexManifestModels(t, manifest.Body)
			require.Equal(t, []any{}, models[0]["service_tiers"])
			require.Equal(t, map[string]any{"kept": true}, models[0]["unknown"])
			require.Equal(t, codexModelsManifestBodyETag(manifest.Body), manifest.ETag)
			require.NotEqual(t, upstreamETag, manifest.ETag)
			require.Equal(t, upstreamBody, string(manifest.upstreamSourceBody))
			require.False(t, manifest.convertedFromOpenAIModelList)

			notModified := openAIModelsResponseForClient(manifest, manifest.ETag)
			require.True(t, notModified.NotModified)
			require.Empty(t, notModified.Body)
			require.Equal(t, manifest.ETag, notModified.ETag)
			if upstreamETag != "" {
				require.False(t, openAIModelsResponseForClient(manifest, upstreamETag).NotModified)
			}
		})
	}
}

func TestCompleteAPIKeyCodexModelsManifestDoesNotRestoreNullServiceTiers(t *testing.T) {
	const upstreamBody = `{"models":[{"slug":"custom","service_tiers":null,"unknown":{"kept":true}}]}`
	normalized, err := normalizeCodexModelsManifestServiceTiers([]byte(upstreamBody))
	require.NoError(t, err)
	manifest := &OpenAIModelsResponse{
		Body:               normalized,
		upstreamSourceBody: []byte(upstreamBody),
	}
	account := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://models.example.test/v1"},
	}
	s := &OpenAIGatewayService{}
	require.NoError(t, s.CompleteAPIKeyCodexModelsManifestForClient(manifest, account))
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(manifest.Body, &envelope))
	models := decodeCodexManifestModels(t, manifest.Body)
	require.Equal(t, []any{}, models[0]["service_tiers"])
	require.Equal(t, map[string]any{"kept": true}, models[0]["unknown"])
	require.Equal(t, upstreamBody, string(manifest.upstreamSourceBody))
	require.Equal(t, codexModelsManifestBodyETag(manifest.Body), manifest.ETag)
}
