package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Metadata is retained even when upstream body logging is disabled. No text,
// response output, identifiers, headers or arbitrary provider values are copied.
type OpenAIStreamFailureObservation struct {
	EventType             string `json:"event_type"`
	SemanticStatus        int    `json:"semantic_status"`
	ErrorCode             string `json:"error_code"`
	ErrorType             string `json:"error_type"`
	ProtectionUnavailable bool   `json:"protection_unavailable"`
	ClientOutputStarted   *bool  `json:"client_output_started,omitempty"`
	ResponseOutputItems   *int   `json:"response_output_items,omitempty"`
	UsagePresent          bool   `json:"usage_present"`
	InputTokens           *int64 `json:"input_tokens,omitempty"`
	OutputTokens          *int64 `json:"output_tokens,omitempty"`
}

func openAIStreamFailureSymbol(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return ""
	case "server_error", "internal_error", "service_unavailable", "temporarily_unavailable",
		"upstream_error", "api_error", "invalid_request_error", "invalid_request",
		"content_policy", "content_policy_violation", "safety", "policy",
		"authentication_error", "authentication_failed", "unauthorized_error",
		"invalid_api_key", "invalid_token", "token_revoked", "token_invalidated",
		"permission_error", "forbidden", "rate_limit_error", "rate_limit_exceeded",
		"usage_limit_reached", "insufficient_quota", "server_is_overloaded",
		"state_rejection", "admission_rejected":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "other"
	}
}

func newOpenAIStreamFailureObservation(payload []byte, message string, outputStarted *bool, eventTypes ...string) *OpenAIStreamFailureObservation {
	eventType := gjson.GetBytes(payload, "type").String()
	if len(eventTypes) > 0 {
		eventType = eventTypes[0]
	}
	if eventType != "error" && eventType != "response.failed" {
		return nil
	}
	errorObject := gjson.GetBytes(payload, "response.error")
	if !errorObject.IsObject() {
		errorObject = gjson.GetBytes(payload, "error")
	}
	// Some compatible streams use a flat error event.
	if !errorObject.IsObject() && eventType == "error" {
		errorObject = gjson.ParseBytes(payload)
	}
	result := &OpenAIStreamFailureObservation{
		EventType:             eventType,
		SemanticStatus:        openAIStreamFailureStatus(payload, message),
		ErrorCode:             openAIStreamFailureSymbol(errorObject.Get("code").String()),
		ErrorType:             openAIStreamFailureSymbol(errorObject.Get("type").String()),
		ProtectionUnavailable: strings.EqualFold(strings.TrimSpace(message), "response protection is unavailable"),
	}
	if outputStarted != nil {
		value := *outputStarted
		result.ClientOutputStarted = &value
	}
	if output := gjson.GetBytes(payload, "response.output"); output.IsArray() {
		n := len(output.Array())
		result.ResponseOutputItems = &n
	}
	usage := gjson.GetBytes(payload, "response.usage")
	if !usage.IsObject() {
		usage = gjson.GetBytes(payload, "usage")
	}
	result.UsagePresent = usage.IsObject()
	for key, target := range map[string]**int64{
		"input_tokens": &result.InputTokens, "output_tokens": &result.OutputTokens,
	} {
		if value := usage.Get(key); value.Type == gjson.Number && value.Int() >= 0 {
			n := value.Int()
			*target = &n
		}
	}
	return result
}

func openAIStreamExplicitServerFailure(payload []byte, message string) bool {
	// A generic server type cannot override a concrete request/policy refusal.
	if !openAIStreamFailedEventShouldFailover(payload, message) {
		return false
	}
	for _, path := range []string{"response.error.code", "response.error.type", "error.code", "error.type"} {
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String())) {
		case "server_error", "internal_error", "service_unavailable", "temporarily_unavailable":
			return true
		}
	}
	return false
}
