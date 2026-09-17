package repository

// in-vitro 裁决测试(诊断用):用生产同款 uTLS transport(档案来自 DB 行)
// 打一发资格探针 body,对照不同 ALPN 配置下的上游行为差异。
// 双 env 门控:PROBE_REPRO_DIR(含 body.json/meta.txt/profile.json) + PROBE_REPRO_TOKEN。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
)

func TestTLSFingerprintProbeRepro(t *testing.T) {
	dir := os.Getenv("PROBE_REPRO_DIR")
	token := os.Getenv("PROBE_REPRO_TOKEN")
	if dir == "" || token == "" {
		t.Skip("tls repro not requested")
	}
	proxyURL := os.Getenv("PROBE_REPRO_PROXY")
	if proxyURL == "" {
		proxyURL = "http://127.0.0.1:17901"
	}
	alpnOverride := os.Getenv("PROBE_REPRO_ALPN") // "asis"=按档案原样;"h2,http/1.1"=覆盖
	if alpnOverride == "" {
		alpnOverride = "asis"
	}
	rawProfile, err := os.ReadFile(dir + "/profile.json")
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	var mp model.TLSFingerprintProfile
	if err := json.Unmarshal(rawProfile, &mp); err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	profile := mp.ToTLSProfile()
	if alpnOverride != "asis" {
		profile.ALPNProtocols = strings.Split(alpnOverride, ",")
	}
	body, err := os.ReadFile(dir + "/body.json")
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	parsed, _ := url.Parse(proxyURL)
	var transport http.RoundTripper
	if os.Getenv("PROBE_REPRO_NATIVE") == "1" {
		// 原生臂:与生产 Do() 同款 openai_h2 传输(无 uTLS)
		native, terr := buildUpstreamTransport(poolSettings{}, parsed, upstreamProtocolModeOpenAIH2)
		if terr != nil {
			t.Fatalf("build native transport: %v", terr)
		}
		transport = native
		defer native.CloseIdleConnections()
	} else {
		fp, terr := buildUpstreamTransportWithTLSFingerprint(poolSettings{}, parsed, profile)
		if terr != nil {
			t.Fatalf("build transport: %v", terr)
		}
		transport = fp
		defer fp.CloseIdleConnections()
	}
	client := &http.Client{Transport: transport}

	// header 集一次性解析,每针重建请求(body 需全新 reader)
	meta, err := os.Open(dir + "/meta.txt")
	if err != nil {
		t.Fatalf("open meta: %v", err)
	}
	defer meta.Close()
	var headerLines []string
	scanner := bufio.NewScanner(meta)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "H ") {
			headerLines = append(headerLines, strings.TrimPrefix(line, "H "))
		}
	}

	fireProbe := func(tag string) {
		req, rerr := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(body))
		if rerr != nil {
			t.Fatalf("new request: %v", rerr)
		}
		for _, kv := range headerLines {
			idx := strings.Index(kv, ":")
			if idx < 0 {
				continue
			}
			req.Header.Set(kv[:idx], kv[idx+2:])
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Host = "chatgpt.com"

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		req = req.WithContext(ctx)
		started := time.Now()
		resp, derr := client.Do(req)
		if derr != nil {
			t.Fatalf("[%s] request failed after %s: %v", tag, time.Since(started).Round(time.Second), derr)
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		summary := summarizeReproStream(respBody)
		t.Logf("[%s] alpn=%v http=%d elapsed=%s %s", tag, profile.ALPNProtocols, resp.StatusCode,
			time.Since(started).Round(time.Second), summary)
	}

	rounds := 1
	if os.Getenv("PROBE_REPRO_TWICE") == "1" {
		rounds = 2
	}
	for i := 1; i <= rounds; i++ {
		fireProbe(fmt.Sprintf("第%d针", i))
	}
}

func summarizeReproStream(body []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 8<<20)
	terminal := map[string]any{}
	deltas := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &ev) != nil {
			continue
		}
		switch ev["type"] {
		case "response.output_text.delta":
			deltas++
		case "response.completed", "response.failed", "response.incomplete":
			terminal = ev
		}
	}
	kind, _ := terminal["type"].(string)
	if kind == "" {
		tail := string(body)
		if len(tail) > 160 {
			tail = tail[len(tail)-160:]
		}
		return fmt.Sprintf("无终态事件! deltas=%d bytes=%d 尾部=%q", deltas, len(body), tail)
	}
	resp, _ := terminal["response"].(map[string]any)
	status, _ := resp["status"].(string)
	usage, _ := resp["usage"].(map[string]any)
	reasoning := ""
	if usage != nil {
		if d, ok := usage["output_tokens_details"].(map[string]any); ok {
			reasoning = fmt.Sprint(d["reasoning_tokens"])
		}
	}
	errInfo := ""
	if e, ok := resp["error"].(map[string]any); ok {
		errInfo = fmt.Sprint(e["message"])
	}
	return fmt.Sprintf("终态=%s status=%s deltas=%d reasoning=%s err=%s", kind, status, deltas, reasoning, errInfo)
}
