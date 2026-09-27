package service

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// r17al 探针留档（2026-09-26 1187 案配套）：每次针的题目、期望答案、判定
// 结果与模型原始响应落 JSONL（默认 <DATA_DIR>/probe-archive/YYYY-MM-DD.jsonl，
// SUB2API_PROBE_ARCHIVE_DIR 可覆盖目录）。此前 raw 不落任何地方，误判无法
// 事后定性——1187 rt4142 答错只能靠统计推断，题目与模型实际输出均不可考。
// 留档失败只记日志，绝不影响探针主判定；响应保尾不保头（判分证据=最终
// 答案文本，几乎总在尾部）。
const (
	// openAIDowngradeSuspectRecheckInterval 滑误嫌疑针的快复检节奏。
	openAIDowngradeSuspectRecheckInterval = 5 * time.Minute
	// openAIDowngradeArchiveResponseCap 响应留档截断上限（保尾）。
	openAIDowngradeArchiveResponseCap = 8 << 10
	// openAIDowngradeArchiveAnswerCap 答案全文留档截断上限（保尾：判分文本
	// 的结论几乎总在末段）。r17am：答案文本经判分内核单独提取，不再依赖
	// 响应尾（终态 usage 记录霸占尾部，9/28 复盘实证答案几乎总被截掉）。
	openAIDowngradeArchiveAnswerCap = 2 << 10
)

var openAIDowngradeArchiveMu sync.Mutex

type openAIDowngradeProbeArchiveEntry struct {
	At              string `json:"at"`
	AccountID       int64  `json:"account_id"`
	Mode            string `json:"mode"`
	Domain          string `json:"domain"`
	QuestionText    string `json:"question_text"`
	AnswerDisplay   string `json:"answer_display"`
	// AnswerText 模型答案全文（判分内核提取，r17am）：答错定性第一证据——
	// 「数字看错」还是「完全胡说」一眼可判，不再依赖被 usage 记录截断的
	// 响应尾。
	AnswerText      string `json:"answer_text,omitempty"`
	AnswerCorrect   bool   `json:"answer_correct"`
	TransportOK     bool   `json:"transport_ok"`
	HTTPStatus      int    `json:"http_status"`
	ReasoningTokens *int   `json:"reasoning_tokens"`
	TurnStateLen    int    `json:"turn_state_len"`
	LatencyMS       int64  `json:"latency_ms"`
	ErrorMessage    string `json:"error_message,omitempty"`
	SuspectMiss     bool   `json:"suspect_miss"`
	ResponseTail    string `json:"response_tail"`
}

// archiveOpenAIDowngradeProbe 落档一针的完整判分证据。runProbe 在
// applyResponse 之后调用；question 与 responseBody 在该点同时在场。
func archiveOpenAIDowngradeProbe(accountID int64, mode string,
	question openAIDowngradeProbeQuestion,
	result *OpenAIDowngradeProbeResult, responseBody []byte) {
	if result == nil {
		return
	}
	dir := os.Getenv("SUB2API_PROBE_ARCHIVE_DIR")
	if dir == "" {
		dir = filepath.Join(os.Getenv("DATA_DIR"), "probe-archive")
	}
	if os.Getenv("DATA_DIR") == "" && os.Getenv("SUB2API_PROBE_ARCHIVE_DIR") == "" {
		slog.Debug("openai_probe_archive_skipped_no_dir")
		return
	}
	tail := responseBody
	if len(tail) > openAIDowngradeArchiveResponseCap {
		tail = tail[len(tail)-openAIDowngradeArchiveResponseCap:]
	}
	answerText := result.gradedText
	if len(answerText) > openAIDowngradeArchiveAnswerCap {
		answerText = answerText[len(answerText)-openAIDowngradeArchiveAnswerCap:]
	}
	entry := openAIDowngradeProbeArchiveEntry{
		At:              time.Now().Format(time.RFC3339),
		AccountID:       accountID,
		Mode:            mode,
		Domain:          question.Domain,
		QuestionText:    question.Text,
		AnswerDisplay:   question.AnswerDisplay,
		AnswerText:      answerText,
		AnswerCorrect:   result.AnswerCorrect,
		TransportOK:     result.TransportOK,
		HTTPStatus:      result.HTTPStatus,
		ReasoningTokens: result.ReasoningTokens,
		TurnStateLen:    result.TurnStateLen,
		LatencyMS:       result.Latency.Milliseconds(),
		ErrorMessage:    result.ErrorMessage,
		SuspectMiss:     result.IsSuspectMiss(),
		ResponseTail:    string(tail),
	}
	openAIDowngradeArchiveMu.Lock()
	defer openAIDowngradeArchiveMu.Unlock()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		slog.Warn("openai_probe_archive_mkdir_failed", "error", err.Error())
		return
	}
	name := filepath.Join(dir, time.Now().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		slog.Warn("openai_probe_archive_open_failed", "error", err.Error())
		return
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(entry); err != nil {
		slog.Warn("openai_probe_archive_encode_failed", "error", err.Error())
	}
}
