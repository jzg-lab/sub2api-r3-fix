//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// r17x D 项回归:openAIResponsesMayContainOrphanToolOutputs 预筛的健全性——
// 预筛返回 false(无 tool-output 类 item)⇒ sanitize 必然 noop ⇒ 不 decode
// 是纯性能优化。previous_response_id 不参与预筛:锚点删除是 patch,
// sanitize 基于 decode 终态自行判断(见
// TestOpenAIGatewayService_OAuthDropsOrphanAfterDroppingPreviousResponse)。

func TestOrphanPrefilter_FalseImpliesSanitizeNoop(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"无 input", `{"model":"gpt-5"}`},
		{"input 空", `{"model":"gpt-5","input":[]}`},
		{"input 全 message", `{"model":"gpt-5","input":[{"type":"message","role":"user"}]}`},
		{"input 是对象非数组", `{"model":"gpt-5","input":{"a":1}}`},
		{"previous_response_id 在场但无 tool output", `{"model":"gpt-5","previous_response_id":"resp_1","input":[{"type":"message"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, openAIResponsesMayContainOrphanToolOutputs([]byte(tc.body)))
		})
	}
}

func TestOrphanPrefilter_TrueWhenCallOutputPresent(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"function_call_output", `{"model":"gpt-5","input":[{"type":"function_call_output","call_id":"c1","output":"x"}]}`},
		{"mcp_tool_call_output 混在 message 里", `{"model":"gpt-5","input":[{"type":"message"},{"type":"mcp_tool_call_output","call_id":"c2"}]}`},
		{"tool_search_output", `{"model":"gpt-5","input":[{"type":"tool_search_output"}]}`},
		{"previous_response_id 在场且带 tool output(锚点删除后需清理)", `{"model":"gpt-5","previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"orphan","output":"x"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, openAIResponsesMayContainOrphanToolOutputs([]byte(tc.body)))
		})
	}
}

// 端到端等价:预筛 true 时走到 sanitize 的行为与旧的无条件调用一致(孤儿被删/非孤儿不动)。
func TestOrphanPrefilter_SanitizeBehaviorUnchanged(t *testing.T) {
	// 孤儿 output(call_id 无匹配 call 且无 item_reference)→ sanitize true
	orphan := map[string]any{
		"model": "gpt-5",
		"input": []any{map[string]any{"type": "function_call_output", "call_id": "orphan", "output": "x"}},
	}
	require.True(t, sanitizeOpenAIResponsesOrphanToolOutputs(orphan, orphan["input"].([]any), false))

	// call_id 有匹配的 call 上下文 → false(不动)
	matched := map[string]any{
		"model": "gpt-5",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "c1", "name": "f"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "x"},
		},
	}
	require.False(t, sanitizeOpenAIResponsesOrphanToolOutputs(matched, matched["input"].([]any), false))
}
