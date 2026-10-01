package service

// 救治区编排器（r17ax Phase 3）：判死号自动进入插件运行的生产内实验台。
// 三入口（自动钩子 / 手动端点 / 对账清扫）汇入同一 EnterRescue 转换：
// 快照原组 → 标记入区 → 绑救治组 → 开调度 → 种子流量 → 事件。
// 设计与通道结论见 openspec/changes/add-account-rescue-lane/design.md 0.3/0.4。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// OpenAIDowngradeEventRescueEntered 判死号进入救治区（三入口共用事件类型，
	// details.trigger 区分 auto|manual|reconcile）。
	OpenAIDowngradeEventRescueEntered = "rescue_entered"
	// OpenAIDowngradeEventRescueGraduated 已复活转正完成（考证通过 + 改绑回原池组）。
	OpenAIDowngradeEventRescueGraduated = "rescue_graduated"
	// OpenAIDowngradeEventRescueSuspected 疑似账号级（插件连错进退避）自动撤调度。
	OpenAIDowngradeEventRescueSuspected = "rescue_suspected_account"
	// OpenAIDowngradeEventRescueRecovered 疑似账号级回暖（退避期满复探过针）
	// 恢复调度，或插件状态丢失（重启）后的陈旧撤调恢复。
	OpenAIDowngradeEventRescueRecovered = "rescue_recovered"

	// 救治区触发源（rescue_entered.details.trigger）。
	OpenAIRescueTriggerAuto      = "auto"
	OpenAIRescueTriggerManual    = "manual"
	OpenAIRescueTriggerReconcile = "reconcile"
)

// Extra 键（与 openai_downgrade_qualification/sol_fallback 同一 Extra 命名空间）：
//   - openai_rescue_lane      救治区成员标记 + 入区快照（转正后整体清除）
//   - openai_rescue_rescued_at    复活徽标：永久血统标记（转正时打，不清除不重置）
//   - openai_rescue_rescue_count  复活次数（每次转正 +1，与徽标同寿命）
const (
	openAIRescueLaneExtraKey      = "openai_rescue_lane"
	openAIRescueRescuedAtExtraKey = "openai_rescue_rescued_at"
	openAIRescueRescueCountKey    = "openai_rescue_rescue_count"
)

var (
	// ErrRescueLaneDisabled 救治区未启用（rescue_lane.enabled=false 是上线默认）。
	ErrRescueLaneDisabled = errors.New("rescue lane is not enabled")
	// ErrRescueLaneNotConfigured 救治组未配置（group id 缺失）。
	ErrRescueLaneNotConfigured = errors.New("rescue lane group is not configured")
	// ErrRescueLaneIneligible 账号不满足入区资格（非 OpenAI OAuth / 影子号 / 已在区）。
	ErrRescueLaneIneligible = errors.New("account is not eligible for rescue lane")
)

const (
	// openAIRescueDefaultCleanPasses 已复活阈值（连过 ≥ 此数）。proposal 默认 6。
	openAIRescueDefaultCleanPasses = 6
	// openAIRescueDefaultReconcileInterval 对账清扫周期（proposal：5min）。
	openAIRescueDefaultReconcileInterval = 5 * time.Minute
)

// OpenAIRescueLaneConfig 救治区配置（settings 装配层注入；缺省安全值）。
type OpenAIRescueLaneConfig struct {
	// Enabled 总开关，上线默认 false。
	Enabled bool
	// GroupID 救治组 id（须预建；命名避开 openai-default/K12/Team/Plus 池组语义）。
	GroupID int64
	// ConsecutiveCleanPasses 已复活标签阈值（插件连过计数 ≥ 此数）。
	ConsecutiveCleanPasses int
	// ReconcileInterval 对账清扫周期。
	ReconcileInterval time.Duration
}

// DefaultOpenAIRescueLaneConfig 安全缺省：关、无组、阈值 6、5min 对账。
func DefaultOpenAIRescueLaneConfig() OpenAIRescueLaneConfig {
	return OpenAIRescueLaneConfig{
		Enabled:                false,
		GroupID:                0,
		ConsecutiveCleanPasses: openAIRescueDefaultCleanPasses,
		ReconcileInterval:      openAIRescueDefaultReconcileInterval,
	}
}

// OpenAIRescueLaneSnapshot 入区快照：account_groups 行会被 BindGroups 删光重插，
// 不存即丢回绑目标。priority 会被重写为 i+1，需还原则存快照值。
type OpenAIRescueLaneSnapshot struct {
	EnteredAt    time.Time `json:"entered_at"`
	Trigger      string    `json:"trigger"`
	OrigGroupIDs []int64   `json:"orig_group_ids"`
	OrigPriority int       `json:"orig_priority"`
}

// OpenAIRescueLaneMarker 是 Extra[openai_rescue_lane] 的解析形态。
type OpenAIRescueLaneMarker = OpenAIRescueLaneSnapshot

// OpenAIRescueLaneEventSink 救治区事件落库（真实现 = 探针 store 的
// AppendOpenAIDowngradeEvent，与状态机事件同表同列）。
type OpenAIRescueLaneEventSink interface {
	AppendOpenAIDowngradeEvent(ctx context.Context, accountID int64, proxyID *int64, eventType string, details map[string]any) error
}

// OpenAIRescueLane 救治区编排器。全部依赖经窄接口/函数注入，装配层
// （wire）负责接 AccountRepository、探针 store 与种子载体。
type OpenAIRescueLane struct {
	accounts AccountRepository
	events   OpenAIRescueLaneEventSink
	// config 由装配层注入（settings 热更新）；nil 时按安全缺省（关）。
	config func() OpenAIRescueLaneConfig
	// seed 种子流量载体 = TestAccountConnection service 直调（Phase 0.1 定案）。
	// nil 时跳过种子（插件探针自愈等第一笔真实流量再起）。
	seed func(ctx context.Context, accountID int64) error
	// probeStates 批量探针状态源（对账清扫用）；nil 时清扫不补进新号。
	probeStates func(ctx context.Context, accountIDs []int64) (map[int64]string, error)
	// bridge 插件桥状态源（task 3.5 撤调/恢复的数据依据）；nil 时清扫不碰
	// 调度撤复（标签照常由健康列表计算，只是不自动撤）。
	bridge func(ctx context.Context) *PluginBridgeStatus
	now    func() time.Time

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

// NewOpenAIRescueLane 构造编排器；config/seed 允许 nil（缺省关/无种子）。
func NewOpenAIRescueLane(
	accounts AccountRepository,
	events OpenAIRescueLaneEventSink,
	config func() OpenAIRescueLaneConfig,
	seed func(ctx context.Context, accountID int64) error,
) *OpenAIRescueLane {
	lane := &OpenAIRescueLane{
		accounts: accounts,
		events:   events,
		config:   config,
		seed:     seed,
		now:      time.Now,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	if lane.config == nil {
		lane.config = DefaultOpenAIRescueLaneConfig
	}
	return lane
}

// NewOpenAIRescueLaneSeedAdapter 把 AccountTestService 包成种子载体
//（design 0.1 定案：TestAccountConnection service 直调，OAuth 分支目标与
// 业务转发同 URL，经插件 Forward 流给探针喂模板；不校验账号死活）。
// 复用 RunTestBackground 的内存 gin 伪造与 SSE 结果解析。nil 服务返回 nil
//（编排器按无种子运行，插件探针自愈等真实流量再起）。
func NewOpenAIRescueLaneSeedAdapter(
	testService *AccountTestService,
) func(ctx context.Context, accountID int64) error {
	if testService == nil {
		return nil
	}
	return func(ctx context.Context, accountID int64) error {
		result, err := testService.RunTestBackground(ctx, accountID, "")
		if err != nil {
			return err
		}
		if result != nil && result.Status != "success" {
			return fmt.Errorf("rescue seed test %s: %s", result.Status, result.ErrorMessage)
		}
		return nil
	}
}

// ShouldAutoEnterRescueLane 自动钩子入口过滤器（design 0.3 精确规则，纯函数）：
//   - 非 pending_replace 提交 → 不进
//   - mutation.Results 复核含 auth_error（401/403 类）→ 排除（凭据问题交给
//     auth 两振出局线，救治区救不了账号级封禁）
//   - account.RateLimitResetAt 在未来 → 排除（429 不判死；限流持有中不进区）
//   - 已在区（Extra 标记在场）→ 幂等不进
func ShouldAutoEnterRescueLane(
	mutation *OpenAIDowngradeMutation,
	account *Account,
	now time.Time,
) bool {
	if mutation == nil || mutation.State == nil {
		return false
	}
	if mutation.State.State != OpenAIDowngradeStatePendingReplace {
		return false
	}
	for i := range mutation.Results {
		if ClassifyOpenAIDowngradeProxyOutcome(&mutation.Results[i]) == OpenAIProxyOutcomeAuthError {
			return false
		}
	}
	if account != nil && account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
		return false
	}
	if account != nil && GetOpenAIRescueLaneMarker(account) != nil {
		return false
	}
	return true
}

// GetOpenAIRescueLaneMarker 解析账号 Extra 里的救治区标记；不在区返回 nil。
// 容错读：Extra 值经 JSON 往返（[]any/float64/string 都可能出现）。
func GetOpenAIRescueLaneMarker(account *Account) *OpenAIRescueLaneMarker {
	if account == nil || len(account.Extra) == 0 {
		return nil
	}
	raw, ok := account.Extra[openAIRescueLaneExtraKey]
	if !ok || raw == nil {
		return nil
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return parseOpenAIRescueLaneMarkerFields(fields)
}

// ParseOpenAIRescueLaneMarkerJSON 从 JSON 串解析标记（健康快照聚合的仓库
// 通道：extra->'openai_rescue_lane' 原文列）。空/坏 JSON → nil，与 Extra
// 容错读同一语义。
func ParseOpenAIRescueLaneMarkerJSON(raw string) *OpenAIRescueLaneMarker {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(trimmed), &fields); err != nil {
		return nil
	}
	return parseOpenAIRescueLaneMarkerFields(fields)
}

// parseOpenAIRescueLaneMarkerFields 标记字段提取核心（Extra map 与 JSON 串
// 两通道共用）。entered_at 缺失/非法 → nil（入区时间是不可缺字段）。
func parseOpenAIRescueLaneMarkerFields(fields map[string]any) *OpenAIRescueLaneMarker {
	marker := &OpenAIRescueLaneMarker{}
	if enteredAt, ok := parseOpenAIRescueTimeString(fields["entered_at"]); ok {
		marker.EnteredAt = enteredAt
	} else {
		return nil
	}
	if trigger, ok := fields["trigger"].(string); ok {
		marker.Trigger = trigger
	}
	marker.OrigGroupIDs = parseOpenAIRescueInt64Slice(fields["orig_group_ids"])
	if priority, ok := parseOpenAIRescueInt(fields["orig_priority"]); ok {
		marker.OrigPriority = int(priority)
	}
	return marker
}

// rescueLaneMarkerExtraValue 构造可写入 Extra 的标记值（时间用 RFC3339 字符串，
// 与读侧容错解析配对）。
func rescueLaneMarkerExtraValue(marker OpenAIRescueLaneMarker) map[string]any {
	return map[string]any{
		"entered_at":     marker.EnteredAt.UTC().Format(time.RFC3339),
		"trigger":        marker.Trigger,
		"orig_group_ids": marker.OrigGroupIDs,
		"orig_priority":  marker.OrigPriority,
	}
}

// parseOpenAIRescueTimeString 容错解析时间（RFC3339 字符串）。
func parseOpenAIRescueTimeString(value any) (time.Time, bool) {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// parseOpenAIRescueInt 容错解析整数（JSON 往返后是 float64）。
func parseOpenAIRescueInt(value any) (int64, bool) {
	switch n := value.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case json.Number:
		parsed, err := strconv.ParseInt(n.String(), 10, 64)
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return parsed, err == nil
	}
	return 0, false
}

// parseOpenAIRescueInt64Slice 容错解析 []int64（[]int64 或 []any 混合数字）。
func parseOpenAIRescueInt64Slice(value any) []int64 {
	switch typed := value.(type) {
	case []int64:
		return typed
	case []any:
		out := make([]int64, 0, len(typed))
		for _, item := range typed {
			if n, ok := parseOpenAIRescueInt(item); ok {
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}

// MaybeAutoEnterRescue 自动钩子入口（task 3.2，commit 通道 committed 块调用）：
// 先按 design 0.3 规则过滤（非判死/401 复核/限流持有/已在区都不进），
// 过滤通过才走 EnterRescue。开关未开时零成本跳过——上线默认关。
// account 为提交后的账号快照（供限流复核）；nil 时按无账号证据过滤
// （401 复核仍有效，漏掉的限流持有由对账清扫兜底）。
func (l *OpenAIRescueLane) MaybeAutoEnterRescue(
	ctx context.Context,
	mutation *OpenAIDowngradeMutation,
	account *Account,
) {
	if l == nil || mutation == nil {
		return
	}
	if !l.config().Enabled {
		return
	}
	if !ShouldAutoEnterRescueLane(mutation, account, l.now()) {
		return
	}
	if err := l.EnterRescue(ctx, mutation.AccountID, OpenAIRescueTriggerAuto); err != nil {
		// 判死提交本身已落库：入区失败只记日志，绝不反向影响探针通道。
		// 幂等拒绝（已在区）与开关态不是错误；瞬时失败由对账清扫兜底。
		if errors.Is(err, ErrRescueLaneDisabled) || errors.Is(err, ErrRescueLaneIneligible) {
			return
		}
		slog.Warn("openai_rescue_auto_enter_failed",
			"account_id", mutation.AccountID, "error", err)
	}
}

// EnterRescue 三入口共用的入区转换（design 0.4 通道 + CAS 纪律）：
//  1. 开关/配置闸（enabled=false 直接拒绝——上线默认关）
//  2. 重读账号：资格（OpenAI OAuth 非影子）+ 幂等（已在区 → nil 不重复入）
//  3. 快照原组 id+priority（BindGroups 会删光 account_groups，不存即丢）
//  4. 先打标记再改绑（标记-first：中途崩溃对账可补绑；绑-first 无标记=隐形）
//  5. BindGroups 绑救治组（事务删光重插，触发器双发 outbox）
//  6. 重读账号（BindGroups 推 updated_at 代际）→ SetSchedulable(true)
//  7. 种子流量（TestAccountConnection 直调；失败只记账不回滚——插件探针
//     自愈等真实流量再起，回滚反而丢已入区状态）
//  8. rescue_entered 事件（trigger 区分入口）
func (l *OpenAIRescueLane) EnterRescue(ctx context.Context, accountID int64, trigger string) error {
	if l == nil {
		return errors.New("rescue lane is not available")
	}
	cfg := l.config()
	if !cfg.Enabled {
		return ErrRescueLaneDisabled
	}
	if cfg.GroupID <= 0 {
		return ErrRescueLaneNotConfigured
	}
	now := l.now().UTC()

	account, err := l.accounts.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if !isOpenAIDowngradeProbeAccountEligible(account, now) {
		return ErrRescueLaneIneligible
	}
	if GetOpenAIRescueLaneMarker(account) != nil {
		// 幂等：已在区（对账清扫与自动钩子可能同号并发）。
		return nil
	}
	marker := OpenAIRescueLaneMarker{
		EnteredAt:    now,
		Trigger:      trigger,
		OrigGroupIDs: account.GroupIDs,
		OrigPriority: account.Priority,
	}
	if err := l.accounts.UpdateExtra(ctx, accountID, map[string]any{
		openAIRescueLaneExtraKey: rescueLaneMarkerExtraValue(marker),
	}); err != nil {
		return err
	}
	if err := l.accounts.BindGroups(ctx, accountID, []int64{cfg.GroupID}); err != nil {
		return fmt.Errorf("bind rescue group: %w", err)
	}
	// CAS 纪律（design 0.4）：BindGroups 推走 updated_at 代际，改调度前重读。
	fresh, err := l.accounts.GetByID(ctx, accountID)
	if err != nil {
		return fmt.Errorf("reread after bind: %w", err)
	}
	if !fresh.Schedulable {
		if err := l.accounts.SetSchedulable(ctx, accountID, true); err != nil {
			return fmt.Errorf("enable scheduling: %w", err)
		}
	}

	seedOK := true
	if l.seed != nil {
		if err := l.seed(ctx, accountID); err != nil {
			seedOK = false
			slog.Warn("openai_rescue_seed_failed",
				"account_id", accountID, "trigger", trigger, "error", err)
		}
	}
	if l.events != nil {
		details := map[string]any{
			"trigger":        trigger,
			"orig_group_ids": marker.OrigGroupIDs,
			"orig_priority":  marker.OrigPriority,
			"seed_ok":        seedOK,
		}
		if err := l.events.AppendOpenAIDowngradeEvent(ctx, accountID, account.ProxyID,
			OpenAIDowngradeEventRescueEntered, details); err != nil {
			return fmt.Errorf("record rescue_entered: %w", err)
		}
	}
	return nil
}

// ---------- 对账清扫（task 3.3：周期扫描够格未进区→补进；标记-first 崩溃窗自愈） ----------

// openAIRescueSuspectedExtraKey 疑似账号级撤调标记（task 3.5 写入；清扫侧
// 只读）。在场时清扫不得复活调度——撤调是插件 backoff 证据下的有意状态，
// 不是崩溃残留。
const openAIRescueSuspectedExtraKey = "openai_rescue_suspected"

// GetOpenAIRescueSuspected 账号是否处于救治区疑似账号级撤调态（容错读）。
func GetOpenAIRescueSuspected(account *Account) bool {
	if account == nil || len(account.Extra) == 0 {
		return false
	}
	value, ok := account.Extra[openAIRescueSuspectedExtraKey]
	if !ok {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	}
	return false
}

// SetProbeStateSource 注入批量探针状态源（真实现 = 探针仓库的健康快照批量
// 查询）。未注入时清扫只做标记侧自愈，不补进新号（自动钩子照常工作）。
func (l *OpenAIRescueLane) SetProbeStateSource(
	fn func(ctx context.Context, accountIDs []int64) (map[int64]string, error),
) {
	if l == nil {
		return
	}
	l.probeStates = fn
}

// SetBridgeSource 注入插件桥状态源（真实现 = PluginManager.BridgeStatus，
// 与健康列表同源）。未注入时清扫不做撤调/恢复（3.5 语义退化为只标签）。
func (l *OpenAIRescueLane) SetBridgeSource(fn func(ctx context.Context) *PluginBridgeStatus) {
	if l == nil {
		return
	}
	l.bridge = fn
}

// Start 启动对账清扫循环（自 Provider 调用；与探针 runner 同生命周期）。
// 循环体每轮重读配置：开关未开时按间隔空转（3.8 settings 热更新即生效）。
func (l *OpenAIRescueLane) Start() {
	if l == nil {
		return
	}
	l.startOnce.Do(func() {
		go l.sweepLoop()
	})
}

// Stop 终止清扫循环（清理件调用；幂等）。
func (l *OpenAIRescueLane) Stop() {
	if l == nil {
		return
	}
	l.stopOnce.Do(func() {
		close(l.stopCh)
	})
}

func (l *OpenAIRescueLane) sweepLoop() {
	defer close(l.doneCh)
	for {
		interval := l.config().ReconcileInterval
		if interval <= 0 {
			interval = openAIRescueDefaultReconcileInterval
		}
		// 正向散布（1.0x-1.25x）：清扫是周期性批量动作，固定整点会与探针
		// 扫描、其它清理器形成可观察的同步节律。
		wait := time.Duration(float64(interval) * (1 + 0.25*probeRandomFloat()))
		select {
		case <-time.After(wait):
		case <-l.stopCh:
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		entered, healed, withdrawn, err := l.RunReconcileSweep(ctx)
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("openai_rescue_sweep_failed", "error", err)
		}
		if entered > 0 || healed > 0 || withdrawn > 0 {
			slog.Info("openai_rescue_sweep_done",
				"entered", entered, "healed", healed, "withdrawn", withdrawn)
		}
	}
}

// RunReconcileSweep 一轮对账清扫（公开以便测试与启动确定性检查）：
//   - 补进：OpenAI 平台、资格通过、status=active（auth 两振出局走 SetError，
//     status!=active 天然排除凭据死）、无标记、探针态=pending_replace、限流
//     未持有 → EnterRescue{trigger: reconcile}
//   - 自愈：标记在场（已入区）但救治组绑定丢失或调度未开（标记-first 崩溃
//     窗口）→ 补绑/补开。疑似账号级撤调标记在场时不复活调度（3.5 语义）。
//   - 撤调/恢复（task 3.5）：在区账号按插件桥证据——in_backoff → 撤调度+
//     疑似标记+事件（幂等：标记在场不重撤）；疑似标记在场且回暖（退避后
//     新过针）或插件状态丢失（账号从桥消失）→ 清标记+恢复调度+事件。
//     桥缺席/解析失败 → 不碰调度撤复（无证据不动状态机）。
func (l *OpenAIRescueLane) RunReconcileSweep(ctx context.Context) (entered, healed, withdrawn int, err error) {
	if l == nil {
		return 0, 0, 0, nil
	}
	cfg := l.config()
	if !cfg.Enabled || cfg.GroupID <= 0 {
		return 0, 0, 0, nil
	}
	now := l.now()
	accounts, err := l.accounts.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return 0, 0, 0, err
	}
	// 桥证据：拿到 PluginBridgeStatus（含离线时的最近成功缓存）才有 per-账号
	// 证据；prober==nil（空/坏 JSON）按无证据处理（只做自愈，不撤不恢复）。
	var prober *OpenAIPluginBridgeProber
	haveBridge := false
	if l.bridge != nil {
		if bridge := l.bridge(ctx); bridge != nil {
			prober = ParseOpenAIPluginBridgeProber(bridge.StatusJSON)
			haveBridge = true
		}
	}
	var candidateIDs []int64
	for i := range accounts {
		if err := ctx.Err(); err != nil {
			return entered, healed, withdrawn, err
		}
		account := &accounts[i]
		if !isOpenAIDowngradeProbeAccountEligible(account, now) {
			continue
		}
		if GetOpenAIRescueLaneMarker(account) != nil {
			// 已入区：绑定自愈 + 撤调/恢复（桥证据），不重复入区。
			bindingOK := false
			for _, id := range account.GroupIDs {
				if id == cfg.GroupID {
					bindingOK = true
					break
				}
			}
			if !bindingOK {
				if err := l.accounts.BindGroups(ctx, account.ID, []int64{cfg.GroupID}); err != nil {
					slog.Warn("openai_rescue_sweep_heal_bind_failed",
						"account_id", account.ID, "error", err)
				} else {
					healed++
				}
			}
			if haveBridge {
				var bridgeAccount *OpenAIPluginBridgeAccount
				if prober != nil {
					bridgeAccount = prober.Accounts[account.ID]
				}
				// 撤调优先于调度自愈：连错进退避的号先停烧额度。
				if bridgeAccount != nil && bridgeAccount.InBackoff && !GetOpenAIRescueSuspected(account) {
					if l.withdrawScheduling(ctx, account, bridgeAccount) {
						withdrawn++
					}
					continue
				}
				if GetOpenAIRescueSuspected(account) {
					if recovered, basis := rescueLaneRecoveryEvidence(bridgeAccount); recovered {
						if l.restoreScheduling(ctx, account, bridgeAccount, basis) {
							healed++
						}
					}
					// 无回暖证据：维持撤调（有意状态，不是崩溃残留）。
					continue
				}
			}
			if !account.Schedulable && !GetOpenAIRescueSuspected(account) {
				if err := l.accounts.SetSchedulable(ctx, account.ID, true); err != nil {
					slog.Warn("openai_rescue_sweep_heal_sched_failed",
						"account_id", account.ID, "error", err)
				} else {
					healed++
				}
			}
			continue
		}
		// 凭据死（auth 两振 SetError）与禁用号不补进；限流持有中本轮跳过。
		if account.Status != StatusActive {
			continue
		}
		if account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
			continue
		}
		candidateIDs = append(candidateIDs, account.ID)
	}
	if len(candidateIDs) == 0 || l.probeStates == nil {
		return entered, healed, withdrawn, nil
	}
	states, err := l.probeStates(ctx, candidateIDs)
	if err != nil {
		return entered, healed, withdrawn, err
	}
	for _, id := range candidateIDs {
		if states[id] != OpenAIDowngradeStatePendingReplace {
			continue
		}
		if enterErr := l.EnterRescue(ctx, id, OpenAIRescueTriggerReconcile); enterErr != nil {
			// 单号失败（含幂等跳过）不阻断整轮；瞬时失败下轮重试。
			if !errors.Is(enterErr, ErrRescueLaneIneligible) {
				slog.Warn("openai_rescue_sweep_enter_failed", "account_id", id, "error", enterErr)
			}
			continue
		}
		entered++
	}
	return entered, healed, withdrawn, nil
}

// rescueLaneRecoveryEvidence 疑似撤调的回暖证据裁决（纯函数）：
//   - bridgeAccount==nil：账号从桥消失 = 插件状态丢失（重启/清罐），撤调
//     依据已不存在，按陈旧撤调恢复——否则永钉死；
//   - 退避进门时插件把连过清零：consecutive_passes>0 = 退避期满后的新过针
//     （suspect 旗标随 pass 清除）→ 真回暖；
//   - 退避中/仍标记 suspect/尚无新过针 → 无证据，维持撤调等下一轮。
func rescueLaneRecoveryEvidence(bridgeAccount *OpenAIPluginBridgeAccount) (bool, string) {
	if bridgeAccount == nil {
		return true, "plugin_state_lost"
	}
	if bridgeAccount.InBackoff || bridgeAccount.SuspectAccountLevel {
		return false, ""
	}
	if bridgeAccount.ConsecutivePasses > 0 {
		return true, "plugin_pass"
	}
	return false, ""
}

// withdrawScheduling 疑似账号级撤调度（proposal 出口表：连错进退避→停烧
// 额度）：撤调度 → 疑似标记 → 事件。前一步失败不写后一步（下轮整组重试），
// 保证「撤了调度必有标记」的不变式（标记在场=调度已撤）。
func (l *OpenAIRescueLane) withdrawScheduling(
	ctx context.Context, account *Account, bridgeAccount *OpenAIPluginBridgeAccount,
) bool {
	if err := l.accounts.SetSchedulable(ctx, account.ID, false); err != nil {
		slog.Warn("openai_rescue_withdraw_sched_failed",
			"account_id", account.ID, "error", err)
		return false
	}
	if err := l.accounts.UpdateExtra(ctx, account.ID, map[string]any{
		openAIRescueSuspectedExtraKey: true,
	}); err != nil {
		slog.Warn("openai_rescue_withdraw_mark_failed",
			"account_id", account.ID, "error", err)
		return false
	}
	if l.events != nil {
		details := map[string]any{
			"trigger":      "plugin_backoff",
			"consec_fails": bridgeAccount.ConsecFails,
		}
		if !bridgeAccount.BackoffUntil.IsZero() {
			details["backoff_until"] = bridgeAccount.BackoffUntil.UTC().Format(time.RFC3339)
		}
		if err := l.events.AppendOpenAIDowngradeEvent(ctx, account.ID, account.ProxyID,
			OpenAIDowngradeEventRescueSuspected, details); err != nil {
			slog.Warn("openai_rescue_withdraw_event_failed",
				"account_id", account.ID, "error", err)
		}
	}
	slog.Info("openai_rescue_withdrawn",
		"account_id", account.ID, "consec_fails", bridgeAccount.ConsecFails)
	return true
}

// restoreScheduling 疑似撤调的回暖恢复：清疑似标记 → 恢复调度 → 事件
// （basis=plugin_pass 真回暖 / plugin_state_lost 插件状态丢失兜底）。
func (l *OpenAIRescueLane) restoreScheduling(
	ctx context.Context, account *Account, bridgeAccount *OpenAIPluginBridgeAccount, basis string,
) bool {
	if err := l.accounts.UpdateExtra(ctx, account.ID, map[string]any{
		openAIRescueSuspectedExtraKey: false,
	}); err != nil {
		slog.Warn("openai_rescue_restore_mark_failed",
			"account_id", account.ID, "error", err)
		return false
	}
	if err := l.accounts.SetSchedulable(ctx, account.ID, true); err != nil {
		slog.Warn("openai_rescue_restore_sched_failed",
			"account_id", account.ID, "error", err)
		return false
	}
	if l.events != nil {
		details := map[string]any{"basis": basis}
		if bridgeAccount != nil {
			details["consecutive_passes"] = bridgeAccount.ConsecutivePasses
		}
		if err := l.events.AppendOpenAIDowngradeEvent(ctx, account.ID, account.ProxyID,
			OpenAIDowngradeEventRescueRecovered, details); err != nil {
			slog.Warn("openai_rescue_restore_event_failed",
				"account_id", account.ID, "error", err)
		}
	}
	slog.Info("openai_rescue_restored", "account_id", account.ID, "basis", basis)
	return true
}
