package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"time"
)

// 纯被动降额检测：上游 x-codex-* 头与 /wham/usage 都只暴露百分比，绝对额度
// 被砍（降额）时 UsedPercent 依然会走到 100%，百分比口径无法区分「额度没变」
// 与「额度被砍」。唯一可观测的差分信号 = 7d 窗口打满时刻的本窗累计消耗
// （usage_logs 求和）：同一账号连续若干窗口都在明显更低的消耗上打满，
// 高度疑似上游削减了该账号的绝对额度。本检测只读既有数据、只告警
// （事件 + 日志），不停调度、不产生任何新的上游流量。
const (
	// openAI7dExhaustionHistoryExtraKey 存于 accounts.extra，最新记录在前，最多 4 条。
	openAI7dExhaustionHistoryExtraKey = "openai_7d_exhaustion_history"
	// OpenAIDowngradeEventQuotaCut 降额疑似事件，写入 openai_downgrade_probe_events
	// 供运维面板展示（与降智事件同一事件流）。
	OpenAIDowngradeEventQuotaCut = "quota_cut_detected"
	// openAIQuotaCutHistoryMax 保留最近 4 个窗口的打满记录（约一个月）。
	openAIQuotaCutHistoryMax = 4
	// openAIQuotaCutRatioFloor 现窗消耗低于历史中位的 75%（≈降额 ≥25%）即告警。
	openAIQuotaCutRatioFloor = 0.75
	// openAIQuotaCutMinPriors 至少 2 个历史窗口做中位基线，单窗波动不告警。
	openAIQuotaCutMinPriors = 2
	// openAIQuotaCutBurstGuard 同一 429 风暴内的重复打满不重复记录（见
	// appendOpenAI7dExhaustionRecord 的去重说明）。
	openAIQuotaCutBurstGuard = time.Hour
)

// openAI7dExhaustionRecord 一个 7d 窗口打满时刻的快照：本窗消耗是多少。
type openAI7dExhaustionRecord struct {
	ExhaustedAt   time.Time `json:"exhausted_at"`
	WindowStart   time.Time `json:"window_start"`
	WindowResetAt time.Time `json:"window_reset_at"`
	Cost          float64   `json:"cost"`
	Tokens        int64     `json:"tokens"`
	Requests      int64     `json:"requests"`
}

// OpenAIQuotaCutEventWriter 是降额事件的窄写出能力（真实实现 = 探针事件
// repository），保持 RateLimitService 与探针存储解耦，测试桩不受影响。
type OpenAIQuotaCutEventWriter interface {
	AppendOpenAIDowngradeEvent(ctx context.Context, accountID int64, proxyID *int64, eventType string, details map[string]any) error
}

type openAIQuotaCutVerdict struct {
	Current     openAI7dExhaustionRecord
	MedianPrior float64
	Ratio       float64
}

// SetOpenAIQuotaCutEventWriter 注入降额事件写出器（可选依赖，装配期调用）。
func (s *RateLimitService) SetOpenAIQuotaCutEventWriter(w OpenAIQuotaCutEventWriter) {
	if s == nil {
		return
	}
	s.openAIQuotaCutEventWriter = w
}

// noteOpenAI7dExhaustion 在 OpenAI 429 分支被动调用：仅当 7d 窗口打满时记录
// 本窗消耗并与历史基线比对。任何内部失败只记日志，绝不影响 429 主流程。
func (s *RateLimitService) noteOpenAI7dExhaustion(ctx context.Context, account *Account, headers http.Header) {
	if s == nil || s.usageRepo == nil || s.accountRepo == nil || account == nil || headers == nil {
		return
	}
	snapshot := ParseCodexRateLimitHeaders(headers)
	if snapshot == nil {
		return
	}
	normalized := snapshot.Normalize()
	if normalized == nil || normalized.Used7dPercent == nil || *normalized.Used7dPercent < 100 {
		return
	}
	now := time.Now()
	resetAt := now
	if normalized.Reset7dSeconds != nil && *normalized.Reset7dSeconds > 0 {
		resetAt = now.Add(time.Duration(*normalized.Reset7dSeconds) * time.Second)
	}
	windowMinutes := 7 * 24 * 60
	if normalized.Window7dMinutes != nil && *normalized.Window7dMinutes > 0 {
		windowMinutes = *normalized.Window7dMinutes
	}
	windowStart := resetAt.Add(-time.Duration(windowMinutes) * time.Minute)

	stats, err := s.usageRepo.GetAccountWindowStats(ctx, account.ID, windowStart)
	if err != nil || stats == nil {
		slog.Warn("openai_7d_exhaustion_stats_unavailable",
			"account_id", account.ID, "error", err)
		return
	}
	record := openAI7dExhaustionRecord{
		ExhaustedAt:   now,
		WindowStart:   windowStart,
		WindowResetAt: resetAt,
		Cost:          stats.Cost,
		Tokens:        stats.Tokens,
		Requests:      stats.Requests,
	}

	// 重新拉取账号取最新 extra 历史：429 常成串出现，转发路径上的 account
	// 快照可能带着旧历史，读-改-写会覆盖掉同风暴里先落库的记录。
	history := normalizeOpenAI7dExhaustionHistory(nil)
	if fresh, fetchErr := s.accountRepo.GetByID(ctx, account.ID); fetchErr == nil && fresh != nil && fresh.Extra != nil {
		history = normalizeOpenAI7dExhaustionHistory(fresh.Extra[openAI7dExhaustionHistoryExtraKey])
	} else if account.Extra != nil {
		history = normalizeOpenAI7dExhaustionHistory(account.Extra[openAI7dExhaustionHistoryExtraKey])
	}

	updated, appended := appendOpenAI7dExhaustionRecord(history, record)
	if !appended {
		return
	}
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		openAI7dExhaustionHistoryExtraKey: updated,
	}); err != nil {
		slog.Warn("openai_7d_exhaustion_history_persist_failed",
			"account_id", account.ID, "error", err)
		return
	}
	slog.Info("openai_7d_exhaustion_recorded",
		"account_id", account.ID, "cost", record.Cost,
		"tokens", record.Tokens, "requests", record.Requests)

	if verdict := detectOpenAIQuotaCut(updated); verdict != nil {
		slog.Warn("openai_quota_cut_suspected",
			"account_id", account.ID,
			"current_cost", verdict.Current.Cost,
			"median_prior_cost", verdict.MedianPrior,
			"ratio", verdict.Ratio,
			"current_tokens", verdict.Current.Tokens)
		if s.openAIQuotaCutEventWriter != nil {
			details := map[string]any{
				"reason":            "7d_window_consumption_below_baseline",
				"current_cost":      verdict.Current.Cost,
				"current_tokens":    verdict.Current.Tokens,
				"current_requests":  verdict.Current.Requests,
				"median_prior_cost": verdict.MedianPrior,
				"ratio":             verdict.Ratio,
				"window_reset_at":   verdict.Current.WindowResetAt.Format(time.RFC3339),
			}
			if err := s.openAIQuotaCutEventWriter.AppendOpenAIDowngradeEvent(
				ctx, account.ID, account.ProxyID, OpenAIDowngradeEventQuotaCut, details,
			); err != nil {
				slog.Warn("openai_quota_cut_event_persist_failed",
					"account_id", account.ID, "error", err)
			}
		}
	}
}

// normalizeOpenAI7dExhaustionHistory 把 accounts.extra 里 JSONB 往返后的历史
// （[]any/map[string]any/float64/RFC3339 字符串）容错还原为记录切片。
// 单条损坏只跳过该条，不影响其余历史。
func normalizeOpenAI7dExhaustionHistory(raw any) []openAI7dExhaustionRecord {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]openAI7dExhaustionRecord, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var record openAI7dExhaustionRecord
		parseTime := func(key string) time.Time {
			value, _ := entry[key].(string)
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return time.Time{}
			}
			return parsed
		}
		record.ExhaustedAt = parseTime("exhausted_at")
		record.WindowStart = parseTime("window_start")
		record.WindowResetAt = parseTime("window_reset_at")
		cost, _ := entry["cost"].(float64)
		record.Cost = cost
		if tokens, ok := entry["tokens"].(float64); ok {
			record.Tokens = int64(tokens)
		}
		if requests, ok := entry["requests"].(float64); ok {
			record.Requests = int64(requests)
		}
		if record.ExhaustedAt.IsZero() {
			continue
		}
		out = append(out, record)
	}
	return out
}

// appendOpenAI7dExhaustionRecord 前插新记录并裁剪到上限。去重两道闸：
// ① 打满时刻仍在上条记录的窗口内（未到 reset_at）= 同一窗口的持续 429；
// ② 距上条记录不足 1h = 同一 429 风暴（reset 后立刻又打满的情况极少，
// 宁可漏记一次也不让重复记录污染中位基线）。命中任一闸返回 appended=false。
func appendOpenAI7dExhaustionRecord(
	history []openAI7dExhaustionRecord, record openAI7dExhaustionRecord,
) ([]openAI7dExhaustionRecord, bool) {
	if len(history) > 0 {
		latest := history[0]
		if record.ExhaustedAt.Before(latest.WindowResetAt) ||
			record.ExhaustedAt.Sub(latest.ExhaustedAt) < openAIQuotaCutBurstGuard {
			return history, false
		}
	}
	out := append([]openAI7dExhaustionRecord{record}, history...)
	if len(out) > openAIQuotaCutHistoryMax {
		out = out[:openAIQuotaCutHistoryMax]
	}
	return out, true
}

// detectOpenAIQuotaCut 纯比较逻辑：history[0] 是刚记录的现窗，其余是基线。
// 基线不足 2 窗或中位为 0 时不判；现窗消耗 < 75%×中位 → 疑似降额。
func detectOpenAIQuotaCut(history []openAI7dExhaustionRecord) *openAIQuotaCutVerdict {
	if len(history) < openAIQuotaCutMinPriors+1 {
		return nil
	}
	current := history[0]
	priors := make([]float64, 0, len(history)-1)
	for _, item := range history[1:] {
		priors = append(priors, item.Cost)
	}
	sort.Float64s(priors)
	median := priors[len(priors)/2]
	if len(priors)%2 == 0 {
		median = (priors[len(priors)/2-1] + priors[len(priors)/2]) / 2
	}
	if median <= 0 {
		return nil
	}
	ratio := current.Cost / median
	if ratio >= openAIQuotaCutRatioFloor {
		return nil
	}
	return &openAIQuotaCutVerdict{Current: current, MedianPrior: median, Ratio: ratio}
}

// marshalOpenAI7dExhaustionHistory 仅供测试与调试：把记录切片序列化为 JSON
// 以确认落库形态与 normalize 往返一致。
func marshalOpenAI7dExhaustionHistory(history []openAI7dExhaustionRecord) string {
	payload, err := json.Marshal(history)
	if err != nil {
		return "[]"
	}
	return string(payload)
}
