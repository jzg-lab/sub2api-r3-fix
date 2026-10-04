package service

// 救治区标签计算（r17ax Phase 3.4）：把「救治区成员标记 + 插件桥 per-账号
// 状态」折算成账号健康格标签与悬停注记。语义见 proposal.md 标签表：
//   - 救治中     蓝紫  成员 + 插件状态=active（连过 < 阈值）      不可点
//   - 待复核     橙    插件连过达标、宿主资格证据尚缺             可点→既有 reenable 考证
//   - 已复活     绿    当前插件与宿主资格双签通过                 不可点
//   - 疑似账号级 灰红  插件有质量失败并进入退避                   不可点
//   - 插件离线降级：桥缺席/离线 → 沿用救治中 + plugin_offline 注记（调度不变）
// 退避期满插件复探（in_backoff 翻回 false）→ 回升「救治中」（proposal 出口表：
// 退避期满复探给新机会）；suspect_account_level 是插件侧粘滞旗标，只进注记
// 不参与标签裁决。
//
// 插件 status_json 形状（lb-cookie-pin v0.3，transport/server.go probeStatus）：
// 顶层 "prober"{enabled, interval_seconds, adaptive_scheduling, model,
// accounts:[{account_id, consecutive_passes, consec_fails,
// suspect_account_level, in_backoff, backoff_until, last_probe_at, ...}]}。

import (
	"encoding/json"
	"strings"
	"time"
)

// 救治区标签枚举（与前端 i18n 键一一对应；接在相位A 标签族之后）。
const (
	OpenAIHealthLabelRescuing  = "rescuing"  // 救治中 蓝紫 不可点
	OpenAIHealthLabelRevived   = "revived"   // 已复活 绿 可点（转正触发器）
	OpenAIHealthLabelSuspected = "suspected" // 疑似账号级 灰红 不可点
)

// OpenAIHealthColorBluePurple 救治中色 token（前端映射主题蓝紫）。
const OpenAIHealthColorBluePurple = "blue-purple"

// OpenAIPluginBridgeAccount 插件桥 prober.accounts[] 的单账号消费视图
// （只取救治区要用的字段；签寿命三件由前端直接读原始 status_json）。
type OpenAIPluginBridgeAccount struct {
	AccountID           int64
	ConsecutivePasses   int
	ConsecFails         int
	SuspectAccountLevel bool
	InBackoff           bool
	BackoffUntil        time.Time
	LastProbeAt         time.Time
	PreviousPassAt      time.Time
	// LastVerdict 插件最近一针判定（pass/fail；空=尚无针）。
	LastVerdict string
}

// OpenAIPluginBridgeProber 桥 status_json 的 prober 区段解析形态。
type OpenAIPluginBridgeProber struct {
	Enabled  bool
	Accounts map[int64]*OpenAIPluginBridgeAccount
}

// ParseOpenAIPluginBridgeProber 容错解析插件 status_json 的 prober 区段。
// 空/坏 JSON/无 prober 键 → nil：调用方按无数据降级，桥解析失败绝不拖垮
// 健康列表（与 BridgeStatus 源侧吞错同一纪律）。该解析同时服务 3.4 标签
// 与 3.5 疑似账号级撤调（同一数据源，两处消费）。
func ParseOpenAIPluginBridgeProber(statusJSON string) *OpenAIPluginBridgeProber {
	trimmed := strings.TrimSpace(statusJSON)
	if trimmed == "" {
		return nil
	}
	var envelope struct {
		Prober *struct {
			Enabled  bool `json:"enabled"`
			Accounts []struct {
				AccountID           int64     `json:"account_id"`
				ConsecutivePasses   int       `json:"consecutive_passes"`
				ConsecFails         int       `json:"consec_fails"`
				SuspectAccountLevel bool      `json:"suspect_account_level"`
				InBackoff           bool      `json:"in_backoff"`
				BackoffUntil        time.Time `json:"backoff_until"`
				LastProbeAt         time.Time `json:"last_probe_at"`
				PreviousPassAt      time.Time `json:"previous_pass_at"`
				LastVerdict         string    `json:"last_verdict"`
			} `json:"accounts"`
		} `json:"prober"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return nil
	}
	if envelope.Prober == nil {
		return nil
	}
	prober := &OpenAIPluginBridgeProber{
		Enabled:  envelope.Prober.Enabled,
		Accounts: make(map[int64]*OpenAIPluginBridgeAccount, len(envelope.Prober.Accounts)),
	}
	for i := range envelope.Prober.Accounts {
		raw := &envelope.Prober.Accounts[i]
		prober.Accounts[raw.AccountID] = &OpenAIPluginBridgeAccount{
			AccountID:           raw.AccountID,
			ConsecutivePasses:   raw.ConsecutivePasses,
			ConsecFails:         raw.ConsecFails,
			SuspectAccountLevel: raw.SuspectAccountLevel,
			InBackoff:           raw.InBackoff,
			BackoffUntil:        raw.BackoffUntil,
			LastProbeAt:         raw.LastProbeAt,
			PreviousPassAt:      raw.PreviousPassAt,
			LastVerdict:         raw.LastVerdict,
		}
	}
	return prober
}

// OpenAIAccountRescueHealth 健康行上的救治注记（悬停数据源 + 徽标判定）：
// 救治时长（entered_at）/连过计数/连错次数/退避到期/插件离线角标/毕业阈值。
type OpenAIAccountRescueHealth struct {
	EnteredAt           time.Time  `json:"entered_at"`
	Trigger             string     `json:"trigger"`
	ConsecutivePasses   int        `json:"consecutive_passes"`
	ConsecFails         int        `json:"consec_fails"`
	InBackoff           bool       `json:"in_backoff"`
	SuspectAccountLevel bool       `json:"suspect_account_level"`
	BackoffUntil        *time.Time `json:"backoff_until,omitempty"`
	PluginOffline       bool       `json:"plugin_offline"`
	GraduationThreshold int        `json:"graduation_threshold"`
	// PluginLastProbeAt/PluginLastVerdict 插件侧最近一针（r17ba 悬停证据）：
	// 宿主证据行冻结在判死针（ListDue 排除 pending_replace），救治区里活跃
	// 的是插件针——两本账分置，「检测还是 7 小时前」的观感差由此解释。
	PluginLastProbeAt *time.Time `json:"plugin_last_probe_at,omitempty"`
	PluginLastVerdict string     `json:"plugin_last_verdict,omitempty"`
}

// OpenAIAccountRescuedBadge 永久复活徽标（task 4.4）：救治区毕业的血统
// 标记，转正时打、永不清除——与在区注记 OpenAIAccountRescueHealth 是两个
// 东西（后者转正即清）。前端据此渲染永久「已复活」角标（悬停时间+次数）。
type OpenAIAccountRescuedBadge struct {
	At    time.Time `json:"at"`
	Count int       `json:"count"`
}

// GraduationThreshold 毕业阈值（已复活判定的连过下限）。配置缺省时用默认 6。
func (l *OpenAIRescueLane) GraduationThreshold() int {
	if l == nil {
		return openAIRescueDefaultCleanPasses
	}
	if threshold := l.config().ConsecutiveCleanPasses; threshold > 0 {
		return threshold
	}
	return openAIRescueDefaultCleanPasses
}

// LabelOpenAIRescueAccount 纯函数：in-lane 账号的标签裁决（proposal 标签表）。
// bridge=nil（桥缺席/账号未入插件视图/缓存也没有）按无插件证据处理 → 救治中。
// 调用方只提供健康在线桥的证据；离线缓存只用于注记，不参与恢复判定。
// 调用方保证 marker!=nil（不在区的账号不走本函数）。
func LabelOpenAIRescueAccount(
	marker *OpenAIRescueLaneMarker,
	threshold int,
	bridge *OpenAIPluginBridgeAccount,
	host OpenAIProbeHealthSnapshot,
	now time.Time,
) (label, color string, clickable bool, reason string) {
	if threshold <= 0 {
		threshold = openAIRescueDefaultCleanPasses
	}
	if recovered, _ := rescueLaneRecoveryEvidence(bridge, host, marker, now); recovered &&
		bridge.ConsecutivePasses >= threshold {
		return OpenAIHealthLabelRevived, OpenAIHealthColorGreen, false, "plugin_and_host_pass"
	}
	if bridge != nil && bridge.InBackoff && bridge.ConsecFails >= 1 {
		// 插件连错达阈值停探退避：疑似账号级。退避期满 in_backoff 翻回
		// false → 回升救治中（复探给新机会）。
		return OpenAIHealthLabelSuspected, OpenAIHealthColorGrayRed, false, "plugin_backoff"
	}
	if rescuePluginPassEvidence(bridge, marker, now) && bridge.ConsecutivePasses >= threshold {
		return OpenAIHealthLabelReview, OpenAIHealthColorOrange,
			host.State == OpenAIDowngradeStatePendingReplace, "plugin_ready_for_qualification"
	}
	if marker != nil && marker.Observation != nil && !marker.Observation.SuspectedAt.IsZero() {
		return OpenAIHealthLabelSuspected, OpenAIHealthColorGrayRed, false, "recovery_unverified"
	}
	if bridge != nil {
		return OpenAIHealthLabelRescuing, OpenAIHealthColorBluePurple, false, "in_rescue"
	}
	return OpenAIHealthLabelRescuing, OpenAIHealthColorBluePurple, false, "no_plugin_evidence"
}

// BuildOpenAIAccountRescueHealth 构造健康行注记（悬停数据）。bridge=nil 时
// 计数字段全零——诚实呈现「插件侧无此账号数据」，不冒充计数为零的探针结论。
func BuildOpenAIAccountRescueHealth(
	marker *OpenAIRescueLaneMarker,
	threshold int,
	bridge *OpenAIPluginBridgeAccount,
	pluginOffline bool,
) *OpenAIAccountRescueHealth {
	if marker == nil {
		return nil
	}
	if threshold <= 0 {
		threshold = openAIRescueDefaultCleanPasses
	}
	note := &OpenAIAccountRescueHealth{
		EnteredAt:           marker.EnteredAt,
		Trigger:             marker.Trigger,
		PluginOffline:       pluginOffline,
		GraduationThreshold: threshold,
	}
	if bridge != nil {
		note.ConsecutivePasses = bridge.ConsecutivePasses
		note.ConsecFails = bridge.ConsecFails
		note.InBackoff = bridge.InBackoff
		note.SuspectAccountLevel = bridge.SuspectAccountLevel
		if !bridge.BackoffUntil.IsZero() {
			until := bridge.BackoffUntil
			note.BackoffUntil = &until
		}
		if !bridge.LastProbeAt.IsZero() {
			at := bridge.LastProbeAt
			note.PluginLastProbeAt = &at
			note.PluginLastVerdict = bridge.LastVerdict
		}
	}
	return note
}
