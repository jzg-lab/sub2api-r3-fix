package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// 探针 turn 的头集合黑白名单：真实 codex 0.151 抓包会发的头全部在场，
// 不会发的头（version / openai-beta / origin / referer）一个不多。
func TestOpenAIDowngradeProbeTurnHeadersMatchCapture(t *testing.T) {
	turn := newOpenAIDowngradeProbeTurn(&Account{ID: 42})
	h := make(http.Header)
	turn.applyRequestHeaders(h, true)

	require.Equal(t, "text/event-stream", h.Get("Accept"))
	require.Equal(t, "codex_exec", h.Get("originator"))
	require.Equal(t, turn.userAgent(), h.Get("user-agent"))
	require.Contains(t, h.Get("user-agent"), "codex_exec/")
	require.Equal(t, turn.sessionID, h.Get("session-id"))
	require.Equal(t, turn.threadID, h.Get("thread-id"))
	require.Equal(t, turn.sessionID, h.Get("x-client-request-id"))
	require.Equal(t, turn.windowID, h.Get("x-codex-window-id"))
	require.Equal(t, "prevent_idle_sleep,remote_compaction_v2", h.Get("x-codex-beta-features"))
	require.JSONEq(t, turn.turnMetaJSON, h.Get("x-codex-turn-metadata"))

	for _, forbidden := range []string{"version", "openai-beta", "origin", "referer", "accept-encoding"} {
		require.Empty(t, h.Get(forbidden), "探针不得携带真实 codex 不发的头: %s", forbidden)
	}
}

// 探针体复刻真实 codex 单轮形态：全量 instructions/tools 模板、developer
// 环境上下文 + user 题目、reasoning/include/prompt_cache_key/client_metadata
// 与抓包一致；身份 ID 全部 uuid v7（时间有序，v4 是可识别差异）。
func TestOpenAIDowngradeProbeTurnBodyMatchesCapture(t *testing.T) {
	account := &Account{ID: 42}
	turn := newOpenAIDowngradeProbeTurn(account)
	body, err := turn.buildRequestBody("gpt-6-astra", "题目", true)
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(body, &payload))

	require.Equal(t, openAIDowngradeProbeBaseInstructions, payload["instructions"])
	require.NotEmpty(t, openAIDowngradeProbeToolsJSON)
	tools, ok := payload["tools"].([]any)
	require.True(t, ok)
	require.Greater(t, len(tools), 10, "tools 应为真实 codex 的全量工具集")

	input, ok := payload["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 2)
	first, _ := input[0].(map[string]any)
	second, _ := input[1].(map[string]any)
	require.Equal(t, "developer", first["role"])
	parts, ok := first["content"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, parts)
	envText, _ := parts[0].(map[string]any)["text"].(string)
	require.Contains(t, envText, "<environment_context>")
	require.Equal(t, "user", second["role"])

	require.Equal(t, "auto", payload["tool_choice"])
	require.Equal(t, true, payload["parallel_tool_calls"])
	reasoning, _ := payload["reasoning"].(map[string]any)
	require.Equal(t, "xhigh", reasoning["effort"])
	require.Equal(t, "auto", reasoning["summary"])
	require.Equal(t, false, payload["store"])
	require.Equal(t, true, payload["stream"])
	require.Equal(t, []any{"reasoning.encrypted_content"}, payload["include"])
	require.Equal(t, turn.sessionID, payload["prompt_cache_key"])

	meta, _ := payload["client_metadata"].(map[string]any)
	require.Equal(t, turn.sessionID, meta["session_id"])
	require.Equal(t, turn.turnID, meta["turn_id"])
	require.Equal(t, turn.installationID, meta["x-codex-installation-id"])

	turnMeta := struct {
		Sandbox     string `json:"sandbox"`
		SandboxMode string `json:"sandbox_mode"`
		AgentName   string `json:"agent_name"`
	}{}
	require.NoError(t, json.Unmarshal([]byte(turn.turnMetaJSON), &turnMeta))
	require.Equal(t, "none", turnMeta.Sandbox)
	require.Equal(t, "danger-full-access", turnMeta.SandboxMode)
	require.Equal(t, "/root", turnMeta.AgentName)

	for _, id := range []string{turn.sessionID, turn.turnID, turn.contextWindowID} {
		parsed, err := uuid.Parse(id)
		require.NoError(t, err)
		require.Equal(t, uuid.Version(7), parsed.Version(), "探针身份 ID 必须是 uuid v7")
	}

	// installation_id 账号级恒定：同一账号两次构造不漂移。
	require.Equal(t, turn.installationID, openAIDowngradeProbeInstallationID(account))
}

// 真实流量顺延：近窗有流量则顺延，连续达上限后照常探测；无流量立即复位。
func TestOpenAIDowngradeProbeRealTrafficDeferral(t *testing.T) {
	runner := &OpenAIDowngradeProbeRunner{
		deferCounts: make(map[int64]int),
	}
	var seen []time.Duration
	runner.SetRecentTrafficChecker(func(_ context.Context, _ int64, within time.Duration) bool {
		seen = append(seen, within)
		return true
	})

	for i := 0; i < openAIDowngradeMaxTrafficDeferrals; i++ {
		require.True(t, runner.shouldDeferForRealTraffic(context.Background(), 7),
			"第 %d 次应顺延", i+1)
	}
	require.False(t, runner.shouldDeferForRealTraffic(context.Background(), 7),
		"连续顺延达上限后必须照常探测")
	require.Equal(t, []time.Duration{openAIDowngradeRecentTrafficWindow,
		openAIDowngradeRecentTrafficWindow, openAIDowngradeRecentTrafficWindow}, seen)

	// 上限后计数已复位：流量仍在时重新进入顺延窗口。
	require.True(t, runner.shouldDeferForRealTraffic(context.Background(), 7))

	// 无真实流量：不顺延且计数清零。
	runner.SetRecentTrafficChecker(func(context.Context, int64, time.Duration) bool { return false })
	require.False(t, runner.shouldDeferForRealTraffic(context.Background(), 7))
	require.Equal(t, 0, runner.deferCounts[7])
}
