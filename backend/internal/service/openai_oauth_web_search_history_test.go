package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIOAuthWebSearchHistory(t *testing.T) {
	for _, lite := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			body    string
			changed bool
			choice  string
		}{
			{"empty tools", `{"tools":[],"input":[{"type":"web_search_call","id":"ws_1"}]}`, true, "none"},
			{"missing tools", `{"input":[{"type":"web_search_call"}]}`, true, "none"},
			{"null tools", `{"tools":null,"input":[{"type":"web_search_call"}]}`, true, "none"},
			{"null choice", `{"tool_choice":null,"input":[{"type":"web_search_call"}]}`, true, "none"},
			{"auto choice", `{"tool_choice":"auto","input":[{"type":"web_search_call"}]}`, true, "none"},
			{"none choice", `{"tool_choice":"none","input":[{"type":"web_search_call"}]}`, true, "none"},
			{"required preserved", `{"tool_choice":"required","input":[{"type":"web_search_call"}]}`, true, "required"},
			{"existing function", `{"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"auto","input":[{"type":"web_search_call"}]}`, true, "auto"},
			{"named choice", `{"tools":[{"type":"function","name":"lookup"}],"tool_choice":{"type":"function","name":"lookup"},"input":[{"type":"web_search_call"}]}`, true, `{"type":"function","name":"lookup"}`},
			{"additional function", `{"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"lookup"}]},{"type":"web_search_call"}]}`, true, ""},
			{"empty additional tools", `{"input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"web_search_call"}]}`, true, "none"},
			{"multiple search results", `{"input":[{"type":"web_search_call","id":"ws_1"},{"type":"web_search_call","id":"ws_2"},{"type":"compaction_trigger"}]}`, true, "none"},
			{"trigger stays last", `{"input":[{"type":"web_search_call","seq":9007199254740993},{"type":"compaction_trigger"}]}`, true, "none"},
			{"declared hosted search", `{"tools":[{"type":"web_search","external_web_access":true}],"input":[{"type":"web_search_call"}]}`, false, ""},
			{"declared preview search", `{"tools":[{"type":"web_search_preview"}],"input":[{"type":"web_search_call"}]}`, false, ""},
			{"additional search", `{"input":[{"type":"web_search_call"},{"type":"additional_tools","tools":[{"type":"web_search"}]}]}`, false, ""},
			{"text is not history", `{"input":[{"type":"message","role":"user","content":"web_search_call"}]}`, false, ""},
			{"string input", `{"input":"web_search_call"}`, false, ""},
			{"empty object", `{}`, false, ""},
		} {
			t.Run(fmt.Sprintf("%s/lite=%v", tc.name, lite), func(t *testing.T) {
				body := []byte(tc.body)
				got, changed, err := ensureOpenAIOAuthWebSearchToolForHistoryBody(body, lite)
				require.NoError(t, err)
				require.Equal(t, tc.changed, changed)
				var mapped map[string]any
				require.NoError(t, decodeOpenAIJSONUseNumber(body, &mapped))
				require.Equal(t, tc.changed, ensureOpenAIOAuthWebSearchToolForHistory(mapped, lite))
				encoded, err := json.Marshal(mapped)
				require.NoError(t, err)
				require.JSONEq(t, string(got), string(encoded), "map and raw paths must agree")
				choice := gjson.GetBytes(got, "tool_choice")
				if choice.IsObject() {
					require.JSONEq(t, tc.choice, choice.Raw)
				} else {
					require.Equal(t, tc.choice, choice.String())
				}
				if !changed {
					require.Equal(t, body, got, "no-op preserves bytes")
					return
				}
				if !lite {
					require.Equal(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(got, "input").Raw)
				}
				if gjson.GetBytes(body, "input.0.seq").Exists() {
					require.Equal(t, "9007199254740993", gjson.GetBytes(got, "input.0.seq").Raw)
					items := gjson.GetBytes(got, "input").Array()
					require.Equal(t, "compaction_trigger", items[len(items)-1].Get("type").String())
				}
				var declarations []gjson.Result
				declarations = append(declarations, gjson.GetBytes(got, "tools").Array()...)
				for _, item := range gjson.GetBytes(got, "input").Array() {
					if item.Get("type").String() == "additional_tools" {
						declarations = append(declarations, item.Get("tools").Array()...)
					}
				}
				searchCount := 0
				for _, tool := range declarations {
					if tool.Get("type").String() == "web_search" {
						searchCount++
						require.Equal(t, gjson.False, tool.Get("external_web_access").Type)
					}
				}
				require.Equal(t, 1, searchCount)
				second, changedAgain, err := ensureOpenAIOAuthWebSearchToolForHistoryBody(got, lite)
				require.NoError(t, err)
				require.False(t, changedAgain)
				require.Equal(t, got, second)
				require.False(t, ensureOpenAIOAuthWebSearchToolForHistory(mapped, lite))
			})
		}
	}
}

func TestOpenAIOAuthWebSearchHistoryForward(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, lite := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%v/lite=%v", passthrough, lite), func(t *testing.T) {
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(
						"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")),
				}}
				cfg := &config.Config{}
				cfg.Security.URLAllowlist.Enabled = false
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
				account := &Account{
					ID: 10, Name: "oauth-fixture", Platform: PlatformOpenAI,
					Type: AccountTypeOAuth, Concurrency: 1,
					ProxyID: openAITransportTestProxyID(), Proxy: openAITransportTestProxy(),
					Credentials: map[string]any{"access_token": t.Name()},
					Extra:       map[string]any{"openai_passthrough": passthrough},
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				if lite {
					c.Request.Header.Set(responsesLiteHeader, "true")
				}
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				body := []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"xhigh"},"stream":true,"instructions":"Keep the supplied task.","tools":[],"input":[{"type":"web_search_call","id":"ws_1"},{"type":"message","role":"user","content":"Summarize this history."},{"type":"compaction_trigger"}]}`)
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1, "compatibility is applied before sending, not by replay")
				require.Equal(t, account.Proxy.URL(), upstream.lastProxyURL)
				got := upstream.bodies[0]
				require.Equal(t, "gpt-6-astra", gjson.GetBytes(got, "model").String())
				require.Equal(t, "xhigh", gjson.GetBytes(got, "reasoning.effort").String())
				require.Equal(t, "none", gjson.GetBytes(got, "tool_choice").String())
				items := gjson.GetBytes(got, "input").Array()
				require.Equal(t, "compaction_trigger", items[len(items)-1].Get("type").String())
				if lite {
					require.Empty(t, gjson.GetBytes(got, "tools").Array())
					require.Equal(t, "additional_tools", items[len(items)-2].Get("type").String())
					require.Equal(t, "web_search", items[len(items)-2].Get("tools.0.type").String())
					require.Equal(t, gjson.False, items[len(items)-2].Get("tools.0.external_web_access").Type)
				} else {
					require.Equal(t, "web_search", gjson.GetBytes(got, "tools.0.type").String())
					require.Equal(t, gjson.False, gjson.GetBytes(got, "tools.0.external_web_access").Type)
				}
			})
		}
	}
}

func TestOpenAIOAuthWebSearchHistoryConsumers(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"xhigh"},"tools":[],"input":[{"type":"web_search_call","id":"ws_1"},{"type":"compaction_trigger"}]}`)
	for _, lite := range []bool{false, true} {
		for _, compact := range []bool{false, true} {
			t.Run(fmt.Sprintf("transform/lite=%v/compact=%v", lite, compact), func(t *testing.T) {
				var req map[string]any
				require.NoError(t, decodeOpenAIJSONUseNumber(body, &req))
				result := applyCodexOAuthTransformWithOptions(req, codexOAuthTransformOptions{
					IsCodexCLI: true, IsCompact: compact, ResponsesLite: lite,
					SkipDefaultInstructions: true, PreserveToolCallIDs: true,
				})
				require.NoError(t, result.Error)
				got, err := json.Marshal(req)
				require.NoError(t, err)
				require.Equal(t, "gpt-6-astra", gjson.GetBytes(got, "model").String())
				require.Equal(t, "xhigh", gjson.GetBytes(got, "reasoning.effort").String())
				require.Equal(t, !compact, gjson.GetBytes(got, "tool_choice").String() == "none")
				if !compact && lite {
					require.Empty(t, gjson.GetBytes(got, "tools").Array())
					require.Equal(t, "additional_tools", gjson.GetBytes(got, "input.1.type").String())
					require.Equal(t, "compaction_trigger", gjson.GetBytes(got, "input.2.type").String())
				}
			})
			for _, kind := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
				t.Run(fmt.Sprintf("raw/%s/lite=%v/compact=%v", kind, lite, compact), func(t *testing.T) {
					account := &Account{Platform: PlatformOpenAI, Type: kind}
					got, _, err := normalizeOpenAIResponsesCompatibilityBodyWithOptions(body, account,
						openAIResponsesCompatibilityOptions{ResponsesLite: lite, Compact: compact})
					require.NoError(t, err)
					require.Equal(t, !compact && kind == AccountTypeOAuth,
						gjson.GetBytes(got, "tool_choice").String() == "none")
					require.Equal(t, "gpt-6-astra", gjson.GetBytes(got, "model").String())
					require.Equal(t, "xhigh", gjson.GetBytes(got, "reasoning.effort").String())
					if !compact {
						ws, _, err := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, account, lite)
						require.NoError(t, err)
						require.Equal(t, got, ws)
					}
				})
			}
		}
	}
	unchanged, changed, err := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, nil, false)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, unchanged)
}

func TestEnsureOpenAIOAuthWebSearchToolForHistoryDoesNotShareToolMap(t *testing.T) {
	first := map[string]any{"input": []any{map[string]any{"type": "web_search_call"}}}
	second := map[string]any{"input": []any{map[string]any{"type": "web_search_call"}}}
	require.True(t, ensureOpenAIOAuthWebSearchToolForHistory(first, false))
	require.True(t, ensureOpenAIOAuthWebSearchToolForHistory(second, false))
	firstTools, ok := first["tools"].([]any)
	require.True(t, ok)
	firstTool, ok := firstTools[0].(map[string]any)
	require.True(t, ok)
	firstTool["external_web_access"] = true
	secondTools, ok := second["tools"].([]any)
	require.True(t, ok)
	secondTool, ok := secondTools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, secondTool["external_web_access"])
	require.Equal(t, false, openAIWebSearchHistoryTool["external_web_access"])
}

const openAIWebSearchHistoryCompactionBody = `{"model":"gpt-5.5","instructions":"x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"look it up"}]},{"type":"web_search_call","id":"ws_123","status":"completed","action":{"type":"search","query":"kwin inputmethod"}},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"found"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"summarize"}]}],"tools":[],"tool_choice":"auto","parallel_tool_calls":false,"stream":true,"store":false}`
