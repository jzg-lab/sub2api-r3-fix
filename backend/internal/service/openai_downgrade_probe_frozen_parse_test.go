package service

// 冻结流离线裁决（2026-09-18 r17e 取证后续）：生产日志证实 200-无-usage 谜题
// 形态为 (c)——终态/usage/reasoning 全在场、严格解析器拒收。此前所有进程外
// 「复现通过」用的是 gjson 宽松提取，从未过生产解析器。本测试把 02:06 冻结的
// 真实捕获流（/tmp/probe-repro/sse_p6*.txt，1077/proxy6）直接喂
// parseOpenAIDowngradeProbeResponse，并用 gjson 逐门诊断哪个拒收分支触发。
// 仅诊断用，env 门控默认跳过：
//
//	FROZEN_PARSE=1 go test -tags=unit -run TestFrozenProbeStreamParse -v ./internal/service/

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestFrozenProbeStreamParse(t *testing.T) {
	if os.Getenv("FROZEN_PARSE") != "1" {
		t.Skip("frozen parse diagnosis not requested")
	}
	paths := strings.Split(os.Getenv("FROZEN_PARSE_FILES"), ",")
	if len(paths) == 0 || strings.TrimSpace(paths[0]) == "" {
		paths = []string{
			"/tmp/probe-repro/sse_p6.txt",
			"/tmp/probe-repro/sse_p6_h1.txt",
		}
	}
	for _, path := range paths {
		path = strings.TrimSpace(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(raw)
		// 冻结流题目期望答案=21（糖果组合题）；nil 正则会死在解析器第一行
		// （answerPattern == nil → 拒收），此前误把 nil 门当成了拒收证据。
		pattern := openAIDowngradeNumericAnswerPattern(21)
		correct, reasoning, juice := parseOpenAIDowngradeProbeResponse(raw, pattern)
		t.Logf("[%s] bytes=%d parser: accept=%v correct=%v rt=%v juice=%v",
			path, len(raw), reasoning != nil, correct, reasoning, juice)
		if !strings.Contains(path, "reject") {
			require.True(t, reasoning != nil, "frozen real stream must parse (item-done delivery)")
			require.True(t, correct, "frozen stream answer must match expected pattern")
		}

		// 定位最后一个 response.completed 的 data: 行，逐门复刻解析器判定。
		terminal := ""
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if payload, ok := strings.CutPrefix(line, "data:"); ok &&
				strings.Contains(payload, "response.completed") {
				terminal = strings.TrimSpace(payload)
			}
		}
		if terminal == "" {
			t.Fatalf("[%s] no response.completed data line", path)
		}
		resp := gjson.Get(terminal, "response")
		t.Logf("[%s] terminal: type=%q status=%q error=%v incomplete_details=%v usage=%v rt=%v",
			path,
			gjson.Get(terminal, "type").String(),
			resp.Get("status").String(),
			resp.Get("error").Raw != "" && resp.Get("error").Raw != "null",
			resp.Get("incomplete_details").Raw != "" && resp.Get("incomplete_details").Raw != "null",
			resp.Get("usage").Exists(),
			resp.Get("usage.output_tokens_details.reasoning_tokens").Int(),
		)
		// SSE 框架门：流尾是否以完整记录收束（最后一条 data 行后有无空行）。
		normalized := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
		trimmed := strings.TrimSuffix(normalized, "\n")
		lastLine := ""
		if i := strings.LastIndex(trimmed, "\n"); i >= 0 {
			lastLine = trimmed[i+1:]
		} else {
			lastLine = trimmed
		}
		t.Logf("[%s] framing: ends_with_blank_record=%v last_line=%q trailing_data_leftover=%v",
			path,
			strings.HasSuffix(normalized, "\n\n"),
			lastLine,
			lastLine != "" && !strings.HasPrefix(lastLine, "data:"),
		)
		// 输出门：终态 response.output 里有没有带 output_text 的 assistant 消息。
		hasText := false
		for _, item := range resp.Get("output").Array() {
			if item.Get("type").String() != "message" && item.Get("type").Exists() {
				continue
			}
			for _, part := range item.Get("content").Array() {
				if strings.HasPrefix(part.Get("type").String(), "output_text") {
					hasText = true
				}
			}
		}
		t.Logf("[%s] output: has_output_text=%v output_items=%d",
			path, hasText, len(resp.Get("output").Array()))
	}
}
