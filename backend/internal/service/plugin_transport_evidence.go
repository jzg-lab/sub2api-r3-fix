package service

import (
	"bytes"
	"encoding/json"
	"io"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
)

func (w *pluginEvidenceLog) emitBusinessSuccess(raw []byte) {
	var event struct {
		Time    string `json:"time"`
		Level   string `json:"level"`
		Message string `json:"msg"`
		Account int64  `json:"account_id"`
		Kind    string `json:"response_kind"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&event) != nil || decoder.Decode(new(any)) != io.EOF ||
		event.Message != "plugin_business_succeeded" || event.Account <= 0 {
		return
	}
	switch event.Kind {
	case "responses_stream", "responses_json", "responses_compact":
		w.logger.Info("plugin_business_succeeded", "account_id", event.Account, "response_kind", event.Kind)
	}
}

func (w *pluginEvidenceLog) emitTransportFailure(raw []byte) {
	var event struct {
		Time         string `json:"time"`
		Level        string `json:"level"`
		Message      string `json:"msg"`
		Account      int64  `json:"account_id"`
		Operation    string `json:"operation"`
		Class        string `json:"error_class"`
		Phase        string `json:"phase"`
		ContextState string `json:"context_state"`
		ProxyScheme  string `json:"proxy_scheme"`
		ElapsedMS    int64  `json:"elapsed_ms"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&event) != nil || decoder.Decode(new(any)) != io.EOF ||
		event.Message != "upstream_transport_failure" || event.Account <= 0 ||
		event.ElapsedMS < 0 || !pluginv1.ValidTransportFailureClassification(event.Class, event.Phase) {
		return
	}
	switch event.Operation {
	case "forward_connect", "forward_body", "probe_connect", "probe_body", "unknown":
	default:
		return
	}
	switch event.ContextState {
	case "active", "canceled", "deadline", "unknown":
	default:
		return
	}
	switch event.ProxyScheme {
	case "direct", "http", "https", "socks5", "socks5h", "invalid":
	default:
		return
	}
	fields := []any{
		"account_id", event.Account, "operation", event.Operation,
		"error_class", event.Class, "phase", event.Phase,
		"context_state", event.ContextState, "proxy_scheme", event.ProxyScheme,
		"elapsed_ms", event.ElapsedMS,
	}
	if event.Class == "request_canceled" {
		w.logger.Info("upstream_transport_failure", fields...)
		return
	}
	w.logger.Warn("upstream_transport_failure", fields...)
}
