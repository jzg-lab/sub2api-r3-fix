package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLiftResponsesToolOutputMedia(t *testing.T) {
	var input any
	require.NoError(t, json.Unmarshal([]byte(`[
		{"type":"function_call","call_id":"call_image","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_image","output":[{"type":"input_image","image_url":"data:image/png;base64,AQID"}]}
	]`), &input))

	lifted, changed := LiftResponsesToolOutputMedia(input)
	require.True(t, changed)

	items, ok := lifted.([]any)
	require.True(t, ok)
	require.Len(t, items, 3)

	outputItem, ok := items[1].(map[string]any)
	require.True(t, ok)
	outputText, ok := outputItem["output"].(string)
	require.True(t, ok)
	require.Contains(t, outputText, toolOutputMediaMarker)
	require.NotContains(t, outputText, "data:image/png")

	mediaMessage, ok := items[2].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "message", mediaMessage["type"])
	require.Equal(t, "user", mediaMessage["role"])

	parts, ok := mediaMessage["content"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, parts, 2)
	require.Equal(t, "input_text", parts[0]["type"])
	require.Equal(t, "[Tool output media for call call_image]", parts[0]["text"])
	require.Equal(t, "input_image", parts[1]["type"])
	require.Equal(t, "data:image/png;base64,AQID", parts[1]["image_url"])
}

func TestLiftResponsesToolOutputMediaLeavesPlainOutputUntouched(t *testing.T) {
	input := []any{
		map[string]any{
			"type":    "function_call_output",
			"call_id": "call_text",
			"output":  "plain output",
		},
	}

	lifted, changed := LiftResponsesToolOutputMedia(input)
	require.False(t, changed)
	require.Equal(t, input, lifted)
}

func TestLiftResponsesToolOutputMediaKeepsParallelBatchContiguous(t *testing.T) {
	var input any
	require.NoError(t, json.Unmarshal([]byte(`[
		{"type":"function_call","call_id":"call_A","name":"view_image","arguments":"{}"},
		{"type":"function_call","call_id":"call_B","name":"view_image","arguments":"{}"},
		{"type":"function_call","call_id":"call_C","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":[{"type":"input_image","image_url":"data:image/png;base64,QQ=="}]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"resized A"}]},
		{"type":"function_call_output","call_id":"call_B","output":[{"type":"input_image","image_url":"data:image/png;base64,Qg=="}]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"resized B"}]},
		{"type":"function_call_output","call_id":"call_C","output":[{"type":"input_image","image_url":"data:image/png;base64,Qw=="}]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"resized C"}]}
	]`), &input))

	lifted, changed := LiftResponsesToolOutputMedia(input)
	require.True(t, changed)

	items, ok := lifted.([]any)
	require.True(t, ok)
	require.Len(t, items, 12)

	for index, callID := range []string{"call_A", "call_B", "call_C"} {
		base := 3 + index*3
		item, ok := items[base].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "function_call_output", item["type"])
		require.Equal(t, callID, item["call_id"])
		outputText, ok := item["output"].(string)
		require.True(t, ok)
		require.Contains(t, outputText, toolOutputMediaMarker)
		require.NotContains(t, outputText, "data:image/png")
		mediaMessage, ok := items[base+1].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "message", mediaMessage["type"])
		require.Equal(t, "user", mediaMessage["role"])
		parts, ok := mediaMessage["content"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, parts, 2)
		require.Equal(t, "[Tool output media for call "+callID+"]", parts[0]["text"])
		wantImage := []string{"data:image/png;base64,QQ==", "data:image/png;base64,Qg==", "data:image/png;base64,Qw=="}[index]
		require.Equal(t, wantImage, parts[1]["image_url"])

		notice, ok := items[base+2].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "developer", notice["role"])
		content, ok := notice["content"].([]any)
		require.True(t, ok)
		require.Len(t, content, 1)
		contentPart, ok := content[0].(map[string]any)
		require.True(t, ok)
		wantText := []string{"resized A", "resized B", "resized C"}[index]
		require.Equal(t, wantText, contentPart["text"])
	}
}
