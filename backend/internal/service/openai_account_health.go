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
	// Rescue 救治区注记（r17ax Phase 3.4）：仅在区成员账号上在场；标签
	// 覆盖见 ListOpenAIAccountHealth（paused/rate_limited 仍最高优先）。
	Rescue *OpenAIAccountRescueHealth `json:"rescue,omitempty"`
	// Rescued 永久复活徽标（task 4.4）：曾从救治区毕业的账号永久在场
	// （与 Rescue 是两个东西：转正清 Rescue、留 Rescued 血统）。
	Rescued *OpenAIAccountRescuedBadge `json:"rescued,omitempty"`
}

// OpenAIAccountHealthListResult 批量健康快照响应信封：账号列表 + 全局插件桥
// 区块。plugin_bridge 是救治区标签（救治中/插件离线角标）的数据源；无启用
// 插件或桥源未注入时缺席，前端按无桥降级渲染。
type OpenAIAccountHealthListResult struct {
	Accounts     []OpenAIAccountHealth `json:"accounts"`
	PluginBridge *PluginBridgeStatus   `json:"plugin_bridge,omitempty"`
}

// OpenAIProbeLastEvidence 最近一针证据行（人可自验标签没撒谎）：
// `09-21 01:23 · rt 1532 · 答对 · ts 332 · gpt-6-astra`。
type OpenAIProbeLastEvidence struct {
	ID              int64     `json:"id,omitempty"`
	At              time.Time `json:"at"`
	Mode            string    `json:"mode"`
	ReasoningTokens *int      `json:"reasoning_tokens,omitempty"`
	TransportOK     bool      `json:"transport_ok"`
	AnswerCorrect   *bool     `json:"answer_correct"`
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
	// RescueMarker 救治区成员标记（r17ax Phase 3.4：仓库从
	// extra->'openai_rescue_lane' 原文列解析；不在区为 nil）。
	RescueMarker *OpenAIRescueLaneMarker
	// RescuedAt/RescueCount 永久复活徽标（task 4.4：GraduateRescue 打的
	// 血统标记；RescuedAt 非 nil 即有徽标，坏值按无徽标）。
	RescuedAt   *time.Time
	RescueCount int
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
		// 熔断号由半开复检自动恢复（不可点：打票线已删，无手动救援动作）。
		return OpenAIHealthLabelProblem, OpenAIHealthColorRed, false, "circuit_open"
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

// Manual scheduling and rescue requests are bound to the account and state
// generations they observed. A concurrent winner is a client-visible conflict,
// not an internal server error.
var errOpenAIProbeGenerationChanged = infraerrors.Conflict(
	"OPENAI_PROBE_GENERATION_CHANGED", "account health changed concurrently; refresh and retry")

// errOpenAIProbeNotEligible 号不具探针资格（改平台/改类型/影子/过期）：
// 400 客户端错误（不是服务端故障）。
var errOpenAIProbeNotEligible = infraerrors.BadRequest(
	"OPENAI_PROBE_NOT_ELIGIBLE", "account is not probe-eligible")

// openAIDowngradeProbeExitThrottleWindow 同出口合成探针最小间隔（镜像
// ListDue 的 10 分钟节流）；openAIDowngradeProbeExitThrottleRetryAfter 是
// 被节流时给前端的建议重试等待。
const (
	openAIDowngradeProbeExitThrottleWindow     = 10 * time.Minute
	openAIDowngradeProbeExitThrottleRetryAfter = 10 * time.Minute
)

// OpenAIProbeExitThrottler 窄可选能力：同出口（exit_ip 或 proxy 桶）近窗是否
// 已有合成探针。真实 repo 实现查询 probe_results；测试桩不实现则节流关闭
// （手动诊断针仍受 runMu 叠针闸保护）。
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
// 路径A 排期提前（调度循环会拾取的号）：schedulable、非 on_duty 态
//
//	（熔断/退避/重探——processState 有专门恢复路径）、qualification、
//	status=error。把 state.NextProbeAt 提前到 now（仅当当前排期晚于 now——
//	CAS 语义：已 due 或刚被另一手动请求提前的号直接返回 already_flying）。
//	扫描循环 ≤1 分钟一拍拾取后走 processState 全链路。熔断冷却/判死退避中
//	提前不等于赦免，状态机迁移语义分毫不动，2 连胜照旧。
//
// 路径B 同步诊断针（ListDue 永不拾取的号）：manual_paused、面板停用/
//
//	非调度 on_duty 等。这些号提前排期是静默失效（ListDue 直接排除），
//	改为当场 runProbe 同步打一针：只落 probe_results 证据行，不碰状态机、
//	不改排期、不摘/复调度——诊断语义，与暂停号不参与调度的既有裁定一致。
//	401/403 也只记录诊断证据，不通过无 CAS 的 SetError 覆写账号状态。
//
// 判定镜像 ListDue L123 的闸门（on_duty 需 schedulable 或 qualification
// 或 error），保证路径A 的号下一拍必被拾取。不重置任何配额/限流状态
// （auto-reset 纪律）。手动针与调度针同构：同一 runProbe → probe 传输/
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
	if !isOpenAIDowngradeProbeAccountEligible(account, now) {
		// Reject accounts outside the probe domain before Ensure: a rejected
		// request must not create durable probe state as a side effect.
		return nil, errOpenAIProbeNotEligible
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
	// 路径判定（镜像 ListDue 全部闸门）：调度循环下一拍会不会拾取此号。
	// manual_paused 在 ListDue 被排除（NOT EXISTS 闸）——schedulable=t 也
	// 拦不住，2026-09-21 修复：旧判定漏了这项，manual_paused+schedulable
	// 的号会走路径A 提前排期，但 ListDue 永不拾取 = accepted 却静默失效。
	// on_duty+非 schedulable+非 qualification+非 error 同样被排除——都走
	// 路径B 同步诊断针。判死号（pending_replace）r17x 选项A 起也被 ListDue
	// 排除：提前排期是静默失效。r17an（2026-09-28 用户裁定「被判死的号也要
	// 可以主动检测」）：判死号走路径B 同步诊断针——只落证据行，不动
	// 状态机/排期/调度；判死语义不变，复活唯一入口仍是 ReenableOpenAIAccount
	// 的认证针。
	if state.State == OpenAIDowngradeStatePendingReplace {
		res, err := r.triggerDiagnosticProbeNow(ctx, account, state, now)
		if err == nil && res != nil && res.Accepted {
			res.ProbedNow = true
		}
		return res, err
	}
	// CanRunOpenAIDowngradeProbe = ListDue 的 controls/状态/expires 三道
	// 闸联判（manual_paused + owned_error + auto_pause_on_expired）。控制读取
	// 失败必须 fail-closed；否则会把未知控制状态误判成路径 A 并返回一个永远
	// 不会被调度器兑现的 accepted。
	allowed, err := r.canRunOpenAIProbe(ctx, accountID)
	if err != nil {
		return nil, err
	}
	manualPaused := !allowed
	schedulerWillPick := !manualPaused && (account.Schedulable ||
		state.State != OpenAIDowngradeStateOnDuty ||
		state.ProbeMode == "qualification" ||
		account.Status == StatusError ||
		isOpenAIRescueAccountActive(account) ||
		// auth 一振暂停（r17aq）：schedulable=false 是探针落的，镜像
		// ListDue 豁免，手动针走路径 A 全状态机（洗白/毕业）。
		state.AuthConsecutiveFailures > 0)
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
	committer, ok := r.store.(OpenAIDowngradeAtomicStore)
	if !ok {
		return nil, ErrOpenAIProbeAtomicStore
	}
	candidate := *state
	candidate.NextProbeAt = now
	candidate.UpdatedAt = now
	mutation := &OpenAIDowngradeMutation{
		AccountID:                accountID,
		ExpectedAccountUpdatedAt: account.UpdatedAt,
		ExpectedStateUpdatedAt:   state.UpdatedAt,
		ExpectedProxyID:          cloneOpenAIProbePointer(account.ProxyID),
		ExpectedStatus:           account.Status,
		ExpectedSchedulable:      account.Schedulable,
		State:                    &candidate,
	}
	if _, err := r.commitOpenAIProbeMutation(ctx, committer, mutation); err != nil {
		if errors.Is(err, ErrOpenAIProbeStale) {
			return nil, errOpenAIProbeGenerationChanged.WithCause(err)
		}
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
	expectedAccountUpdatedAt := account.UpdatedAt
	expectedStateUpdatedAt := state.UpdatedAt
	if !r.runMu.TryLock() {
		return &TriggerProbeNowResult{
			Accepted: false, AlreadyFlying: true, QueuedAt: state.NextProbeAt,
		}, nil
	}
	defer r.runMu.Unlock()

	// Path B holds the scheduler lock, then verifies the exact account and state
	// generations selected before locking. A changed generation is not probed
	// with stale routing or state.
	freshState, err := r.store.GetOpenAIDowngradeState(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	freshAccount, err := r.accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	if freshState == nil || freshAccount == nil ||
		!freshState.UpdatedAt.Equal(expectedStateUpdatedAt) ||
		!freshAccount.UpdatedAt.Equal(expectedAccountUpdatedAt) {
		return nil, errOpenAIProbeGenerationChanged.WithCause(ErrOpenAIProbeStale)
	}
	if !isOpenAIDowngradeProbeAccountEligible(freshAccount, r.now()) {
		return nil, errOpenAIProbeGenerationChanged.WithCause(ErrOpenAIProbeStale)
	}
	allowed, err := r.canRunOpenAIProbe(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	schedulerWillPick := freshState.State != OpenAIDowngradeStatePendingReplace &&
		allowed && (freshAccount.Schedulable ||
		freshState.State != OpenAIDowngradeStateOnDuty ||
		freshState.ProbeMode == "qualification" ||
		freshAccount.Status == StatusError ||
		isOpenAIRescueAccountActive(freshAccount) ||
		// auth 一振暂停（r17aq）：与路径 A 判定镜像，limbo 号由调度器拾取。
		freshState.AuthConsecutiveFailures > 0)
	if schedulerWillPick {
		return nil, errOpenAIProbeGenerationChanged.WithCause(ErrOpenAIProbeStale)
	}
	account, state = freshAccount, freshState

	// 不再看 NextProbeAt 是否 due：路径B 的号（manual_paused/非调度）ListDue
	// 永不拾取，排期一旦落在过去就永久冻结——旧「已 due 即让位」闸把死排期
	// 误读成「有针在飞」，手动针被无限吞（2026-09-21 生产 1131 实锤：17:46
	// 面板暂停后 17:42 的排期死冻，此后每次主动检测都 409 already_flying，
	// 实际零针在飞）。与扫描循环的并发互斥由本函数的 runMu.TryLock 承担
	//（RunOnce 全程持锁），叠针另有同出口 10 分钟节流兜底。
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
	// Diagnostic probes are evidence-only. A 401/403 must not overwrite an
	// account status/error owned by another mechanism.
	if err := r.store.RecordOpenAIDowngradeProbe(ctx, &result); err != nil {
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
// 响应信封附全局 plugin_bridge 区块（救治区标签数据源；桥源未注入/无启用
// 插件/桥读失败时缺席，账号列表不受影响）。
func (r *OpenAIDowngradeProbeRunner) ListOpenAIAccountHealth(ctx context.Context, accountIDs []int64) (*OpenAIAccountHealthListResult, error) {
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
	var bridge *PluginBridgeStatus
	if r.pluginBridge != nil {
		bridge = r.pluginBridge(ctx)
	}
	// 救治区标签数据源（r17ax Phase 3.4）：桥缺席/离线=降级注记；离线时
	// StatusJSON 是最近成功 Health 的缓存（Phase 2 语义），照常解析。
	var prober *OpenAIPluginBridgeProber
	pluginOffline := bridge == nil
	if bridge != nil {
		prober = ParseOpenAIPluginBridgeProber(bridge.StatusJSON)
		pluginOffline = bridge.Offline || !bridge.Running || !bridge.Healthy
	}
	if prober == nil || !prober.Enabled {
		pluginOffline = true
	}
	threshold := r.rescueLane.GraduationThreshold()
	now := r.now()
	out := make([]OpenAIAccountHealth, 0, len(snapshots))
	for i := range snapshots {
		s := snapshots[i]
		label, color, clickable, reason := LabelOpenAIAccountHealth(s)
		var rescue *OpenAIAccountRescueHealth
		// 救治区标签覆盖（r17bb 修正）：rate_limited 是额度事实，仍优先于
		// 救治标签；manual_paused 不再遮蔽——r17ba 后入区即关调度，区里
		// 暂停是「防调用保险丝」而非独立状态，遮住救治中/已复活会让操作员
		// 看不到救治进度（1217 试点：满血号停在灰 paused 上不可点）。暂停
		// 以 reason 后缀注记 + ManualPaused 字段双通道保留。
		if s.RescueMarker != nil && label != OpenAIHealthLabelRateLimited {
			var bridgeAccount *OpenAIPluginBridgeAccount
			if prober != nil {
				bridgeAccount = prober.Accounts[s.AccountID]
			}
			evidence := bridgeAccount
			if pluginOffline {
				evidence = nil
			}
			label, color, clickable, reason = LabelOpenAIRescueAccount(s.RescueMarker, threshold, evidence, s, now)
			if s.ManualPaused {
				reason += "+manual_paused"
			}
			rescue = BuildOpenAIAccountRescueHealth(s.RescueMarker, threshold, bridgeAccount, pluginOffline)
		}
		var rescued *OpenAIAccountRescuedBadge
		if s.RescuedAt != nil {
			rescued = &OpenAIAccountRescuedBadge{At: *s.RescuedAt, Count: s.RescueCount}
		}
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
			Rescue:        rescue,
			Rescued:       rescued,
		})
	}
	return &OpenAIAccountHealthListResult{Accounts: out, PluginBridge: bridge}, nil
}

// GetOpenAIAccountHealth 单号健康快照。
func (r *OpenAIDowngradeProbeRunner) GetOpenAIAccountHealth(ctx context.Context, accountID int64) (*OpenAIAccountHealth, error) {
	list, err := r.ListOpenAIAccountHealth(ctx, []int64{accountID})
	if err != nil {
		return nil, err
	}
	if len(list.Accounts) == 0 {
		return nil, ErrAccountNotFound
	}
	return &list.Accounts[0], nil
}

// OpenAIProbeEvidenceDegraded 最近一针的降智布尔（IsDegraded 的证据行版）：
// HTTP 200 且答错，或答对但 rt<800 → 降智证据。r15e 中性语义：截断指纹叠加
// 答对不判降智——指纹判定需要完整 result，状态机侧 ConsecutiveFailures 已
// 承接；证据行只做展示层复核。非 200（401/429/异常）不构成降智证据。
func OpenAIProbeEvidenceDegraded(ev *OpenAIProbeLastEvidence) bool {
	if ev == nil || !ev.TransportOK || ev.HTTPStatus != 200 || ev.AnswerCorrect == nil {
		return false
	}
	if !*ev.AnswerCorrect {
		return true
	}
	return ev.ReasoningTokens != nil && *ev.ReasoningTokens < OpenAIDowngradeFailureReasoningThreshold
}

// =============================================================================
// 手动启用（相位A 补全，r17x 选项A 2026-09-21 用户裁定）
//
// 判死即终态后，判死号的救援唯一入口：清标签 → 资格认证模式 → 1 针通过
// 即上岗（2026-09-15 裁定）。手动启用不直接复调度——先过认证针再上岗，
// 防止把死透的号直接塞回流量池。
// =============================================================================

// errOpenAIReenableNotDead 只对判死号有意义：其它状态（熔断/在岗/认证中）
// 的号本来就在状态机里自愈，手动启用是误操作。
var errOpenAIReenableNotDead = infraerrors.BadRequest(
	"OPENAI_REENABLE_NOT_DEAD", "account is not in pending_replace state")

// errOpenAIReenablePaused 判死号同时处于 manual_paused（静置救援/暂停观察）
// 且未带 unpause：认证针会被 ListDue 的 manual_paused 闸永远排除（静默
// 失效），且静置中拉回探针节奏违反社区救援剧本（反复测试使惩罚窗升级）。
// r17an（2026-09-28 用户裁定「被判死的号也可以手动启用」）：显式带
// unpause 的请求解除刹车继续走认证针——刹车语义保留（默认不松），但
// 出路打通：解除动作独立留审计事件，不再是面板上无处可解的死锁
// （面板「停用调度」开关与 manual_paused 是影子同步，文案互不指认）。
var errOpenAIReenablePaused = infraerrors.Conflict(
	"OPENAI_REENABLE_PAUSED", "account is manual-paused; retry with unpause")

// errOpenAIReenableBlocked 解除暂停后仍被 CanRun 其它闸挡住（status 闸/
// 过期自动暂停/影子号等）：这时强行 reenable 会静默失效（ListDue 同闸
// 永不拾取），必须明确拒绝并引导人工排查，与「paused」区分开。
var errOpenAIReenableBlocked = infraerrors.Conflict(
	"OPENAI_REENABLE_BLOCKED", "account still not probe-runnable after unpause (status/expired gate)")

// ReenableOpenAIAccountResult 手动启用结果。
type ReenableOpenAIAccountResult struct {
	AccountID   int64     `json:"account_id"`
	ReenabledAt time.Time `json:"reenabled_at"`
	NextProbeAt time.Time `json:"next_probe_at"`
	// ProbeQueued: true = 认证针已排到近刻（下一拍扫描循环拾取）。
	ProbeQueued bool `json:"probe_queued"`
	// Unpaused: true = 本次请求实际解除了 manual_paused 刹车（本来就没
	// 暂停时为 false，不是错误）。
	Unpaused bool `json:"unpaused"`
}

// ReenableOpenAIAccount 手动启用判死号：状态回 qualification、清计数与
// 降智痕迹、排近刻认证针。1 针通过即上岗（qualification 既有语义：
// ConsecutiveSuccesses>=1 + IsQualificationPass → on_duty + SetSchedulable）。
// 不重置配额/限流（auto-reset 纪律）；落审计事件供追溯。
// unpause=true（r17an 2026-09-28 用户裁定）：manual_paused 刹车由本请求
// 显式解除——专用解暂停只清 manual_paused 不动 schedulable（开调度解暂停
// 会把死号直回流量池），解除动作独立落 manual_unpause 审计事件。
func (r *OpenAIDowngradeProbeRunner) ReenableOpenAIAccount(ctx context.Context, accountID int64, unpause bool) (*ReenableOpenAIAccountResult, error) {
	return r.reenableOpenAIAccount(ctx, accountID, unpause, false)
}

// Automatic confirmation never releases a manual pause or customer isolation.
func (r *OpenAIDowngradeProbeRunner) ConfirmOpenAIRescueAccount(ctx context.Context, accountID int64) error {
	if _, err := r.reenableOpenAIAccount(ctx, accountID, false, true); err != nil {
		return err
	}
	r.Wake()
	return nil
}

func (r *OpenAIDowngradeProbeRunner) reenableOpenAIAccount(ctx context.Context, accountID int64, unpause, rescueConfirmation bool) (*ReenableOpenAIAccountResult, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("openai probe runner is not available")
	}
	if r.IsStopped() {
		return nil, errors.New("openai probe runner is stopped")
	}
	committer, ok := r.store.(OpenAIAccountReenableStore)
	if !ok {
		return nil, ErrOpenAIProbeAtomicStore
	}
	now := r.now()
	state, err := r.store.GetOpenAIDowngradeState(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, ErrAccountNotFound
	}
	if state.State != OpenAIDowngradeStatePendingReplace {
		return nil, errOpenAIReenableNotDead
	}
	account, err := r.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, ErrAccountNotFound
	}
	if !isOpenAIDowngradeProbeAccountEligible(account, now) {
		// 改平台/改类型/影子/过期：与 probe-now 同判——探针体系对其无意义。
		return nil, errOpenAIProbeNotEligible
	}
	if rescueConfirmation && (r.rescueLane == nil || !isOpenAIRescueAccountActive(account) ||
		!r.rescueLane.confirmationReady(ctx, account, now)) {
		return nil, ErrRescueRecoveryUnverified
	}
	// Control lookup is an explicit fail-closed preflight. The transaction
	// repeats every gate under locks and distinguishes manual pause from other
	// blockers; this read exists only to reject an unavailable control plane
	// before constructing a candidate.
	if _, err := r.canRunOpenAIProbe(ctx, accountID); err != nil {
		return nil, err
	}

	// 清标签回认证态：降智痕迹归零，但连败预置 1——结论针一击定生死
	//（r17y 2026-09-21 用户裁定）：降智失败针把计数推到 2 直接走既有
	// qualification_failed 判死分支，不再进资格循环反复打针（社区实证：
	// 反复测试使惩罚窗升级 10min→30min→1h→4h）。通过针由 Apply 成功
	// 分支清零计数后上岗，语义不变；无结论针（401/传输故障）计数不动、
	// 5min 重试——传输故障不烧掉唯一一击。
	candidate := *state
	candidate.State = OpenAIDowngradeStateOnDuty
	candidate.ProbeMode = "qualification"
	candidate.ConsecutiveFailures = 1
	candidate.ConsecutiveSuccesses = 0
	candidate.CircuitOpenedAt = nil
	candidate.RecoveryDeadline = nil
	candidate.FirstFailureAt = nil
	candidate.Consecutive429s = 0
	// 认证针排近刻（散布几分钟内），扫描循环下一拍拾取。qualification 态
	// 不受 pending_replace 排除闸影响，ListDue 正常拾取。
	candidate.NextProbeAt = now.Add(r.jitter(openAIDowngradeQualificationInterval))
	if rescueConfirmation {
		candidate.NextProbeAt = now
	}
	candidate.UpdatedAt = now
	unpaused, err := committer.CommitOpenAIAccountReenable(ctx, &OpenAIAccountReenableMutation{
		AccountID:                accountID,
		ExpectedAccountUpdatedAt: account.UpdatedAt,
		ExpectedStateUpdatedAt:   state.UpdatedAt,
		ExpectedProxyID:          cloneOpenAIProbePointer(account.ProxyID),
		ExpectedStatus:           account.Status,
		ExpectedSchedulable:      account.Schedulable,
		AllowSchedulable:         GetOpenAIRescueLaneMarker(account) != nil,
		Unpause:                  unpause,
		ReenabledAt:              now,
		State:                    &candidate,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrOpenAIProbeStale):
			return nil, errOpenAIProbeGenerationChanged.WithCause(err)
		case errors.Is(err, ErrOpenAIReenableNotDead):
			return nil, errOpenAIReenableNotDead
		case errors.Is(err, ErrOpenAIReenablePaused):
			return nil, errOpenAIReenablePaused
		case errors.Is(err, ErrOpenAIReenableBlocked):
			return nil, errOpenAIReenableBlocked
		default:
			return nil, err
		}
	}
	return &ReenableOpenAIAccountResult{
		AccountID:   accountID,
		ReenabledAt: now,
		NextProbeAt: candidate.NextProbeAt,
		ProbeQueued: true,
		Unpaused:    unpaused,
	}, nil
}
