package apicompat

import (
	"encoding/json"
	"fmt"
	"strings"
)

type responsesToolOutputMedia struct {
	callID   string
	imageURL string
}

// LiftResponsesToolOutputMedia moves image parts from function outputs into a
// following user message for providers that only accept string tool outputs.
func LiftResponsesToolOutputMedia(input any) (any, bool) {
	items, ok := input.([]any)
	if !ok {
		return input, false
	}
	result := make([]any, 0, len(items)+1)
	changed := false
	for i := 0; i < len(items); {
		item, ok := items[i].(map[string]any)
		if !ok || !isResponsesToolOutputItem(item) {
			result = append(result, items[i])
			i++
			continue
		}
		batchStart := i
		batch := make([]any, 0)
		pending := make([]responsesToolOutputMedia, 0)
		for i < len(items) {
			item, ok := items[i].(map[string]any)
			if !ok || !isResponsesToolOutputItem(item) {
				break
			}
			output, exists := item["output"]
			raw, err := json.Marshal(output)
			text, media, didRewrite := "", []ChatContentPart(nil), false
			if exists && err == nil {
				text, media, didRewrite = extractToolOutputMedia(raw)
			}
			if didRewrite {
				item["output"] = text
				for _, part := range media {
					if part.ImageURL != nil && strings.TrimSpace(part.ImageURL.URL) != "" {
						pending = append(pending, responsesToolOutputMedia{callID: stringValue(item["call_id"]), imageURL: part.ImageURL.URL})
					}
				}
				changed = true
			}
			batch = append(batch, item)
			i++
		}
		if !changed || len(pending) == 0 {
			result = append(result, items[batchStart:i]...)
			continue
		}
		result = append(result, batch...)
		content := make([]map[string]any, 0, len(pending)*2)
		lastCallID := ""
		for _, media := range pending {
			if media.callID != lastCallID {
				label := "Tool output media"
				if media.callID != "" {
					label = fmt.Sprintf(toolOutputMediaAttribution, media.callID)
				}
				content = append(content, map[string]any{"type": "input_text", "text": label})
				lastCallID = media.callID
			}
			content = append(content, map[string]any{"type": "input_image", "image_url": media.imageURL})
		}
		result = append(result, map[string]any{"type": "message", "role": "user", "content": content})
	}
	if !changed {
		return input, false
	}
	return result, true
}

func isResponsesToolOutputItem(item map[string]any) bool {
	switch strings.TrimSpace(stringValue(item["type"])) {
	case "function_call_output", "custom_tool_call_output", "tool_search_output", "tool_search_call_output", "mcp_tool_call_output":
		return true
	default:
		return false
	}
}
