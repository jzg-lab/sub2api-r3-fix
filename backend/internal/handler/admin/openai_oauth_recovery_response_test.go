package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIExchangeRecoveryUsesProofCredentialRepresentation(t *testing.T) {
	oauth := service.NewOpenAIOAuthService(nil, nil)
	t.Cleanup(oauth.Stop)
	h := &OpenAIOAuthHandler{openaiOAuthService: oauth}
	opaque := make([]byte, 16)
	_, err := rand.Read(opaque)
	require.NoError(t, err)
	info := &service.OpenAITokenInfo{
		AccessToken: hex.EncodeToString(opaque), ReauthorizationProof: hex.EncodeToString(opaque),
		ExpiresIn: 3600, ExpiresAt: 1791133200,
	}
	result := h.exchangeCodeResult(info)
	require.Same(t, info, result.OpenAITokenInfo)
	require.True(t, reflect.DeepEqual(oauth.BuildAccountCredentials(info), result.Credentials))

	payload, err := json.Marshal(result)
	require.NoError(t, err)
	var decoded struct {
		Proof       string         `json:"reauthorization_proof"`
		Credentials map[string]any `json:"credentials"`
	}
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.True(t, decoded.Proof == info.ReauthorizationProof)
	require.NotEmpty(t, decoded.Credentials)
	require.True(t, decoded.Credentials["access_token"] == info.AccessToken)
	_, proofInCredentials := decoded.Credentials["reauthorization_proof"]
	require.False(t, proofInCredentials)
}

func TestOpenAIExchangeWithoutRecoveryProofPreservesWireResponse(t *testing.T) {
	info := &service.OpenAITokenInfo{ExpiresIn: 3600}
	// Ordinary exchange must not require a credential builder or add fields.
	h := &OpenAIOAuthHandler{}
	result := h.exchangeCodeResult(info)
	require.Nil(t, result.Credentials)
	before, err := json.Marshal(info)
	require.NoError(t, err)
	after, err := json.Marshal(result)
	require.NoError(t, err)
	require.True(t, string(before) == string(after))
}
