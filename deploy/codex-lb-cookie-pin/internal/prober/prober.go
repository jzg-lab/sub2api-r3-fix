// Package prober 实现质量探针自愈回路（v0.2）：静默降智无法从响应元数据
// 发现（模型字段永远显示所请求模型、faster-model 头不出现），唯一判别法是
// 无歧义判别题。本包提供题库、判分器与按账号的判定状态机；执行器（HTTP
// 往返）在 transport 包，复用业务请求模板保证指纹一致。
//
// 反指纹权衡：探针是主动出站流量，与「零探针」原则冲突，因此默认关闭
// （quality_probe_enabled），且请求复用该账号最近一笔业务请求的 URL/头/
// 代理——除题面外与业务流量同形。
package prober

import (
	"strings"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/pluginconfig"
)

// Verdict 是一次探针的判定结果。
type Verdict string

const (
	// VerdictPass 答对：当前钉扎（或账号）当前窗口满血。
	VerdictPass Verdict = "pass"
	// VerdictFail 答错：疑似路由级/账号级降智，触发重摇判定。
	VerdictFail Verdict = "fail"
	// VerdictError 传输/HTTP/解析失败：不是质量信号，不计入失败
	//（冷会话首发 503 发生率 ~70%，重试即愈）。
	VerdictError Verdict = "error"
)

// Question 是一道判别题。题面必须无歧义——歧义题（如「昨天的明天的后天」）
// 会把满血号误判成降智号，2026-10-01 实测已弃用。判分要求假阴性优先防御：
// 把好号判坏 = 白白重摇烧额度；把降智判好 = 下一周期自然纠正。
// v0.3.5 起题库为宿主资格针同源的 canary 卷（questions_canary.go）——
// 判分难度必须与毕业裁判同卷，见该文件头注释。
type Question struct {
	ID     string
	Prompt string
	Grade  func(text string) bool
}

// ContainsNumberToken 判定文本里是否出现独立的数字答案 token。
// Go 标准库正则不支持环视，故手工切词：连续 [0-9]（至多一个小数点）视作
// 一个 token，与 want 精确比较——「13」≠「3」；小数答案仅宽容纯尾零
// （「434.0」=434，「3.14」≠3）。
func ContainsNumberToken(text, want string) bool {
	var b strings.Builder
	hasDot := false
	flush := func() bool {
		token := b.String()
		b.Reset()
		hasDot = false
		if strings.IndexByte(token, '.') >= 0 {
			token = strings.TrimRight(token, "0")
			token = strings.TrimSuffix(token, ".")
		}
		return token == want
	}
	for _, r := range text {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' && b.Len() > 0 && !hasDot:
			hasDot = true
			b.WriteRune(r)
		default:
			if b.Len() > 0 && flush() {
				return true
			}
		}
	}
	return b.Len() > 0 && flush()
}

// State 是单账号的探针判定状态机。
type State struct {
	AccountID           int64 `json:"account_id"`
	Probes              int64 `json:"probes"`                // 累计探针（轮换计数基准）
	Fails               int64 `json:"fails"`                 // 累计答错
	QualityRerolls      int64 `json:"quality_rerolls"`       // 因答错触发的重摇次数
	ConsecFails         int   `json:"consec_fails"`          // 连续答错（答对清零）
	ConsecPasses        int   `json:"consecutive_passes"`    // 连续答对（答错或 error 清零）——救治区毕业阈值的数据源
	SuspectAccountLevel bool  `json:"suspect_account_level"` // 连续答错达阈值：疑似账号级（重摇无解）
	BurstProbes         int   `json:"burst_probes"`          // 本轮密集档已排针数（答错/达标出档清零）
	InBurst             bool  `json:"in_burst"`              // 当前处于密集档（未验证态，可观测）
	// KnownSignAt 已见过的最新签捕获时刻（新签即探的判据；不序列化——重启后
	// 首个 tick 重新对齐，恢复的旧签不触发拉近）。
	KnownSignAt    time.Time `json:"-"`
	LastVerdict    Verdict   `json:"last_verdict"`
	LastQuestionID string    `json:"last_question_id"`
	LastAnswer     string    `json:"last_answer"` // 模型输出前 80 字符（无敏感信息）
	// LastReasoningTokens 最近一针 completed usage 的推理 token 数（v0.3.4）；
	// 0 = 零推理量或未解析到 usage。仅观测上报，判定在 sendProbe 完成。
	LastReasoningTokens     int       `json:"last_reasoning_tokens"`
	LastProbeAt             time.Time `json:"last_probe_at"`
	PreviousPassAt          time.Time `json:"previous_pass_at"`
	NextProbeAt             time.Time `json:"next_probe_at"`
	BackoffUntil            time.Time `json:"backoff_until"`
	TruncationObservations  int64     `json:"truncation_observations"`
	TruncationWindowSamples int       `json:"truncation_window_samples"`
	TruncationWindowHits    int       `json:"truncation_window_hits"`
	TruncationRateAlert     bool      `json:"truncation_rate_alert"`
	truncationWindow        []bool
}

// NewState 为账号建初始状态。
func NewState(accountID int64) *State {
	return &State{AccountID: accountID}
}

// Due 报告现在是否应探该账号（退避期内或未到期都不探；ProbeAgainNow 的
// 立即复探由执行器绕过本判断）。
func (st *State) Due(now time.Time) bool {
	if now.Before(st.BackoffUntil) {
		return false
	}
	return !now.Before(st.NextProbeAt)
}

// Decision 是一次判定后状态机的动作指令。
type Decision struct {
	// ShouldReroll 当场丢该账号 Cookie 罐（重摇）。
	ShouldReroll bool
	// ProbeAgainNow 丢罐后立即复探（评估新签；执行器留 2-3 秒落定）。
	ProbeAgainNow bool
	// EnterBackoff 连续答错达阈值：判疑似账号级，停探退避。
	EnterBackoff bool
}

// Record 记一次判定并推进状态机。策略依据 2026-10-01 五账号实测：
//   - pass：清连续失败，按档位排下一轮（未验证态密集档，v0.3.2）；
//   - fail：重摇 + 立即复探（重摇可救池级降智，2 号 25%→0% 实证）；
//     连续错满阈值（默认 3）判账号级（3/5 号跨两次重摇稳定答错实证）——
//     继续重摇纯属烧额度，停探退避；
//   - error：非质量信号，不动失败计数，清连续通过证据，按档位重排。
func (st *State) Record(v Verdict, questionID, answer string, now time.Time, cfg pluginconfig.Config) Decision {
	// Preserve the failure evidence throughout backoff. Start a new attempt
	// budget only when the first post-backoff observation actually arrives.
	if !st.BackoffUntil.IsZero() && !now.Before(st.BackoffUntil) {
		st.ConsecFails = 0
		st.BackoffUntil = time.Time{}
	}
	previousProbeAt := st.LastProbeAt
	st.Probes++
	st.LastProbeAt = now
	st.LastVerdict = v
	st.LastQuestionID = questionID
	st.LastAnswer = TruncateAnswer(answer)
	st.recordTruncation(v == VerdictError && strings.HasPrefix(answer, "trunc-fp:"))
	switch v {
	case VerdictPass:
		st.ConsecFails = 0
		if st.ConsecPasses > 0 {
			st.PreviousPassAt = previousProbeAt
		} else {
			st.PreviousPassAt = time.Time{}
		}
		st.ConsecPasses++
		st.SuspectAccountLevel = false
		if st.ConsecPasses >= cfg.ProbeBurstUntilPasses {
			st.BurstProbes = 0 // 出档：下一段（稀疏档）从零计
		}
		st.NextProbeAt = now.Add(st.nextGap(cfg))
		return Decision{}
	case VerdictFail:
		st.Fails++
		st.ConsecFails++
		st.ConsecPasses = 0
		st.PreviousPassAt = time.Time{}
		st.BurstProbes = 0 // 新回合：密集档预算重置（fail 链由退避阈值封顶）
		if st.ConsecFails >= cfg.MaxConsecutiveProbeFailures {
			st.suspectEnterBackoff(now, cfg)
			return Decision{EnterBackoff: true}
		}
		st.QualityRerolls++
		st.NextProbeAt = now.Add(st.nextGap(cfg)) // 复探若中断的兜底排期
		return Decision{ShouldReroll: true, ProbeAgainNow: true}
	default: // VerdictError
		st.ConsecPasses = 0
		st.PreviousPassAt = time.Time{}
		st.NextProbeAt = now.Add(st.nextGap(cfg))
		return Decision{}
	}
}

// The last 20 probes provide a bounded rate signal, separate from quality
// counters. Five observations and at least 50% truncation raise a warning.
func (st *State) recordTruncation(truncated bool) {
	if truncated {
		st.TruncationObservations++
	}
	window := make([]bool, 0, 20)
	previous := st.truncationWindow
	if len(previous) >= 20 {
		previous = previous[len(previous)-19:]
	}
	window = append(window, previous...)
	window = append(window, truncated)
	st.truncationWindow = window
	st.TruncationWindowSamples, st.TruncationWindowHits = len(window), 0
	for _, hit := range window {
		if hit {
			st.TruncationWindowHits++
		}
	}
	st.TruncationRateAlert = len(window) >= 5 && st.TruncationWindowHits*2 >= len(window)
}

// nextGap 计算下一针间隔（v0.3.2 双档）：未验证态（连过 < 退出线且本轮密集
// 针数未封顶）用密集档，已验证/封顶后用稳态档。救治号停在零业务流量状态，
// 密集档是它的快速攒证据通道；健康号恒处已验证态，护栏语义不变。
// 出档/封顶即清 InBurst 标记；BurstProbes 只在本函数递增。
func (st *State) nextGap(cfg pluginconfig.Config) time.Duration {
	steady := time.Duration(cfg.ProbeIntervalSeconds) * time.Second
	burst := time.Duration(cfg.ProbeBurstIntervalSeconds) * time.Second
	if burst <= 0 || burst >= steady {
		st.InBurst = false
		return steady
	}
	if st.ConsecPasses < cfg.ProbeBurstUntilPasses && st.BurstProbes < cfg.ProbeBurstMaxProbes {
		st.BurstProbes++
		st.InBurst = true
		return burst
	}
	st.InBurst = false
	return steady
}

// 卡点排程的间隔边界与 probe_interval_seconds 的合法域对齐：密不过 300 秒
// （烧额度+指纹纪律），疏不过 7200 秒（降智漂移窗口内必有一针）。
// MinAdaptiveSamples 是卡点资格线：实测样本不足时退回固定间隔（冷启动纪律）。
const (
	adaptiveMinGap     = 300 * time.Second
	adaptiveMaxGap     = 7200 * time.Second
	MinAdaptiveSamples = 3
)

// AdaptiveNextProbe 按实测签寿命卡点排下一针（v0.3 计量学）。在 Record 排定
// 固定间隔之后调用可改排；仅适用于 pass/error（复探链与退避不卡点）。
//
// 规则（验收：落点带 jitter、与换签时刻无强相关；年轻签期少探、临死密集探）：
//
//	目标 = 签捕获 + 寿命p80 − 提前量(margin)
//	- 目标远（年轻签）：min(目标 − jitter, now + 上限)，省额度
//	- 目标已过/贴地（签超龄仍满血）：now + 最低间隔 + jitter，密集盯防
//	- jitter 幅度 = min(距目标的 1/4, margin)，均匀分布向前抖——落点不可预测
//	- 无实测寿命（p80<=0）或无签（capturedAt 零值）：退回固定间隔（冷启动纪律）
func AdaptiveNextProbe(now, capturedAt time.Time, p80Lifetime time.Duration, cfg pluginconfig.Config, rnd func() float64) time.Time {
	interval := time.Duration(cfg.ProbeIntervalSeconds) * time.Second
	if p80Lifetime <= 0 || capturedAt.IsZero() || !cfg.AdaptiveProbeScheduling {
		return now.Add(interval)
	}
	margin := time.Duration(cfg.ProbeScheduleMarginSeconds) * time.Second
	target := capturedAt.Add(p80Lifetime - margin)
	if gap := target.Sub(now); gap > adaptiveMinGap {
		jitterCap := gap / 4
		if margin > 0 && jitterCap > margin {
			jitterCap = margin
		}
		next := target.Add(-time.Duration(rnd() * float64(jitterCap)))
		if cap := now.Add(adaptiveMaxGap); next.After(cap) {
			next = cap
		}
		return next
	}
	return now.Add(adaptiveMinGap + time.Duration(rnd()*float64(margin)))
}

// Keep the failure count observable until backoff expires.
func (st *State) suspectEnterBackoff(now time.Time, cfg pluginconfig.Config) {
	st.SuspectAccountLevel = true
	st.BackoffUntil = now.Add(time.Duration(cfg.ProbeBackoffSeconds) * time.Second)
	st.NextProbeAt = st.BackoffUntil
}

// TruncateAnswer 截取答案摘要用于状态面板（模型输出无敏感信息，限长防刷屏）。
func TruncateAnswer(answer string) string {
	answer = strings.TrimSpace(answer)
	runes := []rune(answer)
	if len(runes) > 80 {
		return string(runes[:80]) + "…"
	}
	return answer
}
