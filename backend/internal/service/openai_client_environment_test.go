package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexClientEnvironmentAcrossAccountModes(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"timezone and hostname are business text"}],"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}],"client_metadata":{"session_id":"client-session","timezone":"Asia/Shanghai","ip":"192.0.2.8","locale":"zh-CN","hostname":"customer-host","opaque_environment":{"other":"client"},"x-codex-turn-metadata":"{\"thread_id\":\"client-thread\",\"workspaces\":[\"/customer/private\"],\"timezone\":\"Asia/Shanghai\",\"os\":\"customer-os\",\"extra\":{\"ip\":\"192.0.2.8\"}}"}}`)
	for _, mode := range []string{"off", "device", "session", "full"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestOAuthAccount(70, map[string]any{
				codexFingerprintModeExtraKey: mode,
				codexFingerprintSeedExtraKey: testCodexFingerprintSeed,
			})
			raw, cleaned, err := sanitizeCodexClientEnvironmentRaw(body, account)
			require.NoError(t, err)
			require.True(t, cleaned)
			raw, _, err = applyCodexAccountIdentityClientMetadataRaw(raw, account, 101)
			require.NoError(t, err)
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(body, &decoded))
			require.True(t, sanitizeCodexClientEnvironmentMap(decoded, account))
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
		clean, changed, err := sanitizeCodexClientEnvironmentRaw([]byte(body), account)
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
		sanitizeCodexClientEnvironmentHeaders(headers)
		applyCodexAccountIdentityHeaders(headers, account, 101)
		enforceCodexIdentityHeadersWithUA(headers, "")
		require.JSONEq(t, `{"thread_id":"scoped"}`, headers.Get(openAIWSTurnMetadataHeader))
		before := headers.Clone()
		enforceCodexIdentityHeadersWithUA(headers, "")
		require.Equal(t, before, headers)
		headers[openAIWSTurnMetadataHeader] = []string{`{"ip":"private"}`}
		headers.Add(openAIWSTurnMetadataHeader, `{"hostname":"private"}`)
		sanitizeCodexClientEnvironmentHeaders(headers)
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
			if originator == "" {
				require.Empty(t, headers.Get("originator"))
				require.Equal(t, CodexCanonicalClientVersion(), headers.Get("version"), "an existing version is server-owned even without an originator")
			} else {
				require.Equal(t, CodexCanonicalClientVersion(), headers.Get("version"))
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

func TestCodexEnvironmentCompactBoundary(t *testing.T) {
	{
		endpoint := "/v1/responses/compact"
		passthrough := false
		body := []byte(`{"model":"gpt-5.2","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"client_metadata":{"session_id":"client-session","timezone":"Asia/Shanghai","hostname":"customer-private","x-codex-turn-metadata":"{\"thread_id\":\"client-thread\",\"timezone\":\"Asia/Shanghai\",\"workspaces\":[\"/customer/private\"]}"}}`)
		upstream := &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"captured"}}`)),
		}}
		service := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
		account := newTestOAuthAccount(6001, map[string]any{
			codexFingerprintModeExtraKey: "session", codexFingerprintSeedExtraKey: testCodexFingerprintSeed,
			"openai_passthrough": passthrough,
		})
		account.Name = "oauth-test"
		account.Status = StatusActive
		account.Schedulable = true
		account.Concurrency = 1
		account.RateMultiplier = f64p(1)
		account.Credentials = map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-acc"}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Header.Set("session-id", "client-session")
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
		require.Empty(t, upstream.lastReq.Header.Get("Accept-Language"), "customer environment is outside the OAuth forwarding allowlist")
		require.NotContains(t, string(upstream.lastBody), "customer-private")
		require.NotContains(t, string(upstream.lastBody), "/customer/private")
		require.Contains(t, string(upstream.lastBody), "hi")
	}
}
