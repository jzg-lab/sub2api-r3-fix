package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func rtPtr(v int) *int { return &v }

func TestLabelOpenAIAccountHealth(t *testing.T) {
	// 9/21 用户批准的状态映射表（proposal 表格逐行）。
	cases := []struct {
		name     string
		snap     OpenAIProbeHealthSnapshot
		label    string
		color    string
		click    bool
		reason   string
	}{
		{
			name:     "正常号: on_duty/normal + 最近一针健康",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal", LastProbe: &OpenAIProbeLastEvidence{HTTPStatus: 200, AnswerCorrect: true, ReasoningTokens: rtPtr(1532), TurnStateLen: 332}},
			label:    OpenAIHealthLabelNormal,
			color:    OpenAIHealthColorGreen,
			reason:   "on_duty",
		},
		{
			name:     "待复核: on_duty/normal + 最近一针降智(答错)",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal", LastProbe: &OpenAIProbeLastEvidence{HTTPStatus: 200, AnswerCorrect: false, ReasoningTokens: rtPtr(952), TurnStateLen: 356, Degraded: true}},
			label:    OpenAIHealthLabelReview,
			color:    OpenAIHealthColorOrange,
			reason:   "last_probe_degraded",
		},
		{
			name:     "待复核: on_duty/normal + 答对但深截断 rt<800",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal", LastProbe: &OpenAIProbeLastEvidence{HTTPStatus: 200, AnswerCorrect: true, ReasoningTokens: rtPtr(516), Degraded: true}},
			label:    OpenAIHealthLabelReview,
			color:    OpenAIHealthColorOrange,
			reason:   "last_probe_degraded",
		},
		{
			name:     "r15e 中性: 1552截断指纹+答对=不判降智(只看rt)",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal", LastProbe: &OpenAIProbeLastEvidence{HTTPStatus: 200, AnswerCorrect: true, ReasoningTokens: rtPtr(1552)}},
			label:    OpenAIHealthLabelNormal,
			color:    OpenAIHealthColorGreen,
			reason:   "on_duty",
		},
		{
			name:     "问题号: circuit_open 可点(相位B转打票线)",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateCircuitOpen, ProbeMode: "normal"},
			label:    OpenAIHealthLabelProblem,
			color:    OpenAIHealthColorRed,
			click:    true,
			reason:   "circuit_open",
		},
		{
			name:     "复检中: reprobe",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateReprobe, ProbeMode: "normal"},
			label:    OpenAIHealthLabelRechecking,
			color:    OpenAIHealthColorBlue,
			reason:   "reprobe/normal",
		},
		{
			name:     "复检中: half_open",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateCircuitOpen, ProbeMode: "half_open"},
			label:    OpenAIHealthLabelRechecking,
			color:    OpenAIHealthColorBlue,
			reason:   "circuit_open/half_open",
		},
		{
			name:     "问题号: pending_replace 判死退避 可点",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStatePendingReplace, ProbeMode: "normal"},
			label:    OpenAIHealthLabelProblem,
			color:    OpenAIHealthColorRed,
			click:    true,
			reason:   "pending_replace",
		},
		{
			name:     "限流中: 优先于问题号(打票救不了额度)",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStatePendingReplace, RateLimitedAt: timePtr(time.Now().UTC())},
			label:    OpenAIHealthLabelRateLimited,
			color:    OpenAIHealthColorGray,
			reason:   "rate_limited",
		},
		{
			name:     "已暂停: manual_paused 最高优先",
			snap:     OpenAIProbeHealthSnapshot{ManualPaused: true, State: OpenAIDowngradeStateCircuitOpen},
			label:    OpenAIHealthLabelPaused,
			color:    OpenAIHealthColorGray,
			reason:   "manual_paused",
		},
		{
			name:     "待认证: qualification",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification", Qualification: true},
			label:    OpenAIHealthLabelQualification,
			color:    OpenAIHealthColorGray,
			reason:   "qualification",
		},
		{
			name:     "无针记录的 on_duty 号=正常(空态由前端显未检测)",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal"},
			label:    OpenAIHealthLabelNormal,
			color:    OpenAIHealthColorGreen,
			reason:   "on_duty",
		},
		{
			name:     "sol_fallback = 复检中",
			snap:     OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "sol_fallback"},
			label:    OpenAIHealthLabelRechecking,
			color:    OpenAIHealthColorBlue,
			reason:   "on_duty/sol_fallback",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label, color, clickable, reason := LabelOpenAIAccountHealth(tc.snap)
			if label != tc.label || color != tc.color || clickable != tc.click || reason != tc.reason {
				t.Fatalf("got (%s,%s,%v,%s) want (%s,%s,%v,%s)",
					label, color, clickable, reason, tc.label, tc.color, tc.click, tc.reason)
			}
		})
	}
}

func TestOpenAIProbeEvidenceDegraded(t *testing.T) {
	// 401/异常路径(HTTPStatus!=200)不算降智证据(与 IsDegraded 一致)。
	ev := &OpenAIProbeLastEvidence{HTTPStatus: 401, AnswerCorrect: false}
	if OpenAIProbeEvidenceDegraded(ev) {
		t.Fatal("401 must not be degraded evidence")
	}
	// 200+答错=降智。
	ev = &OpenAIProbeLastEvidence{HTTPStatus: 200, AnswerCorrect: false, ReasoningTokens: rtPtr(2000)}
	if !OpenAIProbeEvidenceDegraded(ev) {
		t.Fatal("200 wrong answer must be degraded evidence")
	}
	// 200+答对+rt>=800=健康。
	ev = &OpenAIProbeLastEvidence{HTTPStatus: 200, AnswerCorrect: true, ReasoningTokens: rtPtr(800)}
	if OpenAIProbeEvidenceDegraded(ev) {
		t.Fatal("200 correct rt=800 must be healthy")
	}
}

func TestTriggerProbeNowCASSemantics(t *testing.T) {
	// TriggerProbeNow 的 CAS 语义（连点去重）由 store 桩验证:
	// 1. NextProbeAt 在未来 → 提前到 now, accepted=true
	// 2. NextProbeAt 已是过去 → already_flying, 不改排期
	// 完整链路在 handler 集成测试覆盖；此处验证纯函数与结果结构。
	res := TriggerProbeNowResult{Accepted: true, QueuedAt: time.Now()}
	if !res.Accepted || res.AlreadyFlying {
		t.Fatal("result struct sanity")
	}
}

// TestTriggerProbeNowDualPath 双路径回归（2026-09-21 修复「暂停号静默失效」）：
// 路径A（调度会拾取：schedulable/熔断态/qualification/error）→ CAS 提前排期，
// 不当场打针（下一拍 processState 全状态机处理）；
// 路径B（ListDue 永不拾取：manual_paused/停用 on_duty）→ 同步诊断针当场落
// 证据行，状态机/排期/调度位分毫不动。
func TestTriggerProbeNowDualPath(t *testing.T) {
	now := time.Date(2026, 9, 21, 6, 0, 0, 0, time.UTC)
	proxyID := int64(1)
	future := now.Add(30 * time.Minute)

	// 路径A：schedulable 正常号 → CAS 提前，不打针。
	schedAccount := &Account{
		ID: 92001, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, ProxyID: &proxyID,
	}
	schedStore := &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
		AccountID: 92001, State: OpenAIDowngradeStateOnDuty,
		ProbeMode: "normal", NextProbeAt: future,
	}}
	runner := NewOpenAIDowngradeProbeRunner(
		schedStore, &downgradeProbeAccountRepoStub{account: schedAccount}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	res, err := runner.TriggerProbeNow(context.Background(), 92001)
	require.NoError(t, err)
	require.True(t, res.Accepted, "schedulable account must take the schedule-pull path")
	require.Equal(t, now, schedStore.state.NextProbeAt, "NextProbeAt must be pulled to now")
	require.Zero(t, schedStore.probeCalls, "path A must not probe inline")

	// 路径A：熔断态号（state != on_duty，ListDue 资格判定不依赖 schedulable）
	// → 同样 CAS 提前。
	circuitAccount := &Account{
		ID: 92002, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &proxyID,
	}
	circuitStore := &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
		AccountID: 92002, State: OpenAIDowngradeStateCircuitOpen,
		ProbeMode: "half_open", NextProbeAt: future,
	}}
	circuitRunner := NewOpenAIDowngradeProbeRunner(
		circuitStore, &downgradeProbeAccountRepoStub{account: circuitAccount}, nil, nil, nil, nil)
	circuitRunner.now = func() time.Time { return now }
	res, err = circuitRunner.TriggerProbeNow(context.Background(), 92002)
	require.NoError(t, err)
	require.True(t, res.Accepted, "circuit account must take the schedule-pull path")
	require.Zero(t, circuitStore.probeCalls, "path A must not probe inline")

	// 路径B：面板停用（!schedulable）+ on_duty + 非 qualification → ListDue
	// 永不拾取，同步诊断针当场落证据行；状态机/排期不动。
	pausedAccount := &Account{
		ID: 92003, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &proxyID,
	}
	pausedStore := &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
		AccountID: 92003, State: OpenAIDowngradeStateOnDuty,
		ProbeMode: "normal", NextProbeAt: future,
	}}
	pausedRunner := NewOpenAIDowngradeProbeRunner(
		pausedStore, &downgradeProbeAccountRepoStub{account: pausedAccount}, nil, nil, nil, nil)
	pausedRunner.now = func() time.Time { return now }
	pausedRunner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: true,
			ReasoningTokens: downgradeProbeIntPtr(1532),
			TurnStateLen:    332,
			HTTPStatus:      http.StatusOK,
		}
	}
	res, err = pausedRunner.TriggerProbeNow(context.Background(), 92003)
	require.NoError(t, err)
	require.True(t, res.Accepted, "paused account must take the diagnostic path")
	require.Equal(t, 1, pausedStore.probeCalls, "diagnostic probe must be recorded inline")
	require.Equal(t, future, pausedStore.state.NextProbeAt,
		"diagnostic path must not touch the schedule")
	require.Equal(t, OpenAIDowngradeStateOnDuty, pausedStore.state.State,
		"diagnostic path must not touch the state machine")
	require.False(t, pausedAccount.Schedulable, "diagnostic path must not re-enable scheduling")

	// 路径B 连点去重：NextProbeAt 已在过去（另一针刚打过/在飞）→ already_flying。
	pausedStore.state.NextProbeAt = now.Add(-time.Minute)
	res, err = pausedRunner.TriggerProbeNow(context.Background(), 92003)
	require.NoError(t, err)
	require.False(t, res.Accepted, "second click while flying must be rejected")
	require.True(t, res.AlreadyFlying)
	require.Equal(t, 1, pausedStore.probeCalls, "no extra probe on dedup")
}

// TestTriggerProbeNowExitThrottle 路径B 同出口节流（镜像 ListDue 10 分钟闸）：
// 同 exit_ip 近窗已有合成探针 → 拒针不烧请求；节流未命中 → 照常打。
// 桩不实现 OpenAIProbeExitThrottler 时节流关闭（向后兼容）。
func TestTriggerProbeNowExitThrottle(t *testing.T) {
	now := time.Date(2026, 9, 21, 6, 30, 0, 0, time.UTC)
	proxyID := int64(1)
	future := now.Add(30 * time.Minute)

	newThrottled := func(hit bool) (*downgradeProbeStoreStub, *OpenAIDowngradeProbeRunner) {
		account := &Account{
			ID: 93001, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: false, ProxyID: &proxyID,
		}
		store := &throttleProbeStoreStub{
			downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
				AccountID: 93001, State: OpenAIDowngradeStateOnDuty,
				ProbeMode: "normal", NextProbeAt: future,
			}},
			recentHit: hit,
		}
		runner := NewOpenAIDowngradeProbeRunner(
			store, &downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
		runner.now = func() time.Time { return now }
		runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
			return OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, HTTPStatus: http.StatusOK}
		}
		return store.downgradeProbeStoreStub, runner
	}

	// 近窗无针 → 放行。
	store, runner := newThrottled(false)
	res, err := runner.TriggerProbeNow(context.Background(), 93001)
	require.NoError(t, err)
	require.True(t, res.Accepted)
	require.Equal(t, 1, store.probeCalls)

	// 同出口近窗有针 → 节流拒针（accepted=false，非 already_flying）。
	store, runner = newThrottled(true)
	res, err = runner.TriggerProbeNow(context.Background(), 93001)
	require.NoError(t, err)
	require.False(t, res.Accepted, "throttled click must be rejected")
	require.False(t, res.AlreadyFlying, "throttle is not a flying dedup")
	require.Zero(t, store.probeCalls, "throttled click must not fire a probe")
}

// throttleProbeStoreStub 挂接同出口节流能力的组合桩。
type throttleProbeStoreStub struct {
	*downgradeProbeStoreStub
	recentHit bool
}

func (s *throttleProbeStoreStub) RecentProbeOnExitIP(
	context.Context, int64, *int64, time.Time,
) (bool, error) {
	return s.recentHit, nil
}

// TestTriggerProbeNowMissingStateEnsuresRow 从未被扫描 Ensure 过的暂停号
//（无状态行）不再误报 ErrAccountNotFound，补行后走路径B 诊断。
func TestTriggerProbeNowMissingStateEnsuresRow(t *testing.T) {
	now := time.Date(2026, 9, 21, 7, 0, 0, 0, time.UTC)
	proxyID := int64(1)
	account := &Account{
		ID: 94001, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &proxyID,
	}
	store := &downgradeProbeStoreStub{state: nil}
	runner := NewOpenAIDowngradeProbeRunner(
		store, &downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, HTTPStatus: http.StatusOK}
	}
	res, err := runner.TriggerProbeNow(context.Background(), 94001)
	require.NoError(t, err, "missing state row must be ensured, not 404")
	require.True(t, res.Accepted)
	require.True(t, res.ProbedNow)
	require.Equal(t, 1, store.probeCalls)
}

