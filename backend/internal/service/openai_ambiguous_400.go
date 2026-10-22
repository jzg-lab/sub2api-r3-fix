package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const OpenAIAmbiguous400Reason GatewayFailureReason = "openai_ambiguous_400"
const openAIAmbiguous400RecoveryKey = "openai_ambiguous_400_recovery"

type openAIAmbiguous400Recovery struct {
	replayAllowed bool
	used          atomic.Bool
}

// Initialize before smart-route copies so every account and package shares one chance.
func InitOpenAIAmbiguous400Recovery(c *gin.Context, body []byte) {
	if c == nil {
		return
	}
	if _, exists := c.Get(openAIAmbiguous400RecoveryKey); exists {
		return
	}
	allowed := gjson.GetBytes(body, "previous_response_id").String() == "" &&
		!gjson.GetBytes(body, "conversation").Exists() &&
		!IsExplicitImageGenerationIntent("/v1/responses", gjson.GetBytes(body, "model").String(), body) &&
		!isOpenAIResponsesCompactPath(c)
	c.Set(openAIAmbiguous400RecoveryKey, &openAIAmbiguous400Recovery{replayAllowed: allowed})
}

func openAIAmbiguous400RecoveryFrom(c *gin.Context) *openAIAmbiguous400Recovery {
	if c == nil {
		return nil
	}
	value, _ := c.Get(openAIAmbiguous400RecoveryKey)
	state, _ := value.(*openAIAmbiguous400Recovery)
	return state
}

func OpenAIAmbiguous400RecoveryUsed(c *gin.Context, account *Account) bool {
	if account == nil || !account.IsOpenAIApiKey() {
		return false
	}
	state := openAIAmbiguous400RecoveryFrom(c)
	return state != nil && state.used.Load()
}

// Call once per failed forward, after checking output commitment and cancellation.
// After an API-key rescue fails, a different error cannot start another retry.
// OAuth keeps its original recovery even in a mixed-account route.
func OpenAIAmbiguous400RecoveryExhausted(c *gin.Context, account *Account, failure *UpstreamFailoverError) bool {
	if account == nil || !account.IsOpenAIApiKey() {
		return false
	}
	state := openAIAmbiguous400RecoveryFrom(c)
	if state == nil {
		return false
	}
	if state.used.Load() {
		return true
	}
	if failure != nil && failure.Reason == OpenAIAmbiguous400Reason {
		return !state.used.CompareAndSwap(false, true)
	}
	return false
}

func isOpenAIAmbiguous400(account *Account, status int, body []byte) bool {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey || status != http.StatusBadRequest {
		return false
	}
	generic := func(value string) bool { return strings.EqualFold(strings.TrimSpace(value), "Upstream request failed") }
	if !gjson.ValidBytes(body) {
		return generic(string(body))
	}
	matched := false
	for _, prefix := range []string{"", "error.", "response.error."} {
		for _, field := range []string{"code", "param", "detail", "details", "reason", "innererror", "inner_error"} {
			if value := gjson.GetBytes(body, prefix+field); value.Exists() && value.String() != "" {
				return false
			}
		}
		if value := gjson.GetBytes(body, prefix+"type"); value.Exists() && value.String() != "" {
			switch strings.ToLower(strings.TrimSpace(value.String())) {
			case "upstream_error", "server_error":
			default:
				return false
			}
		}
		if value := gjson.GetBytes(body, prefix+"message"); value.Exists() && value.String() != "" {
			if value.Type != gjson.String || !generic(value.String()) {
				return false
			}
			matched = true
		}
	}
	return matched
}

func (s *OpenAIGatewayService) openAIAmbiguous400Failure(c *gin.Context, account *Account, resp *http.Response, body []byte, passthrough bool) *UpstreamFailoverError {
	if !isOpenAIAmbiguous400(account, resp.StatusCode, body) {
		return nil
	}
	state := openAIAmbiguous400RecoveryFrom(c)
	reason := "retry_available"
	ruleService := getBoundErrorPassthroughService(c)
	switch {
	case !account.ShouldHandleErrorCode(resp.StatusCode):
		reason = "account_error_code_policy"
	case ruleService != nil && ruleService.MatchRule(account.Platform, resp.StatusCode, body) != nil:
		reason = "error_passthrough_rule"
	case state == nil || !state.replayAllowed:
		reason = "request_not_replayable"
	case c.Request == nil || c.Request.Context().Err() != nil:
		reason = "client_disconnected"
	case IsResponseCommitted(c):
		reason = "response_committed"
	case state.used.Load():
		reason = "retry_exhausted"
	}
	kind := "http_error"
	if reason == "retry_available" {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
		Passthrough: passthrough, Kind: kind, Reason: string(OpenAIAmbiguous400Reason),
		Message: "Upstream request failed", Detail: openAI400DiagnosticDetail(body, reason),
	})
	if reason != "retry_available" && reason != "retry_exhausted" {
		return nil
	}
	action := NextAccountRetry
	if reason == "retry_exhausted" {
		action = NextAccountStop
	}
	setOpsUpstreamError(c, resp.StatusCode, "Upstream request failed", reason)
	return &UpstreamFailoverError{
		StatusCode: resp.StatusCode, ResponseBody: body, ResponseHeaders: resp.Header.Clone(),
		Reason: OpenAIAmbiguous400Reason, Scope: GatewayFailureScopeRequest, NextAccountAction: action,
	}
}

// Preserve useful rejection metadata without logging prompts or the raw response.
func openAI400DiagnosticDetail(body []byte, decision string) string {
	fields := map[string]string{"decision": decision}
	for _, name := range []string{"code", "type", "param"} {
		for _, prefix := range []string{"error.", "response.error.", ""} {
			value := gjson.GetBytes(body, prefix+name)
			if value.Type == gjson.String && value.String() != "" {
				fields[name] = truncateString(sanitizeUpstreamErrorMessage(value.String()), 256)
				break
			}
		}
	}
	encoded, _ := json.Marshal(fields)
	return string(encoded)
}
