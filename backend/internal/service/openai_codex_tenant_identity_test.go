package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func tenantFingerprintContext(keyID int64) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("session-id", "same-client-session")
	c.Request.Header.Set("thread-id", "same-client-session")
	c.Set("api_key", &APIKey{ID: keyID})
	return c
}

func TestCodexTenantRequestIdentityAcrossModes(t *testing.T) {
	for _, mode := range []codexFingerprintMode{codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull} {
		t.Run(string(mode), func(t *testing.T) {
			account := newTestOAuthAccount(70, map[string]any{codexFingerprintSeedExtraKey: testCodexFingerprintSeed})
			account.Extra[codexFingerprintModeExtraKey] = string(mode)
			var sessions, installations []string
			for _, keyID := range []int64{101, 202, 101} {
				c := tenantFingerprintContext(keyID)
				ids := resolveCodexFingerprintIDsForClient(c, account, c.Request.Header)
				require.NotNil(t, ids)
				installation := resolveCodexFingerprintIDsFromRequest(account, c.Request.Header).installationID
				require.Equal(t, installation, ids.installationID)
				installations = append(installations, ids.installationID)
				sessions = append(sessions, ids.sessionID)
			}
			require.Equal(t, installations[0], installations[1])
			require.Equal(t, sessions[0], sessions[2])
			if mode == codexFingerprintDevice {
				require.Empty(t, sessions[0], "device mode must not introduce a session")
			} else {
				require.NotEmpty(t, sessions[0])
				require.Equal(t, sessions[0], sessions[1], "mode-level session is account scoped")
			}
		})
	}
}
