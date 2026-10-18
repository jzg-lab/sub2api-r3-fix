package service

import (
	"context"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWSPassthroughTurnResultCarriesImageUsage(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	upstream := newStagedPassthroughConn()
	upstream.Send(`{"type":"response.completed","response":{"id":"image_usage","model":"gpt-5.1","usage":{"input_tokens":120,"output_tokens":70,"input_tokens_details":{"image_tokens":40},"output_tokens_details":{"reasoning_tokens":5}},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":900},"output_tokens_details":{"image_tokens":55}}}}}`)
	results := make(chan *OpenAIForwardResult, 1)
	hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
		if err == nil {
			results <- result
		}
	}}
	server, serverErr := startPassthroughHookRecordingServer(t, ctx,
		newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream),
		passthroughLifecycleAccount(), hooks)
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer client.CloseNow()
	event, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())
	select {
	case result := <-results:
		require.NotNil(t, result)
		require.Equal(t, 120, result.Usage.InputTokens)
		require.Equal(t, 70, result.Usage.OutputTokens)
		require.Equal(t, 40, result.Usage.ImageInputTokens)
		require.Equal(t, 55, result.Usage.ImageOutputTokens)
		require.Equal(t, 5, result.Usage.ReasoningTokens)
	case <-time.After(3 * time.Second):
		t.Fatal("missing passthrough turn result")
	}
	_ = client.Close(coderws.StatusNormalClosure, "done")
	cancel(context.Canceled)
	select {
	case <-serverErr:
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough did not terminate")
	}
}
