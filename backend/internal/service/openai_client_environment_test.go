package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexClientEnvironmentAcrossAccountModes(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"timezone and hostname are business text"}],"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}],"client_metadata":{"session_id":"client-session","timezone":"Asia/Shanghai","ip":"192.0.2.8","locale":"zh-CN","hostname":"customer-host","opaque_environment":{"other":"client"},"x-codex-turn-metadata":"{\"thread_id\":\"client-thread\",\"workspaces\":[\"/customer/private\"],\"timezone\":\"Asia/Shanghai\",\"os\":\"customer-os\",\"extra\":{\"ip\":\"192.0.2.8\"}}"}}`)
	for _, mode := range []string{"off", "device", "session", "full", "machine"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestMachineAccount(t, 70, testCodexMachineSeedB)
			account.Extra[codexFingerprintModeExtraKey] = mode
			raw, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 101)
			require.NoError(t, err)
			require.True(t, changed)
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(body, &decoded))
			require.True(t, applyCodexAccountIdentityClientMetadataMap(decoded, account, 101))
			expected, err := json.Marshal(decoded)
			require.NoError(t, err)
			require.JSONEq(t, string(expected), string(raw))
			for _, field := range []string{"timezone", "ip", "locale", "hostname", "opaque_environment"} {
				require.False(t, gjson.GetBytes(raw, "client_metadata."+field).Exists())
			}
			turn := gjson.GetBytes(raw, "client_metadata.x-codex-turn-metadata").String()
			require.True(t, gjson.Get(turn, "thread_id").Exists())
			require.False(t, gjson.Get(turn, "workspaces").Exists())
			require.False(t, gjson.Get(turn, "timezone").Exists())
			require.False(t, gjson.Get(turn, "extra").Exists())
			require.Equal(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(raw, "input").Raw)
			require.Equal(t, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(raw, "tools").Raw)
		})
	}
}

func TestCodexEnvironmentBoundaryDoesNotDependOnIdentitySeed(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, body := range []string{
		`{"client_metadata":{"timezone":"Asia/Shanghai"}}`,
		`{"client_metadata":["customer-host"]}`,
		`{"client_metadata":"customer-host"}`,
		`{"client_metadata":{"session_id":{"ip":"192.0.2.8"}}}`,
		`{"client_metadata":{"x-codex-turn-metadata":"malformed customer-host"}}`,
	} {
		clean, changed, err := applyCodexAccountIdentityClientMetadataRaw([]byte(body), account, 101)
		require.NoError(t, err)
		require.True(t, changed)
		require.JSONEq(t, `{}`, string(clean))
	}
}

func TestCodexEnvironmentDuplicateAndNestedMetadata(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	_, _, err := sanitizeCodexClientEnvironmentRaw([]byte(`{"client_metadata":{},"client_metadata":{"ip":"192.0.2.8"}}`), account)
	require.ErrorContains(t, err, "duplicate client_metadata")
	clean, changed, err := sanitizeCodexClientEnvironmentRaw([]byte(`{"client_metadata":{"session_id":"first","session_id":"last","timezone":"private"}}`), account)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"client_metadata":{"session_id":"last"}}`, string(clean))
	turn, changed := sanitizeCodexTurnMetadata(`{"thread_id":{"ip":"private"},"thread_id":"last","timezone":"private"}`)
	require.True(t, changed)
	require.JSONEq(t, `{"thread_id":"last"}`, turn)
}

func TestCodexEnvironmentPreservesResponsesLiteProtocolFlag(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, tc := range []struct {
		value any
		want  string
	}{
		{"true", "true"}, {" TRUE ", "true"}, {"false", "false"},
		{" False ", "false"}, {"customer-host", ""}, {true, ""}, {nil, ""},
		{map[string]any{"timezone": "private"}, ""},
	} {
		fields := map[string]any{responsesLiteWSMetadataKey: tc.value, "timezone": "private"}
		body, err := json.Marshal(map[string]any{"client_metadata": fields, "input": "business"})
		require.NoError(t, err)
		raw, changed, err := sanitizeCodexClientEnvironmentRaw(body, account)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, tc.want, gjson.GetBytes(raw, "client_metadata."+responsesLiteWSMetadataKey).String())
		if tc.want == "" {
			require.False(t, gjson.GetBytes(raw, "client_metadata."+responsesLiteWSMetadataKey).Exists())
		}
		require.Equal(t, tc.want == "true", isOpenAIResponsesLiteWebSocketPayload(raw))
		require.False(t, gjson.GetBytes(raw, "client_metadata.timezone").Exists())
		require.Equal(t, "business", gjson.GetBytes(raw, "input").String())
	}
}

func TestCodexEnvironmentHeaderFinalizer(t *testing.T) {
	old := codexIdentityEnforcement.Load()
	t.Cleanup(func() { codexIdentityEnforcement.Store(old) })
	for _, enforced := range []bool{false, true} {
		codexIdentityEnforcement.Store(enforced)
		headers := http.Header{}
		headers.Set(openAIWSTurnMetadataHeader, `{"thread_id":"scoped","timezone":"private","workspaces":["private"]}`)
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
		applyCodexAccountIdentityHeaders(headers, account, 101)
		enforceCodexIdentityHeadersWithUA(headers, "")
		require.JSONEq(t, `{"thread_id":"scoped"}`, headers.Get(openAIWSTurnMetadataHeader))
		before := headers.Clone()
		enforceCodexIdentityHeadersWithUA(headers, "")
		require.Equal(t, before, headers)
		headers[openAIWSTurnMetadataHeader] = []string{`{"ip":"private"}`}
		headers.Add(openAIWSTurnMetadataHeader, `{"hostname":"private"}`)
		applyCodexAccountIdentityHeaders(headers, account, 101)
		enforceCodexIdentityHeadersWithUA(headers, "")
		require.Empty(t, headers.Get(openAIWSTurnMetadataHeader))
		require.NotContains(t, headers, openAIWSTurnMetadataHeader)
	}
}

func TestCodexEnvironmentVersionIsServerOwned(t *testing.T) {
	old := codexIdentityEnforcement.Load()
	t.Cleanup(func() { codexIdentityEnforcement.Store(old) })
	codexIdentityEnforcement.Store(true)
	for _, version := range []string{"0.1.0", "99.999.0", "customer-version"} {
		for _, originator := range []string{"", "codex_cli_rs"} {
			headers := http.Header{}
			if originator != "" {
				headers.Set("originator", originator)
			}
			headers.Set("version", version)
			enforceCodexIdentityHeadersWithUA(headers, "")
			require.Equal(t, CodexCanonicalClientVersion(), headers.Get("version"))
			if originator == "" {
				require.Empty(t, headers.Get("originator"))
			}
		}
	}
}

func TestCodexEnvironmentUnrelatedPayloadAndAPIKeyRemainUnchanged(t *testing.T) {
	body := []byte(`{"input":"` + strings.Repeat("business content ", 65536) + `","metadata":{"timezone":"intentional-business-data"},"client_metadata":{"session_id":"client"}}`)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	clean, changed, err := sanitizeCodexClientEnvironmentRaw(body, account)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, clean)
	account.Type = AccountTypeAPIKey
	body = []byte(`{"client_metadata":{"timezone":"API-key compatibility"}}`)
	clean, changed, err = sanitizeCodexClientEnvironmentRaw(body, account)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, clean)
}

func TestCodexEnvironmentWebSocketAndBridgeBoundaries(t *testing.T) {
	body := []byte(`{"type":"response.create","input":"business","client_metadata":{"session_id":"same-client-session","timezone":"private","x-codex-turn-metadata":"{\"thread_id\":\"thread\",\"workspaces\":[\"private\"]}"}}`)
	for _, mode := range []string{"off", "machine"} {
		account := newTestMachineAccount(t, 70, testCodexMachineSeedB)
		account.Extra[codexFingerprintModeExtraKey] = mode
		for _, bridge := range []bool{false, true} {
			c := tenantFingerprintContext(101)
			service := &OpenAIGatewayService{}
			var clean []byte
			var err error
			if bridge {
				clean, err = service.stageCodexMachineFingerprintIDsForCompatBridge(c, account, body)
			} else {
				service.stageCodexMachineFingerprintIDsForWSIngress(c, account)
				clean, _, err = applyStagedCodexFingerprintClientMetadataRaw(c, account, body)
			}
			require.NoError(t, err)
			require.False(t, gjson.GetBytes(clean, "client_metadata.timezone").Exists())
			require.NotContains(t, gjson.GetBytes(clean, "client_metadata.x-codex-turn-metadata").String(), "workspaces")
			require.Equal(t, "business", gjson.GetBytes(clean, "input").String())
		}
	}
}

func TestCodexEnvironmentHTTPAndCompactBoundaries(t *testing.T) {
	for _, endpoint := range []string{"/v1/responses", "/v1/responses/compact"} {
		for _, passthrough := range []bool{false, true} {
			c, _, body := machineChainRequest(t, endpoint, testMachineChainRoot, testMachineChainRoot, testMachineChainRoot+":0", "")
			service, upstream := newMachineChainService()
			account := newMachineChainAccount(t, 6001, testCodexMachineSeedB, passthrough, "")
			c.Request.Header.Set("X-Forwarded-For", "192.0.2.8")
			c.Request.Header.Set("X-Timezone", "Asia/Shanghai")
			c.Request.Header.Set("Accept-Language", "zh-CN")
			var err error
			body, err = sjson.SetBytes(body, "client_metadata.customer_environment", map[string]string{"hostname": "customer-private"})
			require.NoError(t, err)
			_, _ = service.Forward(context.Background(), c, account, body)
			require.NotNil(t, upstream.lastReq, "must exercise actual upstream builder")
			require.Empty(t, upstream.lastReq.Header.Get("X-Forwarded-For"))
			require.Empty(t, upstream.lastReq.Header.Get("X-Timezone"))
			require.Empty(t, upstream.lastReq.Header.Get("Accept-Language"))
			require.NotContains(t, string(upstream.lastBody), "customer-private")
			require.NotContains(t, string(upstream.lastBody), "/customer/private")
			require.Contains(t, string(upstream.lastBody), "hi")
		}
	}
}
