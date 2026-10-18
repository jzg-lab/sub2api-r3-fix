package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func tenantFingerprintContext(keyID int64) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("session-id", "same-client-session")
	c.Request.Header.Set("thread-id", "same-client-session")
	c.Set("api_key", &APIKey{ID: keyID})
	return c
}

func TestCodexTenantMachineIngressIsolation(t *testing.T) {
	account := newTestMachineAccount(t, 70, testCodexMachineSeedB)
	service := &OpenAIGatewayService{}
	body := []byte(`{"prompt_cache_key":"same-client-session","client_metadata":{"session_id":"same-client-session"}}`)
	for _, ingress := range []string{"websocket", "compat_bridge"} {
		t.Run(ingress, func(t *testing.T) {
			var sessions, installations []string
			for _, keyID := range []int64{101, 202, 101} {
				c := tenantFingerprintContext(keyID)
				var output []byte
				var err error
				if ingress == "websocket" {
					service.stageCodexMachineFingerprintIDsForWSIngress(c, account)
					output, _, err = applyStagedCodexFingerprintClientMetadataRaw(c, account, body)
				} else {
					output, err = service.stageCodexMachineFingerprintIDsForCompatBridge(c, account, body)
				}
				require.NoError(t, err)
				ids := stagedCodexFingerprintIDs(c, account)
				require.NotNil(t, ids)
				h := c.Request.Header.Clone()
				applyStagedCodexFingerprintHeaders(c, account, h)
				session := gjson.GetBytes(output, "client_metadata.session_id").String()
				require.Equal(t, session, h.Get("session-id"))
				require.Equal(t, session, gjson.GetBytes(output, "prompt_cache_key").String())
				sessions = append(sessions, session)
				installations = append(installations, ids.installationID)
			}
			require.NotEqual(t, sessions[0], sessions[1], "authenticated clients must not share an upstream session")
			require.Equal(t, sessions[0], sessions[2], "same client must remain stable")
			require.Equal(t, installations[0], installations[1], "upstream account device identity must remain stable")
		})
	}
}

func TestCodexTenantRequestIdentityAcrossModes(t *testing.T) {
	for _, mode := range []codexFingerprintMode{codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull, codexFingerprintMachine} {
		t.Run(string(mode), func(t *testing.T) {
			account := newTestMachineAccount(t, 70, testCodexMachineSeedB)
			account.Extra[codexFingerprintModeExtraKey] = string(mode)
			var sessions, installations []string
			for _, keyID := range []int64{101, 202, 101} {
				c := tenantFingerprintContext(keyID)
				ids := resolveCodexFingerprintIDsForClient(c, account, c.Request.Header)
				require.NotNil(t, ids)
				installation := resolveCodexFingerprintIDsFromRequest(account, c.Request.Header).installationID
				require.Equal(t, installation, ids.installationID)
				installations = append(installations, ids.installationID)
				if mode == codexFingerprintMachine {
					sessions = append(sessions, ids.machinePseudonym("same-client-session"))
				} else {
					sessions = append(sessions, ids.sessionID)
				}
			}
			require.Equal(t, installations[0], installations[1])
			require.Equal(t, sessions[0], sessions[2])
			if mode == codexFingerprintDevice {
				require.Empty(t, sessions[0], "device mode must not introduce a session")
			} else {
				require.NotEmpty(t, sessions[0])
				require.NotEqual(t, sessions[0], sessions[1])
			}
		})
	}
}
