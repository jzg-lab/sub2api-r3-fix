package service

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ccVersionInBillingRe matches the semver part of cc_version (X.Y.Z).
var ccVersionInBillingRe = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+`)

// ccVersionWithFingerprintInBillingRe matches cc_version with its message-derived
// fingerprint suffix (X.Y.Z.fff, 3 hex chars)。
var ccVersionWithFingerprintInBillingRe = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+\.[0-9a-fA-F]{3}\b`)

// effectiveBillingUserAgent 返回 billing attribution block 的 cc_version 应当对齐的
// "实际生效" User-Agent。（上游 v0.2.6 移植）
//
// 为什么不能直接用 fingerprint.UserAgent：OAuth 账号开 mimicClaudeCode 时，后续
// applyClaudeCodeMimicHeaders 会把 UA 强制覆写成 claude.DefaultHeaders["User-Agent"]，
// 覆写发生在 syncBillingHeaderVersion 之后——甚至在没有指纹（fingerprint == nil）时
// 也会发生。若 billing 侧只认 fingerprint.UserAgent，wire 上是默认 UA、body 里却是
// 旧指纹版本，头体版本不一致正是上游判第三方的信号之一。
func effectiveBillingUserAgent(tokenType string, mimicClaudeCode bool, fingerprint *Fingerprint) string {
	if tokenType == "oauth" && mimicClaudeCode {
		return claude.DefaultHeaders["User-Agent"]
	}
	if fingerprint == nil {
		return ""
	}
	return fingerprint.UserAgent
}

// syncBillingHeaderVersion rewrites cc_version in x-anthropic-billing-header
// system text blocks to match the version extracted from userAgent.
// Recomputes any recognized fingerprint suffix because its input includes the
// version (and the possibly-rewritten body) — keeping the client's stale suffix
// would make the attribution block internally inconsistent.
// Only touches system array blocks whose text starts with "x-anthropic-billing-header".
func syncBillingHeaderVersion(body []byte, userAgent string) []byte {
	version := ExtractCLIVersion(userAgent)
	if version == "" {
		return body
	}

	systemResult := gjson.GetBytes(body, "system")
	if !systemResult.Exists() || !systemResult.IsArray() {
		return body
	}

	replacement := "cc_version=" + version
	idx := 0
	systemResult.ForEach(func(_, item gjson.Result) bool {
		text := item.Get("text")
		if text.Exists() && text.Type == gjson.String &&
			strings.HasPrefix(text.String(), "x-anthropic-billing-header") {
			// 带指纹后缀的形态（cc_version=X.Y.Z.fff）整段替换为「新版本 + 按当前 body
			// 重算的后缀」：后缀算法的输入含 version 与 messages 首条 user 文本，二者
			// 任一被网关改写（版本同步、tools/user_id 改写等）后旧后缀即失效。
			fingerprintedReplacement := replacement + "." + computeClaudeCodeFingerprint(body, version)
			newText := ccVersionWithFingerprintInBillingRe.ReplaceAllString(text.String(), fingerprintedReplacement)
			// 纯 semver 形态（无后缀）只替换版本号。
			newText = ccVersionInBillingRe.ReplaceAllString(newText, replacement)
			if newText != text.String() {
				if updated, err := sjson.SetBytes(body, fmt.Sprintf("system.%d.text", idx), newText); err == nil {
					body = updated
				}
			}
		}
		idx++
		return true
	})

	return body
}
