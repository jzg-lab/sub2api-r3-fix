package transport

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
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

// The observer is bounded per SSE event, not per response, and never retains
// model text outside transient parsing buffers. Transport delivery is unchanged.
type responseObserver struct {
	sse       bool
	line      []byte
	data      []byte
	eventType string
	invalid   bool
	completed bool
	failed    bool
	bareError bool
	category  string
	onEvent   func(string, []byte)
}

func (o *responseObserver) write(chunk []byte) {
	if o.invalid {
		return
	}
	if !o.sse {
		if len(o.data)+len(chunk) > 2<<20 {
			o.invalid = true
			o.data = nil
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
			o.invalid = true
			o.line, o.data = nil, nil
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
			o.data = o.data[:0]
			o.eventType = ""
		} else if bytes.HasPrefix(line, []byte("event:")) {
			o.eventType = strings.TrimSpace(string(line[6:]))
		} else if bytes.HasPrefix(line, []byte("data:")) {
			if len(o.data)+len(line) > 2<<20 {
				o.invalid = true
				o.line, o.data = nil, nil
				return
			}
			o.data = append(o.data, bytes.TrimPrefix(line[5:], []byte(" "))...)
			o.data = append(o.data, '\n')
		}
		o.line = o.line[:0]
	}
}

func (o *responseObserver) parse() {
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
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &event) != nil {
		o.invalid = true
		return
	}
	eventType := strings.TrimSpace(event.Type)
	if eventType == "" && o.sse {
		eventType = o.eventType
	}
	if category := admissionEventCategory(raw, eventType); category != "" {
		o.category = category
	}
	switch eventType {
	case "error":
		o.bareError = true
	case "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		o.failed = true
	case "response.completed", "response.done":
		o.completed = event.Response != nil && (event.Response.Status == "" || event.Response.Status == "completed") &&
			(len(event.Response.Error) == 0 || string(event.Response.Error) == "null") &&
			(len(event.Error) == 0 || string(event.Error) == "null")
		if o.completed {
			// Match the host: a later successful terminal can supersede a
			// pending bare error, never an authoritative failed terminal.
			o.bareError = false
		}
	}
	if !o.sse && event.Object == "response" && event.Status == "completed" {
		o.completed = len(event.Error) == 0 || string(event.Error) == "null"
	}
	if o.onEvent != nil {
		o.onEvent(eventType, raw)
	}
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
