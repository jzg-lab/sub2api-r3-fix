// Package pluginconfig 定义并校验 Codex LB Cookie Pin 插件配置。
//
// 配置由宿主加密保存，经 ValidateConfig/ApplyConfig 双入口复用同一套
// 严格解析（DisallowUnknownFields），JSON 字段统一 snake_case。
package pluginconfig

import (
	"encoding/json"
	"strings"
)

// 默认值依据社区实测经验：LB 粘性 Cookie 是会话 Cookie（无 Expires/Max-Age），
// 有效窗口分钟级（kumu-ze 实测 ~192s 连续满血，票窗经验值 240s）。
const (
	DefaultCookieNames          = "__cflb,__oailb"
	defaultDefaultTTLSeconds    = 240
	defaultRefreshBeforeSeconds = 30
	// 质量探针默认值：间隔 15 分钟对齐「好签 ~45 分钟内翻坏」的实测漂移速度。
	defaultProbeIntervalSeconds = 900
	defaultMaxConsecutiveFails  = 3
	defaultProbeBackoffSeconds  = 3600
	// 探针 effort 缺省对齐宿主资格针（medium）：考卷同难度，连过才是真毕业证据。
	defaultProbeReasoningEffort = "medium"
	// 判过钉推理门槛缺省（v0.3.4）：与宿主资格针毕业判据（答对 + rt≥800，
	// OpenAIDowngradeFailureReasoningThreshold）同标尺——插件连过证据若不含
	// 推理预算维度，会出现「插件 6 连过、宿主针永远考不过」的错位（低 rt 节点
	// 答对简单题但撑不起 xhigh 推理预算）。
	defaultProbeMinReasoningTokens = 800
	// 卡点排程默认值：提前量 120 秒（在预计死亡前 2 分钟落针）。
	defaultScheduleMarginSeconds = 120
	// 密集档默认值（v0.3.2 救治提速）：未验证态 120 秒一针，连过 3 针出档，
	// 单轮封顶 30 针防 error 空转。签捕获→上岗 ≈ 3×120s+毕业针 ≈ 8 分钟。
	defaultProbeBurstIntervalSeconds = 120
	defaultProbeBurstUntilPasses     = 3
	defaultProbeBurstMaxProbes       = 30
)

// Config 是插件的完整配置。
type Config struct {
	// Enabled 是总开关，关闭时 Forward 直通、零捕获零注入。
	Enabled bool `json:"enabled"`
	// CookieNames 是捕获/注入的 Cookie 名白名单（小写），默认 __cflb + __oailb。
	CookieNames []string `json:"cookie_names"`
	// DefaultTTLSeconds 是会话 Cookie（无 Expires/Max-Age 属性）的兜底有效期。
	DefaultTTLSeconds int `json:"default_ttl_seconds"`
	// RefreshBeforeSeconds 是提前判陈旧窗口：剩余寿命低于该值即视为过期重摇。
	RefreshBeforeSeconds int `json:"refresh_before_seconds"`
	// InjectScope 是注入范围：codex=仅 chatgpt.com/backend-api/codex*，all=全部出站。
	InjectScope string `json:"inject_scope"`
	// RerollOnFasterModel 在响应头出现 faster-model（模型被替换路由）时当场丢 Cookie 重摇。
	RerollOnFasterModel bool `json:"reroll_on_faster_model"`
	// PersistKV 用宿主 KV 存储持久化 Cookie 罐，插件重启不丢。
	PersistKV bool `json:"persist_kv"`
	// DropAccountIDs 是一次性动作：ApplyConfig 时丢弃这些账号的 Cookie（手动重摇入口）。
	// 会随规范化输出保留（宿主用规范化产物回放 ApplyConfig）；Sanitized 才剥除，
	// 因此重放仅触发无害的重复空 Drop。
	DropAccountIDs []int64 `json:"drop_account_ids"`
	// QualityProbeEnabled 开启质量探针自愈回路（v0.2）：定期用无歧义判别题
	// 检测静默降智，答错自动丢 Cookie 重摇，连续答错判账号级退避。探针是
	// 主动出站流量（与零探针反指纹原则冲突），默认关闭，显式开启。
	QualityProbeEnabled bool `json:"quality_probe_enabled"`
	// ProbeIntervalSeconds 探针间隔（每账号独立排期）。
	ProbeIntervalSeconds int `json:"probe_interval_seconds"`
	// ProbeModel 探针模型；空 = 跟随该账号最近一笔业务请求的模型。
	ProbeModel string `json:"probe_model"`
	// ProbeReasoningEffort 探针请求的 reasoning effort（v0.3.3 业务同形）：
	// 非空时探针体附 reasoning/instructions/parallel_tool_calls/include，与
	// 宿主资格针及真实业务流量同形——裸形态简单题判不出「重推理才暴露」的
	// 降智（2026-10-02 生产实证：插件简单题全对而资格针全错 = 零毕业根因）。
	// 合法值 minimal/low/medium/high/xhigh/max；"none" 显式回退旧裸形态；
	// 未配置时缺省 medium（对齐宿主资格针）。
	ProbeReasoningEffort string `json:"probe_reasoning_effort"`
	// ProbeMinReasoningTokens 判过钉推理门槛（v0.3.4）：答对且 completed 带
	// usage.reasoning_tokens 低于该值的针判 Fail（触发重摇，把搜索方向对准
	// 「宿主资格针真能考过」的节点），对齐宿主「答对 + rt≥800」毕业判据。
	// SSE 无 usage 的针不受影响（保持答对主义）；配 1 = 事实关闭。
	// 未配置时缺省 800。
	ProbeMinReasoningTokens int `json:"probe_min_reasoning_tokens"`
	// MaxConsecutiveProbeFailures 连续答错阈值：达到即判疑似账号级（重摇
	// 无解），停探退避省额度。
	MaxConsecutiveProbeFailures int `json:"max_consecutive_probe_failures"`
	// ProbeBackoffSeconds 账号级疑似的退避时长（期满自动复探给新机会）。
	ProbeBackoffSeconds int `json:"probe_backoff_seconds"`
	// AdaptiveProbeScheduling 按实测签寿命卡点排程（v0.3）：下一针落点 =
	// 签捕获 + 寿命p80 − 提前量，年轻签期少探（省额度）、临近预期死亡密集探
	//（早发现早换签）；落点带 jitter 抖动（防探针节奏与换签时刻强相关）。
	// 寿命样本 <3（冷启动）或无签时退回固定间隔。退避/复探路径不适用卡点。
	AdaptiveProbeScheduling bool `json:"adaptive_probe_scheduling"`
	// ProbeScheduleMarginSeconds 卡点提前量：在预计死亡前该秒数落针。
	ProbeScheduleMarginSeconds int `json:"probe_schedule_margin_seconds"`
	// ProbeBurstIntervalSeconds 未验证态密集档间隔（v0.3.2 救治提速）：连过数
	// 未达 ProbeBurstUntilPasses 的账号用本间隔，达标后回 ProbeIntervalSeconds。
	// 密集档只作用于未验证账号——健康号恒处稀疏档，300 秒护栏（烧额度+指纹
	// 纪律）对稳态不变；救治号恰好停在「零业务流量」状态，密集档是它唯一的
	// 快速攒证据通道。
	ProbeBurstIntervalSeconds int `json:"probe_burst_interval_seconds"`
	// ProbeBurstUntilPasses 密集档退出线：连过达到该值即视为已验证，回稀疏档。
	// 默认 3，与宿主救治区毕业阈值对齐（宿主读 consecutive_passes）。
	ProbeBurstUntilPasses int `json:"probe_burst_until_passes"`
	// ProbeBurstMaxProbes 单轮密集档封顶针数：防 error 空转烧额度（如模型 400
	// 死循环时 120s 一针 30 次即止）。答错（重开新回合）或达标出档时清零。
	ProbeBurstMaxProbes int `json:"probe_burst_max_probes"`
}

// Default 返回带完整默认值的配置。
func Default() Config {
	names := strings.Split(DefaultCookieNames, ",")
	return Config{
		Enabled:              true,
		CookieNames:          names,
		DefaultTTLSeconds:    defaultDefaultTTLSeconds,
		RefreshBeforeSeconds: defaultRefreshBeforeSeconds,
		InjectScope:          "codex",
		RerollOnFasterModel:  true,
		PersistKV:            true,

		ProbeIntervalSeconds:        defaultProbeIntervalSeconds,
		MaxConsecutiveProbeFailures: defaultMaxConsecutiveFails,
		ProbeBackoffSeconds:         defaultProbeBackoffSeconds,

		ProbeReasoningEffort: defaultProbeReasoningEffort,

		ProbeMinReasoningTokens: defaultProbeMinReasoningTokens,

		AdaptiveProbeScheduling:    true,
		ProbeScheduleMarginSeconds: defaultScheduleMarginSeconds,

		ProbeBurstIntervalSeconds: defaultProbeBurstIntervalSeconds,
		ProbeBurstUntilPasses:     defaultProbeBurstUntilPasses,
		ProbeBurstMaxProbes:       defaultProbeBurstMaxProbes,
	}
}

// Sanitized 返回清除一次性字段后的配置，用于激活态存储（SetConfig）。
// 注意：不用于 ValidateConfig 的规范化输出——那里必须保留一次性字段。
func (c Config) Sanitized() Config {
	out := c
	out.DropAccountIDs = nil
	return out
}

// incomingConfig 是 Parse 的中间形态：布尔用 *bool 区分「未提供」（nil，保留
// 默认值）与「显式 false」。字段与 Config 的 json 标签一一对应，
// testIncomingTagParity 会锁住这个对应关系，防止两边漂移。
type incomingConfig struct {
	Enabled              *bool    `json:"enabled"`
	CookieNames          []string `json:"cookie_names"`
	DefaultTTLSeconds    int      `json:"default_ttl_seconds"`
	RefreshBeforeSeconds int      `json:"refresh_before_seconds"`
	InjectScope          string   `json:"inject_scope"`
	RerollOnFasterModel  *bool    `json:"reroll_on_faster_model"`
	PersistKV            *bool    `json:"persist_kv"`
	DropAccountIDs       []int64  `json:"drop_account_ids"`

	QualityProbeEnabled         *bool  `json:"quality_probe_enabled"`
	ProbeIntervalSeconds        int    `json:"probe_interval_seconds"`
	ProbeModel                  string `json:"probe_model"`
	ProbeReasoningEffort        string `json:"probe_reasoning_effort"`
	ProbeMinReasoningTokens     int    `json:"probe_min_reasoning_tokens"`
	MaxConsecutiveProbeFailures int    `json:"max_consecutive_probe_failures"`
	ProbeBackoffSeconds         int    `json:"probe_backoff_seconds"`

	AdaptiveProbeScheduling    *bool `json:"adaptive_probe_scheduling"`
	ProbeScheduleMarginSeconds int   `json:"probe_schedule_margin_seconds"`

	ProbeBurstIntervalSeconds int `json:"probe_burst_interval_seconds"`
	ProbeBurstUntilPasses     int `json:"probe_burst_until_passes"`
	ProbeBurstMaxProbes       int `json:"probe_burst_max_probes"`
}

// Parse 严格解析并规范化配置 JSON。空输入返回默认配置。
// 返回 (配置, 错误消息, 是否合法)。
func Parse(raw []byte) (Config, string, bool) {
	cfg := Default()
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg, "", true
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var incoming incomingConfig
	if err := dec.Decode(&incoming); err != nil {
		return Config{}, "配置不是合法 JSON", false
	}
	if dec.More() {
		return Config{}, "配置不是单个 JSON 对象", false
	}
	if incoming.Enabled != nil {
		cfg.Enabled = *incoming.Enabled
	}
	if incoming.CookieNames != nil {
		cfg.CookieNames = incoming.CookieNames
	}
	if incoming.DefaultTTLSeconds != 0 {
		cfg.DefaultTTLSeconds = incoming.DefaultTTLSeconds
	}
	if incoming.RefreshBeforeSeconds != 0 {
		cfg.RefreshBeforeSeconds = incoming.RefreshBeforeSeconds
	}
	if strings.TrimSpace(incoming.InjectScope) != "" {
		cfg.InjectScope = strings.ToLower(strings.TrimSpace(incoming.InjectScope))
	}
	if incoming.RerollOnFasterModel != nil {
		cfg.RerollOnFasterModel = *incoming.RerollOnFasterModel
	}
	if incoming.PersistKV != nil {
		cfg.PersistKV = *incoming.PersistKV
	}
	if incoming.QualityProbeEnabled != nil {
		cfg.QualityProbeEnabled = *incoming.QualityProbeEnabled
	}
	if incoming.ProbeIntervalSeconds != 0 {
		cfg.ProbeIntervalSeconds = incoming.ProbeIntervalSeconds
	}
	cfg.ProbeModel = strings.TrimSpace(incoming.ProbeModel)
	// effort：未配置保留缺省（medium）；"none"（大小写不敏感）显式回退裸形态。
	if effort := strings.TrimSpace(incoming.ProbeReasoningEffort); effort != "" {
		effort = strings.ToLower(effort)
		if effort == "none" {
			effort = ""
		}
		cfg.ProbeReasoningEffort = effort
	}
	if incoming.ProbeMinReasoningTokens != 0 {
		cfg.ProbeMinReasoningTokens = incoming.ProbeMinReasoningTokens
	}
	if incoming.MaxConsecutiveProbeFailures != 0 {
		cfg.MaxConsecutiveProbeFailures = incoming.MaxConsecutiveProbeFailures
	}
	if incoming.ProbeBackoffSeconds != 0 {
		cfg.ProbeBackoffSeconds = incoming.ProbeBackoffSeconds
	}
	if incoming.AdaptiveProbeScheduling != nil {
		cfg.AdaptiveProbeScheduling = *incoming.AdaptiveProbeScheduling
	}
	if incoming.ProbeScheduleMarginSeconds != 0 {
		cfg.ProbeScheduleMarginSeconds = incoming.ProbeScheduleMarginSeconds
	}
	if incoming.ProbeBurstIntervalSeconds != 0 {
		cfg.ProbeBurstIntervalSeconds = incoming.ProbeBurstIntervalSeconds
	}
	if incoming.ProbeBurstUntilPasses != 0 {
		cfg.ProbeBurstUntilPasses = incoming.ProbeBurstUntilPasses
	}
	if incoming.ProbeBurstMaxProbes != 0 {
		cfg.ProbeBurstMaxProbes = incoming.ProbeBurstMaxProbes
	}
	cfg.DropAccountIDs = incoming.DropAccountIDs

	seen := map[string]struct{}{}
	clean := make([]string, 0, len(cfg.CookieNames))
	for _, name := range cfg.CookieNames {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || len(name) > 64 || !validCookieName(name) {
			return Config{}, "Cookie 名不合法（仅允许字母/数字/下划线/连字符/点）", false
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		clean = append(clean, name)
	}
	if len(clean) == 0 || len(clean) > 8 {
		return Config{}, "至少配置 1 个 Cookie 名，最多 8 个", false
	}
	cfg.CookieNames = clean

	if cfg.DefaultTTLSeconds < 30 || cfg.DefaultTTLSeconds > 3600 {
		return Config{}, "default_ttl_seconds 应为 30–3600 秒", false
	}
	if cfg.RefreshBeforeSeconds < 5 || cfg.RefreshBeforeSeconds >= cfg.DefaultTTLSeconds {
		return Config{}, "refresh_before_seconds 至少 5 秒且必须小于 default_ttl_seconds", false
	}
	if cfg.InjectScope != "codex" && cfg.InjectScope != "all" {
		return Config{}, "inject_scope 只支持 codex 或 all", false
	}
	dropSeen := map[int64]struct{}{}
	drops := make([]int64, 0, len(cfg.DropAccountIDs))
	for _, id := range cfg.DropAccountIDs {
		if id <= 0 {
			return Config{}, "drop_account_ids 含非法账号 ID", false
		}
		if _, ok := dropSeen[id]; ok {
			continue
		}
		dropSeen[id] = struct{}{}
		drops = append(drops, id)
	}
	if len(drops) > 200 {
		return Config{}, "drop_account_ids 最多 200 个", false
	}
	cfg.DropAccountIDs = drops

	if cfg.ProbeIntervalSeconds < 300 || cfg.ProbeIntervalSeconds > 7200 {
		return Config{}, "probe_interval_seconds 应为 300–7200 秒（探针过密=烧额度+加指纹）", false
	}
	if len(cfg.ProbeModel) > 128 {
		return Config{}, "probe_model 过长", false
	}
	switch cfg.ProbeReasoningEffort {
	case "", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return Config{}, "probe_reasoning_effort 合法值：none/minimal/low/medium/high/xhigh/max", false
	}
	if cfg.ProbeMinReasoningTokens < 1 || cfg.ProbeMinReasoningTokens > 10000 {
		return Config{}, "probe_min_reasoning_tokens 应为 1–10000（未配置=缺省 800；1=事实关闭）", false
	}
	if cfg.MaxConsecutiveProbeFailures < 2 || cfg.MaxConsecutiveProbeFailures > 10 {
		return Config{}, "max_consecutive_probe_failures 应为 2–10", false
	}
	if cfg.ProbeBackoffSeconds < 600 || cfg.ProbeBackoffSeconds > 86400 {
		return Config{}, "probe_backoff_seconds 应为 600–86400 秒", false
	}
	if cfg.ProbeScheduleMarginSeconds < 30 || cfg.ProbeScheduleMarginSeconds > 600 {
		return Config{}, "probe_schedule_margin_seconds 应为 30–600 秒", false
	}
	if cfg.ProbeBurstIntervalSeconds < 60 || cfg.ProbeBurstIntervalSeconds > 300 {
		return Config{}, "probe_burst_interval_seconds 应为 60–300 秒（密集档也不容许更密：烧额度+指纹）", false
	}
	// 归一：密集档不得疏于稳态档（合法域上 burst≤300≤steady 天然成立，
	// 防未来域调整后语义反转）。
	if cfg.ProbeBurstIntervalSeconds > cfg.ProbeIntervalSeconds {
		cfg.ProbeBurstIntervalSeconds = cfg.ProbeIntervalSeconds
	}
	if cfg.ProbeBurstUntilPasses < 2 || cfg.ProbeBurstUntilPasses > 10 {
		return Config{}, "probe_burst_until_passes 应为 2–10", false
	}
	if cfg.ProbeBurstMaxProbes < 5 || cfg.ProbeBurstMaxProbes > 100 {
		return Config{}, "probe_burst_max_probes 应为 5–100", false
	}
	return cfg, "", true
}

func validCookieName(name string) bool {
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}
