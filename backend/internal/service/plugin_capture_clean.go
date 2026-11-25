package service

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const captureRedaction = "[REDACTED]"

var (
	captureBrandMatcher    = regexp.MustCompile(`(?i:ciyuan\.fast|ciyuan)|词元`)
	captureCredentialName  = regexp.MustCompile(`(?i)^(?:(?:x[-_])?api[-_]?key|access[-_]?token|refresh[-_]?token|client[-_]?secret|authorization|proxy-authorization|password|secret|token)$`)
	captureCredentialField = regexp.MustCompile(`(?i)(\b(?:(?:x[-_])?api[-_]?key|access[-_]?token|refresh[-_]?token|client[-_]?secret|password|secret|token)|[?&]key)(["']?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s&;,}"'#]+)`)
	captureBearer          = regexp.MustCompile(`(?i)\b(Bearer[ \t]+)([^\s,;"'&}]+)`)
	captureCredentialToken = regexp.MustCompile(`\b(?:sk-|gh[pousr]_|xox[baprs]-|AIza)[A-Za-z0-9_-]{12,}`)
	captureQueryPair       = regexp.MustCompile(`[?&]([^?&=;#\s"'<>]+)=([^&#;\s"'<>]*)`)
)

func cleanCaptureText(text string, secrets []string) string {
	if strings.Contains(text, "%") {
		text = cleanCaptureEscapedURL(text, secrets)
	}
	// A single pass through known values avoids masking a shorter key and leaving
	// part of a longer one. The caller orders needles longest first.
	for _, key := range secrets {
		if key != "" && strings.Contains(text, key) {
			text = strings.ReplaceAll(text, key, captureRedaction)
		}
	}
	text = captureBearer.ReplaceAllString(text, `${1}`+captureRedaction)
	text = captureCredentialField.ReplaceAllStringFunc(text, func(m string) string {
		indexes := captureCredentialField.FindStringSubmatchIndex(m)
		end := indexes[5]
		value := m[end:]
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			return m[:end+1] + captureRedaction + value[:1]
		}
		return m[:end] + captureRedaction
	})
	text = captureCredentialToken.ReplaceAllString(text, captureRedaction)
	return captureBrandMatcher.ReplaceAllString(text, "中转")
}

// Inspect raw separators before decoding: a %26 inside a value is data.
// Keep the spelling of the parameter name and all unrelated URL components.
func cleanCaptureEscapedURL(text string, secrets []string) string {
	text = captureQueryPair.ReplaceAllStringFunc(text, func(pair string) string {
		name, _, _ := strings.Cut(pair[1:], "=")
		if !strings.Contains(name, "%") {
			return pair
		}
		decoded, err := url.QueryUnescape(name)
		if err == nil && (captureCredentialName.MatchString(decoded) || strings.EqualFold(decoded, "key")) {
			return pair[:len(name)+2] + captureRedaction
		}
		return pair
	})
	if len(secrets) == 0 {
		return text
	}
	// Match in the decoded view, but copy untouched bytes from the original.
	// An unrelated malformed escape must not suppress a valid match later.
	var decoded strings.Builder
	decoded.Grow(len(text))
	for i := 0; i < len(text); {
		b, width := captureURLByte(text[i:])
		decoded.WriteByte(b)
		i += width
	}
	view := decoded.String()
	var out strings.Builder
	raw, last := 0, 0
	for i := 0; i < len(view); {
		width := 0
		for _, key := range secrets {
			if len(key) > width && strings.HasPrefix(view[i:], key) {
				width = len(key)
			}
		}
		if width == 0 {
			_, n := captureURLByte(text[raw:])
			raw += n
			i++
			continue
		}
		out.WriteString(text[last:raw])
		out.WriteString(captureRedaction)
		for range width {
			_, n := captureURLByte(text[raw:])
			raw += n
		}
		i += width
		last = raw
	}
	if last == 0 {
		return text
	}
	out.WriteString(text[last:])
	return out.String()
}

func captureURLByte(raw string) (byte, int) {
	if raw[0] == '%' && len(raw) >= 3 {
		if decoded, err := url.PathUnescape(raw[:3]); err == nil {
			return decoded[0], 3
		}
	}
	return raw[0], 1
}

func cleanCapturePayload(request, response []byte, frames []string, transport string, secrets []string) (string, any, bool) {
	req, ok := cleanCaptureJSONOrText(request, secrets)
	if !ok {
		return "", nil, false
	}
	switch transport {
	case "http":
		if len(frames) > 0 {
			return "", nil, false
		}
		res, ok := cleanCaptureJSONOrText(response, secrets)
		return req, res, ok
	case "sse", "websocket":
		res, ok := cleanCaptureStream(response, frames, transport, secrets)
		return req, res, ok
	default:
		return "", nil, false
	}
}

func cleanCaptureJSONOrText(raw []byte, secrets []string) (string, bool) {
	text := string(raw)
	if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed != "" && strings.ContainsRune(`{"[`, rune(trimmed[0])) {
		if !json.Valid(raw) {
			return "", false
		}
		return cleanCaptureJSON(text, secrets, 0)
	}
	return cleanCaptureText(text, secrets), true
}

// The envelope carries the original JSON as text. Scan validated tokens rather
// than remarshal a map (which would change whitespace, escaping and numbers).
func cleanCaptureJSON(text string, secrets []string, depth int) (string, bool) {
	if depth > 12 {
		return "", false
	}
	var out strings.Builder
	last := 0
	sensitiveValue := false
	for i := 0; i < len(text); {
		if text[i] != '"' {
			i++
			continue
		}
		start := i
		i++
		for i < len(text) && text[i] != '"' {
			if text[i] == '\\' {
				i++
			}
			i++
		}
		i++
		var value string
		if err := json.Unmarshal([]byte(text[start:i]), &value); err != nil {
			return "", false
		}
		j := i
		for j < len(text) && strings.IndexByte(" \r\n\t", text[j]) >= 0 {
			j++
		}
		if j < len(text) && text[j] == ':' {
			// Preserve field names. A secret in a dynamic key cannot safely be retained.
			if cleanCaptureText(value, secrets) != value {
				return "", false
			}
			sensitiveValue = captureCredentialName.MatchString(value)
			if sensitiveValue {
				k := j + 1
				for k < len(text) && strings.IndexByte(" \r\n\t", text[k]) >= 0 {
					k++
				}
				if k >= len(text) || text[k] != '"' {
					return "", false
				}
			}
			continue
		}
		var clean string
		if sensitiveValue {
			clean = captureRedaction
			sensitiveValue = false
		} else {
			trimmed := strings.TrimSpace(value)
			if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid([]byte(trimmed)) {
				var ok bool
				clean, ok = cleanCaptureJSON(value, secrets, depth+1)
				if !ok {
					return "", false
				}
			} else {
				clean = cleanCaptureText(value, secrets)
			}
		}
		if clean != value {
			out.WriteString(text[last:start])
			out.WriteString(replaceCaptureJSONString(text[start:i], value, clean))
			last = i
		}
	}
	if last == 0 {
		return text, true
	}
	out.WriteString(text[last:])
	return out.String(), true
}

// Keep the original escapes surrounding a changed span (including surrogate pairs).
func replaceCaptureJSONString(raw, before, after string) string {
	prefix, suffix := 0, 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	for suffix < len(before)-prefix && suffix < len(after)-prefix && before[len(before)-1-suffix] == after[len(after)-1-suffix] {
		suffix++
	}
	left, right, decodedLeft, decodedRight := 1, len(raw)-1, 0, len(before)
	for i, decoded := 1, 0; i < len(raw)-1; {
		start := i
		_, width := utf8.DecodeRuneInString(raw[i:])
		i += width
		if raw[start] == '\\' {
			i++
			width = 1
			if raw[start+1] == 'u' {
				i += 4
				// encoding/json decodes both lone surrogates and valid pairs.
				var part string
				_ = json.Unmarshal([]byte(`"`+raw[start:i]+`"`), &part)
				if part == "�" && i+6 <= len(raw)-1 && raw[i:i+2] == `\u` {
					var pair string
					_ = json.Unmarshal([]byte(`"`+raw[start:i+6]+`"`), &pair)
					if utf8.RuneCountInString(pair) == 1 {
						part = pair
						i += 6
					}
				}
				width = len(part)
			}
		}
		if decoded >= len(before)-suffix {
			right, decodedRight = start, decoded
			break
		}
		decoded += width
		if decoded <= prefix {
			left, decodedLeft = i, decoded
		}
	}
	replacement, _ := json.Marshal(after[decodedLeft : len(after)-(len(before)-decodedRight)])
	return raw[:left] + string(replacement[1:len(replacement)-1]) + raw[right:]
}
