package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	openAIDowngradeProbeMaxBodyBytes         = 4 << 20
	OpenAIDowngradeStateOnDuty               = "on_duty"
	OpenAIDowngradeStateCircuitOpen          = "circuit_open"
	OpenAIDowngradeStateReprobe              = "reprobe"
	OpenAIDowngradeStatePendingReplace       = "pending_replace"
	OpenAIDowngradeEventCircuitOpen          = "circuit_open"
	OpenAIDowngradeEventRecovered            = "recovered"
	OpenAIDowngradeEventReplaceRequired      = "replace_required"
	OpenAIDowngradeEventBucketReprobe        = "bucket_reprobe"
	OpenAIDowngradeEventBucketRescue         = "bucket_rescue"
	OpenAIDowngradeEventQualificationBlocked = "qualification_blocked"
	OpenAIDowngradeEventSolFallback          = "sol_fallback"
	OpenAIDowngradeEventRateLimitDeferred    = "rate_limit_deferred"
	// 稀疏复查期间收到上游真实接受（传输 OK 且 2xx）→ CAS 清除观察到的持有，
	// 账号回到调度。这是「官方到点重置 / 供应商提前手动重置」的检测回路终点。
	OpenAIDowngradeEventRateLimitRecheckRecovered = "rate_limit_recheck_recovered"
	// 相位2（2026-09-20 用户批准）：账号级 abuse 标记的响应侧证据事件。
	// turn_state_degraded = 探针针自身 x-codex-turn-state 长度落在降智态；
	// real_traffic_model_mismatch = 真实流量响应 model 与请求不符（usage 侧
	// upstream_model_mismatch 同款判定，经 openAIAbuseRouteSignals 桥投递）。
	// 两者都只驱动「记事件 + 加速复查」，单信号不摘号。
	OpenAIDowngradeEventTurnStateDegraded        = "turn_state_degraded"
	OpenAIDowngradeEventRealTrafficModelMismatch = "real_traffic_model_mismatch"
	OpenAIDowngradeEventRealTrafficRecheckArmed  = "real_traffic_recheck_armed"
	// 探针 auth 两振出局（r17aq，自 r17ap 移植）：401/403 不再一击判死。
	// strike = 首次连击暂停调度；terminal = 达阈值 SetError；cleared =
	// 非 auth 上游应答证明凭据被接受，自动解暂停；skipped_state_changed =
	// 探针在飞窗内账号被人工/并发改动，结果只记遥测不进状态机。
	OpenAIDowngradeEventProbeAuthStrike   = "probe_auth_strike"
	OpenAIDowngradeEventProbeAuthTerminal = "probe_auth_terminal"
	OpenAIDowngradeEventProbeAuthCleared  = "probe_auth_cleared"
	OpenAIDowngradeEventProbeSkippedStale = "probe_skipped_state_changed"
)

const (
	// openAIDowngradeTurnStateDegradedLen 降智态凭据长度（现网 9/18-9/20 全部
	// 降智号实测 356；健康号恒 332 零误报）。±20 容差吸收 Fernet 密文块边界
	// （社区观测同头差一个 AES 块），中心值漂移靠事件流量侧写发现。
	openAIDowngradeTurnStateDegradedLen = 356
	// openAIDowngradeTurnStateLenTolerance 长度容差。
	openAIDowngradeTurnStateLenTolerance = 20
)

const (
	OpenAIDowngradeFailureReasoningThreshold = 800
	OpenAIDowngradeRecoveryReasoningMinimum  = 1400
	openAIDowngradeQualificationExtraKey     = "openai_downgrade_qualification"
	openAIDowngradeSolFallbackExtraKey       = "openai_downgrade_sol_fallback"
	openAIDowngradeProbeProxyMinInterval     = 10 * time.Minute
	openAIDowngradeSolFallbackInterval       = 2 * time.Hour
	// 探针 auth 两振出局（r17aq）：401/403 常是桶 IP 的瞬态旗而非账号死刑。
	// 第一次连击只暂停调度；连续达 OpenAIDowngradeAuthStrikeThreshold 次才
	// SetError，期间按分钟级快复检拿结论。
	OpenAIDowngradeAuthStrikeThreshold = 2
	openAIDowngradeAuthRetryInterval   = 10 * time.Minute
	// 429 是「现在判不了」而非健康信号：被限流的账号若按常规 15-45 分钟排期，
	// 整个忙时段都可能拿不到一针结论性探测，真实降智窗口被无限拉长。
	// 限流后按短周期重探（经 jitter 后 2.5-7.5 分钟），尽快拿回结论。
	openAIDowngradeRateLimitedRetryInterval = 5 * time.Minute
	// 资格认证节奏：新号的认证针按分钟级间隔排（jitter 后 2.5-7.5 分钟），
	// 分钟级完成认证上岗（r15h 起 1 针通过即结业）；若沿用常规 nextDelay
	// （15-75 分钟）认证会拖到小时级。
	openAIDowngradeQualificationInterval = 5 * time.Minute
	// 429 长退避（2026-09-15 用户裁定「额度耗尽还一直探 探个屁啊」）：真实流量
	// 路径的 429 会解析 body 记账，探针路径此前只当「现在判不了」一律 2.5-7.5min
	// 短重试——耗尽号（1029 实测 3 小时 20 针）全程空打，每针都是从账号出口 IP
	// 发出的可聚类请求。两级退避：①429 体带显式重置时间（usage_limit_reached
	// 的 resets_at/resets_in_seconds，复用 parseOpenAIRateLimitResetTime）→ 按
	// 重置点+错峰或稀疏复查回来；②不带时间的 429 连打达阈值 → 退到小时级。
	openAIDowngradeRateLimitResetFloor   = 30 * time.Minute   // 重置点已过/过近时的排期下限
	openAIDowngradeRateLimitResetCap     = 8 * 24 * time.Hour // 重置点离谱远时的排期上钳
	openAIDowngradeRateLimitResetStagger = 30 * time.Minute   // 重置点后的错峰窗（jitter 15-45min）
	// Preserve r17i's account hold for unknown windows with distant reset evidence.
	openAIDowngradeRateLimitQuotaLikeDistance = 5*time.Hour + 30*time.Minute
	// 稀疏复查（2026-09-16 用户裁定「重置不是固定的，有时候可以手动重置」）：
	// 长持有不能死等重置点。每天最多一针的随机复查（spread 后 22-27.5h），
	// 每次重新抽签无可聚类周期；重置点更近时仍取重置点一侧（min 规则）。
	openAIDowngradeRateLimitRecheckInterval = 22 * time.Hour
	openAIDowngrade429StreakThreshold       = 6 // 无时间信息的连续 429 达此数即风暴退避
	openAIDowngrade429StreakBackoff         = time.Hour
	// 真实流量顺延：活跃账号的降智会直接体现在真实流量里，探针紧跟着真实请求
	// 发出反而制造可聚类的合成流量。近窗有真实推理流量时顺延常规探针；连续
	// 顺延有上限，保证 canary 覆盖不出现永久盲区。仅作用于 on_duty/normal。
	openAIDowngradeRecentTrafficWindow = 15 * time.Minute
	openAIDowngradeTrafficDeferral     = 45 * time.Minute
	openAIDowngradeMaxTrafficDeferrals = 3
	openAIDowngradeInterruptedRecheck  = time.Minute
)

var openAIDowngradeTruncationFingerprints = [...]int{516, 1034, 1552}

var (
	errOpenAIDowngradeProbeBodyUnavailable = errors.New("probe response body unavailable")
	errOpenAIDowngradeProbeBodyTooLarge    = errors.New("probe response body exceeds limit")
)

// OpenAIDowngradeSolFallbackExtraKey is exported for the repository adapter;
// the value remains an implementation detail of the account Extra contract.
const OpenAIDowngradeSolFallbackExtraKey = openAIDowngradeSolFallbackExtraKey

// OpenAIDowngradeQualificationExtraKey is exported for the repository adapter;
// the value remains an implementation detail of the account Extra contract.
const OpenAIDowngradeQualificationExtraKey = openAIDowngradeQualificationExtraKey

func isOpenAIDowngradeSolFallbackAccount(account *Account) bool {
	if account == nil || len(account.Extra) == 0 {
		return false
	}
	value, ok := account.Extra[openAIDowngradeSolFallbackExtraKey]
	if !ok {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func isOpenAIDowngradeSolModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "gpt-5.6-sol" || strings.HasPrefix(model, "gpt-5.6-sol-")
}

func isOpenAIDowngradeTruncationFingerprint(tokens int) bool {
	for _, fingerprint := range openAIDowngradeTruncationFingerprints {
		if tokens >= fingerprint-8 && tokens <= fingerprint+8 {
			return true
		}
	}
	return false
}

// isOpenAIDowngradeTurnStateLenDegraded 判定 x-codex-turn-state 头长度是否落在
// 降智态带（356±20；0=无头不算——401/异常路径本来就没头，不能当降智证据）。
// 单独成立只记事件+加速复查；与 IsDegraded 同针在场才双信号熔断。
func isOpenAIDowngradeTurnStateLenDegraded(length int) bool {
	return length > 0 &&
		length >= openAIDowngradeTurnStateDegradedLen-openAIDowngradeTurnStateLenTolerance &&
		length <= openAIDowngradeTurnStateDegradedLen+openAIDowngradeTurnStateLenTolerance
}

// OpenAIDowngradeProbeResult is the redacted, bill-free result of one probe.
// It intentionally contains no response text or credential material.
type OpenAIDowngradeProbeResult struct {
	AccountID       int64
	ProxyID         *int64
	Mode            string
	TransportOK     bool
	AnswerCorrect   bool
	ReasoningTokens *int
	Juice           *int
	Latency         time.Duration
	HTTPStatus      int
	ErrorMessage    string
	// RateLimitResetAt 非 nil 表示 429 带显式重置时间（x-codex-* 窗口头或
	// usage_limit_reached 体），调度层据此长退避，不再短周期空打。
	RateLimitResetAt *time.Time
	// Window identity is evidence, not the distance to the next reset.
	RateLimitWindow string
	// TurnStateLen 是该针响应 x-codex-turn-state 头的字符长度（0=无头）。
	// 只记长度不记值（隐私边界与既有观察哨一致）。现网双态：332=健康 /
	// 356=降智（9/18 死亡链 1082-1086 + 9/20 1097/1098 全部 356，健康号
	// 零误报）。相位2：356 记事件 + 加速复查；与降智证据同针在场时双信号
	// 熔断（2026-09-20 用户批准）。
	TurnStateLen int
	// gradedText 是该针判分所用的模型答案全文（applyResponse 填充；仅留档
	// 层读取——unexported 对 encoding/json 与仓库层不可见，绝不入库）。
	gradedText string
}

func (r OpenAIDowngradeProbeResult) answerVerdict() any {
	if !r.TransportOK {
		return nil
	}
	return r.AnswerCorrect
}

func (r OpenAIDowngradeProbeResult) IsDegraded() bool {
	if !r.TransportOK || (r.HTTPStatus != 0 && r.HTTPStatus != http.StatusOK) {
		return false
	}
	// 纯单针杀（2026-09-22 用户裁定）：降智态凭据长度（356±20）本身就是
	// 降智证据——答对+高 rt 也不豁免（1143 实证：356 针后紧跟答对针，
	// 治愈判据放行会在污染流量继续流的窗口里放走弹跳号）。无头（0）不算
	//（401/异常路径本来就没头，不能当降智证据）。
	if isOpenAIDowngradeTurnStateLenDegraded(r.TurnStateLen) {
		return true
	}
	if !r.AnswerCorrect {
		return true
	}
	// 2026-09-15 用户裁定：截断指纹叠加「答对」判中性——新号(1034/1035 实测
	// rt 恒落 1552±8 且答案正确)偶发被上游按推理预算截断，答案仍对说明推理
	// 没伤到结论；指纹只有在叠加答错(上方 !AnswerCorrect)时才构成降智证据。
	// 真截断伤害(1029 校准针03: 1552+答错)照走降智路径。IsRecovered 仍排除
	// 指纹——恢复连胜必须来自干净 ≥1400 针，1552+答对只保底不惩罚。
	return r.ReasoningTokens != nil &&
		*r.ReasoningTokens < OpenAIDowngradeFailureReasoningThreshold
}

// IsQualificationPass 认证档的答对主义判定（2026-09-15 用户裁定「他只要答对
// 就行」+「一次检测合格就上岗」）：答对 + rt≥800 即算通过针，不排除截断指纹
// ——1035 实测 rt 恒落 1552 且连续答对，若认证仍要求干净 ≥1400，中性针永远
// 凑不齐连胜，账号被钉死在资格节奏上。仅认证档使用；正常档判定不变。
func (r OpenAIDowngradeProbeResult) IsQualificationPass() bool {
	return r.TransportOK &&
		(r.HTTPStatus == 0 || r.HTTPStatus == http.StatusOK) &&
		r.AnswerCorrect &&
		r.ReasoningTokens != nil &&
		*r.ReasoningTokens >= OpenAIDowngradeFailureReasoningThreshold
}

// IsSuspectMiss r17al 滑误嫌疑针：答错但推理预算满血（rt≥恢复下限 1400）
// 且无 356 票。2026-09-26 统计定案：two_dim 随机题对健康号固有 ~18% 滑误率
// （rt1000-2000 带 52/296 答错；1187 rt4142 答错为全库唯一高 rt 孤例），而
// 降智执法的形态学是低 rt（131:11）+ 356 票——执法削推理预算，不污染推理
// 质量。嫌疑针不单针杀：清连胜 + 快复检（分钟级），两连错才熔断；铁证形态
// （356 票/低 rt/截断指纹答对）的单针杀纪律不变（见 Apply 分层）。
func (r OpenAIDowngradeProbeResult) IsSuspectMiss() bool {
	return r.TransportOK &&
		(r.HTTPStatus == 0 || r.HTTPStatus == http.StatusOK) &&
		!r.AnswerCorrect &&
		!isOpenAIDowngradeTurnStateLenDegraded(r.TurnStateLen) &&
		r.ReasoningTokens != nil &&
		*r.ReasoningTokens >= OpenAIDowngradeRecoveryReasoningMinimum
}

func (r OpenAIDowngradeProbeResult) IsRecovered() bool {
	return r.TransportOK &&
		(r.HTTPStatus == 0 || r.HTTPStatus == http.StatusOK) &&
		r.AnswerCorrect &&
		r.ReasoningTokens != nil &&
		*r.ReasoningTokens >= OpenAIDowngradeRecoveryReasoningMinimum &&
		!isOpenAIDowngradeTruncationFingerprint(*r.ReasoningTokens) &&
		// 纯单针杀配套（2026-09-22）：恢复连胜必须来自健康凭据长度的针——
		// 356 票在场就不是干净针，与 IsDegraded 的 356 判定互为镜像。
		!isOpenAIDowngradeTurnStateLenDegraded(r.TurnStateLen)
}

type OpenAIDowngradeProbeState struct {
	AccountID                 int64
	State                     string
	ProbeMode                 string
	OriginalProxyID           *int64
	CurrentProxyID            *int64
	ConsecutiveFailures       int
	ConsecutiveSuccesses      int
	Consecutive429s           int
	FirstFailureAt            *time.Time
	CircuitOpenedAt           *time.Time
	RecoveryDeadline          *time.Time
	NextProbeAt               time.Time
	SwapCount7d               int
	LastSwapAt                *time.Time
	LastProbeAt               *time.Time
	AuthConsecutiveFailures   int
	AstraConsecutiveFailures  int
	AstraConsecutiveSuccesses int
	AstraNextProbeAt          *time.Time
	UpdatedAt                 time.Time
}

// OpenAI proxy outcome kinds feed the per-proxy outcome statistics table
// (proxy_outcome_stats), which auto-syncs proxies.bucket_risk_score.
const (
	OpenAIProxyOutcomeSuccess      = "success"
	OpenAIProxyOutcomeDegraded     = "degraded"
	OpenAIProxyOutcomeAuthError    = "auth_error"
	OpenAIProxyOutcomeNetworkError = "network_error"
	OpenAIProxyOutcomeInconclusive = "inconclusive"
)

// ClassifyOpenAIDowngradeProxyOutcome buckets one probe result into a
// proxy-attributable outcome kind. Failures that happened before any proxy
// contact (token unavailable, missing local dependencies) and non-auth
// upstream rejections (429/5xx) are inconclusive: they say nothing about the
// bucket and must not move its risk score.
func ClassifyOpenAIDowngradeProxyOutcome(result *OpenAIDowngradeProbeResult) string {
	if result == nil {
		return OpenAIProxyOutcomeInconclusive
	}
	switch {
	case result.HTTPStatus == http.StatusUnauthorized || result.HTTPStatus == http.StatusForbidden:
		return OpenAIProxyOutcomeAuthError
	case !result.TransportOK:
		if result.HTTPStatus != 0 {
			return OpenAIProxyOutcomeInconclusive
		}
		if strings.Contains(result.ErrorMessage, "token") ||
			strings.Contains(result.ErrorMessage, "dependencies") {
			return OpenAIProxyOutcomeInconclusive
		}
		return OpenAIProxyOutcomeNetworkError
	case result.IsDegraded():
		return OpenAIProxyOutcomeDegraded
	case result.IsRecovered():
		return OpenAIProxyOutcomeSuccess
	default:
		return OpenAIProxyOutcomeInconclusive
	}
}

type OpenAIDowngradeTransition struct {
	State            OpenAIDowngradeProbeState
	NextState        string
	Circuit          bool
	Recovered        bool
	NeedsReplacement bool
	EventType        string
}

// ApplyOpenAIDowngradeProbeResult is pure state-machine logic. Inconclusive
// results neither punish nor heal an account; only the documented thresholds
// advance counters.
func ApplyOpenAIDowngradeProbeResult(
	state OpenAIDowngradeProbeState,
	result OpenAIDowngradeProbeResult,
	now time.Time,
) OpenAIDowngradeTransition {
	if state.State == "" {
		state.State = OpenAIDowngradeStateOnDuty
	}
	if state.ProbeMode == "" {
		state.ProbeMode = "normal"
	}

	if result.IsRecovered() ||
		(state.ProbeMode == "qualification" && result.IsQualificationPass()) {
		state.ConsecutiveFailures = 0
		state.ConsecutiveSuccesses++
		if (state.State == OpenAIDowngradeStateCircuitOpen ||
			state.State == OpenAIDowngradeStateReprobe) &&
			state.ConsecutiveSuccesses >= 2 {
			return OpenAIDowngradeTransition{
				State:     state,
				NextState: state.State,
				Recovered: true,
				EventType: OpenAIDowngradeEventRecovered,
			}
		}
		return OpenAIDowngradeTransition{State: state, NextState: state.State}
	}

	if !result.IsDegraded() {
		if result.TransportOK && (result.HTTPStatus == 0 || result.HTTPStatus == http.StatusOK) {
			state.ConsecutiveFailures = 0
			// 指纹+答对=中性且保留连胜(2026-09-15 用户裁定):若像普通中带
			// (800≤rt<1400)那样清零,穿插的 1552 会让认证的 2 连胜永远凑不齐,
			// 账号被钉死在 qualification 5min 节奏上,反而制造规律性探针流量。
			if !(result.AnswerCorrect && result.ReasoningTokens != nil &&
				isOpenAIDowngradeTruncationFingerprint(*result.ReasoningTokens)) {
				state.ConsecutiveSuccesses = 0
			}
		}
		return OpenAIDowngradeTransition{State: state, NextState: state.State}
	}

	state.ConsecutiveSuccesses = 0
	state.ConsecutiveFailures++
	// 纯单针杀（2026-09-22 用户裁定，1143 弹跳形态实证：换 IP→答对→快速
	// 再降智，拖第二针毫无意义）：normal 档任何一针**铁证**降智当场熔断。
	// qualification 新号线不叠加（2026-09-21 裁定 1115/1116 案保持）：
	// 新号无历史基线，首针失败走 qualification_failed 既有判死分支。
	// r17al 滑误分层（2026-09-26 1187 案）：满血答错（IsSuspectMiss）是
	// 嫌疑不是铁证——铁证单针杀保留，嫌疑针两连错才熔断（下方通用
	// ConsecutiveFailures>=2 分支自然涵盖）。误杀率从 ~18%（题库固有
	// 滑误率）压到 ~3%（滑误独立近似），代价=真降智号多活一个分钟级
	// 快复检窗口（processState 侧对嫌疑针按 suspectRecheckInterval 快排）。
	hardEvidence := !result.IsSuspectMiss()
	if state.State == OpenAIDowngradeStateOnDuty &&
		state.ProbeMode != "qualification" &&
		state.ConsecutiveFailures >= 1 &&
		(hardEvidence || state.ConsecutiveFailures >= 2) {
		return OpenAIDowngradeTransition{
			State:     state,
			NextState: OpenAIDowngradeStateCircuitOpen,
			Circuit:   true,
			EventType: OpenAIDowngradeEventCircuitOpen,
		}
	}
	if (state.State == OpenAIDowngradeStateCircuitOpen ||
		state.State == OpenAIDowngradeStateReprobe) &&
		state.ProbeMode != "qualification" &&
		state.ConsecutiveFailures >= 1 &&
		(hardEvidence || state.ConsecutiveFailures >= 2) {
		return OpenAIDowngradeTransition{
			State:            state,
			NextState:        OpenAIDowngradeStatePendingReplace,
			NeedsReplacement: true,
			EventType:        OpenAIDowngradeEventReplaceRequired,
		}
	}
	// qualification 新号线维持 2 连败判死（既有节奏）。
	if state.ConsecutiveFailures >= 2 {
		if state.State == OpenAIDowngradeStateOnDuty {
			return OpenAIDowngradeTransition{
				State:     state,
				NextState: OpenAIDowngradeStateCircuitOpen,
				Circuit:   true,
				EventType: OpenAIDowngradeEventCircuitOpen,
			}
		}
		if state.State == OpenAIDowngradeStateCircuitOpen ||
			state.State == OpenAIDowngradeStateReprobe {
			return OpenAIDowngradeTransition{
				State:            state,
				NextState:        OpenAIDowngradeStatePendingReplace,
				NeedsReplacement: true,
				EventType:        OpenAIDowngradeEventReplaceRequired,
			}
		}
	}

	_ = now
	return OpenAIDowngradeTransition{State: state, NextState: state.State}
}

type OpenAIDowngradeProbeStore interface {
	EnsureOpenAIDowngradeState(context.Context, int64, *int64, time.Time) (*OpenAIDowngradeProbeState, error)
	GetOpenAIDowngradeState(context.Context, int64) (*OpenAIDowngradeProbeState, error)
	ReconcileOpenAIRateLimitProbeSchedules(context.Context, time.Time, time.Duration) (int64, error)
	ListDueOpenAIDowngradeStates(context.Context, time.Time, int) ([]OpenAIDowngradeProbeState, error)
	SaveOpenAIDowngradeState(context.Context, *OpenAIDowngradeProbeState) error
	RecordOpenAIDowngradeProbe(context.Context, *OpenAIDowngradeProbeResult) error
	AppendOpenAIDowngradeEvent(context.Context, int64, *int64, string, map[string]any) error
	SetOpenAIAccountProxy(context.Context, int64, *int64) error
	FindOpenAIDowngradeMainProxy(context.Context, int64) (*int64, error)
	FindOpenAIDowngradeEscapeProxy(context.Context, int64, *int64) (*int64, error)
	ListOpenAIDowngradeBuckets(context.Context) ([]OpenAIDowngradeBucket, error)
	ListOpenAIDowngradeEvents(context.Context, time.Time, int) ([]OpenAIDowngradeEvent, error)
	ListOpenAIDowngradeAccountStats(context.Context, time.Time, int) ([]OpenAIDowngradeAccountStat, error)
	ListOpenAIDowngradeDashboard(context.Context, time.Time) (*OpenAIDowngradeDashboard, error)
}

// OpenAIDowngradeProbeHistoryCleaner is a narrow optional capability for
// retention cleanup (P2-10). Only the real repository implements it, so
// existing OpenAIDowngradeProbeStore test doubles stay unchanged.
type OpenAIDowngradeProbeHistoryCleaner interface {
	PurgeOpenAIDowngradeProbeHistory(ctx context.Context, resultsBefore, eventsBefore time.Time) (purgedResults, purgedEvents int64, err error)
}

// OpenAIDowngradeGoneAccountStateCleaner 清理已删除账号残留的探针状态。软删只置
// accounts.deleted_at，不触发 states 的 FK 级联；残留 state 会以陈旧的 next_probe_at
// 永久占据 ListDue 同 IP 分区的 rank-1，把同桶的活账号饿死在 rank-2。同样是窄可选
// 能力，只有真实 repository 实现，测试桩不受影响。
type OpenAIDowngradeGoneAccountStateCleaner interface {
	DeleteOpenAIDowngradeStatesForGoneAccounts(ctx context.Context) (int64, error)
}

// OpenAIDowngradeInterruptedProbeRechecker is a narrow optional capability.
// The real repository uses successful billable traffic after an interrupted
// probe to shorten the next normal recheck without changing account state.
type OpenAIDowngradeInterruptedProbeRechecker interface {
	AccelerateOpenAIInterruptedProbeRechecks(context.Context, time.Time, time.Duration) (int64, error)
}

// OpenAIDowngradeStateDeleter 就地删除单个账号的探针状态。processState 在账号
// 已被软删（GetByID → ErrAccountNotFound 哨兵）时调用：每日清理器要等 24h 周期，
// 期间僵尸 state 钉死 ListDue 每 IP 分区的 rank-1，同桶活账号零探针
// （2026-09-17 根因：1054 钉死 p5 → 1055 降智 5.5h 未检出，15 僵尸钉死全部
// 5 个在用桶）。窄可选能力，同上模式；未实现或删除失败时 processState 退化为
// 让位重排，绝不留在队首。
type OpenAIDowngradeStateDeleter interface {
	DeleteOpenAIDowngradeState(ctx context.Context, accountID int64) error
}

// OpenAIDowngradeFallbackModeStore persists the model-family restriction used
// while an account is healthy only on the sol track. It is deliberately a
// narrow optional capability so existing AccountRepository test doubles and
// non-OpenAI implementations do not need to grow unrelated methods.
type OpenAIDowngradeFallbackModeStore interface {
	SetOpenAIDowngradeFallbackMode(context.Context, int64, bool) error
}

type OpenAIDowngradeRateLimitStore interface {
	SetRateLimitedIfLater(context.Context, int64, time.Time) error
}

// OpenAIDowngradeRateLimitReleaser 以 CAS 语义清除一次已观察到的 OpenAI 账号
// 级限流持有（仅当 (limited_at, reset_at) 与观察值一致时清除）。供探针在持有
// 期间收到上游真实接受（传输 OK 且 2xx）的结果时回岗：官方到点重置与供应商
// 提前手动重置都由此回到调度，无需人为 reset。窄可选能力，与上面的 Store 同
// 模式：探测期间他处延长/改写的持有不会被旧观察值误清，由后续探针自纠。
// 注意与 Grok 的 ClearRateLimitIfObserved 分开：那条 CAS 钉了 platform=grok。
type OpenAIDowngradeRateLimitReleaser interface {
	ClearOpenAIRateLimitIfObserved(ctx context.Context, id int64, observedLimitedAt, observedResetAt time.Time) (bool, error)
}

// 探针题库已迁移至 openai_downgrade_probe_questions.go：四个语义互不相交的
// 题域（糖果组合 / 数字谜题 / 星期推算 / 网格路径）按针均匀抽取，期望答案
// 由生成器本地真值现场算出，判分正则按题动态构造。

const (
	// 常规档探针基准间隔（2026-09-22 用户裁定「上岗后探针频率不要那么高
	// 免得废IP」）：单针杀上线后检测灵敏度不再依赖针次密度——一针见血，
	// 常规节奏只服务「健康号的巡检」，拉长到 90 分钟基准（jitter 后 45-225
	// 分钟），同桶号均摊到每天 ~10 针内，护 IP 信誉。
	openAIDowngradeDefaultInterval     = 90 * time.Minute
	openAIDowngradeHalfOpenInterval    = 30 * time.Minute
	openAIDowngradeAcceleratedInterval = 5 * time.Minute
	// P2-10 数据保留：results 是高频遥测留 30 天，events 是审计依据留 90 天，
	// 每 24 小时清一次，防止两表无限增长。
	openAIDowngradeResultsRetention   = 30 * 24 * time.Hour
	openAIDowngradeEventsRetention    = 90 * 24 * time.Hour
	openAIDowngradePurgeInterval      = 24 * time.Hour
	openAIDowngradePurgeRetryInterval = 5 * time.Minute
	openAIDowngradeRecoveryWindow     = 30 * time.Minute
	openAIDowngradeReplacementWindow  = 24 * time.Hour
	openAIDowngradeSwapWindow         = 7 * 24 * time.Hour
	// 判死账号永不放弃：pending_replace 后按指数退避复活重试。
	// 静默 2h 起，每失败一轮翻倍，封顶 24h；重试次数从近 7 天的
	// replace_required 事件数推导，无需新增表字段。
	openAIDowngradeReplacementRetryBase   = 2 * time.Hour
	openAIDowngradeReplacementRetryMax    = 24 * time.Hour
	openAIDowngradeReplacementCountWindow = 7 * 24 * time.Hour
	// 判死终态（r17x 选项A）防御性让位间隔：processState 兜底分支把误入的
	// pending_replace 号排远，不参与正常调度节奏。
	openAIDowngradeReplacedQuietSchedule = 7 * 24 * time.Hour
)

// OpenAIDowngradeReplaceEventCounter 是用于判死重试退避的窄接口能力。
// 只有真实 repository 实现它，现有 OpenAIDowngradeProbeStore 测试桩不受影响。
type OpenAIDowngradeReplaceEventCounter interface {
	CountOpenAIDowngradeEvents(ctx context.Context, accountID int64, eventType string, since time.Time) (int, error)
}

// OpenAIDowngradeProbeRunner runs account probes on the account's currently
// bound proxy. It never writes usage_logs and never changes account priority.
type OpenAIDowngradeProbeRunner struct {
	store         OpenAIDowngradeProbeStore
	accountRepo   AccountRepository
	proxyRepo     ProxyRepository
	tokenProvider *OpenAITokenProvider
	httpUpstream  HTTPUpstream
	tlsProfiles   *TLSFingerprintProfileService
	interval      time.Duration
	now           func() time.Time
	nextDelay     func() time.Duration
	probeFn       func(context.Context, *Account, string) OpenAIDowngradeProbeResult
	runMu         sync.Mutex
	lifecycleMu   sync.Mutex
	stopped       bool
	runCancel     context.CancelFunc
	runDone       <-chan struct{}
	lastPurgeAt   time.Time
	purgeRetryAt  time.Time
	// recentTraffic 由装配层注入（usage_logs 近窗查询）；nil 时顺延逻辑关闭。
	// deferCounts 记录各账号连续顺延次数，仅在 runMu 临界区内访问。
	recentTraffic func(ctx context.Context, accountID int64, within time.Duration) bool
	deferCounts   map[int64]int
	// pluginBridge 由装配层注入（PluginManager.BridgeStatus）；nil 时健康快照
	// 不携带 plugin_bridge 区块（与未启用插件同形）。桥源读失败在源侧吞掉
	// 返回 nil，绝不拖垮健康快照。
	pluginBridge func(ctx context.Context) *PluginBridgeStatus
	// rescueLane 救治区编排器（r17ax Phase 3）；nil 时判死提交不触发自动
	// 入区（手动/对账入口不受影响）。自动钩子在 commit 通道 committed 块
	// 异步触发，入区失败绝不反向影响探针提交。
	rescueLane *OpenAIRescueLane
	// The staged runner retains this observation until its database commit.
	abuseSignal *openAIAbuseRouteSignal

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

func NewOpenAIDowngradeProbeRunner(
	store OpenAIDowngradeProbeStore,
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	tokenProvider *OpenAITokenProvider,
	httpUpstream HTTPUpstream,
	tlsProfiles *TLSFingerprintProfileService,
) *OpenAIDowngradeProbeRunner {
	runner := &OpenAIDowngradeProbeRunner{
		store:         store,
		accountRepo:   accountRepo,
		proxyRepo:     proxyRepo,
		tokenProvider: tokenProvider,
		httpUpstream:  httpUpstream,
		tlsProfiles:   tlsProfiles,
		interval:      openAIDowngradeDefaultInterval,
		now:           time.Now,
		deferCounts:   make(map[int64]int),
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	runner.nextDelay = func() time.Duration {
		// 15-75 分钟均匀随机（0.5x-2.5x）：探针体已复刻真实 codex 形态（数十 KB
		// instructions/tools，见 openai_downgrade_probe_template.go），单针输入
		// 成本不再可忽略，拉长常规节奏把额度成本摊薄；半开/加速/资格/sol 路径
		// 不走本函数，仍保持各自紧凑节奏。均匀随机也防止池级同步探测。
		return time.Duration(float64(runner.interval) * (0.5 + 2*probeRandomFloat()))
	}
	// 遥测出口兜底：转发链路构造的账号对象可能只带 ProxyID 而未预载 Proxy
	// 关联，遥测发送期经此函数补齐代理出口；仍解析不出则遥测丢弃，绝不直连
	// （openai_codex_telemetry.go 的 send 硬闸）。
	openAICodexTelemetryGlobal.bindProxyLookup(runner.resolveTelemetryProxyURL)
	return runner
}

func (r *OpenAIDowngradeProbeRunner) resolveTelemetryProxyURL(ctx context.Context, queued *Account) string {
	if queued == nil || queued.ProxyID == nil || r.accountRepo == nil || ctx.Err() != nil {
		return ""
	}
	current, err := r.accountRepo.GetByID(ctx, queued.ID)
	if err != nil || current == nil || current.ID != queued.ID ||
		current.Status != StatusActive || current.ProxyID == nil || *current.ProxyID != *queued.ProxyID {
		return ""
	}
	route, err := resolveOpenAIOAuthProxyURL(ctx, r.proxyRepo, current.ProxyID)
	if err != nil {
		return ""
	}
	if queued.Proxy != nil && queued.Proxy.URL() != route {
		return ""
	}
	return route
}

func (r *OpenAIDowngradeProbeRunner) Start() {
	if r == nil || r.store == nil {
		return
	}
	r.startOnce.Do(func() {
		go r.loop()
	})
}

func (r *OpenAIDowngradeProbeRunner) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.lifecycleMu.Lock()
		r.stopped = true
		cancel := r.runCancel
		r.lifecycleMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if r.stopCh != nil {
			close(r.stopCh)
		}
		// Consume Start when stopping an unstarted runner; no loop will close doneCh.
		r.startOnce.Do(func() {
			if r.doneCh != nil {
				close(r.doneCh)
			}
		})
	})
	r.lifecycleMu.Lock()
	runDone := r.runDone
	r.lifecycleMu.Unlock()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for _, done := range []<-chan struct{}{r.doneCh, runDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-timer.C:
			logger.LegacyPrintf("service.openai_downgrade_probe",
				"[OpenAIDowngradeProbe] stop timed out waiting for scan shutdown")
			return
		}
	}
}

func (r *OpenAIDowngradeProbeRunner) loop() {
	defer close(r.doneCh)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// Bounded context: an uncancellable hang (DB/network) must not
			// wedge every future tick behind runMu.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			if err := r.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.LegacyPrintf("service.openai_downgrade_probe",
					"[OpenAIDowngradeProbe] scan failed: %v", err)
			}
			cancel()
		case <-r.stopCh:
			return
		}
	}
}

// maybePurgeHistory runs daily retention, with bounded retries for incomplete
// attempts. Stores without the optional cleaner capability remain a no-op.
func (r *OpenAIDowngradeProbeRunner) maybePurgeHistory(ctx context.Context, now time.Time) {
	cleaner, hasHistoryCleaner := r.store.(OpenAIDowngradeProbeHistoryCleaner)
	staleCleaner, hasStateCleaner := r.store.(OpenAIDowngradeGoneAccountStateCleaner)
	if !hasHistoryCleaner && !hasStateCleaner {
		return
	}
	if !r.lastPurgeAt.IsZero() && now.Sub(r.lastPurgeAt) < openAIDowngradePurgeInterval {
		return
	}
	if ctx.Err() != nil || now.Before(r.purgeRetryAt) {
		return
	}
	completed := false
	defer func() {
		finishedAt := r.now()
		if !completed || ctx.Err() != nil {
			// A slow failure must not consume its own retry backoff.
			r.purgeRetryAt = finishedAt.Add(openAIDowngradePurgeRetryInterval)
			return
		}
		r.lastPurgeAt = finishedAt
		r.purgeRetryAt = time.Time{}
	}()
	failed := false
	if hasHistoryCleaner {
		purgedResults, purgedEvents, err := cleaner.PurgeOpenAIDowngradeProbeHistory(
			ctx, now.Add(-openAIDowngradeResultsRetention), now.Add(-openAIDowngradeEventsRetention))
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logger.LegacyPrintf("service.openai_downgrade_probe", "[OpenAIDowngradeProbe] history purge failed: %v", err)
			failed = true
		} else if purgedResults > 0 || purgedEvents > 0 {
			logger.LegacyPrintf("service.openai_downgrade_probe", "[OpenAIDowngradeProbe] history purged results=%d events=%d", purgedResults, purgedEvents)
		}
	}
	// The two cleaners are independent; only cancellation stops the next phase.
	if hasStateCleaner {
		purgedStates, staleErr := staleCleaner.DeleteOpenAIDowngradeStatesForGoneAccounts(ctx)
		if ctx.Err() != nil {
			return
		}
		if staleErr != nil {
			logger.LegacyPrintf("service.openai_downgrade_probe", "[OpenAIDowngradeProbe] stale state cleanup failed: %v", staleErr)
			failed = true
		} else if purgedStates > 0 {
			logger.LegacyPrintf("service.openai_downgrade_probe", "[OpenAIDowngradeProbe] stale states purged for gone accounts: %d", purgedStates)
		}
	}
	completed = !failed
}

// RunOnce performs one bounded scan. It is public to make startup/acceptance
// checks deterministic without waiting for the background ticker.
func (r *OpenAIDowngradeProbeRunner) RunOnce(ctx context.Context) error {
	if r == nil || r.store == nil || r.accountRepo == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !r.runMu.TryLock() {
		return nil
	}
	defer r.runMu.Unlock()
	r.lifecycleMu.Lock()
	if r.stopped {
		r.lifecycleMu.Unlock()
		return context.Canceled
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.runCancel = cancel
	r.runDone = done
	r.lifecycleMu.Unlock()
	defer func() {
		cancel()
		r.lifecycleMu.Lock()
		r.runCancel = nil
		r.runDone = nil
		close(done)
		r.lifecycleMu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := r.now()
	openAIAbuseRouteSignals.PurgeExpired(now)
	r.maybePurgeHistory(ctx, now)
	if err := ctx.Err(); err != nil {
		return err
	}
	accounts, err := r.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return err
	}
	for i := range accounts {
		if err := ctx.Err(); err != nil {
			return err
		}
		account := accounts[i]
		if !isOpenAIDowngradeProbeAccountEligible(&account, now) || account.Status != StatusActive {
			continue
		}
		qualification := isOpenAIDowngradeQualificationCandidate(&account) ||
			account.ProxyID == nil
		if !account.Schedulable && !qualification {
			continue
		}
		allowed, err := r.canRunOpenAIProbe(ctx, account.ID)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
		nextProbeAt := now.Add(r.nextDelay())
		if qualification {
			nextProbeAt = now
		}
		state, err := r.store.EnsureOpenAIDowngradeState(ctx, account.ID, account.ProxyID, nextProbeAt)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// One failing account must not abort the whole scan silently.
			logger.LegacyPrintf("service.openai_downgrade_probe",
				"[OpenAIDowngradeProbe] ensure state failed account=%d: %v", account.ID, err)
			continue
		}
		if qualification && state != nil && state.LastProbeAt == nil &&
			state.State == OpenAIDowngradeStateOnDuty && state.ProbeMode != "qualification" {
			if err := r.armQualificationAtomic(ctx, &account, state, now); err != nil {
				logger.LegacyPrintf("service.openai_downgrade_probe",
					"[OpenAIDowngradeProbe] arm qualification failed account=%d: %v", account.ID, err)
				continue
			}
		}
		if !qualification {
			if err := r.armAbuseSignalAtomic(ctx, &account, state, now); err != nil {
				logger.LegacyPrintf("service.openai_downgrade_probe",
					"[OpenAIDowngradeProbe] arm mismatch recheck failed account=%d: %v", account.ID, err)
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	reconciled, err := r.store.ReconcileOpenAIRateLimitProbeSchedules(ctx, now, openAIDowngradeRateLimitRecheckInterval)
	if err != nil {
		return err
	}
	if reconciled > 0 {
		logger.LegacyPrintf("service.openai_downgrade_probe",
			"[OpenAIDowngradeProbe] historical rate limit schedules reconciled=%d", reconciled)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if rechecker, ok := r.store.(OpenAIDowngradeInterruptedProbeRechecker); ok {
		accelerated, err := rechecker.AccelerateOpenAIInterruptedProbeRechecks(
			ctx, now, openAIDowngradeInterruptedRecheck)
		if err != nil {
			return err
		}
		if accelerated > 0 {
			logger.LegacyPrintf("service.openai_downgrade_probe",
				"[OpenAIDowngradeProbe] interrupted probes accelerated=%d", accelerated)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	due, err := r.store.ListDueOpenAIDowngradeStates(ctx, now, 100)
	if err != nil {
		return err
	}
	processed := 0
	for i := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := r.processStateAtomic(ctx, &due[i], now)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// One broken account must not starve the remaining pool — but the
			// failure must be visible, or a wedged account goes dark for days.
			logger.LegacyPrintf("service.openai_downgrade_probe",
				"[OpenAIDowngradeProbe] process failed account=%d state=%s: %v",
				due[i].AccountID, due[i].State, err)
			continue
		}
		processed++
	}
	if len(due) > 0 {
		logger.LegacyPrintf("service.openai_downgrade_probe",
			"[OpenAIDowngradeProbe] scan done: due=%d processed=%d", len(due), processed)
	}
	return ctx.Err()
}

func isOpenAIDowngradeProbeStatusAllowed(status string, state *OpenAIDowngradeProbeState) bool {
	if state == nil {
		return false
	}
	if status == StatusActive {
		return true
	}
	if status != StatusError {
		return false
	}
	return state.State == OpenAIDowngradeStateCircuitOpen ||
		state.State == OpenAIDowngradeStateReprobe ||
		state.State == OpenAIDowngradeStatePendingReplace ||
		(state.State == OpenAIDowngradeStateOnDuty && state.ProbeMode == "qualification") ||
		state.ProbeMode == "sol_fallback"
}

func isOpenAIDowngradeProbeAccountEligible(account *Account, now time.Time) bool {
	return account != nil && account.Platform == PlatformOpenAI &&
		account.Type == AccountTypeOAuth && !account.IsCredentialShadow() &&
		(!account.AutoPauseOnExpired || account.ExpiresAt == nil || now.Before(*account.ExpiresAt))
}

// dropGoneAccountState 处理账号已消失的 state：优先就地删除（每日清理器的
// 单账号版，让同桶活账号立刻赢回 rank-1）；删除失败或 store 未实现删除能力
// 时退化为让位重排——两种出路都保证下一轮扫描不再被该 state 钉死队首。
func (r *OpenAIDowngradeProbeRunner) dropGoneAccountState(
	ctx context.Context,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	if deleter, ok := r.store.(OpenAIDowngradeStateDeleter); ok {
		if err := deleter.DeleteOpenAIDowngradeState(ctx, state.AccountID); err == nil {
			logger.LegacyPrintf("service.openai_downgrade_probe",
				"[OpenAIDowngradeProbe] dropped state for gone account=%d state=%s",
				state.AccountID, state.State)
			return nil
		} else {
			logger.LegacyPrintf("service.openai_downgrade_probe",
				"[OpenAIDowngradeProbe] drop state failed account=%d: %v (yielding queue head instead)",
				state.AccountID, err)
		}
	}
	state.NextProbeAt = now.Add(r.spread(openAIDowngradeHalfOpenInterval))
	state.UpdatedAt = now
	return r.store.SaveOpenAIDowngradeState(ctx, state)
}

func (r *OpenAIDowngradeProbeRunner) processState(
	ctx context.Context,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	if state == nil {
		return nil
	}
	account, err := r.accountRepo.GetByID(ctx, state.AccountID)
	if errors.Is(err, ErrAccountNotFound) || (err == nil && account == nil) {
		// 面板软删的账号（ent 软删 → GetByID 走 ErrAccountNotFound 哨兵，
		// translatePersistenceError 保留 Code+Reason，errors.Is 可穿透）：
		// 残留 state 的 next_probe_at 冻结在过去，ListDue 每 IP 桶只取
		// rank-1，僵尸会永久钉死队首饿死同桶活号。就地删除；
		// 瞬时 DB 错误（非 NotFound 哨兵）不走这里，原样上抛下轮重试。
		return r.dropGoneAccountState(ctx, state, now)
	}
	if err != nil {
		return err
	}
	if !isOpenAIDowngradeProbeAccountEligible(account, now) {
		// 活账号但当前不可探测（改平台/改类型/影子/过期）：不探，但排期
		// 后移让出队首，否则同桶其它号被永久饿死——与手动暂停分支同语义，
		// 30 分钟后回看是否改回。
		state.NextProbeAt = now.Add(r.spread(openAIDowngradeHalfOpenInterval))
		state.UpdatedAt = now
		return r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	// status='error' 的号不参与流量调度，但熔断/重探/判死状态下的号正是
	// 靠探测换 IP 救回来的，不能因 error 状态被永久跳过。
	if !isOpenAIDowngradeProbeStatusAllowed(account.Status, state) {
		// 同上：不探但让位重排（status=disabled/inactive 等），钉死队首
		// 与账号消失同罪。
		state.NextProbeAt = now.Add(r.spread(openAIDowngradeHalfOpenInterval))
		state.UpdatedAt = now
		return r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	if state.State == OpenAIDowngradeStatePendingReplace {
		// 判死终态不再自动排探针。ListDue 已在 SQL 层排除 pending_replace，
		// 理论到不了这里；防御性让位（NextProbeAt 推远）防止其它路径把
		// 判死号又拉回普通探测循环。判死号的唯一救援入口 = 手动启用/救治区。
		state.NextProbeAt = now.Add(openAIDowngradeReplacedQuietSchedule)
		state.UpdatedAt = now
		return r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	if state.State == OpenAIDowngradeStateOnDuty && !account.Schedulable &&
		state.ProbeMode != "qualification" &&
		state.AuthConsecutiveFailures == 0 {
		// 用户手动暂停的号不探测，但排期必须后移让出同 IP 的队首位置，
		// 否则同 IP 的其它号会被永久饿死；30 分钟后回来看是否被重新启用。
		// spread 错开同批暂停号的回访时刻，避免同一分钟集体回队。
		// auth 一振暂停豁免（r17aq）：schedulable=false 是探针自己落的，
		// 只有后续探针能洗白（非 auth 应答）或毕业到 error（二振）；
		// 不豁免 = 暂停号永不再被探，死锁。
		state.NextProbeAt = now.Add(r.spread(openAIDowngradeHalfOpenInterval))
		state.UpdatedAt = now
		return r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	if state.ProbeMode == "qualification" && account.ProxyID == nil &&
		state.OriginalProxyID != nil &&
		IsOpenAIBrowserOAuthAccount(account) {
		// A browser OAuth account that was already bound must retain its
		// authorization route. A genuinely fresh import has no original
		// binding and continues into automatic bucket assignment below.
		if account.Schedulable {
			if err := r.accountRepo.SetSchedulable(ctx, account.ID, false); err != nil {
				return err
			}
		}
		state.ConsecutiveFailures, state.ConsecutiveSuccesses = 0, 0
		state.NextProbeAt = now.Add(r.nextDelay())
		state.UpdatedAt = now
		if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
			OpenAIDowngradeEventQualificationBlocked, map[string]any{"reason": "authorization_proxy_missing"}); err != nil {
			return err
		}
		return r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	if !sameOpenAIProbeProxy(state.CurrentProxyID, account.ProxyID) {
		// A manual binding change invalidates counters from the old account/IP pair.
		state.CurrentProxyID = account.ProxyID
		state.OriginalProxyID = account.ProxyID
		state.ConsecutiveFailures = 0
		state.ConsecutiveSuccesses = 0
	}
	if account.Proxy != nil && (account.ProxyID == nil || account.Proxy.ID != *account.ProxyID) {
		account.Proxy = nil
	}
	if state.ProbeMode == "qualification" && account.ProxyID == nil {
		mainProxyID, findErr := r.store.FindOpenAIDowngradeMainProxy(ctx, account.ID)
		if findErr != nil {
			return findErr
		}
		if mainProxyID == nil {
			state.NextProbeAt = now.Add(r.nextDelay())
			state.UpdatedAt = now
			if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
				OpenAIDowngradeEventQualificationBlocked, map[string]any{"reason": "no_available_main_bucket"}); err != nil {
				return err
			}
			return r.store.SaveOpenAIDowngradeState(ctx, state)
		}
		if err := r.store.SetOpenAIAccountProxy(ctx, state.AccountID, mainProxyID); err != nil {
			return err
		}
		account.ProxyID = mainProxyID
		state.CurrentProxyID = mainProxyID
		state.OriginalProxyID = mainProxyID
		state.UpdatedAt = now
		if err := r.store.SaveOpenAIDowngradeState(ctx, state); err != nil {
			return err
		}
	}
	if state.ProbeMode == "sol_fallback" && state.State != OpenAIDowngradeStateOnDuty {
		return r.startSolFallback(ctx, account, state, now)
	}
	if state.State == OpenAIDowngradeStateReprobe {
		return r.processReprobe(ctx, account, state, now)
	}
	if state.ProbeMode == "sol_fallback" {
		return r.processSolFallback(ctx, account, state, now)
	}
	if state.State == OpenAIDowngradeStateCircuitOpen &&
		state.ProbeMode == "half_open" {
		return r.processHalfOpen(ctx, account, state, now)
	}
	if state.State == OpenAIDowngradeStateCircuitOpen {
		// Circuit opening is a cooling period. Do not probe or move the
		// account until the 30-minute quiet window has elapsed.
		if state.CircuitOpenedAt != nil &&
			now.Before(state.CircuitOpenedAt.Add(openAIDowngradeRecoveryWindow)) {
			state.NextProbeAt = state.CircuitOpenedAt.Add(r.spread(openAIDowngradeRecoveryWindow))
			state.UpdatedAt = now
			return r.store.SaveOpenAIDowngradeState(ctx, state)
		}
		return r.beginReprobe(ctx, account, state, now)
	}

	// Read without consuming: only processStateAtomic may acknowledge the
	// observation, after the event and probe state have committed together.
	absorbedAbuseSignal := false
	if signal, ok := openAIAbuseRouteSignals.PeekRealTrafficSignal(state.AccountID, now); ok {
		absorbedAbuseSignal = true
		if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
			OpenAIDowngradeEventRealTrafficModelMismatch, map[string]any{
				"requested_model": signal.RequestedModel,
				"response_model":  signal.ResponseModel,
				"observed_at":     signal.ObservedAt.Format(time.RFC3339),
			}); err != nil {
			return err
		}
		r.abuseSignal = &signal
	}

	if !absorbedAbuseSignal &&
		state.State == OpenAIDowngradeStateOnDuty && state.ProbeMode == "normal" &&
		r.shouldDeferForRealTraffic(ctx, state.AccountID) {
		recheck, err := r.hasPendingAbuseRecheck(ctx, state)
		if err != nil {
			return err
		}
		// 近窗有真实推理流量：降智会直接体现在真实流量里，此刻插入合成探针
		// 只会制造紧随真实请求的可聚类流量。顺延（jitter 后 22-67 分钟），
		// 连续顺延达上限后照常探测，保证 canary 覆盖无永久盲区。
		if !recheck {
			state.NextProbeAt = now.Add(r.jitter(openAIDowngradeTrafficDeferral))
			state.UpdatedAt = now
			return r.store.SaveOpenAIDowngradeState(ctx, state)
		}
		r.deferCounts[state.AccountID] = 0
	}

	result, stop, err := r.runProbeRecorded(ctx, account, state, state.ProbeMode, now)
	if stop {
		return err
	}
	if handled, err := r.applyRateLimitDeferral(ctx, account, state, result, now); handled {
		return err
	}
	// 相位2（2026-09-20 用户批准）+ 纯单针杀（2026-09-22 用户裁定）：
	// turn_state_len 落降智态（356±20）时记事件；normal 档单 356 即熔断
	// （1143 弹跳形态实证：换 IP→答对→快速再降智，等第二针只是放行污染
	// 流量）。qualification 新号线不叠加（2026-09-21 裁定 1115/1116 案）。
	turnStateDegraded := isOpenAIDowngradeTurnStateLenDegraded(result.TurnStateLen)
	if turnStateDegraded {
		if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
			OpenAIDowngradeEventTurnStateDegraded, map[string]any{
				"mode":             result.Mode,
				"turn_state_len":   result.TurnStateLen,
				"http_status":      result.HTTPStatus,
				"transport_ok":     result.TransportOK,
				"answer_correct":   result.answerVerdict(),
				"reasoning_tokens": result.ReasoningTokens,
			}); err != nil {
			return err
		}
	}
	singleShotCircuit := turnStateDegraded && state.ProbeMode == "normal" &&
		state.State == OpenAIDowngradeStateOnDuty
	transition := ApplyOpenAIDowngradeProbeResult(*state, result, now)
	if singleShotCircuit && !transition.Circuit {
		// 单 356（答对也在场）：把连败计数推到熔断阈值，复用既有熔断路径
		// （事件、摘调度、冷却排期全部同款），不另起一套摘除逻辑。
		state.ConsecutiveFailures = 1
		transition = ApplyOpenAIDowngradeProbeResult(*state, result, now)
	}
	*state = transition.State
	state.LastProbeAt = &now
	state.UpdatedAt = now
	state.NextProbeAt = now.Add(r.nextDelay())
	if state.ProbeMode == "qualification" && !transition.Circuit {
		// 资格认证节奏：认证针按分钟级排（429 已在上方走同量级短周期）；
		// 通过即解锁（r15h 起 1 针结业），此排期仅服务「未通过前的重试」
		// 与「解锁针后的首针 on-duty 复查」。
		state.NextProbeAt = now.Add(r.jitter(openAIDowngradeQualificationInterval))
	}
	if state.ProbeMode == "accelerated" && state.RecoveryDeadline != nil &&
		now.Before(*state.RecoveryDeadline) {
		state.NextProbeAt = now.Add(r.jitter(openAIDowngradeAcceleratedInterval))
	}
	// r17al 滑误嫌疑针快复检：未熔断未判死的嫌疑针按分钟级快排，把「冤枉
	// 一个健康号的摘调度窗口」从常规节奏压到分钟级；复检答对即清败洗白，
	// 再错（任何形态）按两连熔断。qualification 档本身已是 5min 节奏，
	// 不叠加。
	if !transition.Circuit && !transition.NeedsReplacement &&
		result.IsSuspectMiss() &&
		state.ProbeMode == "normal" &&
		state.State == OpenAIDowngradeStateOnDuty {
		state.NextProbeAt = now.Add(r.jitter(openAIDowngradeSuspectRecheckInterval))
	}
	if state.ProbeMode == "qualification" && result.IsQualificationPass() &&
		state.ConsecutiveSuccesses >= 1 {
		// 2026-09-15 用户裁定「新号一次检测合格就可以上岗，不要整那么多次」：
		// 1 针通过即解锁 schedulable 并回归 normal 档，认证不再凑 2 连胜。
		state.State = OpenAIDowngradeStateOnDuty
		state.ProbeMode = "normal"
		state.ConsecutiveSuccesses = 0
		if err := r.accountRepo.SetSchedulable(ctx, state.AccountID, true); err != nil {
			return err
		}
	}
	if state.ProbeMode == "qualification" && transition.Circuit {
		state.State = OpenAIDowngradeStatePendingReplace
		state.ProbeMode = "normal"
		state.NextProbeAt = now.Add(r.spread(openAIDowngradeReplacementWindow))
		state.UpdatedAt = now
		if err := r.accountRepo.SetSchedulable(ctx, state.AccountID, false); err != nil {
			return err
		}
		if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
			OpenAIDowngradeEventReplaceRequired, map[string]any{"reason": "qualification_failed"}); err != nil {
			return err
		}
		return r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	if transition.Circuit {
		state.State = OpenAIDowngradeStateCircuitOpen
		state.ProbeMode = "normal"
		state.CircuitOpenedAt = &now
		state.ConsecutiveFailures = 0
		state.ConsecutiveSuccesses = 0
		if err := r.accountRepo.SetSchedulable(ctx, state.AccountID, false); err != nil {
			return err
		}
		if err := r.store.SaveOpenAIDowngradeState(ctx, state); err != nil {
			return err
		}
		if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
			OpenAIDowngradeEventCircuitOpen, map[string]any{"reason": "downgrade_probe"}); err != nil {
			return err
		}
		// Keep the account fully isolated for the cooling period. The next
		// due run will select an escape bucket after this deadline.
		// spread 随机化冷却时长（2026-09-15 用户裁定「监测要随机时间」）：固定
		// 30 分钟会让同批熔断号在同一秒集体进入换桶，形成可聚类节律。
		state.NextProbeAt = now.Add(r.spread(openAIDowngradeRecoveryWindow))
		state.UpdatedAt = now
		return r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	if state.RecoveryDeadline != nil && !now.Before(*state.RecoveryDeadline) {
		state.RecoveryDeadline = nil
		state.ProbeMode = "normal"
	}
	if state.AuthConsecutiveFailures > 0 &&
		state.NextProbeAt.After(now.Add(openAIDowngradeAuthRetryInterval)) {
		// auth 暂停 limbo 里的无结论针：保持分钟级快复检节奏，不吃常规
		// 15-75 分钟排期。
		state.NextProbeAt = now.Add(openAIDowngradeAuthRetryInterval)
	}
	return r.store.SaveOpenAIDowngradeState(ctx, state)
}

func (r *OpenAIDowngradeProbeRunner) processReprobe(
	ctx context.Context,
	account *Account,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	result, stop, err := r.runProbeRecorded(ctx, account, state, "reprobe", now)
	if stop {
		return err
	}
	if handled, err := r.applyRateLimitDeferral(ctx, account, state, result, now); handled {
		return err
	}
	transition := ApplyOpenAIDowngradeProbeResult(*state, result, now)
	*state = transition.State
	state.LastProbeAt = &now
	state.UpdatedAt = now
	if transition.Recovered {
		return r.finishRescue(ctx, state, now)
	}
	if transition.NeedsReplacement {
		return r.startSolFallback(ctx, account, state, now)
	}
	// Reprobe attempts are deliberately serialized. The repository also
	// filters due states by proxy cooldown so different accounts sharing an IP
	// cannot be probed back-to-back. spread 保持 10 分钟串行下界不变、只随机化
	// 上沿（10-12.5 分钟），固定间隔本身是可聚类节律（用户裁定「监测要随机时间」）。
	state.NextProbeAt = now.Add(r.spread(openAIDowngradeProbeProxyMinInterval))
	return r.store.SaveOpenAIDowngradeState(ctx, state)
}

func isOpenAIDowngradeQualificationCandidate(account *Account) bool {
	if account == nil || account.Extra == nil {
		return false
	}
	value, ok := account.Extra[openAIDowngradeQualificationExtraKey]
	return ok && value == true
}

func (r *OpenAIDowngradeProbeRunner) processHalfOpen(
	ctx context.Context,
	account *Account,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	result, stop, err := r.runProbeRecorded(ctx, account, state, "half_open", now)
	if stop {
		return err
	}
	if handled, err := r.applyRateLimitDeferral(ctx, account, state, result, now); handled {
		return err
	}
	transition := ApplyOpenAIDowngradeProbeResult(*state, result, now)
	*state = transition.State
	state.LastProbeAt = &now
	state.UpdatedAt = now
	if transition.Recovered {
		return r.finishRescue(ctx, state, now)
	}
	if state.RecoveryDeadline != nil && !now.Before(*state.RecoveryDeadline) {
		return r.startSolFallback(ctx, account, state, now)
	}
	state.NextProbeAt = now.Add(r.jitter(openAIDowngradeHalfOpenInterval))
	return r.store.SaveOpenAIDowngradeState(ctx, state)
}

func (r *OpenAIDowngradeProbeRunner) beginReprobe(
	ctx context.Context,
	account *Account,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	if account == nil || account.ProxyID == nil || *account.ProxyID <= 0 ||
		!sameOpenAIProbeProxy(account.ProxyID, state.CurrentProxyID) {
		return errOpenAIOAuthProxyUnavailable
	}
	// Recovery is not authorization to change a browser login's egress.
	state.State = OpenAIDowngradeStateReprobe
	state.ProbeMode = "normal"
	state.ConsecutiveFailures = 0
	state.ConsecutiveSuccesses = 0
	state.RecoveryDeadline = nil
	state.NextProbeAt = now
	state.UpdatedAt = now
	if err := r.store.SaveOpenAIDowngradeState(ctx, state); err != nil {
		return err
	}
	if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
		OpenAIDowngradeEventBucketReprobe, map[string]any{"route_preserved": true}); err != nil {
		return err
	}
	return r.processReprobe(ctx, account, state, now)
}

func (r *OpenAIDowngradeProbeRunner) startSolFallback(
	ctx context.Context,
	account *Account,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	result, stop, err := r.runProbeRecorded(ctx, account, state, "sol_fallback", now)
	if stop {
		return err
	}
	// Preserve the pending model track across an inconclusive attempt/restart.
	state.ProbeMode = "sol_fallback"
	if handled, err := r.applyRateLimitDeferral(ctx, account, state, result, now); handled {
		return err
	}
	state.LastProbeAt = &now
	state.UpdatedAt = now
	if result.IsDegraded() {
		return r.finishReplacement(ctx, state, now)
	}
	if !result.IsRecovered() {
		state.NextProbeAt = now.Add(r.spread(openAIDowngradeProbeProxyMinInterval))
		return r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	if err := r.setSolFallbackMode(ctx, state.AccountID, true); err != nil {
		return err
	}
	state.State = OpenAIDowngradeStateOnDuty
	state.ProbeMode = "sol_fallback"
	state.ConsecutiveFailures = 0
	state.ConsecutiveSuccesses = 1
	state.AstraConsecutiveFailures = 0
	state.AstraConsecutiveSuccesses = 0
	// astra 复查针随机化（jitter 后 1-3 小时），不落整点 2h 固定节律。
	state.AstraNextProbeAt = timePtr(now.Add(r.jitter(openAIDowngradeSolFallbackInterval)))
	state.RecoveryDeadline = nil
	state.NextProbeAt = now.Add(r.jitter(openAIDowngradeDefaultInterval))
	if err := r.accountRepo.SetSchedulable(ctx, state.AccountID, true); err != nil {
		return err
	}
	if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
		OpenAIDowngradeEventSolFallback, map[string]any{"model": "gpt-5.6-sol"}); err != nil {
		return err
	}
	return r.store.SaveOpenAIDowngradeState(ctx, state)
}

func (r *OpenAIDowngradeProbeRunner) processSolFallback(
	ctx context.Context,
	account *Account,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	astraDue := state.AstraNextProbeAt != nil && !now.Before(*state.AstraNextProbeAt)
	mode := "sol_fallback"
	if astraDue {
		mode = "sol_fallback_astra"
	}
	result, stop, err := r.runProbeRecorded(ctx, account, state, mode, now)
	if stop {
		return err
	}
	if handled, err := r.applyRateLimitDeferral(ctx, account, state, result, now); handled {
		return err
	}
	state.LastProbeAt = &now
	state.UpdatedAt = now
	if astraDue {
		if result.IsRecovered() {
			state.AstraConsecutiveFailures = 0
			state.AstraConsecutiveSuccesses++
			if state.AstraConsecutiveSuccesses >= 2 {
				if err := r.setSolFallbackMode(ctx, state.AccountID, false); err != nil {
					return err
				}
				state.ProbeMode = "normal"
				state.AstraConsecutiveSuccesses = 0
				state.AstraNextProbeAt = nil
				if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
					OpenAIDowngradeEventRecovered, map[string]any{"model": "gpt-6-astra", "return_to_astra": true}); err != nil {
					return err
				}
			}
		} else if result.IsDegraded() {
			state.AstraConsecutiveFailures++
			state.AstraConsecutiveSuccesses = 0
		}
		if state.ProbeMode == "sol_fallback" {
			state.AstraNextProbeAt = timePtr(now.Add(r.jitter(openAIDowngradeSolFallbackInterval)))
		}
	} else {
		if result.IsRecovered() {
			state.ConsecutiveFailures = 0
			state.ConsecutiveSuccesses++
		} else if result.IsDegraded() {
			// A sol failure means the account is not safe for either model
			// track. Do not keep serving while waiting for another probe.
			return r.finishReplacement(ctx, state, now)
		}
	}
	// Due selection uses NextProbeAt, not AstraNextProbeAt. Advance both tracks
	// so an Astra recheck cannot leave this account at the head of the due queue.
	state.NextProbeAt = now.Add(r.jitter(openAIDowngradeDefaultInterval))
	return r.store.SaveOpenAIDowngradeState(ctx, state)
}

func (r *OpenAIDowngradeProbeRunner) setSolFallbackMode(ctx context.Context, accountID int64, active bool) error {
	store, ok := r.accountRepo.(OpenAIDowngradeFallbackModeStore)
	if !ok {
		return errors.New("openai downgrade fallback mode store unavailable")
	}
	return store.SetOpenAIDowngradeFallbackMode(ctx, accountID, active)
}

func (r *OpenAIDowngradeProbeRunner) finishRescue(
	ctx context.Context,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	if err := r.setSolFallbackMode(ctx, state.AccountID, false); err != nil {
		return err
	}
	state.State = OpenAIDowngradeStateOnDuty
	state.ProbeMode = "accelerated"
	state.ConsecutiveFailures = 0
	state.ConsecutiveSuccesses = 0
	state.RecoveryDeadline = timePtr(now.Add(openAIDowngradeRecoveryWindow))
	state.NextProbeAt = now.Add(r.jitter(openAIDowngradeAcceleratedInterval))
	state.UpdatedAt = now
	if err := r.accountRepo.SetSchedulable(ctx, state.AccountID, true); err != nil {
		return err
	}
	if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
		OpenAIDowngradeEventBucketRescue, map[string]any{"swap_count_7d": state.SwapCount7d}); err != nil {
		return err
	}
	return r.store.SaveOpenAIDowngradeState(ctx, state)
}

func (r *OpenAIDowngradeProbeRunner) finishReplacement(
	ctx context.Context,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	if err := r.setSolFallbackMode(ctx, state.AccountID, false); err != nil {
		return err
	}
	state.State = OpenAIDowngradeStatePendingReplace
	state.ProbeMode = "normal"
	state.ConsecutiveFailures = 0
	state.ConsecutiveSuccesses = 0
	state.UpdatedAt = now
	if err := r.accountRepo.SetSchedulable(ctx, state.AccountID, false); err != nil {
		return err
	}
	if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
		OpenAIDowngradeEventReplaceRequired, map[string]any{"swap_count_7d": state.SwapCount7d}); err != nil {
		return err
	}
	// 判死即终态（r17x 选项A）：静默字段仅作展示层「何时死透」参考，
	// NextProbeAt 已无调度语义（ListDue 排除 pending_replace）。散布保留——
	// 防止极端路径下同批判死号形成同步节律。
	silence := r.replacementRetrySilence(ctx, state.AccountID, now)
	state.RecoveryDeadline = timePtr(now.Add(silence))
	state.NextProbeAt = now.Add(r.spread(silence))
	return r.store.SaveOpenAIDowngradeState(ctx, state)
}

// replacementRetrySilence 计算判死账号本轮的静默时长。
// 计数包含刚刚写入的 replace_required 事件：第 1 次 2h，第 2 次 4h，依此类推。
func (r *OpenAIDowngradeProbeRunner) replacementRetrySilence(ctx context.Context, accountID int64, now time.Time) time.Duration {
	retryCount := 1
	if counter, ok := r.store.(OpenAIDowngradeReplaceEventCounter); ok {
		if n, err := counter.CountOpenAIDowngradeEvents(ctx, accountID,
			OpenAIDowngradeEventReplaceRequired, now.Add(-openAIDowngradeReplacementCountWindow)); err == nil && n > 0 {
			retryCount = n
		}
	}
	silence := openAIDowngradeReplacementRetryBase
	for i := 1; i < retryCount; i++ {
		silence *= 2
		if silence >= openAIDowngradeReplacementRetryMax {
			return openAIDowngradeReplacementRetryMax
		}
	}
	return silence
}

// retryReplacement 已废止（r17x 选项A，2026-09-21 用户裁定「判死即终态，
// 不再自动检测，手动启用」）。判死号的救援唯一入口是 ReenableOpenAIAccount；
// 本方法保留为占位防止旧测试/调用点编译断裂，语义上不可达。
func (r *OpenAIDowngradeProbeRunner) retryReplacement(
	ctx context.Context,
	account *Account,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	return r.beginReprobe(ctx, account, state, now)
}

func sameOpenAIProbeProxy(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// probeOpenAI429ResetTime 提取探针 429 的显式重置时间：优先 x-codex-* 响应头
// （primary=周限/secondary=5h 窗口类限流，calculateOpenAI429ResetTime 自带
// 「已打满的窗口优先」判定），其次响应体的 usage_limit_reached/resets_at
// （用量耗尽类）。与真实流量路径同款解析器；nil 表示该 429 不带时间信息
// （走短周期重探或风暴退避）。
func probeOpenAI429ResetTime(headers http.Header, body []byte) *time.Time {
	if resetAt := calculateOpenAI429ResetTime(headers); resetAt != nil {
		return resetAt
	}
	if ts := parseOpenAIRateLimitResetTime(body); ts != nil {
		resetAt := time.Unix(*ts, 0)
		return &resetAt
	}
	return nil
}

func probeOpenAI429Window(headers http.Header) string {
	snapshot := ParseCodexRateLimitHeaders(headers)
	if snapshot == nil {
		return "unknown_window"
	}
	if snapshot.PrimaryWindowMinutes != nil && snapshot.SecondaryWindowMinutes != nil &&
		*snapshot.PrimaryWindowMinutes == *snapshot.SecondaryWindowMinutes {
		return "unknown_window"
	}
	limits := snapshot.Normalize()
	if limits == nil {
		return "unknown_window"
	}
	if limits.Used7dPercent != nil && *limits.Used7dPercent >= 100 {
		if limits.Window7dMinutes != nil && *limits.Window7dMinutes == 7*24*60 &&
			limits.Reset7dSeconds != nil && *limits.Reset7dSeconds >= 0 {
			return "7d_window"
		}
		// Do not relabel a known or ambiguous exhausted long window as short.
		return "unknown_window"
	}
	if limits.Used5hPercent != nil && *limits.Used5hPercent >= 100 &&
		limits.Window5hMinutes != nil && *limits.Window5hMinutes == 5*60 &&
		limits.Reset5hSeconds != nil && *limits.Reset5hSeconds >= 0 {
		return "5h_window"
	}
	return "unknown_window"
}

// SetRecentTrafficChecker 注入「账号近窗是否有真实推理流量」查询（usage_logs）。
// 未注入时真实流量顺延逻辑整体关闭，行为与 r15 之前一致。
func (r *OpenAIDowngradeProbeRunner) SetRecentTrafficChecker(
	fn func(ctx context.Context, accountID int64, within time.Duration) bool,
) {
	if r == nil {
		return
	}
	r.recentTraffic = fn
}

// SetPluginBridgeSource 注入救治区插件桥状态源（PluginManager.BridgeStatus）。
// 未注入时健康快照响应不带 plugin_bridge 区块，行为与桥合入前一致。
func (r *OpenAIDowngradeProbeRunner) SetPluginBridgeSource(
	fn func(ctx context.Context) *PluginBridgeStatus,
) {
	if r == nil {
		return
	}
	r.pluginBridge = fn
}

// SetRescueLane 注入救治区编排器（r17ax Phase 3）。未注入时判死提交不触发
// 自动入区，行为与救治区合入前一致；手动端点与对账清扫入口不经此字段。
func (r *OpenAIDowngradeProbeRunner) SetRescueLane(lane *OpenAIRescueLane) {
	if r == nil {
		return
	}
	r.rescueLane = lane
}

// applyRateLimitDeferral 是全部探测路径共用的 429 长退避闸（2026-09-15 用户
// 裁定：机制必须能检测到额度耗尽，耗尽号不能一直探）。两级：
//  1. 429 带显式重置时间（x-codex-* 窗口头或 usage_limit_reached 体，真实
//     流量路径同款解析）→ 下一针取 min(重置点+错峰, 现在+稀疏复查)（r17
//     稀疏复查：重置不是固定的，供应商可能提前手动重置——长持有每天最多
//     一针随机复查，重置点更近时自然收敛回重置点一侧）；重置点过近/过远
//     分别落 floor/cap。分窗差异化：5h 短窗只顺探针不动账号；7d 显式周限
//     额外单调延长真实流量冷却（到点自动放行）；未知窗口仍保留分类，
//     但服务端重置点超过 5.5h 时按额度级冷却持有账号。
//  2. 不带时间信息的 429 连续达阈值 → 风暴退避 1 小时（spread 错开）；
//     账号已处于限流持有中（rate_limit_reset_at 未到）的无信息 429 不进
//     风暴闸——账号已被长退避摘出真实流量，1 小时连打正是 1029 事故形态，
//     锚定已持久化的持有继续稀疏复查节奏。
//
// 非 429 且为上游真实接受（传输 OK 且 2xx）时，按 CAS 清除观察到的持有
// （rate_limit_recheck_recovered）——这是官方重置/提前手动重置的检测回路
// 终点，检测而非猜测，auto-reset 纪律不破。
// 命中任一级时计数器一律不动（限流不是降智证据），落事件后返回 handled=true，
// 所有 429 均返回 handled=true，包括普通短周期重探，不能落入失败/换号路径。
// 官方重置的检测回路：错峰首针在重置点后落下一探，200 即回正常轨道；仍未
// 重置则吃新 429 带新重置点继续顺延（自纠错）。
// The streak is persisted with the probe mutation, including its commit fence.
func (r *OpenAIDowngradeProbeRunner) applyRateLimitDeferral(
	ctx context.Context,
	account *Account,
	state *OpenAIDowngradeProbeState,
	result OpenAIDowngradeProbeResult,
	now time.Time,
) (bool, error) {
	if state == nil {
		return false, nil
	}
	if result.HTTPStatus != http.StatusTooManyRequests {
		state.Consecutive429s = 0
		r.releaseObservedRateLimitOnAcceptedProbe(ctx, account, state, result, now)
		return false, nil
	}
	if result.RateLimitResetAt != nil {
		state.Consecutive429s = 0
		resetAt := *result.RateLimitResetAt
		window := result.RateLimitWindow
		if window != "5h_window" && window != "7d_window" {
			window = "unknown_window"
		}
		details := map[string]any{"class": window}
		if floor := now.Add(openAIDowngradeRateLimitResetFloor); resetAt.Before(floor) {
			resetAt = floor
			details["floored"] = true
		}
		if capLimit := now.Add(openAIDowngradeRateLimitResetCap); resetAt.After(capLimit) {
			resetAt = capLimit
			details["capped"] = true
		}
		capped := details["capped"] == true
		quotaLike := window == "7d_window" || (window == "unknown_window" &&
			(capped || resetAt.Sub(now) > openAIDowngradeRateLimitQuotaLikeDistance))
		if quotaLike {
			store, ok := r.accountRepo.(OpenAIDowngradeRateLimitStore)
			if !ok {
				return true, errors.New("openai downgrade monotonic rate limit store unavailable")
			}
			if err := store.SetRateLimitedIfLater(ctx, state.AccountID, resetAt); err != nil {
				return true, err
			}
		}
		details["quota_like"] = quotaLike
		// 重置点后错峰首探：同窗口打满的多个账号不会在同一秒集体醒来（jitter
		// 后 15-45min），「重置时刻整点回访」本身也是可聚类特征。r17 稀疏
		// 复查：长持有不死等重置点——min(重置点+错峰, 现在+spread复查)，
		// 复查每次重新抽签，无可聚类周期；5h 短窗的重置点恒早于复查侧，
		// 行为不变；接近重置点时收敛回官方重置检测回路。
		nextProbeAt := resetAt.Add(r.jitter(openAIDowngradeRateLimitResetStagger))
		if recheck := now.Add(r.spread(openAIDowngradeRateLimitRecheckInterval)); recheck.Before(nextProbeAt) {
			nextProbeAt = recheck
			details["recheck"] = true
		}
		state.NextProbeAt = nextProbeAt
		state.LastProbeAt = &now
		state.UpdatedAt = now
		details["reset_at"] = resetAt.Format(time.RFC3339)
		if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
			OpenAIDowngradeEventRateLimitDeferred, details); err != nil {
			return true, err
		}
		return true, r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	// 持有中的无时间信息 429：不进风暴闸、不计数（限流非降智证据），锚定
	// 已持久化的持有走稀疏复查节奏。新证据（带重置点的 429 / 2xx 接受）由
	// 对应分支自纠。未持有的账号保持原风暴退避语义。
	if account != nil && account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
		nextProbeAt := now.Add(r.spread(openAIDowngradeRateLimitRecheckInterval))
		// A missing response header must not discard an already observed nearer reset.
		if resetProbeAt := account.RateLimitResetAt.Add(r.jitter(openAIDowngradeRateLimitResetStagger)); resetProbeAt.Before(nextProbeAt) {
			nextProbeAt = resetProbeAt
		}
		state.NextProbeAt = nextProbeAt
		state.LastProbeAt = &now
		state.UpdatedAt = now
		if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
			OpenAIDowngradeEventRateLimitDeferred,
			map[string]any{"class": "recheck_streak_suppressed"}); err != nil {
			return true, err
		}
		return true, r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	// Saturate after escalation; a long outage must not overflow the counter.
	if state.Consecutive429s < openAIDowngrade429StreakThreshold {
		state.Consecutive429s = max(state.Consecutive429s, 0) + 1
	} else {
		state.Consecutive429s = openAIDowngrade429StreakThreshold
	}
	if state.Consecutive429s < openAIDowngrade429StreakThreshold {
		state.NextProbeAt = now.Add(r.jitter(openAIDowngradeRateLimitedRetryInterval))
		state.LastProbeAt = &now
		state.UpdatedAt = now
		return true, r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	state.NextProbeAt = now.Add(r.spread(openAIDowngrade429StreakBackoff))
	state.LastProbeAt = &now
	state.UpdatedAt = now
	if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
		OpenAIDowngradeEventRateLimitDeferred,
		map[string]any{"class": "streak", "count": state.Consecutive429s}); err != nil {
		return true, err
	}
	return true, r.store.SaveOpenAIDowngradeState(ctx, state)
}

// releaseObservedRateLimitOnAcceptedProbe 在账号处于限流持有期间收到上游真实
// 接受（传输 OK 且 2xx）的探针结果时，按 CAS 语义清除「本针观察到的那一次
// 持有」（2026-09-16 用户裁定「有时候可以手动重置」——检测而非猜测，恢复
// 动作有上游证据，auto-reset 纪律不破）。CAS 只清观察到的 (limited_at,
// reset_at) 对，探测期间他处延长/改写的持有不被覆盖，由后续探针自纠。任何
// 失败只记日志：成功针的状态机处理不能被持有清除的副产物打断。
func (r *OpenAIDowngradeProbeRunner) releaseObservedRateLimitOnAcceptedProbe(
	ctx context.Context,
	account *Account,
	state *OpenAIDowngradeProbeState,
	result OpenAIDowngradeProbeResult,
	now time.Time,
) {
	if r == nil || account == nil || state == nil || account.ID <= 0 {
		return
	}
	if account.RateLimitedAt == nil || account.RateLimitResetAt == nil {
		return
	}
	if !now.Before(*account.RateLimitResetAt) {
		// 持有已自然过期：调度侧本就会放行，无需探针清理。
		return
	}
	if !result.TransportOK || result.HTTPStatus < 200 || result.HTTPStatus >= 300 {
		return
	}
	releaser, ok := r.accountRepo.(OpenAIDowngradeRateLimitReleaser)
	if !ok {
		return
	}
	cleared, err := releaser.ClearOpenAIRateLimitIfObserved(ctx, account.ID, *account.RateLimitedAt, *account.RateLimitResetAt)
	if err != nil {
		slog.Warn("openai_downgrade_rate_limit_release_failed", "account_id", account.ID, "error", err)
		return
	}
	if !cleared {
		return
	}
	if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
		OpenAIDowngradeEventRateLimitRecheckRecovered,
		map[string]any{
			"reset_at": account.RateLimitResetAt.Format(time.RFC3339),
			"http":     result.HTTPStatus,
		}); err != nil {
		slog.Warn("openai_downgrade_rate_limit_release_event_failed", "account_id", account.ID, "error", err)
	}
}

// shouldDeferForRealTraffic 判断是否因近窗真实流量顺延本次常规探针。
// 只在 runMu 临界区内（RunOnce→processState）调用，deferCounts 无需额外加锁。
func (r *OpenAIDowngradeProbeRunner) shouldDeferForRealTraffic(ctx context.Context, accountID int64) bool {
	if r == nil || r.recentTraffic == nil || r.deferCounts == nil {
		return false
	}
	if r.deferCounts[accountID] >= openAIDowngradeMaxTrafficDeferrals {
		// 连续顺延达上限：重置并照常探测，保证降智 canary 的覆盖上限。
		r.deferCounts[accountID] = 0
		return false
	}
	if !r.recentTraffic(ctx, accountID, openAIDowngradeRecentTrafficWindow) {
		r.deferCounts[accountID] = 0
		return false
	}
	r.deferCounts[accountID]++
	return true
}

// The preceding probe's committed event survives runner restarts. A later
// successful probe advances LastProbeAt, so old signals cannot disable deferral.
func (r *OpenAIDowngradeProbeRunner) hasPendingAbuseRecheck(ctx context.Context, state *OpenAIDowngradeProbeState) (bool, error) {
	counter, ok := r.store.(OpenAIDowngradeReplaceEventCounter)
	if !ok {
		return false, errors.New("OpenAI probe event counter unavailable")
	}
	since := time.Time{}
	if state.LastProbeAt != nil {
		since = *state.LastProbeAt
	}
	count, err := counter.CountOpenAIDowngradeEvents(
		ctx, state.AccountID, OpenAIDowngradeEventRealTrafficModelMismatch, since)
	if err != nil || count > 0 {
		return count > 0, err
	}
	if state.LastProbeAt == nil {
		return false, nil
	}
	count, err = counter.CountOpenAIDowngradeEvents(
		ctx, state.AccountID, OpenAIDowngradeEventTurnStateDegraded, since)
	if err != nil || count > 0 {
		return count > 0, err
	}
	count, err = counter.CountOpenAIDowngradeEvents(
		ctx, state.AccountID, OpenAIDowngradeEventRealTrafficRecheckArmed, since)
	return count > 0, err
}

func (r *OpenAIDowngradeProbeRunner) jitter(interval time.Duration) time.Duration {
	return time.Duration(float64(interval) * (0.5 + probeRandomFloat()))
}

// spread 只做正向散布（1.0x-1.25x），与 jitter 的对称抖动（0.5x-1.5x）不同：
// 静默/冷却/判死等截止型排期有最小时长语义，不能被缩短。RunOnce 一轮内共享
// 同一个 now，固定时长会让同批账号在同一微秒集体醒来（同 IP 集中探测、同时
// 换桶），是可被上游聚类的同步突发；spread 保持下界不变，只错开醒来时刻。
func (r *OpenAIDowngradeProbeRunner) spread(base time.Duration) time.Duration {
	return time.Duration(float64(base) * (1 + 0.25*probeRandomFloat()))
}

func (r *OpenAIDowngradeProbeRunner) runProbe(ctx context.Context, account *Account, mode string) OpenAIDowngradeProbeResult {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	probe := r.probeFn
	if probe == nil {
		probe = r.probe
	}
	result := probe(ctx, account, mode)
	result.Mode = mode
	return result
}

// openAIProbeAccountUnchanged reports whether the admin-visible scheduling
// surface of the account still matches the snapshot taken before the probe
// spent up to two minutes in flight. Only the three fields that corrupt
// state-machine attribution are compared: proxy binding, the schedulable
// switch and lifecycle status. Cosmetic edits (name, Extra knobs) are
// deliberately excluded — the probe outcome stays valid for attribution —
// because comparing Extra would also break on this runner's own sol-fallback
// Extra writes within the same tick.
func openAIProbeAccountUnchanged(before, after *Account) bool {
	if before == nil || after == nil {
		return false
	}
	return before.Status == after.Status &&
		before.Schedulable == after.Schedulable &&
		sameOpenAIProbeProxy(before.ProxyID, after.ProxyID)
}

// probeAccountFresh re-reads the account after an in-flight probe and reports
// whether the pre-probe snapshot is still the scheduling truth.
func (r *OpenAIDowngradeProbeRunner) probeAccountFresh(
	ctx context.Context,
	account *Account,
) (bool, error) {
	fresh, err := r.accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		return false, err
	}
	return openAIProbeAccountUnchanged(account, fresh), nil
}

// skipStaleProbeResult parks a probe result whose account changed mid-flight.
// Telemetry was already recorded; every counter, transition and account
// mutation is dropped so the next due run re-evaluates from the admin's
// current truth instead of a stale snapshot.
func (r *OpenAIDowngradeProbeRunner) skipStaleProbeResult(
	ctx context.Context,
	state *OpenAIDowngradeProbeState,
	result OpenAIDowngradeProbeResult,
	now time.Time,
) error {
	state.LastProbeAt = &now
	state.UpdatedAt = now
	state.NextProbeAt = now.Add(r.nextDelay())
	if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
		OpenAIDowngradeEventProbeSkippedStale, map[string]any{
			"http_status": result.HTTPStatus,
			"mode":        result.Mode,
		}); err != nil {
		return err
	}
	return r.store.SaveOpenAIDowngradeState(ctx, state)
}

// applyOpenAIProbeAuthPolicy is the degrade-not-kill policy for probe
// authentication failures. The first consecutive 401/403 puts the account
// into auth-strike limbo: scheduling is paused and a fast recheck runs in
// openAIDowngradeAuthRetryInterval. Limbo exits three ways:
//   - another auth strike reaches OpenAIDowngradeAuthStrikeThreshold and the
//     account graduates to error status (stop=true);
//   - any upstream answer that is not 401/403 proves the credentials were
//     accepted, so the pause is cleared and the normal state machine runs;
//   - a no-answer result (network/token) proves nothing: limbo persists and
//     the fast recheck cadence is kept.
//
// stop=true means the caller must not run the degradation state machine for
// this result.
func (r *OpenAIDowngradeProbeRunner) applyOpenAIProbeAuthPolicy(
	ctx context.Context,
	account *Account,
	state *OpenAIDowngradeProbeState,
	result OpenAIDowngradeProbeResult,
	now time.Time,
) (bool, error) {
	isAuthFailure := !result.TransportOK &&
		(result.HTTPStatus == http.StatusUnauthorized || result.HTTPStatus == http.StatusForbidden)
	if !isAuthFailure {
		if state.AuthConsecutiveFailures == 0 {
			return false, nil
		}
		reachedUpstream := result.TransportOK ||
			(result.HTTPStatus != 0 &&
				result.HTTPStatus != http.StatusUnauthorized &&
				result.HTTPStatus != http.StatusForbidden)
		if !reachedUpstream {
			return false, nil
		}
		// The upstream accepted the credentials: clear the strike pause. The
		// unconditional re-enable deliberately favors auto-recovery over
		// preserving a pause that was ours to begin with.
		state.AuthConsecutiveFailures = 0
		state.LastProbeAt = &now
		state.UpdatedAt = now
		if account.Status == StatusActive && !account.Schedulable {
			if err := r.accountRepo.SetSchedulable(ctx, state.AccountID, true); err != nil {
				return true, err
			}
		}
		if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
			OpenAIDowngradeEventProbeAuthCleared, map[string]any{
				"http_status": result.HTTPStatus,
			}); err != nil {
			return true, err
		}
		return false, r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	state.AuthConsecutiveFailures++
	state.LastProbeAt = &now
	state.UpdatedAt = now
	if state.AuthConsecutiveFailures >= OpenAIDowngradeAuthStrikeThreshold {
		if err := r.accountRepo.SetError(ctx, account.ID, "OpenAI probe authentication failed"); err != nil {
			return true, err
		}
		state.AuthConsecutiveFailures = 0
		state.NextProbeAt = now.Add(openAIDowngradeReplacementWindow)
		if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
			OpenAIDowngradeEventProbeAuthTerminal, map[string]any{
				"http_status": result.HTTPStatus,
			}); err != nil {
			return true, err
		}
		return true, r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	if err := r.accountRepo.SetSchedulable(ctx, state.AccountID, false); err != nil {
		return true, err
	}
	state.NextProbeAt = now.Add(openAIDowngradeAuthRetryInterval)
	if err := r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
		OpenAIDowngradeEventProbeAuthStrike, map[string]any{
			"http_status": result.HTTPStatus,
			"strikes":     state.AuthConsecutiveFailures,
		}); err != nil {
		return true, err
	}
	return true, r.store.SaveOpenAIDowngradeState(ctx, state)
}

// runProbeRecorded is the single guarded entry point for executing one probe:
// it records telemetry, verifies the account did not change during the
// in-flight window, then applies the auth-failure policy. stop=true means
// the caller must not apply further state-machine transitions; err carries
// the store failure when present.
func (r *OpenAIDowngradeProbeRunner) runProbeRecorded(
	ctx context.Context,
	account *Account,
	state *OpenAIDowngradeProbeState,
	mode string,
	now time.Time,
) (result OpenAIDowngradeProbeResult, stop bool, err error) {
	result = r.runProbe(ctx, account, mode)
	if err := r.store.RecordOpenAIDowngradeProbe(ctx, &result); err != nil {
		return result, true, err
	}
	fresh, err := r.probeAccountFresh(ctx, account)
	if err != nil {
		return result, true, err
	}
	if !fresh {
		return result, true, r.skipStaleProbeResult(ctx, state, result, now)
	}
	authStopped, err := r.applyOpenAIProbeAuthPolicy(ctx, account, state, result, now)
	if err != nil {
		return result, true, err
	}
	if authStopped {
		return result, true, nil
	}
	return result, false, nil
}

// recordProbeResult 只落遥测行。401/403 的降级不杀策略在状态机侧
// （applyOpenAIProbeAuthPolicy，经 runProbeRecorded）——记录入口不再直接
// SetError，诊断针（只落证据行不动状态机）复用本入口也安全。
func (r *OpenAIDowngradeProbeRunner) recordProbeResult(ctx context.Context, result *OpenAIDowngradeProbeResult) error {
	return r.store.RecordOpenAIDowngradeProbe(ctx, result)
}

func (r *OpenAIDowngradeProbeRunner) probe(
	ctx context.Context,
	account *Account,
	mode string,
) OpenAIDowngradeProbeResult {
	started := time.Now()
	result := OpenAIDowngradeProbeResult{
		AccountID: account.ID,
		ProxyID:   account.ProxyID,
		Mode:      mode,
	}
	if r.tokenProvider == nil || r.httpUpstream == nil {
		result.ErrorMessage = "probe dependencies unavailable"
		result.Latency = time.Since(started)
		return result
	}
	token, err := r.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		result.ErrorMessage = "access token unavailable"
		result.Latency = time.Since(started)
		return result
	}
	var proxyURL string
	if r.proxyRepo != nil {
		proxyURL, err = resolveOpenAIOAuthProxyURL(ctx, r.proxyRepo, account.ProxyID)
	} else {
		proxyURL, err = openAIOAuthProxySnapshotURL(account.Proxy, account.ProxyID)
	}
	if err != nil {
		// 出口硬闸：解析不出桶代理就不发探针。直连会把家用 IP 暴露给
		// chatgpt.com，且裸 Go TLS 指纹与账号流量冲突；无结论（不奖不罚）
		// 等下一轮资格流程绑桶后再探。
		result.ErrorMessage = "probe requires account proxy bucket"
		result.Latency = time.Since(started)
		return result
	}

	question := openAIDowngradeProbeNextQuestion(time.Now())
	turn := newOpenAIDowngradeProbeTurn(account)
	probeModel := "gpt-6-astra"
	if mode == "sol_fallback" {
		probeModel = "gpt-5.6-sol"
	}
	telemetryProfile := r.beginProbeTelemetry(account, turn, proxyURL, token, probeModel)
	defer func() {
		status := "failed"
		if result.TransportOK && (result.HTTPStatus == 0 || result.HTTPStatus == http.StatusOK) {
			status = "completed"
		}
		r.finishProbeTelemetry(telemetryProfile, status, started)
	}()
	requestProbe := func(stream bool) (int, http.Header, []byte, error) {
		body, marshalErr := turn.buildRequestBody(probeModel, question.Text, stream)
		if marshalErr != nil {
			return 0, nil, nil, marshalErr
		}
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexAPIURL, bytes.NewReader(body))
		if requestErr != nil {
			return 0, nil, nil, requestErr
		}
		req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
		// 探针请求逐字段复刻真实 codex exec 单轮 turn 的头与体（2026-09-15 本机
		// 0.151 抓包地面真值）：UA/originator=codex_exec、session-id/thread-id/
		// x-client-request-id、x-codex-window-id、x-codex-turn-metadata、
		// x-codex-beta-features；不带 version/openai-beta/origin/referer/accept-encoding。
		turn.applyRequestHeaders(req.Header, stream)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Host = "chatgpt.com"
		setOpenAIChatGPTAccountHeaders(req.Header, account)
		account.ApplyHeaderOverrides(req.Header)

		// 探针走一次性专用传输（DoProbeWithTLS）：与账号共享连接池隔离。
		// 共享池中探针与真实 turn 同连接复用会被服务端按连接降级
		// （2026-09-18 生产实证，0/N 全败于 200 全长流缺 usage，而同 body
		// 的进程外冷连接 12/12 通过）；真实 codex exec 单轮本就是每进程
		// 新连接，独占传输与真实形态一致。
		var probeProfile *tlsfingerprint.Profile
		if r.tlsProfiles != nil {
			probeProfile = r.tlsProfiles.ResolveTLSProfile(account)
		}
		var resp *http.Response
		resp, requestErr = r.httpUpstream.DoProbeWithTLS(req, proxyURL, account.Concurrency, probeProfile)
		if requestErr != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			return 0, nil, nil, requestErr
		}
		if resp == nil {
			return 0, nil, nil, errors.New("probe upstream returned no response")
		}
		responseBody, readErr := readOpenAIDowngradeProbeBody(resp.Body)
		respHeader := resp.Header
		statusCode := resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		if readErr != nil {
			return statusCode, respHeader, nil, readErr
		}
		return statusCode, respHeader, responseBody, nil
	}

	// 上游已实测拒绝非流式（HTTP 400 "Stream must be set to true"），
	// 因此第一针直接走流式；非流式仅作为历史兼容路径保留在重试逻辑里。
	status, respHeader, responseBody, requestErr := requestProbe(true)
	// Once a request attempt has been made, archive exactly one final outcome.
	// The archive contains only the fixed error class and response tail, never
	// the request, credential, or raw transport error.
	defer func() {
		archiveOpenAIDowngradeProbe(account.ID, mode, question, &result, responseBody)
	}()
	result.Latency = time.Since(started)
	failureStage := "probe transport failed: "
	if requestErr == nil && shouldRetryOpenAIDowngradeStreamProbe(status, responseBody) {
		status, respHeader, responseBody, requestErr = requestProbe(true)
		result.Latency = time.Since(started)
		failureStage = "probe stream retry failed: "
	}
	// Observe only signal metadata; never log or replay the header value.
	codexTurnStateLen := openAIProbeCodexTurnStateLen(respHeader)
	if turnStateLen, status292 := openAIProbeTurnStateSignal(status, respHeader); turnStateLen > 0 || status292 {
		slog.Warn("openai_probe_turn_state_signal_observed",
			"account_id", account.ID,
			"mode", mode,
			"http_status", status,
			"status_292", status292,
			"current_turn_state_len", turnStateLen,
			"codex_turn_state_len", codexTurnStateLen)
	}
	slog.Info("openai_probe_codex_turn_state_len",
		"account_id", account.ID,
		"mode", mode,
		"http_status", status,
		"turn_state_len", codexTurnStateLen)
	result.TurnStateLen = codexTurnStateLen
	result.HTTPStatus = status
	if status == http.StatusTooManyRequests {
		// 429 带显式重置时间（x-codex-* 窗口头或 usage_limit_reached 体）就
		// 提取出来交给调度层长退避——真实流量路径同款解析，探针侧此前完全不
		// 判，导致耗尽号以 2.5-7.5min 短周期持续空打（1029 实测 3 小时 20 针）。
		if resetAt := probeOpenAI429ResetTime(respHeader, responseBody); resetAt != nil {
			result.RateLimitResetAt = resetAt
			result.RateLimitWindow = probeOpenAI429Window(respHeader)
		}
	}
	// A failed body read does not invalidate received HTTP status/reset headers.
	// Persist only fixed error classes, never raw transport diagnostics.
	if requestErr != nil {
		result.ErrorMessage = failureStage + openAIDowngradeProbeErrorClass(requestErr)
		return result
	}
	if status != http.StatusOK {
		result.ErrorMessage = "probe upstream returned HTTP " + strconv.Itoa(status)
		return result
	}
	result.applyResponse(responseBody, question.AnswerPattern)
	if !result.TransportOK {
		slog.With("account_id", account.ID, "mode", mode).
			Warn("openai_probe_parse_failed_forensics", openAIProbeParseFailureFields(responseBody)...)
	}
	return result
}

func openAIDowngradeProbeErrorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errOpenAIDowngradeProbeBodyUnavailable):
		return "response body unavailable"
	case errors.Is(err, errOpenAIDowngradeProbeBodyTooLarge):
		return "response body exceeds limit"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, net.ErrClosed), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.EPIPE):
		return "connection interrupted"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "network or response error"
}

func openAIProbeParseFailureFields(body []byte) []any {
	// The response is untrusted even for synthetic probes. Log metadata only.
	text := string(body)
	records := 0
	for _, line := range strings.Split(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n") {
		if strings.HasPrefix(line, "data:") {
			records++
		}
	}
	return []any{
		"bytes", len(body),
		"data_records", records,
		"has_terminal", strings.Contains(text, `"response.completed"`),
		"has_usage", strings.Contains(text, `"usage"`),
		"has_reasoning", strings.Contains(text, `"reasoning_tokens"`),
	}
}

func openAIProbeTurnStateSignal(status int, header http.Header) (turnStateLen int, status292 bool) {
	if status == 292 {
		status292 = true
	}
	for name, values := range header {
		if !strings.EqualFold(strings.ReplaceAll(name, "-", "_"), "current_turn_state") {
			continue
		}
		if len(values) > 0 {
			turnStateLen = len(values[0])
		}
		break
	}
	return turnStateLen, status292
}

func openAIProbeCodexTurnStateLen(header http.Header) int {
	for name, values := range header {
		if !strings.EqualFold(strings.ReplaceAll(name, "-", "_"), "x_codex_turn_state") {
			continue
		}
		if len(values) > 0 {
			return len(values[0])
		}
		break
	}
	return 0
}

func (r *OpenAIDowngradeProbeResult) applyResponse(body []byte, answerPattern *regexp.Regexp) {
	text, reasoningTokens, juice := parseOpenAIDowngradeProbeCompletion(body)
	if answerPattern == nil {
		// nil 判分正则 = 拒收（与 parseOpenAIDowngradeProbeResponse 首行同形）：
		// 不产出可判定结果，零计数。
		text, reasoningTokens, juice = "", nil, nil
	}
	r.gradedText = text
	r.AnswerCorrect = text != "" && answerPattern.MatchString(text)
	r.ReasoningTokens = reasoningTokens
	r.Juice = juice
	// An incomplete body is not a wrong answer. Keep it out of all votes.
	r.TransportOK = r.ReasoningTokens != nil
	r.ErrorMessage = ""
	if !r.TransportOK {
		r.ErrorMessage = "probe response missing valid completion or reasoning usage"
	}
}

func readOpenAIDowngradeProbeBody(reader io.Reader) ([]byte, error) {
	if reader == nil {
		return nil, errOpenAIDowngradeProbeBodyUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(reader, openAIDowngradeProbeMaxBodyBytes+1))
	if len(body) > openAIDowngradeProbeMaxBodyBytes {
		return nil, errOpenAIDowngradeProbeBodyTooLarge
	}
	if err != nil && len(body) == 0 {
		return nil, err
	}
	// A closed stream may already contain a complete terminal record.
	// The parser, not partial bytes alone, decides whether it is usable.
	return body, nil
}

// openAIDowngradeProbeTurn 是一次探针 turn 的完整身份构件，逐字段对齐真实
// codex exec 单轮 turn（2026-09-15 本机 codex-cli 0.151.0 抓包地面真值，本地
// 监听器捕获 /responses 请求，模板见 openai_downgrade_probe_template.go）。
// session/thread/turn/window/context_window 全部用 uuid v7（真实 codex 的 ID
// 均为时间有序形态，v4 会成为可识别差异）；installation_id 账号级恒定且与
// machine 指纹身份同源，保证同一账号的探针 turn 与真实 turn 处在同一身份宇宙。
type openAIDowngradeProbeTurn struct {
	sessionID       string
	threadID        string
	turnID          string
	windowID        string
	contextWindowID string
	installationID  string
	version         string
	turnMetaJSON    string
	started         time.Time
}

func newOpenAIDowngradeProbeTurn(account *Account) *openAIDowngradeProbeTurn {
	session := uuid.Must(uuid.NewV7()).String()
	turnID := uuid.Must(uuid.NewV7()).String()
	turn := &openAIDowngradeProbeTurn{
		sessionID:       session,
		threadID:        session, // exec 单轮：thread == session
		turnID:          turnID,
		windowID:        session + ":0",
		contextWindowID: uuid.Must(uuid.NewV7()).String(),
		installationID:  openAIDowngradeProbeInstallationID(account),
		version:         codexClientVersionFromUA(codexCanonicalUserAgent()),
		started:         time.Now(),
	}
	turn.turnMetaJSON = turn.marshalTurnMetadata()
	return turn
}

// userAgent 生成 codex_exec 形态的规范 UA。探针本质是一次一次性提问，对应
// 真实世界里 `codex exec "问题"` 的单轮 turn，故 originator 取 codex_exec 而
// 非 TUI 的 codex-tui；版本段与全站生效版本同源（面板/自动同步/编译期兜底），
// 后缀复用 codexCLIUserAgentSuffix，尾部 (originator; version) 组按本版本拼装。
func (t *openAIDowngradeProbeTurn) userAgent() string {
	return "codex_exec/" + t.version + codexCLIUserAgentSuffix + " (codex_exec; " + t.version + ")"
}

func (t *openAIDowngradeProbeTurn) originator() string { return "codex_exec" }

// openAIDowngradeProbeTurnMetadata 与抓包同字段同顺序。sandbox="none" +
// sandbox_mode="danger-full-access" 是用户真实配置（rewriteCodexMachineSandbox
// 对平台无关的 none 原样保留，账号真实出站就是这个组合）；agent_name="/root"
// 为本机 exec 会话的实测值。
type openAIDowngradeProbeTurnMetadata struct {
	InstallationID             string `json:"installation_id"`
	SessionID                  string `json:"session_id"`
	ThreadID                   string `json:"thread_id"`
	AgentName                  string `json:"agent_name"`
	TurnID                     string `json:"turn_id"`
	WindowID                   string `json:"window_id"`
	WindowNumber               int    `json:"window_number"`
	ContextWindowID            string `json:"context_window_id"`
	RequestKind                string `json:"request_kind"`
	RootTurnID                 string `json:"root_turn_id"`
	ThreadSource               string `json:"thread_source"`
	Sandbox                    string `json:"sandbox"`
	SandboxMode                string `json:"sandbox_mode"`
	AutoReviewEnabled          bool   `json:"auto_review_enabled"`
	NodeReplAutoReviewRequired bool   `json:"node_repl_auto_review_required"`
	NodeReplDisabled           bool   `json:"node_repl_disabled"`
	TurnStartedAtUnixMs        int64  `json:"turn_started_at_unix_ms"`
}

func (t *openAIDowngradeProbeTurn) marshalTurnMetadata() string {
	meta, err := json.Marshal(openAIDowngradeProbeTurnMetadata{
		InstallationID:      t.installationID,
		SessionID:           t.sessionID,
		ThreadID:            t.threadID,
		AgentName:           "/root",
		TurnID:              t.turnID,
		WindowID:            t.windowID,
		WindowNumber:        0,
		ContextWindowID:     t.contextWindowID,
		RequestKind:         "turn",
		RootTurnID:          t.turnID,
		ThreadSource:        "user",
		Sandbox:             "none",
		SandboxMode:         "danger-full-access",
		TurnStartedAtUnixMs: t.started.UnixMilli(),
	})
	if err != nil {
		return "{}"
	}
	return string(meta)
}

// openAIDowngradeProbeCWDPool 探针 environment_context 的工作目录池：
// 真实 codex turn 的 input 首条是 developer 环境上下文，cwd 恒定会形成指纹，
// 从通用开发目录形态的池里取。
var openAIDowngradeProbeCWDPool = [...]string{
	"/Users/liyunlong/dev/scratch",
	"/Users/liyunlong/Documents/dev/notes",
	"/Users/liyunlong/dev/tools",
	"/Users/liyunlong/Projects/tmp-check",
}

func openAIDowngradeProbeEnvContext(now time.Time) string {
	cwd := openAIDowngradeProbeCWDPool[rand.IntN(len(openAIDowngradeProbeCWDPool))]
	gitRepo := "No"
	if rand.IntN(2) == 0 {
		gitRepo = "Yes"
	}
	return "<environment_context>\nTime: " + now.Format("Mon Jan 02 15:04:05 2006") +
		"\nWorking directory: " + cwd +
		"\nIs directory a git repo: " + gitRepo +
		"\nPlatform: macos\nOS Version: 26.3.1\n</environment_context>"
}

// buildRequestBody 按真实 codex 形态组装 /responses 体：instructions/tools
// 逐字节取自抓包模板，reasoning{effort,summary}/include/text.verbosity/
// tool_choice/parallel_tool_calls/store/stream/prompt_cache_key 与抓包一致，
// canary 题作为 user 消息跟在 developer 环境上下文之后。
func (t *openAIDowngradeProbeTurn) buildRequestBody(model, question string, stream bool) ([]byte, error) {
	payload := struct {
		Model          string           `json:"model"`
		Instructions   string           `json:"instructions"`
		Input          []map[string]any `json:"input"`
		Tools          json.RawMessage  `json:"tools"`
		ToolChoice     string           `json:"tool_choice"`
		ParallelCalls  bool             `json:"parallel_tool_calls"`
		Reasoning      map[string]any   `json:"reasoning"`
		Store          bool             `json:"store"`
		Stream         bool             `json:"stream"`
		Include        []string         `json:"include"`
		PromptCacheKey string           `json:"prompt_cache_key"`
		Text           map[string]any   `json:"text"`
		ClientMetadata map[string]any   `json:"client_metadata"`
	}{
		Model:        model,
		Instructions: openAIDowngradeProbeBaseInstructions,
		Input: []map[string]any{
			{"type": "message", "id": "msg_" + uuid.Must(uuid.NewV7()).String(), "role": "developer",
				"content": []map[string]any{{"type": "input_text", "text": openAIDowngradeProbeEnvContext(t.started)}}},
			{"type": "message", "id": "msg_" + uuid.Must(uuid.NewV7()).String(), "role": "user",
				"content": []map[string]any{{"type": "input_text", "text": question}}},
		},
		Tools:          json.RawMessage(openAIDowngradeProbeToolsJSON),
		ToolChoice:     "auto",
		ParallelCalls:  true,
		Reasoning:      map[string]any{"effort": "xhigh", "summary": "auto"},
		Store:          false,
		Stream:         stream,
		Include:        []string{"reasoning.encrypted_content"},
		PromptCacheKey: t.sessionID,
		Text:           map[string]any{"verbosity": "medium"},
		ClientMetadata: map[string]any{
			"session_id":              t.sessionID,
			"turn_id":                 t.turnID,
			"thread_id":               t.threadID,
			"root_turn_id":            t.turnID,
			"x-codex-installation-id": t.installationID,
			"x-codex-window-id":       t.windowID,
			"x-codex-turn-metadata":   t.turnMetaJSON,
		},
	}
	return json.Marshal(payload)
}

// applyRequestHeaders 与抓包逐头对齐：会话身份头（x-client-request-id ==
// session-id == thread-id）、x-codex-window-id、x-codex-turn-metadata、
// x-codex-beta-features；不带 version / openai-beta / origin / referer /
// accept-encoding（真实 codex 全都不发，多一个头就是一处合成特征）。
func (t *openAIDowngradeProbeTurn) applyRequestHeaders(h http.Header, stream bool) {
	if h == nil {
		return
	}
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("originator", t.originator())
	h.Set("user-agent", t.userAgent())
	h.Set("session-id", t.sessionID)
	h.Set("thread-id", t.threadID)
	h.Set("x-client-request-id", t.sessionID)
	h.Set("x-codex-window-id", t.windowID)
	h.Set("x-codex-beta-features", "prevent_idle_sleep,remote_compaction_v2")
	h.Set("x-codex-turn-metadata", t.turnMetaJSON)
}

// openAIDowngradeProbeInstallNamespace 探针 installation_id 的账号级派生命名空间。
var openAIDowngradeProbeInstallNamespace = uuid.MustParse("9c1f0a76-2f5e-4b8a-9d3c-6e0b2a5d4c88")

// openAIDowngradeProbeInstallationID 账号级恒定：machine 指纹账号直接复用收敛
// installation ID（与账号真实出站流量同一身份），其余账号从账号 ID 稳定派生。
func openAIDowngradeProbeInstallationID(account *Account) string {
	if account == nil {
		return uuid.NewSHA1(openAIDowngradeProbeInstallNamespace, []byte("anonymous")).String()
	}
	if seed, ok := codexFingerprintSeed(account.Extra); ok && strings.TrimSpace(seed) != "" {
		return resolveConvergedInstallationID(account, seed)
	}
	return uuid.NewSHA1(openAIDowngradeProbeInstallNamespace,
		[]byte("openai-downgrade-probe-install:"+strconv.FormatInt(account.ID, 10))).String()
}

// beginProbeTelemetry 为探针 turn 补发与真实 turn 同源的客户端遥测。默认关闭
// （openAIProbeTelemetryEnabled）：analytics 端点对探针形事件回 400，且遥测与
// 探针共用上游连接会被服务端按连接降级（2026-09-18 生产实证，详见开关注释）；
// 真实客户端丢遥测不构成异常特征。返回 nil 时 finishProbeTelemetry 安全空转。
func (r *OpenAIDowngradeProbeRunner) beginProbeTelemetry(
	account *Account, turn *openAIDowngradeProbeTurn, proxyURL, token, model string,
) *openAICodexTelemetryProfile {
	if r == nil || account == nil || turn == nil || !openAIProbeTelemetryEnabled() {
		return nil
	}
	if r.httpUpstream == nil {
		return nil
	}
	accountID := strings.TrimSpace(account.GetCredential("chatgpt_account_id"))
	if token == "" || accountID == "" {
		return nil
	}
	var tlsProfile *tlsfingerprint.Profile
	if r.tlsProfiles != nil {
		tlsProfile = r.tlsProfiles.ResolveTLSProfile(account)
	}
	manager := openAICodexTelemetryGlobal
	manager.bindUpstream(r.httpUpstream)
	profile := openAICodexTelemetryProfile{
		client: openAICodexTelemetryIdentity{
			account:     account,
			accessToken: token,
			accountID:   accountID,
			proxyURL:    proxyURL,
			userAgent:   turn.userAgent(),
			originator:  turn.originator(),
			version:     turn.version,
			tlsProfile:  tlsProfile,
		},
		sessionID:   turn.sessionID,
		threadID:    turn.threadID,
		turnID:      turn.turnID,
		rootTurnID:  turn.turnID,
		model:       model,
		effort:      "xhigh",
		serviceTier: "default",
		started:     turn.started,
		turnMeta:    gjson.Parse(turn.turnMetaJSON),
	}
	profile.firstThread = manager.markThread(account.ID, profile.threadID, profile.started)
	// 与真实会话同分布的工具事件抽样（对齐 beginOpenAICodexTelemetry）。
	profile.dynamicTool = rand.IntN(5) < 2
	profile.command = profile.dynamicTool && rand.IntN(2) == 0
	profile.fileChange = rand.IntN(5) == 0
	manager.enqueueAnalytics(openAICodexInitializationEvents(profile))
	manager.touchMetrics(profile)
	return &profile
}

// finishProbeTelemetry 提交探针 turn 的终止事件与指标（经 defer 调用，
// status 取 completed/failed，与真实 turn 的终态语义一致）。
func (r *OpenAIDowngradeProbeRunner) finishProbeTelemetry(profile *openAICodexTelemetryProfile, status string, started time.Time) {
	if profile == nil {
		return
	}
	finished := time.Now()
	terminal := openAICodexTelemetryTerminal{
		status:     status,
		firstEvent: finished,
		firstToken: finished,
	}
	openAICodexTelemetryGlobal.enqueueAnalytics(openAICodexTerminalEvents(*profile, terminal))
	openAICodexTelemetryGlobal.recordTurnMetrics(*profile, terminal)
}

func shouldRetryOpenAIDowngradeStreamProbe(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		return false
	}
	lower := strings.ToLower(string(body))
	if !strings.Contains(lower, "stream") {
		return false
	}
	return strings.Contains(lower, "unsupported") ||
		strings.Contains(lower, "not support") ||
		strings.Contains(lower, "invalid") ||
		strings.Contains(lower, "must be true") ||
		strings.Contains(lower, "only supports")
}

// parseOpenAIDowngradeProbeResponse 按该针题目专属的期望答案正则判分；
// 正则由题域生成器构造（数字带边界 / 星期X 字面），见
// openai_downgrade_probe_questions.go。nil 正则 = 拒收（判分依赖缺席，
// 与结构失败同形：false + 零计数）。
func parseOpenAIDowngradeProbeResponse(body []byte, answerPattern *regexp.Regexp) (bool, *int, *int) {
	text, reasoningTokens, juice := parseOpenAIDowngradeProbeCompletion(body)
	if answerPattern == nil || text == "" {
		return false, nil, nil
	}
	return answerPattern.MatchString(text), reasoningTokens, juice
}

// parseOpenAIDowngradeProbeCompletion 是 parseOpenAIDowngradeProbeResponse 的
// 判分无关内核（r17am）：解析 SSE / 终态 JSON，返回模型答案全文与用量计数。
// 结构损坏、无 usage、无 reasoning 计数、答案文本为空等一切失败路径统一
// 返回空文本——空文本即「无法判定」。留档层（probe-archive）直接调用本函数
// 拿答案全文，不再依赖响应尾截断（终态 usage 记录霸占尾部，答案文本几乎
// 总被截掉——9/28 事故复盘实证）。
func parseOpenAIDowngradeProbeCompletion(body []byte) (string, *int, *int) {
	if len(body) > openAIDowngradeProbeMaxBodyBytes {
		return "", nil, nil
	}
	decode := func(data []byte) (map[string]any, bool) {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value map[string]any
		if decoder.Decode(&value) != nil || value == nil {
			return nil, false
		}
		var trailing any
		return value, decoder.Decode(&trailing) == io.EOF
	}
	completion, isJSON := decode(body)
	var deltas strings.Builder
	var itemDoneText strings.Builder
	hasItemDoneOutput := false
	allowLegacyDeltas := false
	if !isJSON {
		var eventName string
		var dataLines []string
		var responseID, deltaLane string
		mixedDeltaLanes := false
		acceptID := func(value any) bool {
			if value == nil {
				return true
			}
			id, ok := value.(string)
			if !ok || id == "" || (responseID != "" && responseID != id) {
				return false
			}
			responseID = id
			return true
		}
		consume := func() bool {
			if len(dataLines) == 0 {
				return true
			}
			data := strings.Join(dataLines, "\n")
			if data == "[DONE]" {
				return completion != nil && (eventName == "" || eventName == "message")
			}
			event, ok := decode([]byte(data))
			if !ok || completion != nil {
				return false
			}
			kind, validKind := event["type"].(string)
			if _, exists := event["type"]; exists && (!validKind || kind == "") {
				return false
			}
			if eventName != "" && eventName != "message" {
				if kind != "" && kind != eventName {
					return false
				}
				kind = eventName
			}
			response, nested := event["response"].(map[string]any)
			if _, exists := event["response"]; exists && !nested {
				return false
			}
			if !acceptID(event["response_id"]) || (nested && !acceptID(response["id"])) {
				return false
			}
			switch kind {
			case "response.failed", "response.incomplete", "error":
				return false
			case "response.output_text.delta":
				delta, ok := event["delta"].(string)
				if !ok {
					return false
				}
				lane, err := json.Marshal([]any{event["item_id"], event["output_index"], event["content_index"]})
				if err != nil {
					return false
				}
				if deltaLane != "" && deltaLane != string(lane) {
					mixedDeltaLanes = true
				}
				deltaLane = string(lane)
				deltas.WriteString(delta)
			case "response.output_item.done":
				// 2026-09-18 服务端流形态变更（冻结流实证）：终态 response.completed
				// 不再回显 output 条目（response.output=[]），答案全文改经
				// output_item.done 的 message 条目交付。此处与终态 output 数组
				// 走查同一规则累积文本；旧形态（终态带回显）优先级在前不受影响。
				// item 事件无 response_id，acceptID(nil) 天然放行。
				item, valid := event["item"].(map[string]any)
				if !valid {
					return false
				}
				hasItemDoneOutput = true
				if !appendOpenAIProbeItemOutputText(&itemDoneText, item) {
					return false
				}
			case "response.completed":
				completion = event
				if nested {
					completion = response
				} else if !acceptID(event["id"]) {
					return false
				}
				// Only the legacy terminal shape can omit the final output.
				allowLegacyDeltas = !nested && !mixedDeltaLanes
			}
			return true
		}
		// Dispatch only complete SSE records; a partial terminal record is
		// not evidence even when its last JSON line happens to parse.
		stream := strings.ReplaceAll(strings.ReplaceAll(string(body), "\r\n", "\n"), "\r", "\n")
		for _, line := range strings.Split(strings.TrimSuffix(stream, "\n"), "\n") {
			if line == "" {
				if !consume() {
					return "", nil, nil
				}
				eventName, dataLines = "", nil
				continue
			}
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				eventName = value
			case "data":
				dataLines = append(dataLines, value)
			}
		}
		if completion == nil || len(dataLines) != 0 {
			return "", nil, nil
		}
	}
	if value, exists := completion["response"]; isJSON && exists {
		nested, ok := value.(map[string]any)
		if !ok || completion["type"] != "response.completed" {
			return "", nil, nil
		}
		completion = nested
	}
	if kind, exists := completion["type"]; exists && kind != "response" && kind != "response.completed" {
		return "", nil, nil
	}
	if status, exists := completion["status"]; exists && status != "completed" {
		return "", nil, nil
	}
	if completion["error"] != nil || completion["incomplete_details"] != nil {
		return "", nil, nil
	}
	usage, ok := completion["usage"].(map[string]any)
	if !ok {
		return "", nil, nil
	}
	// Do not search output, metadata or echoed input for usage counters.
	var reasoningTokens, juice *int
	var details map[string]any
	if value, exists := usage["output_tokens_details"]; exists {
		details, ok = value.(map[string]any)
		if !ok {
			return "", nil, nil
		}
	}
	for _, counters := range []map[string]any{usage, details} {
		if value, exists := counters["reasoning_tokens"]; exists {
			n, valid := jsonNumberAsInt(value)
			if !valid || (reasoningTokens != nil && *reasoningTokens != n) {
				return "", nil, nil
			}
			reasoningTokens = &n
		}
	}
	if reasoningTokens == nil {
		return "", nil, nil
	}
	if value, exists := usage["juice"]; exists {
		n, valid := jsonNumberAsInt(value)
		if !valid {
			return "", nil, nil
		}
		juice = &n
	}
	var text strings.Builder
	terminalOutputEmpty := true
	if value, exists := completion["output"]; exists {
		output, ok := value.([]any)
		if !ok {
			return "", nil, nil
		}
		terminalOutputEmpty = len(output) == 0
		for _, value := range output {
			item, ok := value.(map[string]any)
			if !ok {
				return "", nil, nil
			}
			if !appendOpenAIProbeItemOutputText(&text, item) {
				return "", nil, nil
			}
		}
	}
	if terminalOutputEmpty && itemDoneText.Len() > 0 {
		// 新形态（2026-09-18 服务端流形态变更，冻结流实证）：终态不再回显
		// output 条目——键可能缺席，也可能在场但为空数组，两种都算空回显。
		// 答案全文改经 output_item.done 的 message 条目交付，此处回退到条目
		// 终文。排在 delta 兜底之前：条目是权威交付形态，delta 只是过程流。
		text.WriteString(itemDoneText.String())
	}
	if terminalOutputEmpty && !hasItemDoneOutput && text.Len() == 0 && allowLegacyDeltas {
		text.WriteString(deltas.String())
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", nil, nil
	}
	return text.String(), reasoningTokens, juice
}

// appendOpenAIProbeItemOutputText 按终态 output 数组同款规则走查单个输出条目：
// 跳过非 message / 非 assistant 条目；status 非 completed 视为结构损坏（与终态
// 走查的硬拒一致）；output_text 部件逐段追加、每段尾随 \n（与旧终态行为逐字节
// 一致）。终态回显路径与 output_item.done 路径共用，保证两形态判分同源同规则。
func appendOpenAIProbeItemOutputText(dst *strings.Builder, item map[string]any) bool {
	if kind, exists := item["type"]; exists && kind != "message" {
		return true
	}
	if role, exists := item["role"]; exists && role != "assistant" {
		return true
	}
	if status, exists := item["status"]; exists && status != "completed" {
		return false
	}
	content, ok := item["content"].([]any)
	if !ok {
		return false
	}
	for _, value := range content {
		part, ok := value.(map[string]any)
		if !ok {
			return false
		}
		if kind, exists := part["type"]; exists && kind != "output_text" {
			continue
		}
		text, ok := part["text"].(string)
		if !ok {
			return false
		}
		dst.WriteString(text)
		dst.WriteByte('\n')
	}
	return true
}

func jsonNumberAsInt(value any) (int, bool) {
	switch n := value.(type) {
	case float64:
		if n < 0 || n >= float64(uint64(1)<<(strconv.IntSize-1)) {
			return 0, false
		}
		i := int(n)
		return i, float64(i) == n
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil && i >= 0 && int64(int(i)) == i
	case int:
		return n, n >= 0
	case int64:
		return int(n), n >= 0 && int64(int(n)) == n
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil && i >= 0
	default:
		return 0, false
	}
}

func probeRandomFloat() float64 {
	// crypto/rand is intentionally not required for scheduling jitter; this
	// only prevents pool-wide synchronization and carries no security meaning.
	return rand.Float64()
}

func timePtr(value time.Time) *time.Time { return &value }

type OpenAIDowngradeBucket struct {
	ProxyID      int64   `json:"proxy_id"`
	Name         string  `json:"name"`
	Role         string  `json:"role"`
	ExitIP       string  `json:"exit_ip"`
	Capacity     int     `json:"capacity"`
	RiskScore    int     `json:"risk_score"`
	AccountIDs   []int64 `json:"account_ids"`
	HealthyCount int     `json:"healthy_count"`
	CircuitCount int     `json:"circuit_count"`
	ReprobeCount int     `json:"reprobe_count"`
	ReplaceCount int     `json:"replace_count"`
	AtCapacity   bool    `json:"at_capacity"`
}

type OpenAIDowngradeDashboard struct {
	Buckets               []OpenAIDowngradeBucket      `json:"buckets"`
	RecentEvents          []OpenAIDowngradeEvent       `json:"recent_events"`
	AccountStats          []OpenAIDowngradeAccountStat `json:"account_stats"`
	ProbeCount24h         int64                        `json:"probe_count_24h"`
	SuccessCount24h       int64                        `json:"success_count_24h"`
	DegradedAccountCount  int64                        `json:"degraded_account_count"`
	AtCapacityBucketCount int64                        `json:"at_capacity_bucket_count"`
}

type OpenAIDowngradeEvent struct {
	ID        int64          `json:"id"`
	AccountID *int64         `json:"account_id,omitempty"`
	ProxyID   *int64         `json:"proxy_id,omitempty"`
	EventType string         `json:"event_type"`
	Details   map[string]any `json:"details"`
	CreatedAt time.Time      `json:"created_at"`
}

type OpenAIDowngradeAccountStat struct {
	AccountID           int64      `json:"account_id"`
	State               string     `json:"state"`
	ProbeMode           string     `json:"probe_mode"`
	Schedulable         bool       `json:"schedulable"`
	ProxyID             *int64     `json:"proxy_id,omitempty"`
	ProxyExitIP         string     `json:"proxy_exit_ip,omitempty"`
	ProbeCount24h       int64      `json:"probe_count_24h"`
	SuccessCount24h     int64      `json:"success_count_24h"`
	SuccessRate24h      float64    `json:"success_rate_24h"`
	AvgReasoningTokens  *float64   `json:"avg_reasoning_tokens,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastProbeAt         *time.Time `json:"last_probe_at,omitempty"`
}
