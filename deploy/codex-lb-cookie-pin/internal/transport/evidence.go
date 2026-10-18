package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/evidence"
	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
)

func cohort(model, effort, proxy string) evidence.Cohort {
	if len(model) > 80 || strings.IndexFunc(model, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:", r))
	}) >= 0 {
		model = "unrecognized"
	}
	form := "omitted"
	if effort != "" {
		switch effort {
		case "none", "minimal", "low", "medium", "high", "xhigh":
		default:
			effort = "unrecognized"
		}
		form = "explicit-" + effort
	}
	bucket := "direct"
	if proxy != "" {
		bucket = "invalid"
		if u, err := url.Parse(proxy); err == nil && u.Host != "" {
			sum := sha256.Sum256([]byte(u.Scheme + "://" + strings.ToLower(u.Host)))
			bucket = hex.EncodeToString(sum[:12])
		}
	}
	return evidence.Cohort{Model: model, Effort: effort, EffortForm: form, Bucket: bucket}
}

func bodyCohort(body []byte, proxy string) evidence.Cohort {
	var request struct {
		Model     string `json:"model"`
		Reasoning struct {
			Effort json.RawMessage `json:"effort"`
		} `json:"reasoning"`
	}
	if len(body) > 2<<20 || json.Unmarshal(body, &request) != nil {
		return cohort("unrecognized", "unrecognized", proxy)
	}
	if len(request.Reasoning.Effort) == 0 {
		return cohort(request.Model, "", proxy)
	}
	c := cohort(request.Model, "unrecognized", proxy)
	if bytes.Equal(bytes.TrimSpace(request.Reasoning.Effort), []byte("null")) {
		c.Effort, c.EffortForm = "", "explicit-null"
		return c
	}
	var effort string
	if json.Unmarshal(request.Reasoning.Effort, &effort) != nil {
		return c
	}
	if effort == "" {
		c.Effort, c.EffortForm = "", "explicit-empty"
		return c
	}
	return cohort(request.Model, effort, proxy)
}

func (s *Server) evidenceError(err error) {
	if err != nil && !s.evidenceFailed.Swap(true) {
		// Do not include filesystem paths or upstream payloads.
		slog.Error("plugin_evidence_unavailable")
	}
}

// Caller holds probeMu. Restore only account-level failure protection, never
// identity-dependent passing evidence, templates or cookie generations.
func (s *Server) restoreProtectionLocked(id int64, state *prober.State) bool {
	saved, err := s.evidence.Read(id)
	if err != nil {
		s.evidenceError(err)
		return false
	}
	state.RetryNotBefore = maxTime(state.RetryNotBefore, saved.NotBefore)
	state.BackoffUntil = maxTime(state.BackoffUntil, saved.QualityUntil)
	state.ConsecFails = max(state.ConsecFails, saved.ConsecutiveFails)
	return (saved.Operation.Phase != "unknown" || saved.Operation.Reconciled) &&
		saved.Operation.Phase != "reserved" && saved.Operation.Phase != "dispatched"
}

func (s *Server) saveProtectionLocked(state *prober.State) {
	s.evidenceError(s.evidence.Policy(state.AccountID, state.ConsecFails,
		state.BackoffUntil, state.RetryNotBefore, time.Now()))
}

// Inspect error fields only. A model discussing these signatures is not an
// upstream rejection. Every returned value is from this finite allowlist.
func admissionCategory(raw []byte) string {
	return admissionEventCategory(raw, "")
}

func admissionEventCategory(raw []byte, eventType string) string {
	var envelope struct {
		Type     string          `json:"type"`
		Code     string          `json:"code"`
		Message  string          `json:"message"`
		Error    json.RawMessage `json:"error"`
		Response *struct {
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return ""
	}
	if strings.TrimSpace(envelope.Type) == "" {
		envelope.Type = eventType
	}
	errorRaw := envelope.Error
	if envelope.Response != nil {
		errorRaw = envelope.Response.Error
	}
	var fields struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if len(errorRaw) > 0 && string(errorRaw) != "null" {
		if json.Unmarshal(errorRaw, &fields) != nil {
			return ""
		}
	} else if envelope.Type == "error" {
		fields.Code, fields.Type, fields.Message = envelope.Code, envelope.Type, envelope.Message
	} else {
		return ""
	}
	code := strings.ToLower(fields.Code + " " + fields.Type)
	message := strings.ToLower(fields.Message)
	switch {
	case strings.Contains(message, "turn-state ticket"), strings.Contains(message, "freshly validated ticket"):
		return "turn_state_ticket"
	case strings.Contains(code, "state_rejection"):
		return "state_rejection"
	case strings.Contains(code, "admission_reject"), strings.Contains(code, "admission_den"),
		strings.Contains(code, "admission_error"), strings.Contains(message, "session admission"),
		strings.Contains(message, "turn admission"), strings.Contains(message, "admission rejected"):
		return "admission_rejected"
	default:
		return ""
	}
}

func (s *Server) observeAdmission(id int64, c evidence.Cohort, category string) {
	if category == "" || id <= 0 {
		return
	}
	alert, err := s.evidence.Signal(id, c, category, time.Now())
	s.evidenceError(err)
	if alert {
		slog.Warn("plugin_upstream_admission_rejected", "account_id", id,
			"category", category, "window_seconds", 600)
	}
}

func responsesRequest(method, rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(method, "POST") &&
		(strings.HasSuffix(u.Path, "/responses") || strings.HasSuffix(u.Path, "/responses/compact"))
}

func compactRequest(method, rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(method, "POST") && strings.HasSuffix(u.Path, "/responses/compact")
}

// The observer is bounded per SSE event, not per response, and never retains
// model text outside transient parsing buffers. Transport delivery is unchanged.
type responseObserver struct {
	sse             bool
	sniffSSE        bool
	inferredSSE     bool
	compact         bool
	line            []byte
	data            []byte
	eventType       string
	invalid         bool
	invalidCause    responseInvalidCause
	completed       bool
	failed          bool
	bareError       bool
	errorStatus     int
	bareErrorStatus int
	quotaUntil      time.Time
	bareQuotaUntil  time.Time
	category        string
	onEvent         func(string, []byte)
}

type responseInvalidCause uint8

const (
	responseInvalidUnknown responseInvalidCause = iota
	responseInvalidBodyLimit
	responseInvalidLineLimit
	responseInvalidEventLimit
	responseInvalidJSONSyntax
	responseInvalidJSONSchema
)

func (o *responseObserver) invalidate(cause responseInvalidCause) {
	if !o.invalid {
		o.invalidCause = cause
	}
	o.invalid = true
	o.line, o.data = nil, nil
}

func (o *responseObserver) write(chunk []byte) {
	if o.invalid {
		return
	}
	// Decide from a bounded physical-line prefix, before JSON string contents
	// could be confused with SSE fields. Forwarding retains the original bytes.
	for o.sniffSSE && len(chunk) > 0 {
		o.data = append(o.data, chunk[0])
		chunk = chunk[1:]
		prefix := bytes.TrimLeft(o.data, "\r\n")
		possible := len(prefix) == 0
		for _, field := range []string{"data:", "event:", ":", "id:", "retry:"} {
			if bytes.HasPrefix(prefix, []byte(field)) {
				o.sse, o.inferredSSE, o.sniffSSE = true, true, false
				buffered := o.data
				o.data = nil
				o.write(buffered)
				break
			}
			possible = possible || bytes.HasPrefix([]byte(field), prefix)
		}
		if !possible || len(o.data) >= 64 {
			o.sniffSSE = false
		}
	}
	if !o.sse {
		if len(o.data)+len(chunk) > 2<<20 {
			o.invalidate(responseInvalidBodyLimit)
			return
		}
		o.data = append(o.data, chunk...)
		return
	}
	for len(chunk) > 0 {
		n := bytes.IndexByte(chunk, '\n')
		if n < 0 {
			n = len(chunk)
		}
		if len(o.line)+n > 2<<20 {
			o.invalidate(responseInvalidLineLimit)
			return
		}
		o.line = append(o.line, chunk[:n]...)
		chunk = chunk[n:]
		if len(chunk) == 0 {
			return
		}
		chunk = chunk[1:]
		line := bytes.TrimSuffix(o.line, []byte{'\r'})
		if len(line) == 0 {
			o.parse()
			if o.invalid {
				return
			}
			o.data = o.data[:0]
			o.eventType = ""
		} else if bytes.HasPrefix(line, []byte("event:")) {
			o.eventType = strings.TrimSpace(string(line[6:]))
		} else if bytes.HasPrefix(line, []byte("data:")) {
			if len(o.data)+len(line) > 2<<20 {
				o.invalidate(responseInvalidEventLimit)
				return
			}
			o.data = append(o.data, bytes.TrimPrefix(line[5:], []byte(" "))...)
			o.data = append(o.data, '\n')
		}
		o.line = o.line[:0]
	}
}

func (o *responseObserver) parse() {
	if o.invalid {
		return
	}
	raw := bytes.TrimSpace(o.data)
	if len(raw) == 0 || bytes.Equal(raw, []byte("[DONE]")) {
		return
	}
	var event struct {
		Type     string `json:"type"`
		Object   string `json:"object"`
		Status   string `json:"status"`
		Response *struct {
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"response"`
		Error  json.RawMessage `json:"error"`
		Output []struct {
			Type             string `json:"type"`
			Status           string `json:"status"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		cause := responseInvalidUnknown
		var syntax *json.SyntaxError
		var schema *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax):
			cause = responseInvalidJSONSyntax
		case errors.As(err, &schema):
			cause = responseInvalidJSONSchema
		}
		o.invalidate(cause)
		return
	}
	eventType := strings.TrimSpace(event.Type)
	if eventType == "" && o.sse {
		eventType = o.eventType
	}
	if category := admissionEventCategory(raw, eventType); category != "" {
		o.category = category
	}
	errorRaw := event.Error
	if event.Response != nil && len(event.Response.Error) > 0 && string(event.Response.Error) != "null" {
		errorRaw = event.Response.Error
	}
	if (len(errorRaw) == 0 || string(errorRaw) == "null") && eventType == "error" {
		errorRaw = raw
	}
	var quotaUntil time.Time
	if len(errorRaw) > 0 && string(errorRaw) != "null" {
		quotaUntil = probeQuotaErrorDeadline(errorRaw, time.Now())
	}
	switch eventType {
	case "error":
		o.bareError = true
		o.bareErrorStatus = strongerFailureStatus(o.bareErrorStatus, responseErrorStatus(raw))
		o.bareQuotaUntil = maxTime(o.bareQuotaUntil, quotaUntil)
	case "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		o.failed = true
		o.errorStatus = strongerFailureStatus(o.errorStatus, responseErrorStatus(raw))
		o.quotaUntil = maxTime(o.quotaUntil, quotaUntil)
	case "response.completed", "response.done":
		o.completed = event.Response != nil && (event.Response.Status == "" || event.Response.Status == "completed") &&
			(len(event.Response.Error) == 0 || string(event.Response.Error) == "null") &&
			(len(event.Error) == 0 || string(event.Error) == "null")
		if o.completed {
			// Match the host: a later successful terminal can supersede a
			// pending bare error, never an authoritative failed terminal.
			o.bareError = false
			o.bareErrorStatus = 0
			o.bareQuotaUntil = time.Time{}
		} else if event.Response != nil && (event.Response.Status == "failed" ||
			event.Response.Status == "incomplete" || event.Response.Status == "cancelled" ||
			event.Response.Status == "canceled" ||
			len(event.Response.Error) > 0 && string(event.Response.Error) != "null") ||
			len(event.Error) > 0 && string(event.Error) != "null" {
			o.failed = true
			o.errorStatus = strongerFailureStatus(o.errorStatus, responseErrorStatus(raw))
			o.quotaUntil = maxTime(o.quotaUntil, quotaUntil)
		}
	}
	if o.compact {
		// Compact uses a JSON response.compaction object, not an SSE terminal.
		// Do not allow this shape to certify an ordinary Responses request.
		o.completed = false
		if !o.sse && event.Object == "response.compaction" &&
			(event.Status == "" || event.Status == "completed") &&
			(len(event.Error) == 0 || string(event.Error) == "null") {
			compactions := 0
			valid := true
			for _, item := range event.Output {
				if item.Type == "compaction" {
					compactions++
					valid = valid && strings.TrimSpace(item.EncryptedContent) != "" &&
						(item.Status == "" || item.Status == "completed")
				}
			}
			o.completed = valid && compactions > 0
		}
	} else if !o.sse && event.Object == "response" && event.Status == "completed" {
		o.completed = len(event.Error) == 0 || string(event.Error) == "null"
	}
	if !o.sse && (event.Status == "failed" || event.Status == "incomplete" ||
		event.Status == "cancelled" || event.Status == "canceled" ||
		len(event.Error) > 0 && string(event.Error) != "null") {
		o.failed = true
		o.errorStatus = strongerFailureStatus(o.errorStatus, responseErrorStatus(raw))
		o.quotaUntil = maxTime(o.quotaUntil, quotaUntil)
	}
	if o.onEvent != nil {
		o.onEvent(eventType, raw)
	}
}

// A received terminal rejection is not an ambiguous dispatched operation. Do
// not infer retry eligibility or authentication state from error prose.
func (o *responseObserver) knownFailure() bool {
	return !o.invalid && (o.failed || o.bareError)
}

func (o *responseObserver) failureStatus() int {
	if !o.knownFailure() {
		return 0
	}
	status := o.errorStatus
	if o.bareError {
		status = strongerFailureStatus(status, o.bareErrorStatus)
	}
	return status
}

func (o *responseObserver) failureRetryDeadline() time.Time {
	if o.failureStatus() != http.StatusTooManyRequests {
		return time.Time{}
	}
	if o.bareError {
		return maxTime(o.quotaUntil, o.bareQuotaUntil)
	}
	return o.quotaUntil
}

func strongerFailureStatus(current, next int) int {
	if current == http.StatusUnauthorized || current == http.StatusForbidden {
		return current
	}
	if next == http.StatusUnauthorized || next == http.StatusForbidden {
		return next
	}
	if current == http.StatusTooManyRequests {
		return current
	}
	if next != 0 {
		return next
	}
	return current
}

func responseErrorStatus(raw []byte) int {
	type failure struct {
		Code string `json:"code"`
		Type string `json:"type"`
	}
	var event struct {
		Code     string   `json:"code"`
		Type     string   `json:"type"`
		Error    *failure `json:"error"`
		Response *struct {
			Error *failure `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return 0
	}
	fields := event.Error
	if event.Response != nil && event.Response.Error != nil {
		fields = event.Response.Error
	}
	if fields == nil {
		fields = &failure{Code: event.Code, Type: event.Type}
	}
	status := 0
	for _, value := range []string{fields.Code, fields.Type} {
		if isProbeQuotaError(value) {
			status = strongerFailureStatus(status, http.StatusTooManyRequests)
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "invalid_api_key", "api_key_disabled", "unauthorized", "authentication_error",
			"authentication_failed", "unauthorized_error", "invalid_token", "token_expired",
			"token_revoked", "token_invalidated", "session_expired", "refresh_token_reused":
			status = strongerFailureStatus(status, http.StatusUnauthorized)
		case "permission_error", "permission_denied", "forbidden":
			status = strongerFailureStatus(status, http.StatusForbidden)
		case "server_overloaded", "overloaded_error", "service_unavailable":
			status = strongerFailureStatus(status, http.StatusServiceUnavailable)
		}
	}
	return status
}

func forEachResponseSSE(raw []byte, visit func(string, []byte)) {
	o := responseObserver{sse: true, onEvent: visit}
	o.write(raw)
}

func (o *responseObserver) finish() bool {
	// A final SSE record requires its terminating blank line. EOF after a
	// partial event cannot certify completion.
	if !o.sse {
		o.parse()
	}
	return o.completed && !o.failed && !o.bareError && !o.invalid && len(o.line) == 0 && (len(o.data) == 0 || !o.sse)
}

func (o *responseObserver) settledSSE() bool {
	return o.sse && !o.invalid && len(o.line) == 0 && len(o.data) == 0 &&
		(o.failed || o.completed && !o.bareError)
}

// Finite diagnostics only: never retain response text, IDs or error messages.
func (o *responseObserver) incompleteReason() string {
	switch {
	case o.invalid:
		switch o.invalidCause {
		case responseInvalidBodyLimit:
			return "json_body_limit"
		case responseInvalidLineLimit:
			return "sse_line_limit"
		case responseInvalidEventLimit:
			return "sse_event_limit"
		case responseInvalidJSONSyntax:
			return "json_syntax"
		case responseInvalidJSONSchema:
			return "json_schema"
		default:
			return "invalid_event"
		}
	case o.failed || o.bareError:
		return "failed_event"
	case len(o.line) != 0 || o.sse && len(o.data) != 0:
		return "partial_event"
	case o.completed:
		return "delivery_interrupted"
	case !o.sse:
		return "unrecognized_json"
	default:
		return "missing_terminal"
	}
}
