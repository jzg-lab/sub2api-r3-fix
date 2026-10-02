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

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
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
	// OpenAIDowngradeEventRescueAuthRejected 种子流量吃凭据级拒绝（401/403/
	// token 吊销类）自动出区：cookie 插件治不了 OAuth 令牌本身，号回判死
	// 原位（问题号标签），人工走删号重授权。
	OpenAIDowngradeEventRescueAuthRejected = "rescue_auth_rejected"

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
	// openAIRescueAuthRejectedAtExtraKey 凭据级出区冷却戳（RFC3339）：
	// 出区时打、成功入区时清；窗内自动补进被压住（见冷却窗常量注释）。
	openAIRescueAuthRejectedAtExtraKey = "openai_rescue_auth_rejected_at"
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
	// openAIRescueDefaultCleanPasses 已复活阈值（连过 ≥ 此数）。proposal 默认
	// 6；r17bb 降为 3，与插件 0.3.2 密集档退出线（probe_burst_until_passes=3）
	// 对齐——插件侧攒满 3 连过后若还要再等 3 针才毕业，密集档退回稳态档的
	// 稀疏间隔会把上岗拖长数十分钟。settings 的 ConsecutiveCleanPasses 覆盖
	// 仍有效。
	openAIRescueDefaultCleanPasses = 3
	// openAIRescueDefaultReconcileInterval 对账清扫周期（proposal：5min）。
	openAIRescueDefaultReconcileInterval = 5 * time.Minute
	// openAIRescueSeedMaxAttempts 连续种子失败上限（成功清零）。超限后不再
	// 空打——凭据级失败早已出区，非凭据失败说明上游/链路本身有问题，等
	// 插件真实流量兜底。
	openAIRescueSeedMaxAttempts = 5
	// openAIRescueReseedInterval 插件失忆补种节流：插件探针模板在内存里，
	// 进程换代即丢（fork 宿主无 KV 持久化）。种子喂上模板后插件最多一个
	// probe interval 才出首针进 prober 视图，此窗内不重复补种（15min 盖住
	// 生产默认 interval=900s；更长 interval 的窗内最多每小时 4 发，良性）。
	openAIRescueReseedInterval = 15 * time.Minute
	// openAIRescueAutoNeedleCooldown 自动资格针冷却（r17bb）：针失败（r17y
	// 一击回判死）后到下次自动重试的最小间隔。10min 盖住两轮清扫 + 针
	// qualification 节奏，失败重试有呼吸窗。
	openAIRescueAutoNeedleCooldown = 10 * time.Minute
	// openAIRescueAutoNeedleMaxAttempts 自动资格针触发上限（每账号每轮救治；
	// 毕业清标记即重置）。达上限后停自动留人工——插件证据仍在累积，手动
	// 毕业入口不受影响。
	openAIRescueAutoNeedleMaxAttempts = 3
	// openAIRescueExitAuthRejected 出区原因：种子流量吃凭据级拒绝
	// （401/403/token 吊销类）。cookie 插件治不了 OAuth 令牌本身。
	openAIRescueExitAuthRejected = "auth_rejected"
	// openAIRescueAuthRejectReentryCooldown 凭据级出区后的自动补进冷却窗：
	// 出区即清标记，若无冷却戳，Status 仍 Active 的判死号会被清扫下一轮
	// 立刻再补进 → 种子 401 → 再出区，每 5 分钟空转一圈。戳在窗内时自动
	// 钩子与清扫补进都跳过；手动入口（送入实验台）不受限——人工重试明志。
	openAIRescueAuthRejectReentryCooldown = 24 * time.Hour
)

var (
	// ErrRescueSeedAuthRejected 入区/补种子吃凭据级拒绝后已自动出区（号回
	// 判死原位）。自动钩子与清扫按已处理忽略；手动入口透传给管理端展示。
	ErrRescueSeedAuthRejected = infraerrors.Conflict(
		"OPENAI_RESCUE_SEED_AUTH_REJECTED",
		"rescue seed was rejected by upstream credentials (401/403): the OAuth token problem cannot be healed by the rescue lane; account returned to pending_replace",
	)
)

// OpenAIRescueLaneScheduler 救治区专用调度通道（r17ax 引入；r17ba 起入区
// 不再开调度，编排器不再消费——仓库实现保留作手动运维口）。
type OpenAIRescueLaneScheduler interface {
	SetSchedulableInRescueLane(ctx context.Context, accountID int64, rescueGroupID int64, schedulable bool) error
}

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
	// SeedOK 种子流量已成功打过至少一次（插件探针模板已喂上；清扫据此
	// 判断是否补种子）。注意：种子成功 ≠ 模板永在——插件进程换代会丢光
	// 内存模板，清扫按桥证据（prober 未跟踪）触发补种（r17ba）。
	SeedOK bool `json:"seed_ok,omitempty"`
	// SeedAttempts 连续种子失败次数（成功即清零；达 openAIRescueSeedMaxAttempts
	// 后清扫不再补种子——持续失败的号留在区里等插件真实流量，不无限空打）。
	SeedAttempts int `json:"seed_attempts,omitempty"`
	// LastSeedAt 最近一次种子尝试时刻（补种节流：模板喂上后插件最多一个
	// interval 才出首针，这窗内不重复补种）。
	LastSeedAt time.Time `json:"last_seed_at,omitempty"`
	// AutoNeedleAt 最近一次自动资格针触发时刻（r17bb 冷却节流：针失败 r17y
	// 一击回判死后，等冷却再重试，不每轮清扫空打）。
	AutoNeedleAt time.Time `json:"auto_needle_at,omitempty"`
	// AutoNeedleAttempts 自动资格针连续触发次数（针通过→转正收敛会清整个
	// 标记，无需手动归零；达 openAIRescueAutoNeedleMaxAttempts 后停自动，
	// 留人工处置——插件证据继续累积，手动毕业入口随时可用）。
	AutoNeedleAttempts int `json:"auto_needle_attempts,omitempty"`
	// ExitReason 非空 = 出区中（唯一现值 auth_rejected：种子吃凭据级拒绝）。
	// 清扫见到即续走出区，不做绑定/调度自愈（否则与出区意图打架）。
	ExitReason string `json:"exit_reason,omitempty"`
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
	// probeStates 批量探针健康快照源（对账清扫用：补进判死复核 + 转正收敛
	// 都需要 State+ProbeMode）；nil 时清扫不补进新号、不做转正收敛。
	probeStates func(ctx context.Context, accountIDs []int64) (map[int64]OpenAIProbeHealthSnapshot, error)
	// bridge 插件桥状态源（task 3.5 撤调/恢复的数据依据）；nil 时清扫不碰
	// 调度撤复（标签照常由健康列表计算，只是不自动撤）。
	bridge func(ctx context.Context) *PluginBridgeStatus
	// schedulingGate 调度闸预检（r17ba；真实现 = CanRunOpenAIDowngradeProbe，
	// 镜像 ListDue 的 manual_paused/owned_error 排除闸）。唯一消费点=清扫
	// 补进的候选过滤：手动暂停（静置刹车，r17an 语义）的判死号不被清扫
	// 强拉入区。入区/在区不再有任何开调度动作（用户裁定：唯一开调度点=
	// 考证通过后的资格完成），此闸与调度开无关。nil = 无闸（测试桩）。
	schedulingGate func(ctx context.Context, accountID int64) (bool, error)
	// needleTrigger 自动资格针载体（r17bb；真实现 = 探针 runner 的
	// ReenableOpenAIAccount(unpause=true)）。插件连过达阈值的在区判死号
	// 由清扫自动打针；nil 时清扫不自动打（转正收敛与手动毕业不受影响）。
	needleTrigger func(ctx context.Context, accountID int64) error
	now           func() time.Time

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
// （design 0.1 定案：TestAccountConnection service 直调，OAuth 分支目标与
// 业务转发同 URL，经插件 Forward 流给探针喂模板；不校验账号死活）。
// 复用 RunTestBackground 的内存 gin 伪造与 SSE 结果解析。nil 服务返回 nil
// （编排器按无种子运行，插件探针自愈等真实流量再起）。
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
	if account != nil && OpenAIRescueAuthRejectSuppressed(account, now) {
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
	marker.SeedOK = parseOpenAIRescueBool(fields["seed_ok"])
	if attempts, ok := parseOpenAIRescueInt(fields["seed_attempts"]); ok && attempts > 0 {
		marker.SeedAttempts = int(attempts)
	}
	if lastSeedAt, ok := parseOpenAIRescueTimeString(fields["last_seed_at"]); ok {
		marker.LastSeedAt = lastSeedAt
	}
	if autoNeedleAt, ok := parseOpenAIRescueTimeString(fields["auto_needle_at"]); ok {
		marker.AutoNeedleAt = autoNeedleAt
	}
	if attempts, ok := parseOpenAIRescueInt(fields["auto_needle_attempts"]); ok && attempts > 0 {
		marker.AutoNeedleAttempts = int(attempts)
	}
	if reason, ok := fields["exit_reason"].(string); ok {
		marker.ExitReason = reason
	}
	return marker
}

// parseOpenAIRescueBool 容错解析布尔（JSON 往返后可能是 bool/string）。
func parseOpenAIRescueBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	}
	return false
}

// rescueLaneMarkerExtraValue 构造可写入 Extra 的标记值（时间用 RFC3339 字符串，
// 与读侧容错解析配对）。
func rescueLaneMarkerExtraValue(marker OpenAIRescueLaneMarker) map[string]any {
	value := map[string]any{
		"entered_at":     marker.EnteredAt.UTC().Format(time.RFC3339),
		"trigger":        marker.Trigger,
		"orig_group_ids": marker.OrigGroupIDs,
		"orig_priority":  marker.OrigPriority,
	}
	// 可选字段只在非零值时写入（保持老标记 JSON 形态稳定，减少无谓代际）。
	if marker.SeedOK {
		value["seed_ok"] = true
	}
	if marker.SeedAttempts > 0 {
		value["seed_attempts"] = marker.SeedAttempts
	}
	if !marker.LastSeedAt.IsZero() {
		value["last_seed_at"] = marker.LastSeedAt.UTC().Format(time.RFC3339)
	}
	if !marker.AutoNeedleAt.IsZero() {
		value["auto_needle_at"] = marker.AutoNeedleAt.UTC().Format(time.RFC3339)
	}
	if marker.AutoNeedleAttempts > 0 {
		value["auto_needle_attempts"] = marker.AutoNeedleAttempts
	}
	if marker.ExitReason != "" {
		value["exit_reason"] = marker.ExitReason
	}
	return value
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
		// 幂等拒绝（已在区）与开关态不是错误；凭据级拒绝已出区（种子
		// 401/403 类，cookie 插件治不了）；瞬时失败由对账清扫兜底。
		if errors.Is(err, ErrRescueLaneDisabled) || errors.Is(err, ErrRescueLaneIneligible) ||
			errors.Is(err, ErrRescueSeedAuthRejected) {
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
//  6. 调度强制关（r17ba：在区一律不调度，判死前在岗残留也关；唯一开
//     调度点=考证通过后的资格完成）
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
		// 上一轮凭据级出区的冷却戳随入区一并清除（手动重试明志；坏值同清）。
		openAIRescueAuthRejectedAtExtraKey: nil,
	}); err != nil {
		return err
	}
	if err := l.accounts.BindGroups(ctx, accountID, []int64{cfg.GroupID}); err != nil {
		return fmt.Errorf("bind rescue group: %w", err)
	}
	// 在区一律不调度（r17ba 用户裁定「没确认救活绝不进正式调用」）：入区
	// 不但不开调度，判死前在岗残留的 schedulable=true 也强制关。全流程唯一
	// 开调度点 = 复活点击 → 考证针通过 → 资格完成 SetSchedulable(true)
	//（即「检测通过自动启用」），随后转正回绑原池。救治证据链（种子=服务层
	// 直调、插件探针=插件自有通道）都不经宿主调度，关调度零代价。
	if account.Schedulable {
		if err := l.accounts.SetSchedulable(ctx, accountID, false); err != nil {
			return fmt.Errorf("disable scheduling in lane: %w", err)
		}
	}

	seedErr := l.seedAndAccount(ctx, account, trigger)
	if seedErr != nil {
		if errors.Is(seedErr, ErrRescueSeedAuthRejected) {
			// 凭据级死（401/403/token 吊销）：插件治不了 OAuth 令牌。出区
			// 已在 seedAndAccount 内完成（号回判死原位），入区不算失败也
			// 不算成功——调用方（自动钩子/清扫）忽略，手动入口透传展示。
			return seedErr
		}
		// 非凭据失败：留在区里（插件真实流量/下轮清扫补种子兜底）。
	}
	if l.events != nil {
		details := map[string]any{
			"trigger":        trigger,
			"orig_group_ids": marker.OrigGroupIDs,
			"orig_priority":  marker.OrigPriority,
			"seed_ok":        seedErr == nil,
		}
		if seedErr != nil {
			details["seed_error"] = seedErr.Error()
		}
		if err := l.events.AppendOpenAIDowngradeEvent(ctx, accountID, account.ProxyID,
			OpenAIDowngradeEventRescueEntered, details); err != nil {
			return fmt.Errorf("record rescue_entered: %w", err)
		}
	}
	return nil
}

// seedAndAccount 打种子并把结果记账进标记（SeedOK/SeedAttempts，清扫补
// 种子的依据）。凭据级拒绝（401/403/token 吊销类）时完成出区并返回
// ErrRescueSeedAuthRejected；其余失败只返回原错误（号留在区里）。
// 种子载体未注入（测试桩）时零成本通过。
func (l *OpenAIRescueLane) seedAndAccount(ctx context.Context, account *Account, trigger string) error {
	if l == nil || l.seed == nil {
		return nil
	}
	seedErr := l.seed(ctx, account.ID)
	if seedErr != nil {
		slog.Warn("openai_rescue_seed_failed",
			"account_id", account.ID, "trigger", trigger, "error", seedErr)
	}
	if markErr := l.updateMarkerFields(ctx, account.ID, func(m *OpenAIRescueLaneMarker) {
		m.SeedAttempts++
		m.LastSeedAt = l.now().UTC()
		m.SeedOK = seedErr == nil
		if seedErr == nil {
			// 成功清零连续失败计数（r17ba）：插件进程换代丢模板后的补种
			// 也走本函数，若成功不清零，跨重启多次补种会耗尽上限卡死。
			m.SeedAttempts = 0
		}
	}); markErr != nil {
		// 记账失败不改变种子结局（清扫下轮按 SeedOK 缺失补打，幂等）。
		slog.Warn("openai_rescue_seed_mark_failed",
			"account_id", account.ID, "error", markErr)
	}
	if seedErr != nil && rescueSeedAuthRejected(seedErr) {
		if exitErr := l.ExitRescue(ctx, account.ID, openAIRescueExitAuthRejected); exitErr != nil {
			slog.Warn("openai_rescue_exit_failed",
				"account_id", account.ID, "reason", openAIRescueExitAuthRejected, "error", exitErr)
		}
		return ErrRescueSeedAuthRejected
	}
	return seedErr
}

// updateMarkerFields 读-改-写标记字段（UpdateExtra 是 JSONB 整键合并，
// 标记值必须整体重写）。变更回调内改副本，写回整键。
func (l *OpenAIRescueLane) updateMarkerFields(
	ctx context.Context, accountID int64,
	mutate func(m *OpenAIRescueLaneMarker),
) error {
	account, err := l.accounts.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil {
		return ErrRescueLaneIneligible
	}
	mutate(marker)
	return l.accounts.UpdateExtra(ctx, accountID, map[string]any{
		openAIRescueLaneExtraKey: rescueLaneMarkerExtraValue(*marker),
	})
}

// autoNeedle 自动资格针（r17bb）：插件连过证据达阈值后由清扫触发。触发即
// 记账（AutoNeedleAt/Attempts）——针本体异步收敛：成功 → state 进
// qualification → 连过转 normal → 下一轮清扫转正毕业清标记；失败 → r17y
// 一击回判死 → 冷却后重试。记账失败只记日志（最坏形态=下轮清扫重触发，
// 5min 间隔有界）。毕业清整个标记，Attempts 天然按轮重置。
func (l *OpenAIRescueLane) autoNeedle(ctx context.Context, accountID int64) {
	needleErr := l.needleTrigger(ctx, accountID)
	if needleErr != nil {
		slog.Warn("openai_rescue_auto_needle_failed",
			"account_id", accountID, "error", needleErr)
	}
	if markErr := l.updateMarkerFields(ctx, accountID, func(m *OpenAIRescueLaneMarker) {
		m.AutoNeedleAt = l.now().UTC()
		m.AutoNeedleAttempts++
	}); markErr != nil {
		slog.Warn("openai_rescue_auto_needle_mark_failed",
			"account_id", accountID, "error", markErr)
	}
	slog.Info("openai_rescue_auto_needle_triggered",
		"account_id", accountID, "error", needleErr)
}

// rescueSeedAuthRejected 种子流量的凭据级失败判定（401/403/token 吊销类）。
// 匹配 AccountTestService 自家错误格式（"API returned 401: ..."）与上游
// 错误码词面（token_revoked/invalid_grant），均在自家代码与上游 body
// 可控范围。cookie 插件管的是 __cflb/__oailb 会话 cookie，治不了 OAuth
// 令牌本身——这类号留在区里只会白烧清扫轮次。
func rescueSeedAuthRejected(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, pattern := range [...]string{
		"API returned 401",
		"API returned 403",
		"token_revoked",
		"invalid_grant",
	} {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

// ExitRescue 出区（种子凭据级失败出口）。顺序设计（崩溃窗各自可收敛）：
//  1. 标记打 exit_reason（在场 = 出区中；清扫见到续走本函数而非自愈回区）
//  2. 撤调度（判死号本就该不可调度；先停烧再改绑，无流量泄漏窗）
//  3. 改绑回原组（入区快照）
//  4. 清标记（连带 seed/exit 字段）
//  5. rescue_auth_rejected 事件（失败只记日志——出区写已落库）
func (l *OpenAIRescueLane) ExitRescue(ctx context.Context, accountID int64, reason string) error {
	if l == nil {
		return errors.New("rescue lane is not available")
	}
	account, err := l.accounts.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil {
		return nil // 幂等：出区半途标记已被清，或从未入区。
	}
	origGroupIDs := marker.OrigGroupIDs
	if marker.ExitReason == "" {
		if err := l.updateMarkerFields(ctx, accountID, func(m *OpenAIRescueLaneMarker) {
			m.ExitReason = reason
		}); err != nil {
			return fmt.Errorf("mark exit: %w", err)
		}
	}
	if account.Schedulable {
		if err := l.accounts.SetSchedulable(ctx, accountID, false); err != nil {
			return fmt.Errorf("withdraw scheduling: %w", err)
		}
	}
	if err := l.accounts.BindGroups(ctx, accountID, origGroupIDs); err != nil {
		return fmt.Errorf("rebind orig groups: %w", err)
	}
	clearWrites := map[string]any{
		openAIRescueLaneExtraKey:      nil,
		openAIRescueSuspectedExtraKey: false,
	}
	if reason == openAIRescueExitAuthRejected {
		// 冷却戳：出区后自动补进在窗内被压住（号留给删号重授权），
		// 手动入口不受限。与清标记同一写，原子完成。
		clearWrites[openAIRescueAuthRejectedAtExtraKey] = l.now().UTC().Format(time.RFC3339)
	}
	if err := l.accounts.UpdateExtra(ctx, accountID, clearWrites); err != nil {
		return fmt.Errorf("clear marker: %w", err)
	}
	if l.events != nil {
		if err := l.events.AppendOpenAIDowngradeEvent(ctx, accountID, account.ProxyID,
			OpenAIDowngradeEventRescueAuthRejected, map[string]any{"reason": reason}); err != nil {
			slog.Warn("openai_rescue_exit_event_failed",
				"account_id", accountID, "error", err)
		}
	}
	slog.Info("openai_rescue_exited", "account_id", accountID, "reason", reason)
	return nil
}

// EnterRescueManual 手动入区入口（task 3.7，管理端「送入实验台」按钮）：
// 与自动入口共用 EnterRescue 全部语义，返回 entered=false 表示已在区
// （幂等，前端可据此提示）。凭据级拒绝以 ErrRescueSeedAuthRejected 透传。
func (l *OpenAIRescueLane) EnterRescueManual(ctx context.Context, accountID int64) (bool, error) {
	if l == nil {
		return false, errors.New("rescue lane is not available")
	}
	account, err := l.accounts.GetByID(ctx, accountID)
	if err != nil {
		return false, err
	}
	already := GetOpenAIRescueLaneMarker(account) != nil
	if err := l.EnterRescue(ctx, accountID, OpenAIRescueTriggerManual); err != nil {
		return false, err
	}
	return !already, nil
}

// ---------- 转正（task 3.6：考证通过 → 改绑回原池组 → 清标记 → 复活徽标） ----------

// GraduateRescue 复活转正（两调用方：考证通过急挂钩 + 对账清扫崩溃窗收敛）：
//  1. 重读账号取标记；无标记 → nil（幂等：钩子与清扫可能并发同号，先到者
//     转正、后到者空转）
//  2. 先改绑回原池组再清标记（绑-first：中途崩溃时标记在场 + 探针态仍是
//     on_duty+normal，下轮清扫按同一判据收敛转正，不产生隐形号）
//  3. 单次 UpdateExtra 原子完成：清救治标记 + 清疑似标记 + 打永久复活徽标
//     （rescued_at）+ 复活计数 +1（计数读新写旧，并发双写都写 n+1，良性竞态）
//  4. rescue_graduated 事件（basis 区分 qualification_pass / sweep_converge），
//     事件失败只记日志——转正写已落库，不能因事件账翻盘
//
// 转正是出口，不做 enabled 闸——开关关掉的瞬间已在区的号照样允许毕业。
func (l *OpenAIRescueLane) GraduateRescue(ctx context.Context, accountID int64, basis string) error {
	if l == nil {
		return errors.New("rescue lane is not available")
	}
	account, err := l.accounts.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil {
		return nil
	}
	if err := l.accounts.BindGroups(ctx, accountID, marker.OrigGroupIDs); err != nil {
		return fmt.Errorf("rebind orig groups: %w", err)
	}
	count := 1
	if n, ok := parseOpenAIRescueInt(account.Extra[openAIRescueRescueCountKey]); ok {
		count = int(n) + 1
	}
	if err := l.accounts.UpdateExtra(ctx, accountID, map[string]any{
		openAIRescueLaneExtraKey:      nil,
		openAIRescueSuspectedExtraKey: false,
		openAIRescueRescuedAtExtraKey: l.now().UTC().Format(time.RFC3339),
		openAIRescueRescueCountKey:    count,
	}); err != nil {
		return fmt.Errorf("stamp rescue badge: %w", err)
	}
	if l.events != nil {
		details := map[string]any{
			"basis":          basis,
			"orig_group_ids": marker.OrigGroupIDs,
			"orig_priority":  marker.OrigPriority,
			"rescue_count":   count,
		}
		if err := l.events.AppendOpenAIDowngradeEvent(ctx, accountID, account.ProxyID,
			OpenAIDowngradeEventRescueGraduated, details); err != nil {
			slog.Warn("openai_rescue_graduate_event_failed",
				"account_id", accountID, "error", err)
		}
	}
	slog.Info("openai_rescue_graduated",
		"account_id", accountID, "basis", basis, "rescue_count", count)
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

// OpenAIRescueAuthRejectSuppressed 凭据级出区冷却戳是否仍在窗内（容错读：
// 缺失/坏值 = 不压制）。自动钩子与清扫补进据此跳过，防止「补进 → 种子
// 401 → 出区 → 再补进」每轮空转；手动入口不查它。
func OpenAIRescueAuthRejectSuppressed(account *Account, now time.Time) bool {
	if account == nil || len(account.Extra) == 0 {
		return false
	}
	raw, ok := account.Extra[openAIRescueAuthRejectedAtExtraKey]
	if !ok || raw == nil {
		return false
	}
	stamp, ok := parseOpenAIRescueTimeString(raw)
	if !ok {
		return false
	}
	return now.Sub(stamp) < openAIRescueAuthRejectReentryCooldown
}

// SetProbeStateSource 注入批量探针健康快照源（真实现 = 探针仓库的健康快照
// 批量查询）。未注入时清扫只做标记侧自愈，不补进新号、不做转正收敛
// （自动钩子与转正急挂钩照常工作）。
func (l *OpenAIRescueLane) SetProbeStateSource(
	fn func(ctx context.Context, accountIDs []int64) (map[int64]OpenAIProbeHealthSnapshot, error),
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

// SetSchedulingGate 注入调度闸预检（真实现 = 探针控制仓 CanRunOpenAIDowngradeProbe）。
// 唯一消费点 = 清扫补进候选过滤（手动暂停的判死号不入区，r17an 静置语义）。
func (l *OpenAIRescueLane) SetSchedulingGate(fn func(ctx context.Context, accountID int64) (bool, error)) {
	if l == nil {
		return
	}
	l.schedulingGate = fn
}

// SetNeedleTrigger 注入自动资格针载体（真实现 = ReenableOpenAIAccount 带
// unpause=true：在区手动暂停是防调用刹车而非防针——r17ba 后入区本就不开
// 调度，暂停留着只会把针永远堵死，1217 试点实证的死锁形态；针本身是合成
// 流量且仍以针通过为上岗前置，不破坏「确认救活才进正式调用」的保证）。
// 未注入时清扫不自动打针，行为与 r17ba 一致。
func (l *OpenAIRescueLane) SetNeedleTrigger(fn func(ctx context.Context, accountID int64) error) {
	if l == nil {
		return
	}
	l.needleTrigger = fn
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
//   - 转正收敛（task 3.6）：标记在场 + 探针态已回 on_duty+normal = 考证
//     已过但急挂钩崩溃窗残留（或钩子注入前老进程升上来的残留）→
//     GraduateRescue{basis: sweep_converge}。on_duty+qualification = 考证针
//     还在飞不收敛；考证挂了回判死的号转正条件已不成立，维持救治。
//   - 补进：OpenAI 平台、资格通过、status=active（auth 两振出局走 SetError，
//     status!=active 天然排除凭据死）、无标记、探针态=pending_replace、限流
//     未持有 → EnterRescue{trigger: reconcile}
//   - 自愈：标记在场（已入区）但救治组绑定丢失或调度未开（标记-first 崩溃
//     窗口）→ 补绑/补开。疑似账号级撤调标记在场时不复活调度（3.5 语义）。
//   - 撤调/恢复（task 3.5）：在区账号按插件桥证据——in_backoff → 撤调度+
//     疑似标记+事件（幂等：标记在场不重撤）；疑似标记在场且回暖（退避后
//     新过针）或插件状态丢失（账号从桥消失）→ 清标记+恢复调度+事件。
//     桥缺席/解析失败 → 不碰调度撤复（无证据不动状态机）。
//
// healed 计数含转正（转正是把号送回原位的最终自愈动作）。
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
	// 第一遍只分拣：在区号（标记在场）与候选号（可能补进）分开收集，
	// 探针快照一次批量取回后统一处置。
	var marked []*Account
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
			marked = append(marked, account)
			continue
		}
		// 凭据死（auth 两振 SetError）与禁用号不补进；限流持有中本轮跳过。
		if account.Status != StatusActive {
			continue
		}
		if account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
			continue
		}
		// 凭据级出区冷却窗内不补进（出区即清标记，无此闸会每轮空转）。
		if OpenAIRescueAuthRejectSuppressed(account, now) {
			continue
		}
		candidateIDs = append(candidateIDs, account.ID)
	}
	// 批量探针快照：转正收敛（State+ProbeMode）与补进复核（State）共用一次
	// 查询。失败时标记侧自愈照常（不依赖探针态），补进与转正收敛本轮放弃、
	// err 上抛记日志下轮重试。
	var states map[int64]OpenAIProbeHealthSnapshot
	if (len(marked) > 0 || len(candidateIDs) > 0) && l.probeStates != nil {
		ids := make([]int64, 0, len(marked)+len(candidateIDs))
		for _, account := range marked {
			ids = append(ids, account.ID)
		}
		ids = append(ids, candidateIDs...)
		fetched, fetchErr := l.probeStates(ctx, ids)
		if fetchErr != nil {
			slog.Warn("openai_rescue_sweep_states_failed", "error", fetchErr)
			err = fetchErr
		} else {
			states = fetched
		}
	}
	for _, account := range marked {
		if err := ctx.Err(); err != nil {
			return entered, healed, withdrawn, err
		}
		marker := GetOpenAIRescueLaneMarker(account)
		if marker == nil {
			continue // 并发转正/出区已清标记：本轮快照过期，下轮归位。
		}
		// 出区续走（种子凭据级拒绝的崩溃窗）：exit_reason 在场 = 出区中，
		// 不做绑定/调度自愈（否则把正要送回原池的号又拉回救治组）。
		if marker.ExitReason != "" {
			if exitErr := l.ExitRescue(ctx, account.ID, marker.ExitReason); exitErr != nil {
				slog.Warn("openai_rescue_sweep_exit_failed",
					"account_id", account.ID, "reason", marker.ExitReason, "error", exitErr)
			}
			continue
		}
		// 转正收敛（design 0.4 出口判据）：标记在场 + 探针态已回 on_duty+normal。
		if snapshot, ok := states[account.ID]; ok &&
			snapshot.State == OpenAIDowngradeStateOnDuty && snapshot.ProbeMode == "normal" {
			if gradErr := l.GraduateRescue(ctx, account.ID, "sweep_converge"); gradErr != nil {
				slog.Warn("openai_rescue_sweep_graduate_failed",
					"account_id", account.ID, "error", gradErr)
			} else {
				healed++
			}
			continue
		}
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
			// 自动资格针（r17bb）：插件连过达阈值 + 本号仍在判死位 → 清扫
			// 自动打针（针走 pluginRoundTrip 与真实流量同路，钉扎救治效果
			// 可被观测）。冷却与上限防空转；针在途（state=qualification）
			// 自然跳过——不满足 pending_replace；针通过转 normal 后由上方
			// 转正收敛段收编毕业。
			if l.needleTrigger != nil && bridgeAccount != nil &&
				!bridgeAccount.InBackoff && !bridgeAccount.SuspectAccountLevel &&
				bridgeAccount.ConsecutivePasses >= cfg.ConsecutiveCleanPasses &&
				marker.AutoNeedleAttempts < openAIRescueAutoNeedleMaxAttempts &&
				(marker.AutoNeedleAt.IsZero() ||
					now.Sub(marker.AutoNeedleAt) >= openAIRescueAutoNeedleCooldown) {
				if snapshot, ok := states[account.ID]; ok &&
					snapshot.State == OpenAIDowngradeStatePendingReplace {
					l.autoNeedle(ctx, account.ID)
				}
			}
		}
		// 在区号 schedulable=false 是期望形态（r17ba 用户裁定：入区即关、
		// 唯一开调度点=考证通过后的资格完成）——不再是崩溃窗，无调度自愈。
		// 偶发 schedulable=true（r17az 时代入区残留/并发竞态）由撤调与
		// 出区路径兜底关掉；确定性收敛靠下轮判死复核，不开调度。
		// 补种子（半进区收尾 + 种子失败重试 + 插件失忆自愈 r17ba）：插件
		// 探针模板来自真实转发流量，救治组无客户流量，没种子的号插件无从
		// 学起。SeedOK 只证种子曾打过——插件进程换代丢光内存模板后，按桥
		// 证据（prober 启用却未跟踪本号）判失忆补种；openAIRescueReseedInterval
		// 节流覆盖「模板已喂、等首针」窗（首针最多一个 interval 后出现）。
		// 上限 openAIRescueSeedMaxAttempts 防空打（成功清零=连续失败语义）；
		// 凭据级拒绝由 seedAndAccount 内部出区。
		needSeed := !marker.SeedOK
		if !needSeed && l.seed != nil && haveBridge && prober != nil && prober.Enabled &&
			prober.Accounts[account.ID] == nil {
			needSeed = marker.LastSeedAt.IsZero() ||
				now.Sub(marker.LastSeedAt) >= openAIRescueReseedInterval
		}
		if l.seed != nil && !GetOpenAIRescueSuspected(account) &&
			needSeed && marker.SeedAttempts < openAIRescueSeedMaxAttempts {
			if err := l.seedAndAccount(ctx, account, OpenAIRescueTriggerReconcile); err != nil {
				if errors.Is(err, ErrRescueSeedAuthRejected) {
					// 已出区（号回判死原位），事件与日志在 seedAndAccount 内。
					continue
				}
				// 其余失败已在 seedAndAccount 内 Warn，下轮按 attempts 继续。
			}
		}
	}
	for _, id := range candidateIDs {
		if err := ctx.Err(); err != nil {
			return entered, healed, withdrawn, err
		}
		if snapshot, ok := states[id]; !ok || snapshot.State != OpenAIDowngradeStatePendingReplace {
			continue
		}
		// 手动暂停的判死号不入区（r17ba）：静置刹车是有意态（r17an 语义），
		// 清扫强拉入区=换绑+开调度双违反；解暂停后下轮自然收。
		if l.schedulingGate != nil {
			if ok, gErr := l.schedulingGate(ctx, id); gErr == nil && !ok {
				continue
			}
		}
		if enterErr := l.EnterRescue(ctx, id, OpenAIRescueTriggerReconcile); enterErr != nil {
			// 单号失败（含幂等跳过）不阻断整轮；瞬时失败下轮重试；
			// 凭据级拒绝已出区（种子 401/403 类），不是故障不 Warn 刷屏。
			if !errors.Is(enterErr, ErrRescueLaneIneligible) && !errors.Is(enterErr, ErrRescueSeedAuthRejected) {
				slog.Warn("openai_rescue_sweep_enter_failed", "account_id", id, "error", enterErr)
			}
			continue
		}
		entered++
	}
	return entered, healed, withdrawn, err
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

// restoreScheduling 疑似撤调的回暖恢复：清疑似标记 → 事件（basis=plugin_pass
// 真回暖 / plugin_state_lost 插件状态丢失兜底）。r17ba 起不开调度——在区
// 期望形态就是 schedulable=false，回暖后回到「救治中」继续攒连过证据，
// 开调度的唯一时刻仍是考证通过后的资格完成（转正）。
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

// ---------- 手动入口（task 3.7：管理端「送入实验台」按钮） ----------

// RescueAccount 手动送入救治区（handler 经 runner 转调救治编排器）。
// 返回 {entered, already_in_lane}：entered=false = 已在区（幂等）。
// 凭据级拒绝（种子 401/403 类）透传 ErrRescueSeedAuthRejected——号已回
// 判死原位，管理端把「救不了」说清楚而不是报个含糊的 500。
func (r *OpenAIDowngradeProbeRunner) RescueAccount(ctx context.Context, accountID int64) (map[string]any, error) {
	if r == nil || r.rescueLane == nil {
		return nil, errors.New("rescue lane is not available")
	}
	entered, err := r.rescueLane.EnterRescueManual(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"entered":         entered,
		"already_in_lane": !entered,
		"trigger":         OpenAIRescueTriggerManual,
	}, nil
}
