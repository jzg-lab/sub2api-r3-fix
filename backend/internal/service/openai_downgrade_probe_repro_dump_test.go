package service

// 手工复现资格探针的 dump 工具(诊断用,不联网):
// PROBE_REPRO_DIR + PROBE_REPRO_ACCOUNT_JSON 双 env 门控,构造与生产
// 探针逐字节同款的 /responses body 与请求头,落盘供 curl 对照复现。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestProbeReproDumpBodyAndHeaders(t *testing.T) {
	dir := os.Getenv("PROBE_REPRO_DIR")
	acctPath := os.Getenv("PROBE_REPRO_ACCOUNT_JSON")
	if dir == "" || acctPath == "" {
		t.Skip("repro dump not requested")
	}
	raw, err := os.ReadFile(acctPath)
	if err != nil {
		t.Fatalf("read account json: %v", err)
	}
	var spec struct {
		ID               int64          `json:"id"`
		Platform         string         `json:"platform"`
		Type             string         `json:"type"`
		Extra            map[string]any `json:"extra"`
		ChatGPTAccountID string         `json:"chatgpt_account_id"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse account json: %v", err)
	}
	account := &Account{
		ID:          spec.ID,
		Platform:    spec.Platform,
		Type:        spec.Type,
		Extra:       spec.Extra,
		Credentials: map[string]any{"chatgpt_account_id": spec.ChatGPTAccountID},
	}
	question := openAIDowngradeProbeNextQuestion(time.Now())
	turn := newOpenAIDowngradeProbeTurn(account)
	body, err := turn.buildRequestBody("gpt-6-astra", question.Text, true)
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	if err := os.WriteFile(dir+"/body.json", body, 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}
	h := http.Header{}
	turn.applyRequestHeaders(h, true)
	h.Set("Content-Type", "application/json")
	setOpenAIChatGPTAccountHeaders(h, account)
	meta := fmt.Sprintf("account=%d question_domain=%s expected_answer=%s\ninstallation_id=%s\nsession_id=%s\nturn_model=gpt-6-astra stream=true\n",
		spec.ID, question.Domain, question.AnswerDisplay,
		turn.installationID, turn.sessionID)
	for _, key := range []string{"Accept", "Content-Type", "originator", "user-agent",
		"session-id", "thread-id", "x-client-request-id", "x-codex-window-id",
		"x-codex-beta-features", "x-codex-turn-metadata", "chatgpt-account-id"} {
		meta += fmt.Sprintf("H %s: %s\n", key, h.Get(key))
	}
	if err := os.WriteFile(dir+"/meta.txt", []byte(meta), 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	t.Logf("dumped body(%d bytes)+meta to %s", len(body), dir)
}
