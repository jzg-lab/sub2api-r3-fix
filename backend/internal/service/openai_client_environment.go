package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Only protocol correlation and agent-role fields may cross the OAuth boundary.
// Inference input, tools and their results are outside this metadata envelope.
// Unknown environment telemetry must be reviewed before adding it here.
var codexClientMetadataFields = map[string]bool{
	"installation_id": true, "x-codex-installation-id": true,
	"session_id": true, "session-id": true,
	"thread_id": true, "thread-id": true,
	"turn_id": true, "turn-id": true,
	"window_id": true, "x-codex-window-id": true,
	"x-client-request-id": true, "x-codex-parent-thread-id": true,
	"x-openai-subagent": true,
}

var codexTurnMetadataFields = map[string]bool{
	"installation_id": true, "session_id": true, "thread_id": true,
	"turn_id": true, "window_id": true, "parent_thread_id": true,
	"forked_from_thread_id": true,
}

func sanitizeCodexTurnMetadata(raw string) (string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || fields == nil {
		return "", raw != ""
	}
	for key, value := range fields {
		var text string
		if !codexTurnMetadataFields[key] || gjson.ParseBytes(value).Type != gjson.String ||
			json.Unmarshal(value, &text) != nil {
			delete(fields, key)
		}
	}
	// Preserve bytes when the entire object is already safe and has no duplicate
	// fields. This also makes the final header boundary idempotent.
	count := 0
	gjson.Parse(raw).ForEach(func(_, _ gjson.Result) bool { count++; return true })
	if count == len(fields) {
		return raw, false
	}
	if len(fields) == 0 {
		return "", true
	}
	clean, _ := json.Marshal(fields)
	return string(clean), true
}

func sanitizeCodexClientMetadata(fields map[string]any) bool {
	changed := false
	for key, value := range fields {
		if key == responsesLiteWSMetadataKey {
			raw, ok := value.(string)
			clean := strings.ToLower(strings.TrimSpace(raw))
			if !ok || (clean != "true" && clean != "false") {
				delete(fields, key)
				changed = true
			} else if clean != raw {
				fields[key] = clean
				changed = true
			}
			continue
		}
		if key == openAIWSTurnMetadataHeader {
			raw, ok := value.(string)
			clean, modified := sanitizeCodexTurnMetadata(raw)
			if !ok || clean == "" {
				delete(fields, key)
				changed = true
			} else if modified {
				fields[key] = clean
				changed = true
			}
			continue
		}
		if _, ok := value.(string); !codexClientMetadataFields[key] || !ok {
			delete(fields, key)
			changed = true
		}
	}
	return changed
}

func sanitizeCodexClientEnvironmentMap(body map[string]any, account *Account) bool {
	if account == nil || !account.IsOpenAIOAuthLike() || body == nil {
		return false
	}
	value, exists := body["client_metadata"]
	if !exists {
		return false
	}
	fields, ok := value.(map[string]any)
	if !ok {
		delete(body, "client_metadata")
		return true
	}
	changed := sanitizeCodexClientMetadata(fields)
	if len(fields) == 0 {
		delete(body, "client_metadata")
		return true
	}
	return changed
}

func sanitizeCodexClientEnvironmentRaw(body []byte, account *Account) ([]byte, bool, error) {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return body, false, nil
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body, false, nil
	}
	count := 0
	var metadata gjson.Result
	root.ForEach(func(key, value gjson.Result) bool {
		if key.String() == "client_metadata" {
			metadata = value
			count++
		}
		return true
	})
	if count > 1 {
		return body, false, fmt.Errorf("duplicate client_metadata is not allowed")
	}
	if count == 0 {
		return body, false, nil
	}
	var fields map[string]any
	if metadata.IsObject() {
		if err := json.Unmarshal([]byte(metadata.Raw), &fields); err != nil {
			return body, false, fmt.Errorf("invalid client_metadata")
		}
	}
	cleaned := sanitizeCodexClientMetadata(fields)
	if len(fields) == 0 {
		next, err := sjson.DeleteBytes(body, "client_metadata")
		return next, true, err
	}
	// A map keeps the last duplicate key, unlike gjson/sjson's first-match view.
	fieldCount := 0
	metadata.ForEach(func(_, _ gjson.Result) bool { fieldCount++; return true })
	if !cleaned && fieldCount == len(fields) {
		return body, false, nil
	}
	clean, err := json.Marshal(fields)
	if err != nil {
		return body, false, fmt.Errorf("encode filtered client_metadata: %w", err)
	}
	next, err := sjson.SetRawBytes(body, "client_metadata", clean)
	return next, true, err
}

func sanitizeCodexClientEnvironmentHeaders(headers http.Header) {
	// Noncanonical keys and multiple values cannot hide a second environment.
	for name, values := range headers {
		if !strings.EqualFold(name, openAIWSTurnMetadataHeader) {
			continue
		}
		if len(values) != 1 || name != http.CanonicalHeaderKey(openAIWSTurnMetadataHeader) {
			delete(headers, name)
			continue
		}
		clean, changed := sanitizeCodexTurnMetadata(values[0])
		if clean == "" {
			delete(headers, name)
		} else if changed {
			headers[name] = []string{clean}
		}
	}
}
