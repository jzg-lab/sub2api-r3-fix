package service

// 救治区编排器（r17ax Phase 3）：判死号自动进入插件运行的生产内实验台。
// 三入口（自动钩子 / 手动端点 / 对账清扫）汇入同一 EnterRescue 转换：
// 快照原组 → 标记入区 → 绑救治组并隔离 → 种子流量 → 事件。
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
	// OpenAIDowngradeEventRescueRecovered 插件连续通过且宿主资格针通过。
	OpenAIDowngradeEventRescueRecovered            = "rescue_recovered"
	OpenAIDowngradeEventRescuePluginStateLost      = "rescue_plugin_state_lost"
	OpenAIDowngradeEventRescuePluginStateLostAlert = "rescue_plugin_state_lost_alert"
	OpenAIDowngradeEventRescueBackoffObserved      = "rescue_backoff_observed"
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
	ErrRescueLaneDisabled = infraerrors.Conflict("OPENAI_RESCUE_LANE_DISABLED", "rescue lane is not enabled; enable rescue lane settings before sending accounts")
	// ErrRescueLaneNotConfigured 救治组未配置（group id 缺失）。
	ErrRescueLaneNotConfigured = infraerrors.Conflict("OPENAI_RESCUE_LANE_NOT_CONFIGURED", "rescue lane group is not configured; select a rescue group before sending accounts")
	// ErrRescueLaneIneligible 账号不满足入区资格（非 OpenAI OAuth / 影子号 / 已在区）。
	ErrRescueLaneIneligible = infraerrors.BadRequest("OPENAI_RESCUE_LANE_INELIGIBLE", "account is not eligible for rescue lane")
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
	// Check current bridge evidence without repeating the full roster/seed sweep.
	openAIRescueConfirmationInterval = 30 * time.Second
	openAIRescueConfirmationPasses   = 3
	// openAIRescueSeedSlowLaneAttempts 种子失败慢道门槛（成功清零）。r17bc
	// 用户裁定「所有问题号一直救到救回来」——种子永不停止；但连败达此数
	// 后进慢道：每小时最多一试（openAIRescueSeedSlowInterval）。种子是真实
	// 上游请求，密集空打会喂执法升级（1223 实证：救治中连探 → token 吊销）。
	// 老常量 openAIRescueSeedMaxAttempts 的硬停预算语义已移除。
	openAIRescueSeedSlowLaneAttempts = 5
	// openAIRescueSeedSlowInterval 慢道补种最小间隔（连败达慢道门槛后，
	// 两次种子尝试之间的最小等待）。
	openAIRescueSeedSlowInterval = time.Hour
	// openAIRescueReseedInterval 插件失忆补种节流：插件探针模板在内存里，
	// 进程换代即丢（fork 宿主无 KV 持久化）。种子喂上模板后插件最多一个
	// probe interval 才出首针进 prober 视图，此窗内不重复补种（15min 盖住
	// 生产默认 interval=900s；更长 interval 的窗内最多每小时 4 发，良性）。
	openAIRescueReseedInterval = 15 * time.Minute
	// openAIRescueAutoNeedleCooldown 自动资格针基础冷却（r17bb 起）：每次
	// 自动针后到下次重试的最小间隔；随连败次数 ×3 递增（见
	// openAIRescueAutoNeedleCooldownFor），封顶 openAIRescueAutoNeedleMaxCooldown。
	openAIRescueAutoNeedleCooldown = 10 * time.Minute
	// openAIRescueAutoNeedleMaxCooldown 自动资格针冷却封顶（r17bc）：重试
	// 永不停止（用户裁定「一直救到救回来」），但节奏指数退避——考证针是
	// 真实上游流量，1223 实证连探可把号探到 token 吊销。节奏 10min→30min→
	// 90min→2h（封顶后恒定）。老上限 openAIRescueAutoNeedleMaxAttempts=3
	// 是预算设计，r17bc 移除。
	openAIRescueAutoNeedleMaxCooldown = 2 * time.Hour
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
	// SeedAttempts 连续种子失败次数（成功即清零）。r17bc 起不再停种：达
	// openAIRescueSeedSlowLaneAttempts 后进慢道（每小时最多一试），永不停。
	SeedAttempts int `json:"seed_attempts,omitempty"`
	// LastSeedAt 最近一次种子尝试时刻（补种节流：模板喂上后插件最多一个
	// interval 才出首针，这窗内不重复补种）。
	LastSeedAt time.Time `json:"last_seed_at,omitempty"`
	// AutoNeedleAt 最近一次自动资格针触发时刻（r17bb 冷却节流：针失败 r17y
	// 一击回判死后，等冷却再重试，不每轮清扫空打）。
	AutoNeedleAt time.Time `json:"auto_needle_at,omitempty"`
	// AutoNeedleAttempts 自动资格针连败次数（针通过→转正收敛会清整个标记，
	// 无需手动归零）。r17bc 起不设上限：只驱动冷却递增（×3 封顶 2h），
	// 重试永不停止。
	AutoNeedleAttempts int `json:"auto_needle_attempts,omitempty"`
	// ExitReason 非空 = 出区中（唯一现值 auth_rejected：种子吃凭据级拒绝）。
	// 清扫见到即续走出区，不做绑定/调度自愈（否则与出区意图打架）。
	ExitReason  string                   `json:"exit_reason,omitempty"`
	Observation *openAIRescueObservation `json:"observation,omitempty"`
}

// OpenAIRescueLaneMarker 是 Extra[openai_rescue_lane] 的解析形态。
type OpenAIRescueLaneMarker = OpenAIRescueLaneSnapshot

// Keep persisted and in-memory markers identical for probe input binding.
func (marker OpenAIRescueLaneSnapshot) MarshalJSON() ([]byte, error) {
	return json.Marshal(rescueLaneMarkerExtraValue(marker))
}

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
	// bridge 插件桥状态源；nil 时仍纠正宿主隔离，不毕业、不确认或补种。
	bridge func(ctx context.Context) *PluginBridgeStatus
	// schedulingGate 调度闸预检（r17ba；真实现 = CanRunOpenAIDowngradeProbe，
	// 镜像 ListDue 的 manual_paused/owned_error 排除闸）。唯一消费点=清扫
	// 补进的候选过滤：手动暂停（静置刹车，r17an 语义）的判死号不被清扫
	// 强拉入区。入区/在区不再有任何开调度动作（用户裁定：唯一开调度点=
	// 考证通过后的资格完成），此闸与调度开无关。nil = 无闸（测试桩）。
	schedulingGate func(ctx context.Context, accountID int64) (bool, error)
	// needleTrigger queues host confirmation without releasing manual pause.
	needleTrigger        func(ctx context.Context, accountID int64) error
	pluginProbePauseSync func(context.Context) error
	now                  func() time.Time
	sweepMu              sync.Mutex
	loopContext          context.Context
	loopCancel           context.CancelFunc

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
	loopContext, loopCancel := context.WithCancel(context.Background())
	lane := &OpenAIRescueLane{
		accounts:    accounts,
		events:      events,
		config:      config,
		seed:        seed,
		now:         time.Now,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
		loopContext: loopContext,
		loopCancel:  loopCancel,
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
	if OpenAIRescueManuallyTerminated(account) {
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
	if raw, err := json.Marshal(fields["observation"]); err == nil {
		_ = json.Unmarshal(raw, &marker.Observation)
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
	groupIDs := marker.OrigGroupIDs
	if groupIDs == nil {
		groupIDs = []int64{}
	}
	value := map[string]any{
		"entered_at":     marker.EnteredAt.UTC().Format(time.RFC3339Nano),
		"trigger":        marker.Trigger,
		"orig_group_ids": groupIDs,
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
		value["last_seed_at"] = marker.LastSeedAt.UTC().Format(time.RFC3339Nano)
	}
	if !marker.AutoNeedleAt.IsZero() {
		value["auto_needle_at"] = marker.AutoNeedleAt.UTC().Format(time.RFC3339Nano)
	}
	if marker.AutoNeedleAttempts > 0 {
		value["auto_needle_attempts"] = marker.AutoNeedleAttempts
	}
	if marker.ExitReason != "" {
		value["exit_reason"] = marker.ExitReason
	}
	if marker.Observation != nil {
		// Store JSON-native values locally too: a struct's field order differs
		// from the map returned by PostgreSQL, changing the probe input digest.
		observation := map[string]any{
			"suspected_at": marker.Observation.SuspectedAt.UTC().Format(time.RFC3339Nano),
		}
		if marker.Observation.PluginSeen {
			observation["plugin_seen"] = true
		}
		if marker.Observation.StateLost {
			observation["state_lost"] = true
		}
		if len(marker.Observation.StateLosses) > 0 {
			losses := make([]string, len(marker.Observation.StateLosses))
			for i, lost := range marker.Observation.StateLosses {
				losses[i] = lost.UTC().Format(time.RFC3339Nano)
			}
			observation["state_losses"] = losses
		}
		if marker.Observation.BackoffObserved {
			observation["backoff_observed"] = true
		}
		value["observation"] = observation
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
//  4. 同一版本绑定事务写标记、独绑救治组、撤调及调度 outbox；
//     唯一开调度点仍为考证通过后的资格完成。
//  5. 种子流量（TestAccountConnection 直调；失败只记账不回滚——插件探针
//     自愈等真实流量再起，回滚反而丢已入区状态）
//  6. rescue_entered 事件（trigger 区分入口）；过期结果不得发布入区成功。
func (l *OpenAIRescueLane) EnterRescue(ctx context.Context, accountID int64, trigger string) error {
	_, err := l.enterRescue(ctx, accountID, trigger)
	return err
}

func (l *OpenAIRescueLane) enterRescue(ctx context.Context, accountID int64, trigger string) (bool, error) {
	if l == nil {
		return false, errors.New("rescue lane is not available")
	}
	cfg := l.config()
	if !cfg.Enabled {
		return false, ErrRescueLaneDisabled
	}
	if cfg.GroupID <= 0 {
		return false, ErrRescueLaneNotConfigured
	}
	now := l.now().UTC()

	account, err := l.accounts.GetByID(ctx, accountID)
	if err != nil {
		return false, err
	}
	if !isOpenAIDowngradeProbeAccountEligible(account, now) {
		return false, ErrRescueLaneIneligible
	}
	if GetOpenAIRescueLaneMarker(account) != nil {
		// 幂等：已在区（对账清扫与自动钩子可能同号并发）。
		return false, nil
	}
	if trigger != OpenAIRescueTriggerManual && OpenAIRescueManuallyTerminated(account) {
		return false, ErrOpenAIRescueTerminated
	}
	marker := OpenAIRescueLaneMarker{
		EnteredAt:    now,
		Trigger:      trigger,
		OrigGroupIDs: account.GroupIDs,
		OrigPriority: account.Priority,
	}
	markerFields := rescueLaneMarkerExtraValue(marker)
	if lifecycle, ok := l.accounts.(OpenAIRescueLifecycleRepository); ok {
		entered, err := lifecycle.EnterOpenAIRescue(ctx, account, markerFields, cfg.GroupID, trigger == OpenAIRescueTriggerManual)
		if err != nil || !entered {
			return false, err
		}
		account, err = l.accounts.GetByID(ctx, accountID)
		if err != nil {
			return false, err
		}
		current := GetOpenAIRescueLaneMarker(account)
		if current == nil || !current.EnteredAt.Equal(marker.EnteredAt) || OpenAIRescueManuallyTerminated(account) {
			return false, ErrOpenAIProbeStale
		}
	} else if err := l.commitTransition(ctx, account, OpenAIRescueTransitionEnter, []int64{cfg.GroupID}, map[string]any{
		openAIRescueLaneExtraKey:           markerFields,
		openAIRescueAuthRejectedAtExtraKey: nil,
		OpenAIRescueTerminatedAtExtraKey:   nil,
	}); err != nil {
		return false, err
	}
	seedErr := l.seedAndAccount(ctx, account, trigger)
	if seedErr != nil {
		if errors.Is(seedErr, ErrOpenAIProbeStale) ||
			errors.Is(seedErr, context.Canceled) || errors.Is(seedErr, context.DeadlineExceeded) {
			return false, seedErr
		}
		if errors.Is(seedErr, ErrRescueSeedAuthRejected) {
			// 凭据级死（401/403/token 吊销）：插件治不了 OAuth 令牌。出区
			// 已在 seedAndAccount 内完成（号回判死原位），入区不算失败也
			// 不算成功——调用方（自动钩子/清扫）忽略，手动入口透传展示。
			return false, seedErr
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
			return false, fmt.Errorf("record rescue_entered: %w", err)
		}
	}
	return true, nil
}

// seedAndAccount 打种子并把结果记账进标记（SeedOK/SeedAttempts，清扫补
// 种子的依据）。凭据级拒绝（401/403/token 吊销类）时完成出区并返回
// ErrRescueSeedAuthRejected；其余失败只返回原错误（号留在区里）。
// 种子载体未注入（测试桩）时零成本通过。
func (l *OpenAIRescueLane) seedAndAccount(ctx context.Context, account *Account, trigger string) error {
	if l == nil || l.seed == nil {
		return nil
	}
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil {
		return ErrRescueLaneIneligible
	}
	// Reserve the attempt before network work. Concurrent sweeps cannot seed
	// the same read revision, and a crash still leaves the cooldown in place.
	marker.SeedAttempts++
	marker.LastSeedAt = l.now().UTC()
	if err := l.commitMarker(ctx, account, marker, false); err != nil {
		return err
	}
	inputHash := openAIProbeInputHash(account)
	seedErr := l.syncPluginProbePauses(ctx)
	if seedErr == nil {
		seedErr = l.seed(ctx, account.ID)
	}
	if seedErr != nil {
		slog.Warn("openai_rescue_seed_failed",
			"account_id", account.ID, "trigger", trigger, "error", seedErr)
	}
	current, err := l.accounts.GetByID(ctx, account.ID)
	if err != nil {
		return err
	}
	proxyChanged := (current.ProxyID == nil) != (account.ProxyID == nil) ||
		(current.ProxyID != nil && account.ProxyID != nil && *current.ProxyID != *account.ProxyID)
	if proxyChanged || openAIProbeInputHash(current) != inputHash {
		return ErrOpenAIProbeStale
	}
	marker = GetOpenAIRescueLaneMarker(current)
	if marker == nil {
		return ErrOpenAIProbeStale
	}
	marker.SeedOK = seedErr == nil
	if seedErr == nil {
		marker.SeedAttempts = 0
	}
	if markErr := l.commitMarker(ctx, current, marker, false); markErr != nil {
		slog.Warn("openai_rescue_seed_mark_failed",
			"account_id", account.ID, "error", markErr)
		return markErr
	}
	if seedErr != nil && rescueSeedAuthRejected(seedErr) {
		if exitErr := l.exitRescueAccount(ctx, current, openAIRescueExitAuthRejected); exitErr != nil {
			slog.Warn("openai_rescue_exit_failed",
				"account_id", account.ID, "reason", openAIRescueExitAuthRejected, "error", exitErr)
			return exitErr
		}
		return ErrRescueSeedAuthRejected
	}
	return seedErr
}

// openAIRescueAutoNeedleCooldownFor 自动资格针重试节奏（r17bc）：连败次数
// 越多冷却越长——基础 10min × 3^attempts，封顶 2h。永不停止重试，但失败
// 越多打得越慢：针是真实上游流量，固定短间隔重试在账号已被降智的场合
// 等于主动喂执法升级（1223 实证：连探 → probe_auth_terminal → token 吊销）。
func openAIRescueAutoNeedleCooldownFor(attempts int) time.Duration {
	cooldown := openAIRescueAutoNeedleCooldown
	for i := 0; i < attempts; i++ {
		cooldown *= 3
		if cooldown >= openAIRescueAutoNeedleMaxCooldown {
			return openAIRescueAutoNeedleMaxCooldown
		}
	}
	return cooldown
}

// autoNeedle 自动资格针（r17bb）：插件连过证据达阈值后由清扫触发。先以
// CAS 预留 AutoNeedleAt/Attempts，失败不发针；针本体异步收敛：成功 → state 进
// qualification → 连过转 normal → 下一轮清扫转正毕业清标记；失败 → r17y
// 一击回判死 → 冷却后重试。毕业清整个标记，Attempts 天然按轮重置。
func (l *OpenAIRescueLane) autoNeedle(ctx context.Context, account *Account) error {
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil {
		return ErrRescueRecoveryUnverified
	}
	marker.AutoNeedleAt = l.now().UTC()
	marker.AutoNeedleAttempts++
	if markErr := l.commitMarker(ctx, account, marker, false); markErr != nil {
		slog.Warn("openai_rescue_auto_needle_mark_failed",
			"account_id", account.ID, "error", markErr)
		return markErr
	}
	needleErr := l.needleTrigger(ctx, account.ID)
	if needleErr != nil {
		slog.Warn("openai_rescue_auto_needle_failed",
			"account_id", account.ID, "error", needleErr)
	}
	slog.Info("openai_rescue_auto_needle_triggered",
		"account_id", account.ID, "error", needleErr)
	return needleErr
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

// ExitRescue 原子撤调、恢复原组并清标记。历史 exit_reason 半成品仍由清扫
// 续走；新转换任一步失败均整笔回滚，提交后才记录 rescue_auth_rejected。
func (l *OpenAIRescueLane) ExitRescue(ctx context.Context, accountID int64, reason string) error {
	return l.exitRescueEpisode(ctx, accountID, reason, time.Time{})
}

func (l *OpenAIRescueLane) exitRescueEpisode(ctx context.Context, accountID int64, reason string, enteredAt time.Time) error {
	if l == nil {
		return errors.New("rescue lane is not available")
	}
	account, err := l.accounts.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil || (!enteredAt.IsZero() && !marker.EnteredAt.Equal(enteredAt)) {
		return nil
	}
	return l.exitRescueAccount(ctx, account, reason)
}

func (l *OpenAIRescueLane) exitRescueAccount(ctx context.Context, account *Account, reason string) error {
	accountID := account.ID
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil {
		return nil // 幂等：出区半途标记已被清，或从未入区。
	}
	origGroupIDs, err := OpenAIRescueOriginalGroups(account.Extra[openAIRescueLaneExtraKey])
	if err != nil {
		return err
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
	if err := l.commitTransition(ctx, account, OpenAIRescueTransitionExit, origGroupIDs, clearWrites); err != nil {
		return fmt.Errorf("commit rescue exit: %w", err)
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
	return l.enterRescue(ctx, accountID, OpenAIRescueTriggerManual)
}

// ---------- 转正（task 3.6：考证通过 → 改绑回原池组 → 清标记 → 复活徽标） ----------

// GraduateRescue 复活转正（两调用方：考证通过急挂钩 + 对账清扫崩溃窗收敛）：
//  1. 重读账号取标记；无标记 → nil（幂等：钩子与清扫可能并发同号，先到者
//     转正、后到者空转）
//  2. Recheck current plugin and committed host qualification evidence.
//  3. Atomically restore groups and clear markers under the account generation
//     and latest host-probe guards. A concurrent pause, re-entry or failed probe
//     invalidates this graduation rather than being overwritten.
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
	groupIDs, err := OpenAIRescueOriginalGroups(account.Extra[openAIRescueLaneExtraKey])
	if err != nil {
		return err
	}
	if l.probeStates == nil {
		return ErrRescueRecoveryUnverified
	}
	states, err := l.probeStates(ctx, []int64{accountID})
	if err != nil {
		return err
	}
	bridge := l.recoveryBridge(ctx)
	var plugin *OpenAIPluginBridgeAccount
	if bridge != nil {
		plugin = bridge.Accounts[accountID]
	}
	if plugin == nil || plugin.ConsecutivePasses < l.GraduationThreshold() {
		return ErrRescueRecoveryUnverified
	}
	snapshot := states[accountID]
	if recovered, _ := rescueLaneRecoveryEvidence(plugin, snapshot, marker, l.now()); !recovered {
		return ErrRescueRecoveryUnverified
	}
	wasSuspected := GetOpenAIRescueSuspected(account)
	committer, ok := l.accounts.(OpenAIRescueGraduationStore)
	if !ok {
		return ErrRescueRecoveryUnverified
	}
	count := 1
	if n, ok := parseOpenAIRescueInt(account.Extra[openAIRescueRescueCountKey]); ok {
		count = int(n) + 1
	}
	evidenceExpiresAt := plugin.PreviousPassAt.Add(openAIRescueEvidenceWindow)
	if hostExpiry := snapshot.LastProbe.At.Add(openAIRescueEvidenceWindow); hostExpiry.Before(evidenceExpiresAt) {
		evidenceExpiresAt = hostExpiry
	}
	if err := committer.CommitOpenAIRescueGraduation(ctx, OpenAIRescueGraduation{
		AccountID: accountID, ExpectedUpdatedAt: account.UpdatedAt,
		HostProbeID: snapshot.LastProbe.ID, HostProbeAt: snapshot.LastProbe.At,
		EvidenceExpiresAt: evidenceExpiresAt,
		OldGroupIDs:       account.GroupIDs, GroupIDs: groupIDs,
		Extra: map[string]any{
			openAIRescueLaneExtraKey:      nil,
			openAIRescueSuspectedExtraKey: false,
			openAIRescueRescuedAtExtraKey: l.now().UTC().Format(time.RFC3339),
			openAIRescueRescueCountKey:    count,
		},
	}); err != nil {
		return fmt.Errorf("commit rescue graduation: %w", err)
	}
	if l.events != nil {
		if wasSuspected {
			l.rescueObservationEvent(ctx, account, OpenAIDowngradeEventRescueRecovered,
				map[string]any{
					"basis":              "plugin_and_host_pass",
					"consecutive_passes": plugin.ConsecutivePasses,
					"plugin_probe_at":    plugin.LastProbeAt.UTC().Format(time.RFC3339),
					"host_probe_at":      snapshot.LastProbe.At.UTC().Format(time.RFC3339),
				})
		}
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

// ---------- 对账清扫（task 3.3：周期补进；历史部分状态及组绑定漂移收敛） ----------

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

// SetNeedleTrigger injects automatic confirmation, never a manual unpause.
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
		if l.loopCancel != nil {
			l.loopCancel()
		}
		close(l.stopCh)
		l.startOnce.Do(func() { close(l.doneCh) })
	})
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-l.doneCh:
	case <-timer.C:
		slog.Warn("openai_rescue_stop_timeout")
	}
}

func (l *OpenAIRescueLane) sweepLoop() {
	defer close(l.doneCh)
	nextSweep := func() time.Duration {
		interval := l.config().ReconcileInterval
		if interval <= 0 {
			interval = openAIRescueDefaultReconcileInterval
		}
		return time.Duration(float64(interval) * (1 + 0.25*probeRandomFloat()))
	}
	sweep := time.NewTimer(nextSweep())
	defer sweep.Stop()
	confirmation := time.NewTicker(openAIRescueConfirmationInterval)
	defer confirmation.Stop()
	for {
		select {
		case <-l.stopCh:
			return
		case <-confirmation.C:
			ctx, cancel := context.WithTimeout(l.loopContext, openAIRescueConfirmationInterval)
			err := l.runConfirmationSweep(ctx)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("openai_rescue_confirmation_failed", "error", err)
			}
		case <-sweep.C:
			ctx, cancel := context.WithTimeout(l.loopContext, 5*time.Minute)
			entered, healed, withdrawn, err := l.RunReconcileSweep(ctx)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("openai_rescue_sweep_failed", "error", err)
			}
			if entered > 0 || healed > 0 || withdrawn > 0 {
				slog.Info("openai_rescue_sweep_done",
					"entered", entered, "healed", healed, "withdrawn", withdrawn)
			}
			sweep.Reset(nextSweep())
		}
	}
}

// RunReconcileSweep 一轮对账清扫（公开以便测试与启动确定性检查）：
//   - 转正收敛：标记在场、宿主资格通过且插件达到完整毕业阈值才毕业。
//     on_duty+normal 本身不是恢复证据；证据不足的号维持救治。
//   - 补进：OpenAI 平台、资格通过、status=active（auth 两振出局走 SetError，
//     status!=active 天然排除凭据死）、无标记、探针态=pending_replace、限流
//     未持有 → EnterRescue{trigger: reconcile}
//   - 自愈：救治组绑定或调度位漂移时原子归位；不恢复调度。
//   - Recovery requires the configured plugin streak and current dual evidence.
//     Backoff without a quality failure is observation only. Missing plugin
//     state preserves suspicion and its dwell clock, then throttles reseeding.
//     An unavailable bridge cannot authorize recovery or reseeding.
//
// healed 计数含转正（转正是把号送回原位的最终自愈动作）。
func (l *OpenAIRescueLane) RunReconcileSweep(ctx context.Context) (entered, healed, withdrawn int, err error) {
	if l == nil {
		return 0, 0, 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, 0, err
	}
	if err := l.loopContext.Err(); err != nil {
		return 0, 0, 0, err
	}
	if !l.sweepMu.TryLock() {
		return 0, 0, 0, nil
	}
	defer l.sweepMu.Unlock()
	cfg := l.config()
	active := cfg.Enabled && cfg.GroupID > 0
	if syncErr := l.syncPluginProbePauses(ctx); syncErr != nil {
		return 0, 0, 0, syncErr
	}
	now := l.now()
	accounts, err := l.accounts.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return 0, 0, 0, err
	}
	// Offline, disabled or malformed bridge data is not current evidence.
	prober := l.recoveryBridge(ctx)
	haveBridge := prober != nil
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
		if OpenAIRescueManuallyTerminated(account) {
			continue
		}
		if GetOpenAIRescueLaneMarker(account) != nil {
			marked = append(marked, account)
			continue
		}
		if !active {
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
			if exitErr := l.exitRescueAccount(ctx, account, marker.ExitReason); exitErr != nil {
				slog.Warn("openai_rescue_sweep_exit_failed",
					"account_id", account.ID, "reason", marker.ExitReason, "error", exitErr)
			}
			continue
		}
		// 转正收敛（design 0.4 出口判据）：标记在场 + 探针态已回 on_duty+normal。
		var bridgeAccount *OpenAIPluginBridgeAccount
		if prober != nil {
			bridgeAccount = prober.Accounts[account.ID]
		}
		graduationFailed := false
		if recovered, _ := rescueLaneRecoveryEvidence(bridgeAccount, states[account.ID], marker, now); recovered &&
			bridgeAccount.ConsecutivePasses >= l.GraduationThreshold() {
			if gradErr := l.GraduateRescue(ctx, account.ID, "sweep_converge"); gradErr != nil {
				slog.Warn("openai_rescue_sweep_graduate_failed",
					"account_id", account.ID, "error", gradErr)
				graduationFailed = true
			} else {
				healed++
				continue
			}
		}
		if !active {
			continue
		}
		// 已入区：绑定与调度位归位不依赖质量失败证据，不重复入区。
		// 独绑救治组才算健康（r17az 语义，r17bc 恢复）：启动链路的池组触发器
		// 每次重启会把原池组加回救治号（[原池组,救治组] 双绑），r17ba 重构时
		// 判据被放宽成「含救治组即可」——10/2 生产实证 1215/1217/1218/1219
		// 全部回到双绑。多余组或丢救治组都重绑 [救治组]，≤1 轮中和。
		bindingOK := len(account.GroupIDs) == 1 && account.GroupIDs[0] == cfg.GroupID
		if !bindingOK || account.Schedulable {
			if healErr := l.commitTransition(ctx, account, OpenAIRescueTransitionRebind, []int64{cfg.GroupID}, nil); healErr != nil {
				slog.Warn("openai_rescue_sweep_heal_bind_failed",
					"account_id", account.ID, "error", healErr)
				err = errors.Join(err, fmt.Errorf("contain rescue account %d: %w", account.ID, healErr))
				continue
			} else {
				healed++
			}
		}
		// Failed revalidation still requires containment, but its old evidence
		// must not trigger confirmation, observation or upstream reseeding.
		if graduationFailed {
			continue
		}
		if haveBridge {
			if err := l.observePluginState(ctx, account, marker, bridgeAccount, now); err != nil {
				slog.Warn("openai_rescue_observation_failed", "account_id", account.ID, "error", err)
				continue
			}
			// 撤调优先于调度自愈：连错进退避的号先停烧额度。
			if bridgeAccount != nil && bridgeAccount.InBackoff && !GetOpenAIRescueSuspected(account) {
				if bridgeAccount.ConsecFails >= 1 && l.withdrawScheduling(ctx, account, bridgeAccount) {
					withdrawn++
				}
				continue
			}
			if needleErr := l.maybeConfirm(ctx, account, bridgeAccount, states[account.ID], now); needleErr != nil {
				err = errors.Join(err, needleErr)
			}
		}
		// 补种子（半进区收尾 + 种子失败重试 + 插件失忆自愈 r17ba）：插件
		// 探针模板来自真实转发流量，救治组无客户流量，没种子的号插件无从
		// 学起。SeedOK 只证种子曾打过——插件进程换代丢光内存模板后，按桥
		// 证据（prober 启用却未跟踪本号）判失忆补种；openAIRescueReseedInterval
		// 节流覆盖「模板已喂、等首针」窗（首针最多一个 interval 后出现）。
		// r17bc 永不停种（用户裁定「所有问题号一直救到救回来」）：连败达
		// openAIRescueSeedSlowLaneAttempts 后进慢道（每小时最多一试）——种子
		// 是真实上游请求，密集空打喂执法升级（1223 实证）。插件已在跟踪本号
		//（模板在场）时跳过补种：SeedOK 只证种子曾成功，模板现状以桥证据
		// 为准；凭据级拒绝由 seedAndAccount 内部出区。
		needSeed := !marker.SeedOK
		if needSeed && haveBridge && prober != nil && prober.Enabled &&
			prober.Accounts[account.ID] != nil {
			// 模板在场（插件正跟踪本号、探针在跑）：种子目的已达成，本轮
			// 不补——空打种子只是多余的上游暴露。
			needSeed = false
		}
		if !needSeed && l.seed != nil && haveBridge && prober != nil && prober.Enabled &&
			prober.Accounts[account.ID] == nil {
			needSeed = marker.LastSeedAt.IsZero() ||
				now.Sub(marker.LastSeedAt) >= openAIRescueReseedInterval
		}
		// A missing template must be rehydrated even while suspected; that
		// observation never clears suspicion or its original dwell clock.
		if haveBridge && l.seed != nil && needSeed &&
			(!GetOpenAIRescueSuspected(account) || (haveBridge && bridgeAccount == nil)) {
			slowLane := marker.SeedAttempts >= openAIRescueSeedSlowLaneAttempts
			gap := openAIRescueReseedInterval
			if slowLane {
				gap = openAIRescueSeedSlowInterval
			}
			if marker.LastSeedAt.IsZero() || now.Sub(marker.LastSeedAt) >= gap {
				if err := l.seedAndAccount(ctx, account, OpenAIRescueTriggerReconcile); err != nil {
					if errors.Is(err, ErrRescueSeedAuthRejected) {
						// 已出区（号回判死原位），事件与日志在 seedAndAccount 内。
						continue
					}
					// 其余失败已在 seedAndAccount 内 Warn，下轮按 attempts 继续。
				}
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
			ok, gateErr := l.schedulingGate(ctx, id)
			if gateErr != nil {
				err = errors.Join(err, fmt.Errorf("check rescue entry for account %d: %w", id, gateErr))
				continue
			}
			if !ok {
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

// Withdrawal and suspicion are one revision-bound transaction. A stale plugin
// observation must not pause a newly graduated or reauthorized account.
func (l *OpenAIRescueLane) withdrawScheduling(
	ctx context.Context, account *Account, bridgeAccount *OpenAIPluginBridgeAccount,
) bool {
	if bridgeAccount == nil || !bridgeAccount.InBackoff || bridgeAccount.ConsecFails < 1 {
		return false
	}
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil {
		return false
	}
	if marker.Observation == nil {
		marker.Observation = &openAIRescueObservation{}
	}
	if marker.Observation.SuspectedAt.IsZero() {
		marker.Observation.SuspectedAt = l.now().UTC()
	}
	if err := l.commitMarker(ctx, account, marker, true); err != nil {
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
