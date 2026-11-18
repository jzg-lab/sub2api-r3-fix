package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
