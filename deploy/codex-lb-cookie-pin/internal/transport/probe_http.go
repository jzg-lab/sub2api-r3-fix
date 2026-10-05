package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

const maxProbeErrorBody = 16 << 10

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
