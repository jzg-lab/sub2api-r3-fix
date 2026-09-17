package service

// 校准题面打印机（P2-11 二期 / #22）：用与生产探针完全相同的 turn 构造器与
// 请求模板生成各题域的 /responses 体，落盘给网关校准脚本经 18420 发送。
// 普通测试使用自动清理的临时目录；需保留校准文件时设置 PROBE_CALIBRATION_DIR。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCalibrationProbe(t *testing.T, dir string, seq int, q openAIDowngradeProbeQuestion) {
	t.Helper()
	account := &Account{ID: 1029, Platform: PlatformOpenAI, Extra: map[string]any{}}
	turn := newOpenAIDowngradeProbeTurn(account)
	body, err := turn.buildRequestBody("gpt-6-astra", q.Text, true)
	if err != nil {
		t.Fatalf("build request body: %v", err)
	}
	bodyPath := filepath.Join(dir, fmt.Sprintf("body-%02d.json", seq))
	if err := os.WriteFile(bodyPath, body, 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}
	// curl 配置只含非机码头；网关 key 由发送脚本运行时注入。
	cfg := fmt.Sprintf(`url = "http://127.0.0.1:18420/responses"
request = POST
header = "content-type: application/json"
header = "accept: text/event-stream"
header = "originator: codex_exec"
header = "user-agent: %s"
header = "session-id: %s"
header = "thread-id: %s"
header = "x-client-request-id: %s"
header = "x-codex-window-id: %s"
header = "x-codex-beta-features: prevent_idle_sleep,remote_compaction_v2"
data = "@%s"
`, turn.userAgent(), turn.sessionID, turn.threadID, turn.sessionID,
		turn.windowID, bodyPath)
	cfgPath := filepath.Join(dir, fmt.Sprintf("cfg-%02d.curl", seq))
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	meta := map[string]any{
		"seq": seq, "domain": q.Domain, "answer": q.AnswerDisplay,
		"body": bodyPath,
	}
	metaPath := filepath.Join(dir, fmt.Sprintf("meta-%02d.json", seq))
	payload, _ := json.Marshal(meta)
	if err := os.WriteFile(metaPath, payload, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
}

func collectCalibrationQuestions(t *testing.T) []openAIDowngradeProbeQuestion {
	t.Helper()
	want := map[string]int{
		"two_dim": 3, // 第五轮：年份池移除后的复验（累计第 5/6/7 针 two_dim）
	}
	now := time.Now()
	var out []openAIDowngradeProbeQuestion
	for attempt := 0; attempt < 500 && len(out) < 3; attempt++ {
		q := openAIDowngradeProbeNextQuestion(now)
		if want[q.Domain] > 0 {
			want[q.Domain]--
			out = append(out, q)
		}
	}
	if len(out) != 3 {
		t.Fatalf("failed to collect all domains: got %d", len(out))
	}
	return out
}

func TestPrintCalibrationQuestions(t *testing.T) {
	dir := os.Getenv("PROBE_CALIBRATION_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	questions := collectCalibrationQuestions(t)
	for i, q := range questions {
		writeCalibrationProbe(t, dir, i+1, q)
	}
	t.Logf("wrote %d calibration probes to %s", len(questions), dir)
}
