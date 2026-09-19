package service

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const OpenAIOAuthQualifiedProxyExtraKey = "openai_oauth_qualified_proxy_id"

var (
	ErrOpenAIOAuthProxyRequired = infraerrors.BadRequest(
		"OPENAI_OAUTH_PROXY_REQUIRED",
		"browser-authorized OpenAI OAuth accounts require the authorization proxy",
	)
	ErrOpenAIOAuthStableIdentityRequired = infraerrors.BadRequest(
		"OPENAI_OAUTH_STABLE_IDENTITY_REQUIRED",
		"browser-authorized OpenAI OAuth accounts require chatgpt_account_id or email",
	)
	ErrOpenAIOAuthIdentityExists = infraerrors.Conflict(
		"OPENAI_OAUTH_IDENTITY_EXISTS",
		"an active browser-authorized OpenAI OAuth account already uses this identity",
	)
	ErrOpenAIOAuthHistoryBindingMissing = infraerrors.Conflict(
		"OPENAI_OAUTH_HISTORY_BINDING_MISSING",
		"a deleted account with this identity has no qualified authorization proxy binding",
	)
	ErrOpenAIOAuthHistoryBindingConflict = infraerrors.Conflict(
		"OPENAI_OAUTH_HISTORY_BINDING_CONFLICT",
		"deleted accounts with this identity have conflicting qualified authorization proxy bindings",
	)
	ErrOpenAIOAuthProxyInvalid = infraerrors.Conflict(
		"OPENAI_OAUTH_PROXY_INVALID",
		"the OpenAI OAuth authorization proxy is missing, inactive, or expired",
	)
	ErrOpenAIOAuthProxyMismatch = infraerrors.Conflict(
		"OPENAI_OAUTH_PROXY_MISMATCH",
		"the requested OpenAI OAuth proxy does not match the historical qualified authorization proxy",
	)
	ErrOpenAIOAuthProxyBindingProtected = infraerrors.Conflict(
		"OPENAI_OAUTH_PROXY_BINDING_PROTECTED",
		"the OpenAI OAuth authorization proxy is protected and cannot be changed or removed",
	)
	ErrOpenAIOAuthIdentityChanged = infraerrors.Conflict(
		"OPENAI_OAUTH_IDENTITY_CHANGED",
		"the stable identity of a protected OpenAI OAuth account cannot be changed",
	)
	ErrOpenAIOAuthProxyBindingCorrupt = infraerrors.Conflict(
		"OPENAI_OAUTH_PROXY_BINDING_CORRUPT",
		"the stored OpenAI OAuth qualified proxy binding is invalid",
	)
	ErrOpenAIOAuthQualificationRequired = infraerrors.Conflict(
		"OPENAI_OAUTH_QUALIFICATION_REQUIRED",
		"the OpenAI OAuth authorization proxy must pass qualification before scheduling",
	)
)

func IsOpenAIBrowserOAuthAccount(account *Account) bool {
	return account != nil && account.IsOpenAIOAuth() &&
		account.ParentAccountID == nil &&
		!account.IsOpenAIPersonalAccessToken() &&
		!account.IsOpenAIAgentIdentity()
}

func OpenAIOAuthStableIdentity(account *Account) (kind, value string, ok bool) {
	if !IsOpenAIBrowserOAuthAccount(account) {
		return "", "", false
	}
	// email 是席位级唯一身份（同一工作区多个席位共享 chatgpt_account_id，
	// workspace 重复不代表同一账号）；chatgpt_account_id 仅作无 email 时兜底。
	if value = normalizeOpenAIOAuthIdentity(account.GetCredential("email")); value != "" {
		return "email", value, true
	}
	if value = normalizeOpenAIOAuthIdentity(account.GetCredential("chatgpt_account_id")); value != "" {
		return "chatgpt_account_id", value, true
	}
	return "", "", false
}

func normalizeOpenAIOAuthIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func OpenAIOAuthQualifiedProxyID(extra map[string]any) (int64, bool) {
	if extra == nil {
		return 0, false
	}
	value, exists := extra[OpenAIOAuthQualifiedProxyExtraKey]
	if !exists {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		if typed > 0 {
			return int64(typed), true
		}
	case int32:
		if typed > 0 {
			return int64(typed), true
		}
	case int64:
		if typed > 0 {
			return typed, true
		}
	case float64:
		if typed > 0 && typed <= math.MaxInt64 && typed == math.Trunc(typed) {
			return int64(typed), true
		}
	case json.Number:
		if parsed, err := typed.Int64(); err == nil && parsed > 0 {
			return parsed, true
		}
	case string:
		if parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); err == nil && parsed > 0 {
			return parsed, true
		}
	}
	return 0, false
}
