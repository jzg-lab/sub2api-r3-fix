package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Header values are used only as redaction needles, never as record fields.
func captureHeaderSecrets(headers http.Header) []string {
	var result []string
	for name, values := range headers {
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization":
			for _, v := range values {
				_, value, ok := strings.Cut(strings.TrimSpace(v), " ")
				if !ok {
					value = v
				}
				if value = strings.TrimSpace(value); value != "" {
					result = append(result, value)
				}
			}
		case "x-api-key", "api-key", "x-goog-api-key", "anthropic-api-key":
			result = append(result, values...)
		}
	}
	return result
}

func (s *captureSession) emitRawAttempt(request, response []byte, frames []string, transport string, httpStatus int, complete bool, secrets []string, turnModel ...string) {
	if s.manager.captureRuntime.Load() != s.runtime {
		return
	}
	size := int64(len(request)) + int64(len(response))
	for _, frame := range frames {
		size += int64(len(frame)) + 3
	}
	if size > captureRecordMaxBytes {
		return
	}
	// Even after downstream cancellation the archive owns the observed attempt.
	ctx, release := pkghttputil.WithRequestBodyMemoryScope(context.WithoutCancel(s.ctx))
	defer release()
	if pkghttputil.ReserveRequestBodyMemory(ctx, size*16+4096) != nil {
		return
	}
	keys := make([]string, 0, len(secrets)+len(s.clientSecrets))
	keys = append(append(keys, secrets...), s.clientSecrets...)
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	requestText, responseValue, ok := cleanCapturePayload(request, response, frames, transport, keys)
	if !ok {
		slog.Warn("plugin_capture_record_dropped", "reason", "redaction")
		return
	}
	completedAt := time.Now().UTC()
	requestedModel := s.requestedModel
	if len(turnModel) != 0 {
		requestedModel = turnModel[0]
	}
	model := gjson.GetBytes(request, "model").String()
	record, err := json.Marshal(struct {
		SchemaVersion    int       `json:"schema_version"`
		CompletedAt      time.Time `json:"completed_at"`
		Provider         string    `json:"provider"`
		AccountID        int64     `json:"account_id"`
		GroupID          int64     `json:"group_id"`
		RequestedModel   string    `json:"requested_model"`
		Model            string    `json:"model"`
		Transport        string    `json:"transport"`
		HTTPStatus       int       `json:"http_status,omitempty"`
		ResponseComplete bool      `json:"response_complete"`
		Request          string    `json:"request"`
		Response         any       `json:"response"`
	}{2, completedAt, s.provider, s.accountID, s.groupID, cleanCaptureText(requestedModel, keys), cleanCaptureText(model, keys), transport, httpStatus, complete, requestText, responseValue})
	if err != nil || len(record) > captureRecordMaxBytes {
		return
	}
	_, releaseRecord, err := pkghttputil.DetachRequestBodyMemory(ctx, int64(len(record)))
	if err != nil {
		return
	}
	if !s.manager.enqueueCapture(s.runtime, &pluginv1.CaptureRecordRequest{RecordId: uuid.NewString(), InstanceId: "gateway", CompletedAtUnixNano: completedAt.UnixNano(), RecordJson: record}, releaseRecord, sha256.Sum256([]byte(requestedModel))) {
		releaseRecord()
		slog.Warn("plugin_capture_record_dropped", "reason", "queue")
	}
}
