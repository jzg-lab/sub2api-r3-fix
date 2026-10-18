package service

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func transportDiagnosticFixture() map[string]any {
	return map[string]any{
		"time": "2026-10-14T12:00:00+08:00", "level": "WARN",
		"msg": "upstream_transport_failure", "account_id": 42,
		"operation": "forward_body", "error_class": "connection_eof",
		"phase": "response_body", "context_state": "active",
		"proxy_scheme": "socks5h", "elapsed_ms": 12,
	}
}

func TestPluginTransportEvidenceFragmented(t *testing.T) {
	var out bytes.Buffer
	writer := &pluginEvidenceLog{logger: slog.New(slog.NewJSONHandler(&out, nil))}
	line, _ := json.Marshal(transportDiagnosticFixture())
	cut := len(line) / 2
	_, _ = writer.Write(line[:cut])
	if out.Len() != 0 {
		t.Fatal("incomplete diagnostic was emitted")
	}
	_, _ = writer.Write(append(line[cut:], '\n'))
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["error_class"] != "connection_eof" || got["phase"] != "response_body" ||
		got["account_id"] != float64(42) || got["operation"] != "forward_body" ||
		len(got) != 10 {
		t.Fatal("finite diagnostic fields were lost or expanded")
	}
}

func TestPluginTransportEvidenceRejectsUnsafeMetadata(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"extra", "opaque"}, {"error_class", "opaque"}, {"phase", "opaque"},
		{"operation", "opaque"}, {"context_state", "opaque"},
		{"proxy_scheme", "opaque"}, {"account_id", -1}, {"elapsed_ms", -1},
	} {
		t.Run(tc.key, func(t *testing.T) {
			var out bytes.Buffer
			writer := &pluginEvidenceLog{logger: slog.New(slog.NewJSONHandler(&out, nil))}
			event := transportDiagnosticFixture()
			event[tc.key] = tc.value
			line, _ := json.Marshal(event)
			_, _ = writer.Write(append(line, '\n'))
			if out.Len() != 0 {
				t.Fatal("unsupported diagnostic metadata was emitted")
			}
		})
	}
}

func TestPluginTransportEvidenceOversizeAndCancellation(t *testing.T) {
	var out bytes.Buffer
	writer := &pluginEvidenceLog{logger: slog.New(slog.NewJSONHandler(&out, nil))}
	_, _ = writer.Write([]byte(strings.Repeat("x", 2049)))
	_, _ = writer.Write([]byte("\n"))
	event := transportDiagnosticFixture()
	event["error_class"], event["context_state"] = "request_canceled", "canceled"
	line, _ := json.Marshal(event)
	_, _ = writer.Write(append(line, '\n'))
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["level"] != "INFO" || strings.Contains(out.String(), strings.Repeat("x", 32)) {
		t.Fatal("oversized line leaked or cancellation severity changed")
	}
}

func TestPluginBusinessSuccessEvidence(t *testing.T) {
	for _, kind := range []string{"responses_stream", "responses_json", "responses_compact", "untrusted"} {
		for _, extra := range []bool{false, true} {
			var out bytes.Buffer
			writer := &pluginEvidenceLog{logger: slog.New(slog.NewJSONHandler(&out, nil))}
			event := map[string]any{"msg": "plugin_business_succeeded", "account_id": 42, "response_kind": kind}
			if extra {
				event["payload"] = "untrusted"
			}
			line, _ := json.Marshal(event)
			_, _ = writer.Write(append(line, '\n'))
			want := kind != "untrusted" && !extra
			if (out.Len() > 0) != want {
				t.Fatalf("kind=%s extra=%v: unexpected success evidence", kind, extra)
			}
			if want {
				var got map[string]any
				if json.Unmarshal(out.Bytes(), &got) != nil || len(got) != 5 ||
					got["response_kind"] != kind || got["account_id"] != float64(42) {
					t.Fatal("success evidence lost finite fields or expanded")
				}
			}
		}
	}
}
