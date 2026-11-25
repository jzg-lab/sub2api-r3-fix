package service

import (
	"reflect"
	"strings"
	"testing"
)

func TestCleanCaptureStreamRedactsFragmentedKnownSecret(t *testing.T) {
	frames := []string{`{"type":"response.output_text.delta","item_id":"item_1","delta":"prefix-needle"}`, `{"type":"response.output_text.delta","item_id":"item_1","delta":"-secret-suffix"}`}
	got, ok := cleanCaptureStream(nil, frames, "websocket", []string{"needle-secret"})
	if !ok {
		t.Fatal("frames rejected")
	}
	cleaned := got.([]string)
	joined := strings.Join(cleaned, "")
	if strings.Contains(joined, "needle-secret") || !strings.Contains(joined, captureRedaction) {
		t.Fatalf("fragmented secret remained: %#v", cleaned)
	}
}

func TestCleanCaptureStreamRedactsFragmentedBrandAndToolArgs(t *testing.T) {
	frames := []string{`{"type":"response.function_call_arguments.delta","item_id":"call_1","delta":"{\"model\":\"ciy"}`, `{"type":"response.function_call_arguments.delta","item_id":"call_1","delta":"uan.fast\",\"api_key\":\"dummycredential123\"}"}`}
	got, ok := cleanCaptureStream(nil, frames, "websocket", nil)
	if !ok {
		t.Fatal("tool argument frames rejected")
	}
	cleaned := got.([]string)
	joined := strings.Join(cleaned, "")
	if !strings.Contains(joined, "中转") || strings.Contains(joined, "dummycredential123") {
		t.Fatalf("tool args retained sensitive value: %#v", cleaned)
	}
}

func TestCleanCaptureStreamPreservesSSEEventAndCRLF(t *testing.T) {
	raw := []byte(": keep\r\nevent: message\r\ndata: {\"type\":\"response.completed\",\"response\":{\"api_key\":\"dummycredential123\"}}\r\n\r\n")
	got, ok := cleanCaptureStream(raw, nil, "sse", nil)
	if !ok {
		t.Fatal("SSE rejected")
	}
	cleaned := got.(string)
	if !strings.HasPrefix(cleaned, ": keep\r\nevent: message\r\ndata: ") || !strings.HasSuffix(cleaned, "\r\n\r\n") || strings.Contains(cleaned, "dummycredential123") {
		t.Fatalf("bad SSE output %q", cleaned)
	}
}

func TestCleanCaptureStreamPreservesUnchangedFrames(t *testing.T) {
	frames := []string{`{"type":"response.output_text.delta","item_id":"item_1","delta":"hello"}`}
	got, ok := cleanCaptureStream(nil, frames, "websocket", nil)
	if !ok || !reflect.DeepEqual(got, frames) {
		t.Fatalf("got %#v, ok=%v", got, ok)
	}
}

func TestCaptureInterleavedStreamsAndTerminalEcho(t *testing.T) {
	frames := []string{`{"type":"response.output_text.delta","item_id":"one","delta":"before sample-"}`, `{"type":"response.output_text.delta","item_id":"two","delta":"between"}`, `{"type":"response.output_text.delta","item_id":"one","delta":"needle after"}`, `{"type":"response.completed","response":{"api_key":"another value"}}`}
	want := []string{`{"type":"response.output_text.delta","item_id":"one","delta":"before [REDACTED]"}`, frames[1], `{"type":"response.output_text.delta","item_id":"one","delta":" after"}`, `{"type":"response.completed","response":{"api_key":"[REDACTED]"}}`}
	keys := []string{"sample-needle"}
	got, ok := cleanCaptureStream(nil, frames, "websocket", keys)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("interleaved WS mismatch: %v", got)
	}
	encode := func(frames []string) string {
		return "data: " + strings.Join(frames, "\r\n\r\ndata: ") + "\r\n\r\ndata: [DONE]\r\n\r\n"
	}
	got, ok = cleanCaptureStream([]byte(encode(frames)), nil, "sse", keys)
	if !ok || got != encode(want) {
		t.Fatal("interleaved SSE mismatch")
	}
}
