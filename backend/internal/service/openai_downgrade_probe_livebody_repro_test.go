package service

// live-body 复现（2026-09-18 资格探针 200-无-usage 裁决的最后一劈）：
// 冻结 body（/tmp/probe-repro）在进程外 12/12 通过，生产同代码路径 0/N 失败——
// 本测试用生产同款 turn builder「现场」构建 body/headers（新题、新 UUIDv7、
// 当前时间的环境上下文）经同一代理发出，把「live 请求内容」与「launchd 进程
// 运行时上下文」两个假设分开。仅诊断用，双 env 门控默认跳过。
//
//	PROBE_LIVE_BODY=1 PROBE_LIVE_TOKEN=<access_token> [PROBE_LIVE_PROXY=...] \
//	PROBE_LIVE_ACCOUNT=/tmp/probe-repro/account.json [PROBE_LIVE_TWICE=1] \
//	go test -tags=unit -run TestOpenAIProbeLiveBodyRepro -v ./internal/service/

import (
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

	"github.com/tidwall/gjson"
)

func TestOpenAIProbeLiveBodyRepro(t *testing.T) {
	if os.Getenv("PROBE_LIVE_BODY") != "1" {
		t.Skip("live-body repro not requested")
	}
	token := strings.TrimSpace(os.Getenv("PROBE_LIVE_TOKEN"))
	if token == "" {
		t.Fatal("PROBE_LIVE_TOKEN required")
	}
	accountPath := os.Getenv("PROBE_LIVE_ACCOUNT")
	if accountPath == "" {
		accountPath = "/tmp/probe-repro/account.json"
	}
	raw, err := os.ReadFile(accountPath)
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	var account Account
	if err := json.Unmarshal(raw, &account); err != nil {
		t.Fatalf("parse account: %v", err)
	}
	proxyURL := os.Getenv("PROBE_LIVE_PROXY")
	shots := 1
	if os.Getenv("PROBE_LIVE_TWICE") == "1" {
		shots = 2
	}

	fire := func(tag string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// 与生产 probe() 完全同源的现场构建：新题、新 turn 身份、当前时间。
		question := openAIDowngradeProbeNextQuestion(time.Now())
		turn := newOpenAIDowngradeProbeTurn(&account)
		body, err := turn.buildRequestBody("gpt-6-astra", question.Text, true)
		if err != nil {
			t.Fatalf("%s build body: %v", tag, err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexAPIURL, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("%s new request: %v", tag, err)
		}
		turn.applyRequestHeaders(req.Header, true)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Host = "chatgpt.com"
		setOpenAIChatGPTAccountHeaders(req.Header, &account)
		account.ApplyHeaderOverrides(req.Header)

		transport := &http.Transport{ForceAttemptHTTP2: true}
		if proxyURL != "" {
			transport.Proxy = func(*http.Request) (u *url.URL, e error) {
				return url.Parse(proxyURL)
			}
		}
		defer transport.CloseIdleConnections()
		started := time.Now()
		resp, err := (&http.Client{Transport: transport}).Do(req)
		if err != nil {
			t.Fatalf("%s do: %v", tag, err)
		}
		stream, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		elapsed := time.Since(started).Round(time.Second)
		terminal := ""
		for _, line := range strings.Split(string(stream), "\n") {
			line = strings.TrimSpace(line)
			if payload, ok := strings.CutPrefix(line, "data:"); ok &&
				strings.Contains(payload, "response.completed") {
				terminal = strings.TrimSpace(payload)
			}
		}
		terminalInfo := "<无 response.completed 事件>"
		if terminal != "" {
			head := terminal
			if len(head) > 300 {
				head = head[:300]
			}
			terminalInfo = fmt.Sprintf("status=%v reasoning=%v usage=%v | %s",
				gjson.Get(terminal, "response.status").Raw,
				gjson.Get(terminal, "response.usage.output_tokens_details.reasoning_tokens").Raw,
				gjson.Get(terminal, "response.usage").Raw != "" && gjson.Get(terminal, "response.usage.output_tokens").Exists(),
				head)
		}
		t.Logf("[%s] http=%d elapsed=%v bytes=%d 终态=%s",
			tag, resp.StatusCode, elapsed, len(stream), terminalInfo)
	}
	for i := 1; i <= shots; i++ {
		fire(fmt.Sprintf("第%d针", i))
	}
}
