package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// State is outside versioned installations, whose replacement deletes assets.
// The host supplies only this path, not its environment or credentials.
func pluginEvidenceDir(socketDir, key string) (string, error) {
	hash := sha256.Sum256([]byte(key))
	base := filepath.Join(filepath.Dir(socketDir), "state")
	for _, dir := range []string{base, filepath.Join(base, hex.EncodeToString(hash[:]))} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", err
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("unsafe plugin state directory")
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return "", err
		}
	}
	return filepath.Join(base, hex.EncodeToString(hash[:])), nil
}

// Only fixed structured events reach the host's existing logging channel.
// All other stderr, extra fields and oversized lines are discarded.
type pluginEvidenceLog struct {
	mu      sync.Mutex
	pending []byte
	drop    bool
	logger  *slog.Logger
}

func (w *pluginEvidenceLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			end = len(p)
		}
		if len(w.pending)+end > 2048 {
			w.drop, w.pending = true, nil
		}
		if !w.drop {
			w.pending = append(w.pending, p[:end]...)
		}
		p = p[end:]
		if len(p) == 0 {
			break
		}
		p = p[1:]
		if !w.drop {
			w.emit(w.pending)
		}
		w.pending, w.drop = nil, false
	}
	return n, nil
}

func (w *pluginEvidenceLog) emit(raw []byte) {
	var event struct {
		Message            string `json:"msg"`
		Account            int64  `json:"account_id"`
		Category           string `json:"category"`
		Window             int    `json:"window_seconds"`
		Operation          string `json:"operation_id"`
		TemplateGeneration uint64 `json:"template_generation"`
		CookieGeneration   uint64 `json:"cookie_generation"`
		Probe              int64  `json:"probe"`
		QualityRerolls     int64  `json:"quality_rerolls"`
		Verdict            string `json:"verdict"`
		Action             string `json:"action"`
		ReasoningTokens    int    `json:"reasoning_tokens"`
		ObservedAt         string `json:"observed_at"`
		Stage              string `json:"stage"`
		Reason             string `json:"reason"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	switch event.Message {
	case "plugin_business_succeeded":
		w.emitBusinessSuccess(raw)
	case "upstream_transport_failure":
		w.emitTransportFailure(raw)
	case "plugin_business_unknown":
		if event.Account <= 0 {
			return
		}
		switch event.Stage {
		case "dispatch", "headers", "body", "end":
		default:
			return
		}
		switch event.Reason {
		case "invalid_event", "failed_event", "partial_event", "delivery_interrupted",
			"unrecognized_json", "missing_terminal":
			w.logger.Warn("plugin_business_unknown", "account_id", event.Account,
				"stage", event.Stage, "reason", event.Reason)
		}
	case "quality_probe_observation":
		if event.Account <= 0 || event.TemplateGeneration == 0 || event.Probe <= 0 ||
			event.QualityRerolls < 0 || event.QualityRerolls > event.Probe ||
			event.ReasoningTokens < 0 {
			return
		}
		switch event.Verdict {
		case "pass", "fail", "error":
		default:
			return
		}
		switch event.Action {
		case "scheduled", "reroll", "backoff", "retry_later":
		default:
			return
		}
		observedAt, err := time.Parse(time.RFC3339Nano, event.ObservedAt)
		if err != nil {
			return
		}
		w.logger.Info("quality_probe_observation", "account_id", event.Account,
			"template_generation", event.TemplateGeneration, "cookie_generation", event.CookieGeneration,
			"probe", event.Probe, "quality_rerolls", event.QualityRerolls,
			"verdict", event.Verdict, "action", event.Action,
			"reasoning_tokens", event.ReasoningTokens, "observed_at", observedAt)
	case "plugin_evidence_unavailable":
		w.logger.Error("plugin_evidence_unavailable")
	case "plugin_probe_unknown":
		parts := strings.Split(event.Operation, "-")
		if event.Account <= 0 || len(parts) != 3 || len(parts[0]) != 32 ||
			parts[1] != strconv.FormatInt(event.Account, 10) {
			return
		}
		if _, err := hex.DecodeString(parts[0]); err != nil {
			return
		}
		sequence, err := strconv.ParseUint(parts[2], 10, 64)
		if err == nil && sequence > 0 {
			w.logger.Warn("plugin_probe_unknown", "account_id", event.Account, "operation_id", event.Operation)
		}
	case "plugin_upstream_admission_rejected":
		if event.Account <= 0 || event.Window != 600 {
			return
		}
		switch event.Category {
		case "turn_state_ticket", "state_rejection", "admission_rejected":
			w.logger.Warn("plugin_upstream_admission_rejected", "account_id", event.Account,
				"category", event.Category, "window_seconds", event.Window)
		}
	}
}
