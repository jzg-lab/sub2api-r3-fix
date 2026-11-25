package service

import (
	"reflect"
	"strings"
	"testing"
)

func TestCleanCapturePayloadPreservesUnchangedJSON(t *testing.T) {
	request := []byte("{ \"model\" : \"gpt\\u002d4\", \"prompt\":\"hello\" }\n")
	got, response, ok := cleanCapturePayload(request, []byte(`{"ok":true}`), nil, "http", nil)
	if !ok || got != string(request) || response != `{"ok":true}` {
		t.Fatalf("cleanCapturePayload() = (%q, %#v, %v)", got, response, ok)
	}
}

func TestCleanCapturePayloadRedactsSecretsAndNestedJSON(t *testing.T) {
	request := []byte(`{"authorization":"Bearer abcdefghijklmnop","nested":"{\"api_key\":\"known-secret\"}"}`)
	got, _, ok := cleanCapturePayload(request, []byte("upstream failed with known-secret"), nil, "http", []string{"known-secret"})
	if !ok {
		t.Fatal("valid request was rejected")
	}
	for _, secret := range []string{"abcdefghijklmnop", "known-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("request retained secret %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, `"nested"`) || !strings.Contains(got, captureRedaction) {
		t.Fatalf("nested JSON was not retained and redacted: %s", got)
	}
}

func TestCleanCapturePayloadPreservesSSEAndWebSocketFrames(t *testing.T) {
	frames := []string{"data: {\"text\":\"ordinary\"}\n\n", "data: {\"text\":\"sk-live-12345678901234567890\"}\n\n"}
	_, cleaned, ok := cleanCapturePayload([]byte(`{}`), []byte(strings.Join(frames, "")), nil, "sse", nil)
	if !ok {
		t.Fatal("SSE frames were rejected")
	}
	want := frames[0] + "data: {\"text\":\"[REDACTED]\"}\n\n"
	if !reflect.DeepEqual(cleaned, want) {
		t.Fatalf("cleaned SSE = %#v, want %#v", cleaned, want)
	}
	_, cleaned, ok = cleanCapturePayload([]byte(`{}`), nil, []string{`{"type":"response.output_text.delta","delta":"ordinary"}`}, "websocket", nil)
	if !ok || !reflect.DeepEqual(cleaned, []string{`{"type":"response.output_text.delta","delta":"ordinary"}`}) {
		t.Fatalf("cleaned websocket = %#v, ok=%v", cleaned, ok)
	}
}

func TestCaptureTextRedaction(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"model?api_key=demo", "model?api_key=[REDACTED]"},
		{"Authorization: Bearer demo", "Authorization: Bearer [REDACTED]"},
		{"https://CIYUAN.FAST/ciyuan/词元", "https://中转/中转/中转"},
		{"gpt-4", "gpt-4"},
	} {
		if got := cleanCaptureText(tc.text, nil); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestCleanCapturePayloadRejectsMalformedStructuredInput(t *testing.T) {
	if _, _, ok := cleanCapturePayload([]byte(`{"api_key":"secret"`), nil, nil, "http", nil); ok {
		t.Fatal("malformed JSON was accepted")
	}
}
