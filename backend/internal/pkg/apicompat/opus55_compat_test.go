package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpus55ResponsesAdaptiveThinkingAndToolChoice(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		req := &ResponsesRequest{
			Model:     "claude-opus-5-5",
			Input:     json.RawMessage(`"hello"`),
			Reasoning: &ResponsesReasoning{Effort: effort},
		}
		out, err := ResponsesToAnthropicRequest(req)
		require.NoError(t, err)
		require.NotNil(t, out.Thinking)
		require.Equal(t, "adaptive", out.Thinking.Type)
		require.Zero(t, out.Thinking.BudgetTokens)
		if effort == "" {
			effort = "medium"
		}
		require.Equal(t, effort, out.OutputConfig.Effort)
	}

	for _, choice := range []string{`"required"`, `{"type":"function","name":"lookup"}`} {
		_, err := ResponsesToAnthropicRequest(&ResponsesRequest{
			Model:      "claude-opus-5-5",
			Input:      json.RawMessage(`"hello"`),
			ToolChoice: json.RawMessage(choice),
		})
		require.ErrorContains(t, err, "forced tool_choice")
	}

	_, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model:     "claude-opus-5-5",
		Input:     json.RawMessage(`"hello"`),
		Reasoning: &ResponsesReasoning{Effort: "none"},
	})
	require.ErrorContains(t, err, "reasoning effort")

	old, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model:     "claude-opus-5",
		Input:     json.RawMessage(`"hello"`),
		Reasoning: &ResponsesReasoning{Effort: "xhigh"},
	})
	require.NoError(t, err)
	require.Equal(t, "max", old.OutputConfig.Effort)
	require.Equal(t, "enabled", old.Thinking.Type)
}

func TestOpus55SignedThinkingResponsesRoundTrip(t *testing.T) {
	block := AnthropicContentBlock{
		Type:      "thinking",
		Signature: "upstream-signed-block",
	}
	response := AnthropicToResponsesResponse(&AnthropicResponse{
		Model: "claude-opus-5-5",
		Content: []AnthropicContentBlock{
			block,
			{Type: "tool_use", ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)},
		},
	})
	require.Len(t, response.Output, 2)
	require.NotEmpty(t, response.Output[0].EncryptedContent)

	raw, err := json.Marshal(response.Output)
	require.NoError(t, err)
	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(raw, &items))
	items = append(items, ResponsesInputItem{
		Type:   "function_call_output",
		CallID: response.Output[1].CallID,
		Output: "ok",
	})
	raw, err = json.Marshal(items)
	require.NoError(t, err)

	converted, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model: "claude-opus-5-5",
		Input: raw,
	})
	require.NoError(t, err)
	require.Len(t, converted.Messages, 2)
	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(converted.Messages[0].Content, &blocks))
	require.Equal(t, block, blocks[0])
	require.Equal(t, "tool_use", blocks[1].Type)

	_, _, err = convertResponsesInputToAnthropic(
		"",
		json.RawMessage(`[{"type":"reasoning","encrypted_content":"anthropic-thinking-v1:!"}]`),
		true,
	)
	require.Error(t, err)
}

func TestOpus55StreamingPreservesSignedThinking(t *testing.T) {
	index := 0
	state := NewAnthropicEventToResponsesState()
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
		Type: "message_start",
		Message: &AnthropicResponse{
			ID:    "msg_opus55",
			Model: "claude-opus-5-5",
		},
	}, state)
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
		Type:         "content_block_start",
		Index:        &index,
		ContentBlock: &AnthropicContentBlock{Type: "thinking"},
	}, state)
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: &index,
		Delta: &AnthropicDelta{Type: "thinking_delta", Thinking: "reason"},
	}, state)
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: &index,
		Delta: &AnthropicDelta{Type: "signature_delta", Signature: "signed"},
	}, state)
	events := AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
		Type:  "content_block_stop",
		Index: &index,
	}, state)

	var done *ResponsesStreamEvent
	for i := range events {
		if events[i].Type == "response.output_item.done" {
			done = &events[i]
		}
	}
	require.NotNil(t, done)
	require.NotNil(t, done.Item)
	require.NotEmpty(t, done.Item.EncryptedContent)
}
