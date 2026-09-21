package service

import (
	"context"
	"errors"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// =============================================================================
// 账号健康标签 + 主动检测（相位A，2026-09-21 用户批准 spec）
//
// 数据哲学（用户裁定）：标签永远=探针状态机真值+最近一针实测，绝不读
// 「启用状态」等滞后字段——9/20 批量 401 实战教训：面板启用状态滞后会误导
// 降智判断，判降智必须等首针落地看 rt+答对。
//
// 主动检测纪律：
//   - 手动针与调度针完全同构（同一传输/头/体/判定），singleflight 键=accountID
//   - 双路径（2026-09-21 修复）：调度循环会拾取的号=CAS 提前 NextProbeAt 走
//     全状态机针（熔断/退避态=半开提前针，不改迁移语义）；ListDue 永不拾取的
//     号（manual_paused/停用/非调度 on_duty）=同步诊断针，只落证据行不碰状态机
//     ——原「仅提前排期」版对这些号是静默失效（ListDue 直接排除它们）
//   - 主动检测不重置任何配额/限流状态（auto-reset 纪律）
//   - 两个手动动作（主动检测；相位B 转打票线）都落 audit_logs
// =============================================================================

// OpenAIAccountHealth 单账号健康快照：探针状态机 + 最近一针证据行。
type OpenAIAccountHealth struct {
	AccountID     int64                    `json:"account_id"`
	State         string                   `json:"state"`
	ProbeMode     string                   `json:"probe_mode"`
	Schedulable   bool                     `json:"schedulable"`
	ManualPaused  bool                     `json:"manual_paused"`
	RateLimited   bool                     `json:"rate_limited"`
	Qualification bool                     `json:"qualification"`
	Label         string                   `json:"label"`
	LabelColor    string                   `json:"label_color"`
	Clickable     bool                     `json:"clickable"`
	Reason        string                   `json:"reason,omitempty"`
	LastProbe     *OpenAIProbeLastEvidence `json:"last_probe,omitempty"`
}

// OpenAIProbeLastEvidence 最近一针证据行（人可自验标签没撒谎）：
// `09-21 01:23 · rt 1532 · 答对 · ts 332 · gpt-6-astra`。
type OpenAIProbeLastEvidence struct {
	At              time.Time `json:"at"`
	Mode            string    `json:"mode"`
	ReasoningTokens *int      `json:"reasoning_tokens,omitempty"`
	AnswerCorrect   bool      `json:"answer_correct"`
	TurnStateLen    int       `json:"turn_state_len"`
	HTTPStatus      int       `json:"http_status,omitempty"`
	Degraded        bool      `json:"degraded"`
}

// 标签枚举（与前端 i18n 键一一对应）。
const (
	OpenAIHealthLabelNormal        = "normal"        // 正常号 绿
	OpenAIHealthLabelReview        = "review"        // 待复核 橙
	OpenAIHealthLabelProblem       = "problem"       // 问题号 红 可点（相位B转打票线）
	OpenAIHealthLabelRechecking    = "rechecking"    // 复检中 蓝
	OpenAIHealthLabelRateLimited   = "rate_limited"  // 限流中 灰 不可点（打票救不了额度）
	OpenAIHealthLabelPaused        = "paused"        // 已暂停 灰 不可点
	OpenAIHealthLabelEnforcement   = "enforcement"   // 静置中 灰红（相位B执法型分型后启用）
	OpenAIHealthLabelQualification = "qualification" // 待认证 灰
)

// 标签颜色 token（前端映射到主题色）。
const (
	OpenAIHealthColorGreen   = "green"
	OpenAIHealthColorOrange  = "orange"
	OpenAIHealthColorRed     = "red"
	OpenAIHealthColorBlue    = "blue"
	OpenAIHealthColorGray    = "gray"
	OpenAIHealthColorGrayRed = "gray-red"
)

// OpenAIProbeHealthSnapshot 聚合查询的一行输入。
type OpenAIProbeHealthSnapshot struct {
	AccountID     int64
	State         string
	ProbeMode     string
	Schedulable   bool
	ManualPaused  bool
	RateLimitedAt *time.Time
	Qualification bool
	LastProbe     *OpenAIProbeLastEvidence
}

// OpenAIProbeHealthLister 窄可选能力：批量健康快照聚合（只有真实 repository
// 实现；runner 侧类型断言取得，测试桩不受影响——与 HistoryCleaner 同模式）。
type OpenAIProbeHealthLister interface {
	ListOpenAIProbeHealthSnapshots(ctx context.Context, accountIDs []int64) ([]OpenAIProbeHealthSnapshot, error)
}

// LabelOpenAIAccountHealth 纯函数：状态映射表（proposal 表格逐行实现）。
// 判定优先级自上而下（proposal 裁定）：
// manual_paused > rate_limited > pending_replace > circuit_open >
// reprobe/half_open/sol_fallback > qualification > normal-健康/待复核。
// normal-健康 vs 待复核的分界 = 最近一针降智布尔（r15e 截断+答对=中性）。
// 静置中（enforcement）相位A 无数据源，恒不触发；相位B 分型落地后接入。
func LabelOpenAIAccountHealth(s OpenAIProbeHealthSnapshot) (label, color string, clickable bool, reason string) {
	if s.ManualPaused {
		return OpenAIHealthLabelPaused, OpenAIHealthColorGray, false, "manual_paused"
	}
	if s.RateLimitedAt != nil && !s.RateLimitedAt.IsZero() {
		// 限流中：额度耗尽打票救不了，不进打票线（用户裁定）。
		return OpenAIHealthLabelRateLimited, OpenAIHealthColorGray, false, "rate_limited"
	}
	if s.State == OpenAIDowngradeStatePendingReplace {
		return OpenAIHealthLabelProblem, OpenAIHealthColorRed, true, "pending_replace"
	}
	if s.State == OpenAIDowngradeStateCircuitOpen && s.ProbeMode == "half_open" {
		// 熔断冷却后进入半开复检：复检中（蓝），非问题号。
		return OpenAIHealthLabelRechecking, OpenAIHealthColorBlue, false, "circuit_open/half_open"
	}
	if s.State == OpenAIDowngradeStateCircuitOpen {
		return OpenAIHealthLabelProblem, OpenAIHealthColorRed, true, "circuit_open"
	}
	if s.State == OpenAIDowngradeStateReprobe || s.ProbeMode == "half_open" || s.ProbeMode == "sol_fallback" {
		return OpenAIHealthLabelRechecking, OpenAIHealthColorBlue, false, s.State + "/" + s.ProbeMode
	}
	if s.Qualification || s.ProbeMode == "qualification" {
		return OpenAIHealthLabelQualification, OpenAIHealthColorGray, false, "qualification"
	}
	// on_duty / normal：健康 vs 待复核看最近一针。
	if s.LastProbe != nil && s.LastProbe.Degraded {
		return OpenAIHealthLabelReview, OpenAIHealthColorOrange, false, "last_probe_degraded"
	}
	return OpenAIHealthLabelNormal, OpenAIHealthColorGreen, false, "on_duty"
}

// =============================================================================
// 主动检测（TriggerProbeNow）
// =============================================================================

// ErrOpenAIProbeAlreadyFlying 已有同号针在飞（调度针或另一手动针已提前）。
// 409 Conflict：客户端可据此提示「已有检测在执行」而非弹系统错误。
var ErrOpenAIProbeAlreadyFlying = errors.New("openai probe already flying for account")

// errOpenAIProbeNotEligible 号不具探针资格（改平台/改类型/影子/过期）：
// 400 客户端错误（不是服务端故障）。
var errOpenAIProbeNotEligible = infraerrors.BadRequest(
	"OPENAI_PROBE_NOT_ELIGIBLE", "account is not probe-eligible")

// openAIDowngradeProbeExitThrottleWindow 同出口合成探针最小间隔（镜像
// ListDue 的 10 分钟节流）；openAIDowngradeProbeExitThrottleRetryAfter 是
// 被节流时给前端的建议重试等待。
const (
	openAIDowngradeProbeExitThrottleWindow    = 10 * time.Minute
	openAIDowngradeProbeExitThrottleRetryAfter = 10 * time.Minute
)

// OpenAIProbeExitThrottler 窄可选能力：同出口（exit_ip 或 proxy 桶）近窗是否
// 已有合成探针。真实 repo 实现查询 probe_results；测试桩不实现则节流关闭
//（手动诊断针仍受 runMu 叠针闸保护）。
type OpenAIProbeExitThrottler interface {
	RecentProbeOnExitIP(ctx context.Context, accountID int64, proxyID *int64, within time.Time) (bool, error)
}

// respectExitIPThrottle 同出口节流判定（路径B）：近 10 分钟同 exit_ip（无
// exit_ip 则同 proxy 桶）已有合成探针 → true=放行，false=节流。查询失败
// FailOpen 放行（诊断针是低频手动动作，DB 故障不该吞掉用户指令）。
func (r *OpenAIDowngradeProbeRunner) respectExitIPThrottle(
	ctx context.Context, account *Account, now time.Time,
) bool {
	throttler, ok := r.store.(OpenAIProbeExitThrottler)
	if !ok {
		return true
	}
	hit, err := throttler.RecentProbeOnExitIP(ctx, account.ID, account.ProxyID, now.Add(-openAIDowngradeProbeExitThrottleWindow))
	if err != nil {
		logger.LegacyPrintf("service.openai_downgrade_probe",
			"[OpenAIProbeNow] exit throttle check failed account=%d: %v (fail-open)", account.ID, err)
		return true
	}
	return !hit
}

// TriggerProbeNowResult 手动触发结果。
type TriggerProbeNowResult struct {
	Accepted      bool      `json:"accepted"`
	AlreadyFlying bool      `json:"already_flying,omitempty"`
	ProbedNow     bool      `json:"probed_now,omitempty"`
	QueuedAt      time.Time `json:"queued_at"`
	// RetryAfterSeconds 非 0 = 被同出口节流拒绝（10 分钟闸），建议重试等待。
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// TriggerProbeNow 主动检测入口：任何非软删号可点（含已暂停/限流号——主动
// 检测是诊断动作，不受调度闸限制）。
//
// 双路径（2026-09-21 修复「暂停号静默失效」）：
//
//路径A 排期提前（调度循环会拾取的号）：schedulable、非 on_duty 态
//	（熔断/退避/重探——processState 有专门恢复路径）、qualification、
//	status=error。把 state.NextProbeAt 提前到 now（仅当当前排期晚于 now——
//	CAS 语义：已 due 或刚被另一手动请求提前的号直接返回 already_flying）。
//	扫描循环 ≤1 分钟一拍拾取后走 processState 全链路。熔断冷却/判死退避中
//	提前不等于赦免，状态机迁移语义分毫不动，2 连胜照旧。
//
//路径B 同步诊断针（ListDue 永不拾取的号）：manual_paused、面板停用/
//	非调度 on_duty 等。这些号提前排期是静默失效（ListDue 直接排除），
//	改为当场 runProbe 同步打一针：只落 probe_results 证据行，不碰状态机、
//	不改排期、不摘/复调度——诊断语义，与暂停号不参与调度的既有裁定一致。
//	401/403 与调度针同款走 SetError（凭据失效是事实，记录不是惩罚）。
//
// 判定镜像 ListDue L123 的闸门（on_duty 需 schedulable 或 qualification
// 或 error），保证路径A 的号下一拍必被拾取。不重置任何配额/限流状态
//（auto-reset 纪律）。手动针与调度针同构：同一 runProbe → probe 传输/
// 头/体/判定。
func (r *OpenAIDowngradeProbeRunner) TriggerProbeNow(ctx context.Context, accountID int64) (*TriggerProbeNowResult, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("openai probe runner is not available")
	}
	if r.IsStopped() {
		return nil, errors.New("openai probe runner is stopped")
	}
	now := r.now()
	state, err := r.store.GetOpenAIDowngradeState(ctx, accountID)
	if err != nil {
		return nil, err
	}
	account, err := r.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, ErrAccountNotFound
	}
	if state == nil {
		// 暂停号从未被扫描 Ensure 过（RunOnce 只 Ensure 未暂停号）：
		// 补建状态行再诊断，不误报「账号不存在」。排期取默认间隔后的未来
		// 时刻——诊断针的「已 due 即让位」闸以 NextProbeAt 在未来为前提。
		state, err = r.store.EnsureOpenAIDowngradeState(
			ctx, accountID, account.ProxyID, now.Add(r.nextDelay()))
		if err != nil {
			return nil, err
		}
		if state == nil {
			return nil, ErrAccountNotFound
		}
	}
	if !isOpenAIDowngradeProbeAccountEligible(account, now) {
		// 改平台/改类型/影子/过期：探针体系对其无意义（软删号 GetByID
		// 已走 ErrAccountNotFound）。不烧上游请求，明确拒绝。
		return nil, errOpenAIProbeNotEligible
	}
	// 路径判定（镜像 ListDue L123 闸门）：调度循环下一拍会不会拾取此号。
	// manual_paused 在 ListDue 被排除；on_duty+非 schedulable+非
	// qualification+非 error 同样被排除——都走路径B 同步诊断针。
	schedulerWillPick := account.Schedulable ||
		state.State != OpenAIDowngradeStateOnDuty ||
		state.ProbeMode == "qualification" ||
		account.Status == StatusError
	if !schedulerWillPick {
		res, err := r.triggerDiagnosticProbeNow(ctx, account, state, now)
		if err == nil && res != nil && res.Accepted {
			res.ProbedNow = true
		}
		return res, err
	}
	if !state.NextProbeAt.After(now) {
		// 已排到过去/现在：下一拍本来就会探，无需提前（连点去重）。
		return &TriggerProbeNowResult{Accepted: false, AlreadyFlying: true, QueuedAt: state.NextProbeAt}, nil
	}
	state.NextProbeAt = now
	state.UpdatedAt = now
	if err := r.store.SaveOpenAIDowngradeState(ctx, state); err != nil {
		return nil, err
	}
	return &TriggerProbeNowResult{Accepted: true, QueuedAt: now}, nil
}

// triggerDiagnosticProbeNow 路径B 同步诊断针（manual_paused/停用号）：
// 当场打一针落证据行。singleflight 语义由 runMu 复用实现——扫描循环
// 运行期间手动针直接让位（下一拍再打），不与调度针并发；状态机/排期/
// 调度位分毫不动。返回结果即时回显（证据行刷新可见）。
func (r *OpenAIDowngradeProbeRunner) triggerDiagnosticProbeNow(
	ctx context.Context,
	account *Account,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) (*TriggerProbeNowResult, error) {
	if !r.runMu.TryLock() {
		// 扫描循环正在跑（可能正在处理同号）：让位，避免与调度针叠打。
		return &TriggerProbeNowResult{Accepted: false, AlreadyFlying: true, QueuedAt: state.NextProbeAt}, nil
	}
	defer r.runMu.Unlock()
	if !state.NextProbeAt.After(now) {
		// 极端竞态：手动请求读到排期后、拿锁前，扫描循环恰好拾取了同号
		//（ListDue 变更或号刚被复启）。让位防叠针。
		return &TriggerProbeNowResult{Accepted: false, AlreadyFlying: true, QueuedAt: state.NextProbeAt}, nil
	}
	if !r.respectExitIPThrottle(ctx, account, now) {
		// 同出口 10 分钟节流（镜像 ListDue）：合成探针近距离出同一出口=
		// 可聚类形态。手动指令也不破安全红线，提示稍后再试。
		return &TriggerProbeNowResult{
			QueuedAt:          now.Add(openAIDowngradeProbeExitThrottleRetryAfter),
			RetryAfterSeconds: int(openAIDowngradeProbeExitThrottleRetryAfter.Seconds()),
		}, nil
	}
	mode := state.ProbeMode
	if mode == "" {
		mode = "normal"
	}
	result := r.runProbe(ctx, account, mode)
	if err := r.recordProbeResult(ctx, &result); err != nil {
		return nil, err
	}
	return &TriggerProbeNowResult{Accepted: true, QueuedAt: now}, nil
}

// IsStopped 探针循环是否已停。
func (r *OpenAIDowngradeProbeRunner) IsStopped() bool {
	if r == nil {
		return true
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	return r.stopped
}

// ListOpenAIAccountHealth 批量健康快照（列表页一次查齐，避免 N+1）。
// 数据源：probe_states + controls(manual_paused) + accounts(rate_limited_at,
// extra.qualification) + 最近一针 probe_results（含 turn_state_len）。
func (r *OpenAIDowngradeProbeRunner) ListOpenAIAccountHealth(ctx context.Context, accountIDs []int64) ([]OpenAIAccountHealth, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("openai probe runner is not available")
	}
	healthLister, ok := r.store.(OpenAIProbeHealthLister)
	if !ok {
		return nil, errors.New("openai probe health listing is not supported by store")
	}
	snapshots, err := healthLister.ListOpenAIProbeHealthSnapshots(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	out := make([]OpenAIAccountHealth, 0, len(snapshots))
	for i := range snapshots {
		s := snapshots[i]
		label, color, clickable, reason := LabelOpenAIAccountHealth(s)
		out = append(out, OpenAIAccountHealth{
			AccountID:     s.AccountID,
			State:         s.State,
			ProbeMode:     s.ProbeMode,
			Schedulable:   s.Schedulable,
			ManualPaused:  s.ManualPaused,
			RateLimited:   s.RateLimitedAt != nil && !s.RateLimitedAt.IsZero(),
			Qualification: s.Qualification,
			Label:         label,
			LabelColor:    color,
			Clickable:     clickable,
			Reason:        reason,
			LastProbe:     s.LastProbe,
		})
	}
	return out, nil
}

// GetOpenAIAccountHealth 单号健康快照。
func (r *OpenAIDowngradeProbeRunner) GetOpenAIAccountHealth(ctx context.Context, accountID int64) (*OpenAIAccountHealth, error) {
	list, err := r.ListOpenAIAccountHealth(ctx, []int64{accountID})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrAccountNotFound
	}
	return &list[0], nil
}

// OpenAIProbeEvidenceDegraded 最近一针的降智布尔（IsDegraded 的证据行版）：
// HTTP 200 且答错，或答对但 rt<800 → 降智证据。r15e 中性语义：截断指纹叠加
// 答对不判降智——指纹判定需要完整 result，状态机侧 ConsecutiveFailures 已
// 承接；证据行只做展示层复核。非 200（401/429/异常）不构成降智证据。
func OpenAIProbeEvidenceDegraded(ev *OpenAIProbeLastEvidence) bool {
	if ev == nil || ev.HTTPStatus != 200 {
		return false
	}
	if !ev.AnswerCorrect {
		return true
	}
	return ev.ReasoningTokens != nil && *ev.ReasoningTokens < OpenAIDowngradeFailureReasoningThreshold
}
