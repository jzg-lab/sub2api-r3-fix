package service

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type geminiResponseSignalKind int

const (
	geminiSignalNone geminiResponseSignalKind = iota
	geminiSignalPromptBlocked
	geminiSignalContentFilter
	geminiSignalAbnormalStop
	geminiSignalError
)

const (
	geminiSignalEmptyStreamReason   = "EMPTY_STREAM"
	geminiSignalEmptyResponseReason = "EMPTY_RESPONSE"
)

type geminiResponseSignal struct {
	Kind    geminiResponseSignalKind
	Reason  string
	Message string
	Status  int
	Detail  string
}

func detectGeminiResponseSignal(payload []byte) (geminiResponseSignal, bool) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || !bytes.Contains(payload, []byte(`"error"`)) &&
		!bytes.Contains(payload, []byte(`"promptFeedback"`)) && !bytes.Contains(payload, []byte(`"finishReason"`)) ||
		!gjson.ValidBytes(payload) {
		return geminiResponseSignal{}, false
	}
	if inner := gjson.GetBytes(payload, "response"); inner.Exists() && inner.IsObject() {
		payload = []byte(inner.Raw)
	}
	if errObj := gjson.GetBytes(payload, "error"); errObj.IsObject() {
		status := int(errObj.Get("code").Int())
		reason := strings.ToUpper(strings.TrimSpace(errObj.Get("status").String()))
		if status < 400 || status > 599 {
			status = geminiGoogleStatusToHTTPStatus(reason)
		}
		message := strings.TrimSpace(errObj.Get("message").String())
		if message == "" {
			message = "Gemini upstream returned an error event"
		}
		if reason == "" {
			reason = "UPSTREAM_ERROR"
		}
		return geminiResponseSignal{Kind: geminiSignalError, Reason: reason, Message: message, Status: status, Detail: truncateString(string(payload), 2048)}, true
	}
	blockReason := strings.ToUpper(strings.TrimSpace(gjson.GetBytes(payload, "promptFeedback.blockReason").String()))
	if blockReason != "" && blockReason != "BLOCKED_REASON_UNSPECIFIED" {
		return geminiResponseSignal{Kind: geminiSignalPromptBlocked, Reason: blockReason, Message: fmt.Sprintf("Gemini content policy block (blockReason=%s): prompt was blocked before generation", blockReason), Status: http.StatusBadRequest}, true
	}
	candidate := geminiPrimaryCandidate(payload)
	finishReason := strings.ToUpper(strings.TrimSpace(candidate.Get("finishReason").String()))
	if !isGeminiContentFilterFinishReason(finishReason) {
		return geminiResponseSignal{}, false
	}
	detail := strings.TrimSpace(candidate.Get("finishMessage").String())
	if detail == "" {
		detail = geminiFinishReasonText(finishReason)
	}
	return geminiResponseSignal{Kind: geminiSignalContentFilter, Reason: finishReason, Message: fmt.Sprintf("Gemini content policy stop (finishReason=%s): %s", finishReason, truncateString(detail, 512)), Status: http.StatusBadRequest}, true
}

func geminiPrimaryCandidate(payload []byte) gjson.Result {
	candidates := gjson.GetBytes(payload, "candidates")
	var primary gjson.Result
	if candidates.IsArray() {
		candidates.ForEach(func(_, candidate gjson.Result) bool {
			if index := candidate.Get("index"); index.Exists() && index.Int() != 0 {
				return true
			}
			primary = candidate
			return false
		})
	}
	return primary
}

func detectGeminiResponseSignalInBody(body []byte) (geminiResponseSignal, bool) {
	body = bytes.TrimSpace(body)
	if !gjson.ValidBytes(body) {
		return geminiResponseSignal{}, false
	}
	parsed := gjson.ParseBytes(body)
	if !parsed.IsArray() {
		return detectGeminiResponseSignal(body)
	}
	var best geminiResponseSignal
	parsed.ForEach(func(_, item gjson.Result) bool {
		if signal, ok := detectGeminiResponseSignal([]byte(item.Raw)); ok && signal.Kind > best.Kind {
			best = signal
		}
		return true
	})
	return best, best.Kind != geminiSignalNone
}

func isGeminiEmptyResponseBody(body []byte) bool {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return true
	}
	if !gjson.ValidBytes(body) {
		return false
	}
	parsed := gjson.ParseBytes(body)
	if inner := parsed.Get("response"); inner.IsObject() {
		parsed = inner
	}
	if parsed.IsArray() {
		empty := true
		parsed.ForEach(func(_, item gjson.Result) bool {
			empty = isGeminiEmptyResponseItem(item)
			return empty
		})
		return empty
	}
	return isGeminiEmptyResponseItem(parsed)
}

func isGeminiEmptyResponseItem(item gjson.Result) bool {
	if !item.IsObject() {
		return false
	}
	if inner := item.Get("response"); inner.IsObject() {
		item = inner
	}
	if len(item.Map()) == 0 {
		return true
	}
	candidates := item.Get("candidates")
	return candidates.IsArray() && len(candidates.Array()) == 0 && !item.Get("promptFeedback").Exists()
}

func isGeminiContentFilterFinishReason(reason string) bool {
	switch reason {
	case "SAFETY", "RECITATION", "LANGUAGE", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return true
	default:
		return false
	}
}

func geminiFinishReasonText(reason string) string {
	switch reason {
	case "SAFETY":
		return "Response stopped due to safety reasons."
	case "RECITATION":
		return "Response stopped due to recitation policy."
	case "LANGUAGE":
		return "Response stopped due to unsupported language."
	case "BLOCKLIST":
		return "Response stopped due to forbidden terms."
	case "PROHIBITED_CONTENT":
		return "Response stopped due to prohibited content."
	case "SPII":
		return "Response stopped due to sensitive personally identifiable information."
	case "IMAGE_SAFETY":
		return "Response stopped due to image safety violations."
	case "IMAGE_PROHIBITED_CONTENT":
		return "Response stopped due to prohibited image content."
	default:
		return "Response stopped."
	}
}

func geminiGoogleStatusToHTTPStatus(status string) int {
	switch status {
	case "INVALID_ARGUMENT", "FAILED_PRECONDITION", "OUT_OF_RANGE":
		return http.StatusBadRequest
	case "UNAUTHENTICATED":
		return http.StatusUnauthorized
	case "PERMISSION_DENIED":
		return http.StatusForbidden
	case "NOT_FOUND":
		return http.StatusNotFound
	case "RESOURCE_EXHAUSTED":
		return http.StatusTooManyRequests
	case "INTERNAL", "UNKNOWN", "DATA_LOSS":
		return http.StatusInternalServerError
	case "UNAVAILABLE":
		return http.StatusServiceUnavailable
	case "DEADLINE_EXCEEDED":
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

func geminiSignalOpsErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "upstream_error"
	}
}

func (s *GeminiMessagesCompatService) markGeminiResponseSignal(c *gin.Context, account *Account, signal geminiResponseSignal, stream bool, requestID string) {
	if c == nil || signal.Kind == geminiSignalNone {
		return
	}
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(signal.Message))
	if signal.Kind == geminiSignalPromptBlocked || signal.Kind == geminiSignalContentFilter {
		MarkOpsStreamErrorValue(c, OpsStreamError{ErrType: "invalid_request_error", Code: signal.Reason, Message: message, IntendedStatus: signal.Status, RequestScoped: true, NonStream: !stream})
		return
	}
	setOpsUpstreamError(c, signal.Status, message, "")
	kind := "http_error"
	if stream {
		kind = "stream_failed"
	}
	event := OpsUpstreamErrorEvent{Platform: PlatformGemini, UpstreamStatusCode: signal.Status, UpstreamRequestID: strings.TrimSpace(requestID), Kind: kind, Message: message}
	if account != nil {
		event.Platform, event.AccountID, event.AccountName = account.Platform, account.ID, account.Name
	}
	appendOpsUpstreamError(c, event)
	MarkOpsStreamErrorValue(c, OpsStreamError{ErrType: geminiSignalOpsErrorType(signal.Status), Code: signal.Reason, Message: message, IntendedStatus: signal.Status, CountTowardsSLA: true, NonStream: !stream})
}

func (s *GeminiMessagesCompatService) markGeminiEmptyResponse(c *gin.Context, account *Account, stream bool, requestID string) {
	reason, message := geminiSignalEmptyResponseReason, "Gemini upstream returned an empty response body"
	if stream {
		reason, message = geminiSignalEmptyStreamReason, "Gemini upstream returned an empty stream body"
	}
	s.markGeminiResponseSignal(c, account, geminiResponseSignal{Kind: geminiSignalAbnormalStop, Reason: reason, Message: message, Status: http.StatusBadGateway}, stream, requestID)
}

func (s *GeminiMessagesCompatService) finalizeGeminiSSESignal(c *gin.Context, account *Account, stream bool, requestID string, best geminiResponseSignal, sawData bool, fallback *geminiSSEFallbackBody) {
	if best.Kind != geminiSignalNone {
		s.markGeminiResponseSignal(c, account, best, stream, requestID)
		return
	}
	if sawData || fallback.Truncated() {
		return
	}
	body := fallback.Bytes()
	if len(bytes.TrimSpace(body)) == 0 {
		s.markGeminiEmptyResponse(c, account, stream, requestID)
		return
	}
	if signal, ok := detectGeminiResponseSignalInBody(body); ok {
		s.markGeminiResponseSignal(c, account, signal, stream, requestID)
		return
	}
	if isGeminiEmptyResponseBody(body) {
		s.markGeminiEmptyResponse(c, account, stream, requestID)
	}
}
