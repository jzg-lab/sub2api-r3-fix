package service

import (
	"sync"
	"time"
)

// abuse 路由信号桥（2026-09-20 用户批准的 P1/相位2 联动）。
//
// 背景（现网实证，usage_logs 9/15-9/20）：
//   - 请求 gpt-6-astra 被服务端路由到 gpt-5.6-luna（upstream_model_mismatch=t）
//     的三号（1048/1055/1093）全部死亡，零误报；1093 的 luna 路由比执法 401
//     早 3 分钟——该信号对账号级 abuse 标记有分钟级到小时级的提前量。
//   - 探针侧 x-codex-turn-state 长度双态（332=健康/356=降智）已闭环实证。
//
// 设计约束（用户裁定）：
//   1. 真实流量的 mismatch 信号只记事件 + 加速复查，不直接摘号——单信号不判死。
//   2. 探针针结果同时携带 356 + 降智证据（答错/截断）= 双信号在场，当场熔断。
//   3. 信号采集全部被动：真实流量侧读 usage 落账时已有的 mismatch 判定，
//     探针侧读响应头长度，零额外上游请求、零可聚类特征。
//
// 桥接形态沿用 openAICodexTelemetryGlobal 的全局单例模式：网关热路径
// （RecordUsage）不能持有探针 runner 的引用（装配层两者平行），经此桥投递；
// runner 每轮扫描时吸收。进程内通道，无序列化、无凭据材料。

const (
	// openAIAbuseRouteSignalTTL 信号吸收窗口。真实流量的下一次 mismatch 会
	// 刷新时间戳；窗口过后未被吸收的信号自然过期，不产生迟到的加速复查。
	openAIAbuseRouteSignalTTL = 30 * time.Minute
)

// openAIAbuseRouteSignal 一次真实流量 abuse 路由观测。
type openAIAbuseRouteSignal struct {
	AccountID      int64
	RequestedModel string
	ResponseModel  string
	ObservedAt     time.Time
	generation     uint64
}

// openAIAbuseRouteSignalHub 进程内信号枢纽。网关侧只写，探针侧只读；
// 短临界区互斥锁承接热路径并发（每账号至多一条在途信号，map 足够）。
type openAIAbuseRouteSignalHub struct {
	mu             sync.RWMutex
	signals        map[int64]openAIAbuseRouteSignal
	nextGeneration uint64
	nextPurgeAt    time.Time
}

var openAIAbuseRouteSignals = &openAIAbuseRouteSignalHub{
	signals: make(map[int64]openAIAbuseRouteSignal),
}

// ObserveRealTrafficModelMismatch 由网关 usage 落账路径调用：一笔真实流量
// 的响应 model 与请求不符（upstream_model_mismatch）。只存元数据，不存
// 响应体；调用方已保证 mismatch 判定来源是 usage_logs 同款逻辑。
func (h *openAIAbuseRouteSignalHub) ObserveRealTrafficModelMismatch(accountID int64, requestedModel, responseModel string, observedAt time.Time) {
	if h == nil || accountID <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !observedAt.Before(h.nextPurgeAt) {
		h.purgeExpiredLocked(observedAt)
	}
	if current, ok := h.signals[accountID]; ok && current.ObservedAt.After(observedAt) {
		return
	}
	if h.signals == nil {
		h.signals = make(map[int64]openAIAbuseRouteSignal)
	}
	h.nextGeneration++
	h.signals[accountID] = openAIAbuseRouteSignal{
		AccountID:      accountID,
		RequestedModel: requestedModel,
		ResponseModel:  responseModel,
		ObservedAt:     observedAt,
		generation:     h.nextGeneration,
	}
}

// AcknowledgeRealTrafficSignal runs only after the event's database commit.
// Traffic observed during the probe or commit has a new generation and survives.
func (h *openAIAbuseRouteSignalHub) AcknowledgeRealTrafficSignal(signal openAIAbuseRouteSignal) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if current, ok := h.signals[signal.AccountID]; ok && current.generation == signal.generation {
		delete(h.signals, signal.AccountID)
	}
}

// PurgeExpired also reclaims signals belonging to deleted or ineligible accounts.
func (h *openAIAbuseRouteSignalHub) PurgeExpired(now time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.purgeExpiredLocked(now)
}

func (h *openAIAbuseRouteSignalHub) purgeExpiredLocked(now time.Time) {
	for id, signal := range h.signals {
		if now.Sub(signal.ObservedAt) > openAIAbuseRouteSignalTTL {
			delete(h.signals, id)
		}
	}
	h.nextPurgeAt = now.Add(time.Minute)
}

// PeekRealTrafficSignal 只读探查（测试与观测用），不清除信号。
func (h *openAIAbuseRouteSignalHub) PeekRealTrafficSignal(accountID int64, now time.Time) (openAIAbuseRouteSignal, bool) {
	if h == nil {
		return openAIAbuseRouteSignal{}, false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	signal, ok := h.signals[accountID]
	if !ok || now.Sub(signal.ObservedAt) > openAIAbuseRouteSignalTTL {
		return openAIAbuseRouteSignal{}, false
	}
	return signal, true
}
