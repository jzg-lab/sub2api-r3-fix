package openai_ws_v2

import (
	"context"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestRelay_ImageUsageMatchesHTTPResponsesAccounting(t *testing.T) {
	t.Parallel()
	client := newPassthroughTestFrameConn(nil, false)
	upstream := newPassthroughTestFrameConn([]passthroughTestFrame{
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"image_tool","usage":{"input_tokens":100,"output_tokens":60,"output_tokens_details":{"reasoning_tokens":3}},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":80},"output_tokens_details":{"image_tokens":50}}}}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"image_explicit","usage":{"input_tokens":30,"output_tokens":15,"input_tokens_details":{"image_tokens":20},"output_tokens_details":{"image_tokens":10,"reasoning_tokens":2}},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":800},"output_tokens_details":{"image_tokens":500}}}}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"image_prompt","usage":{"prompt_tokens":12,"completion_tokens":2,"prompt_tokens_details":{"image_tokens":7}}}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"text","usage":{"input_tokens":3,"output_tokens":4}}}`)},
	}, true)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var turns []RelayTurnResult
	result, exit := Relay(ctx, client, upstream,
		[]byte(`{"type":"response.create","model":"gpt-6-astra","input":[]}`),
		RelayOptions{OnTurnComplete: func(turn RelayTurnResult) { turns = append(turns, turn) }})
	require.Nil(t, exit)
	require.Len(t, turns, 4)
	require.Equal(t, 80, turns[0].Usage.ImageInputTokens)
	require.Equal(t, 50, turns[0].Usage.ImageOutputTokens)
	require.Equal(t, 20, turns[1].Usage.ImageInputTokens, "explicit usage must win")
	require.Equal(t, 10, turns[1].Usage.ImageOutputTokens)
	require.Equal(t, 7, turns[2].Usage.ImageInputTokens)
	require.Zero(t, turns[3].Usage.ImageInputTokens, "new turn must not inherit image usage")
	require.Zero(t, turns[3].Usage.ImageOutputTokens)
	require.Zero(t, turns[3].Usage.ReasoningTokens)
	require.Equal(t, 107, result.Usage.ImageInputTokens)
	require.Equal(t, 60, result.Usage.ImageOutputTokens)
	require.Equal(t, 5, result.Usage.ReasoningTokens, "session must retain per-turn reasoning usage")
	require.Equal(t, 145, result.Usage.InputTokens)
	require.Equal(t, 81, result.Usage.OutputTokens, "reasoning details must not double-count output")
}

func TestRelay_ImageUsageBoundaryCases(t *testing.T) {
	for name, test := range map[string]struct {
		event string
		input int
		out   int
	}{
		"top_level_tool": {
			event: `{"type":"response.completed","usage":{"input_tokens":8,"output_tokens":4},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":6},"output_tokens_details":{"image_tokens":3}}}}`,
			input: 6, out: 3,
		},
		"negative_details": {
			event: `{"type":"response.completed","usage":{"input_tokens":8,"output_tokens":4,"input_tokens_details":{"image_tokens":-5},"output_tokens_details":{"image_tokens":-2}}}`,
		},
		"invalid_totals": {
			event: `{"type":"response.completed","usage":{"input_tokens":"invalid","output_tokens":4},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":6}}}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			state := &relayState{}
			parseUsageAndAccumulate(state, []byte(test.event), "response.completed", nil)
			turn := finalizeRelayTurnUsage(state)
			require.Equal(t, test.input, turn.ImageInputTokens)
			require.Equal(t, test.out, turn.ImageOutputTokens)
			require.Equal(t, Usage{}, state.turnUsage)
			require.Equal(t, turn, state.usage)
		})
	}
	t.Run("partial_then_empty_terminal", func(t *testing.T) {
		state := &relayState{}
		mergeRelayUsageNonZero(&state.turnUsage, Usage{ImageInputTokens: 7})
		parseUsageAndAccumulate(state, []byte(`{"usage":{"input_tokens":0,"output_tokens":0}}`), "response.completed", nil)
		require.Equal(t, 7, finalizeRelayTurnUsage(state).ImageInputTokens)
		require.Zero(t, finalizeRelayTurnUsage(state).ImageInputTokens)
		require.Equal(t, 7, state.usage.ImageInputTokens, "settlement must not double-count a cleared turn")
	})
}
