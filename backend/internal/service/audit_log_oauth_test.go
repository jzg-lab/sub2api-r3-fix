package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditOAuthProofHandlesAreRedacted(t *testing.T) {
	for _, key := range []string{
		"authorization_session_id", "reauthorization_session_id", "code_verifier",
		"authorizationSessionId", "reauthorizationSessionId", "codeVerifier",
		"authorization-session-id", "reauthorization.session.id",
	} {
		t.Run(key, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"account_id": 12,
				"extra":      []any{map[string]any{key: "proof-canary"}},
			})
			require.NoError(t, err)
			redacted := RedactAuditBody(body, "application/json")
			require.NotContains(t, redacted, "proof-canary")
			var result map[string]any
			require.NoError(t, json.Unmarshal([]byte(redacted), &result))
			require.EqualValues(t, 12, result["account_id"])
			require.Equal(t, "***", result["extra"].([]any)[0].(map[string]any)[key])
		})
	}
}
