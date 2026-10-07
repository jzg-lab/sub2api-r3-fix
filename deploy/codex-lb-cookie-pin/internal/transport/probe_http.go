package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

const maxProbeErrorBody = 16 << 10

// Rate limits are scheduling signals, not reasons to reroll or retry the
// same request immediately. Even Retry-After: 0 retains a one-minute floor.
func probeRetryDeadline(status int, header http.Header, now time.Time) time.Time {
	return probeRetryDeadlineWithPolicy(status, header, now, pluginv1.ProbeRateLimitPolicy{})
}

func probeRetryDeadlineWithPolicy(status int, header http.Header, now time.Time, policy pluginv1.ProbeRateLimitPolicy) time.Time {
	if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
		return time.Time{}
	}
	raw := strings.TrimSpace(header.Get("Retry-After"))
	var deadline time.Time
	if seconds, ok := retrySeconds(raw); ok {
		deadline = retryResetAfter(seconds, now)
	} else if date, err := http.ParseTime(raw); err == nil {
		deadline = date
	}
	if status == http.StatusTooManyRequests {
		for _, slot := range []string{"primary", "secondary"} {
			prefix := "x-codex-" + slot + "-"
			used, valid := retrySeconds(header.Get(prefix + "used-percent"))
			if !valid || used < 100 {
				continue
			}
			if rawWindow := header.Get(prefix + "window-minutes"); rawWindow != "" {
				if minutes, valid := retrySeconds(rawWindow); !valid || minutes <= 0 {
					continue
				}
			}
			if seconds, valid := retrySeconds(header.Get(prefix + "reset-after-seconds")); valid {
				deadline = maxTime(deadline, retryResetAfter(seconds, now))
			}
		}
	}
	if status == http.StatusTooManyRequests || !deadline.IsZero() {
		if status == http.StatusTooManyRequests && !deadline.After(now) {
			deadline = now.Add(policy.FallbackDuration())
		}
		deadline = maxTime(deadline, policy.NotBefore)
		return maxTime(deadline, now.Add(time.Minute))
	}
	return time.Time{}
}

func retrySeconds(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return value, err == nil && value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func retryResetAfter(seconds float64, now time.Time) time.Time {
	const maximum = time.Duration(1<<63 - 1)
	if seconds >= float64(maximum)/float64(time.Second) {
		return now.Add(maximum)
	}
	return now.Add(time.Duration(seconds * float64(time.Second)))
}

// Read only recognized quota fields from an already bounded error body. The
// message is never persisted and non-quota errors cannot establish a reset.
func probeQuotaBodyDeadline(raw []byte, now time.Time) time.Time {
	if len(raw) > maxProbeErrorBody {
		return time.Time{}
	}
	var envelope struct {
		Error struct {
			Type            string          `json:"type"`
			ResetsAt        json.RawMessage `json:"resets_at"`
			ResetsInSeconds json.RawMessage `json:"resets_in_seconds"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return time.Time{}
	}
	switch envelope.Error.Type {
	case "usage_limit_reached", "rate_limit_exceeded", "GoUsageLimitError":
	default:
		return time.Time{}
	}
	number := func(raw json.RawMessage) (float64, bool) {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return retrySeconds(text)
		}
		return retrySeconds(string(raw))
	}
	var deadline time.Time
	if seconds, valid := number(envelope.Error.ResetsAt); valid {
		maximum := now.Add(time.Duration(1<<63 - 1))
		if seconds >= float64(maximum.Unix()) {
			deadline = maximum
		} else {
			deadline = time.Unix(int64(seconds), 0)
		}
	}
	if seconds, valid := number(envelope.Error.ResetsInSeconds); valid {
		deadline = maxTime(deadline, retryResetAfter(seconds, now))
	}
	return deadline
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Compaction is forwarded unchanged, but its JSON-only endpoint cannot execute
// a streaming quality probe. Both requests belong to the same account route.
func probeEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err == nil && strings.HasSuffix(u.Path, "/responses/compact") {
		u.Path = strings.TrimSuffix(u.Path, "/compact")
		if u.RawPath != "" {
			u.RawPath = strings.TrimSuffix(u.RawPath, "/compact")
		}
		return u.String()
	}
	return endpoint
}

func probeHeaders(original http.Header) http.Header {
	headers := cloneHeader(original)
	// These describe the old body or turn, not the newly generated probe.
	for _, key := range []string{
		"Content-Length", "Content-Encoding", "Content-Md5", "Digest",
		"Transfer-Encoding", "Trailer", "Expect", "Idempotency-Key",
		"X-Codex-Turn-State", "X-Codex-Turn-Metadata",
	} {
		headers.Del(key)
	}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "text/event-stream")
	headers.Set("Accept-Encoding", "identity")
	return headers
}

type probeHTTPFailure struct {
	Status      int
	Kind        string
	Parameter   string
	Fingerprint string
}

func (f probeHTTPFailure) Summary() string {
	result := "http:" + strconv.Itoa(f.Status) + " " + f.Kind
	if f.Parameter != "" {
		result += ":" + f.Parameter
	}
	return result + " e=" + f.Fingerprint
}

// Never serialize an upstream message, code, parameter or header verbatim.
// Unknown replies retain their HTTP status and fingerprint, not guessed causes.
func classifyProbeHTTPError(status int, raw []byte, readErr error) probeHTTPFailure {
	if len(raw) > maxProbeErrorBody {
		raw = raw[:maxProbeErrorBody]
	}
	sum := sha256.Sum256(raw)
	result := probeHTTPFailure{Status: status, Kind: "rejected", Fingerprint: hex.EncodeToString(sum[:6])}
	if readErr != nil {
		result.Kind = "body-unreadable"
		return result
	}
	if len(raw) == 0 {
		result.Kind = "empty-body"
		return result
	}
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	message, code, parameter := "", "", ""
	if json.Unmarshal(raw, &envelope) == nil {
		message = envelope.Message + " " + envelope.Detail
		var upstream struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Param   string `json:"param"`
		}
		if json.Unmarshal(envelope.Error, &upstream) == nil {
			message += " " + upstream.Message
			code, parameter = strings.ToLower(upstream.Code), strings.ToLower(upstream.Param)
		} else {
			var detail string
			if json.Unmarshal(envelope.Error, &detail) == nil {
				message += " " + detail
			}
		}
	} else {
		result.Kind = "non-json"
		return result
	}
	message = strings.ToLower(message)
	knownParameters := []string{
		"reasoning.effort", "reasoning.summary", "max_output_tokens",
		"parallel_tool_calls", "previous_response_id", "content-encoding",
		"instructions", "input", "stream", "store", "model", "reasoning",
		"include", "tools", "temperature", "top_p",
	}
	for _, known := range knownParameters {
		if parameter == known {
			result.Parameter = known
			break
		}
	}
	if result.Parameter == "" {
		for _, known := range knownParameters {
			if hasDiagnosticToken(message, known) {
				result.Parameter = known
				break
			}
		}
	}
	switch {
	case status == http.StatusBadRequest && (code == "client_version_unsupported" ||
		code == "client_version_outdated" || code == "client_upgrade_required" ||
		strings.Contains(message, "please upgrade your codex client") ||
		strings.Contains(message, "please update your codex client") ||
		strings.Contains(message, "codex client version is no longer supported")):
		result.Kind, result.Parameter = "client-upgrade-required", ""
	case code == "model_not_found" || code == "model_not_supported":
		result.Kind, result.Parameter = "model-unavailable", "model"
	case code == "unsupported_parameter" || code == "unsupported_value" ||
		strings.Contains(message, "unsupported") || strings.Contains(message, "not supported") ||
		strings.Contains(message, "unknown parameter") || strings.Contains(message, "unrecognized"):
		result.Kind = "unsupported"
	case code == "missing_required_parameter" || strings.Contains(message, "required") ||
		strings.Contains(message, "missing"):
		result.Kind = "missing"
	case code == "invalid_parameter" || code == "invalid_value" || code == "invalid_header" ||
		strings.Contains(message, "must be") || strings.Contains(message, "invalid"):
		result.Kind = "invalid"
	}
	return result
}

func hasDiagnosticToken(message, token string) bool {
	for offset := 0; offset < len(message); {
		index := strings.Index(message[offset:], token)
		if index < 0 {
			return false
		}
		start, end := offset+index, offset+index+len(token)
		isWord := func(b byte) bool { return unicode.IsLetter(rune(b)) || b >= '0' && b <= '9' || b == '_' }
		if (start == 0 || !isWord(message[start-1])) && (end == len(message) || !isWord(message[end])) {
			return true
		}
		offset = end
	}
	return false
}
