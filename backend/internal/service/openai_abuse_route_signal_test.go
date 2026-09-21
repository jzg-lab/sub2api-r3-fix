package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// drainAbuseRouteSignal 清理全局信号桥，防止用例断言失败时信号泄漏到后续用例。
func drainAbuseRouteSignal(accountID int64) {
	openAIAbuseRouteSignals.mu.Lock()
	defer openAIAbuseRouteSignals.mu.Unlock()
	delete(openAIAbuseRouteSignals.signals, accountID)
}

func TestOpenAIAbuseRouteSignalHubObserveAcknowledgePeekTTL(t *testing.T) {
	hub := &openAIAbuseRouteSignalHub{signals: make(map[int64]openAIAbuseRouteSignal)}
	now := time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)

	hub.ObserveRealTrafficModelMismatch(91001, "gpt-6-astra", "gpt-5.6-luna", now)
	signal, ok := hub.PeekRealTrafficSignal(91001, now)
	require.True(t, ok)
	require.Equal(t, "gpt-6-astra", signal.RequestedModel)
	require.Equal(t, "gpt-5.6-luna", signal.ResponseModel)

	// Confirmation is idempotent, while reads leave the observation pending.
	require.Equal(t, int64(91001), signal.AccountID)
	hub.AcknowledgeRealTrafficSignal(signal)
	hub.AcknowledgeRealTrafficSignal(signal)
	_, ok = hub.PeekRealTrafficSignal(91001, now)
	require.False(t, ok)

	// TTL 过期：未被吸收的信号自然失效，不产生迟到的加速复查。
	hub.ObserveRealTrafficModelMismatch(91001, "gpt-6-astra", "gpt-5.6-luna", now)
	_, ok = hub.PeekRealTrafficSignal(91001, now.Add(openAIAbuseRouteSignalTTL+time.Second))
	require.False(t, ok)

	// 后到的观测刷新时间戳；非法账号 ID 不落。
	hub.ObserveRealTrafficModelMismatch(91001, "gpt-6-astra", "gpt-5.6-luna", now.Add(20*time.Minute))
	_, ok = hub.PeekRealTrafficSignal(91001, now.Add(29*time.Minute))
	require.True(t, ok, "refreshed observation must stay within TTL")
	hub.ObserveRealTrafficModelMismatch(0, "a", "b", now)
	_, ok = hub.PeekRealTrafficSignal(0, now)
	require.False(t, ok)
}

func TestOpenAIGatewayRecordUsageObservesAbuseRouteSignalOnMismatch(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(
		usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)

	const accountID = int64(91002)
	defer drainAbuseRouteSignal(accountID)
	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "abuse-signal-mismatch",
			Usage:     OpenAIUsage{InputTokens: 8, OutputTokens: 4},
			Model:     "gpt-6-astra",
			// 服务端 abuse 路由：请求 astra，响应自报 luna。
			UpstreamResponseModel: "gpt-5.6-luna",
			Duration:              time.Second,
		},
		APIKey:  &APIKey{ID: 501},
		User:    &User{ID: 601},
		Account: &Account{ID: accountID, Platform: PlatformOpenAI},
	})
	require.NoError(t, err)

	signal, ok := openAIAbuseRouteSignals.PeekRealTrafficSignal(accountID, time.Now())
	require.True(t, ok, "mismatch on OpenAI account must deliver a signal")
	require.Equal(t, "gpt-6-astra", signal.RequestedModel)
	require.Equal(t, "gpt-5.6-luna", signal.ResponseModel)
	require.False(t, signal.ObservedAt.IsZero())
}

func TestOpenAIGatewayRecordUsageSkipsAbuseRouteSignalWithoutMismatch(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(
		usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)

	cases := []struct {
		name        string
		platform    string
		sent        string
		respondedAs string
	}{
		{"matched model", PlatformOpenAI, "gpt-6-astra", "gpt-6-astra"},
		{"no response model", PlatformOpenAI, "gpt-6-astra", ""},
		{"non openai platform", PlatformAnthropic, "claude-x", "gpt-5.6-luna"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const accountID = int64(91003)
			defer drainAbuseRouteSignal(accountID)
			err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
				Result: &OpenAIForwardResult{
					RequestID: "abuse-signal-" + tc.name,
					Usage:     OpenAIUsage{InputTokens: 8, OutputTokens: 4},
					Model:     tc.sent,
					UpstreamResponseModel: func() string {
						if tc.respondedAs == tc.sent {
							return ""
						}
						return tc.respondedAs
					}(),
					Duration: time.Second,
				},
				APIKey:  &APIKey{ID: 502},
				User:    &User{ID: 602},
				Account: &Account{ID: accountID, Platform: tc.platform},
			})
			require.NoError(t, err)
			if tc.platform == PlatformOpenAI && tc.respondedAs == "gpt-6-astra" && tc.sent == "gpt-6-astra" {
				// matched 分支：UpstreamResponseModel 置空以避免计费回退干扰，
				// 真正的匹配语义由下方专用断言覆盖。
			}
			_, ok := openAIAbuseRouteSignals.PeekRealTrafficSignal(accountID, time.Now())
			require.False(t, ok, "no signal may be delivered for %s", tc.name)
		})
	}
}

func TestOpenAIGatewayRecordUsageMatchedResponseModelDeliversNoSignal(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(
		usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)

	const accountID = int64(91004)
	defer drainAbuseRouteSignal(accountID)
	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID:             "abuse-signal-match",
			Usage:                 OpenAIUsage{InputTokens: 8, OutputTokens: 4},
			Model:                 "gpt-6-astra",
			UpstreamResponseModel: "gpt-6-astra",
			Duration:              time.Second,
		},
		APIKey:  &APIKey{ID: 503},
		User:    &User{ID: 603},
		Account: &Account{ID: accountID, Platform: PlatformOpenAI},
	})
	require.NoError(t, err)
	_, ok := openAIAbuseRouteSignals.PeekRealTrafficSignal(accountID, time.Now())
	require.False(t, ok, "matched response model must not deliver a signal")
}

// TestProbeRealTrafficMismatchSignalSkipsTrafficDeferral：吸收点在真实流量顺延
// 判定之前——mismatch 正是真实流量刚产生的，若先顺延，活跃账号的信号会在
// TTL 内永远等不到吸收。吸收后本针立即执行并记 real_traffic_model_mismatch
// 事件；无新信号时顺延语义不变。
func TestProbeRealTrafficMismatchSignalSkipsTrafficDeferralAndRecordsEvent(t *testing.T) {
	const accountID = int64(91005)
	defer drainAbuseRouteSignal(accountID)
	account := &Account{
		ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
	}
	base := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, accountRepo: repo}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	now := time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return now }
	runner.nextDelay = func() time.Duration { return time.Minute }
	probeRan := false
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		probeRan = true
		return OpenAIDowngradeProbeResult{
			AccountID:   accountID,
			TransportOK: true, AnswerCorrect: true,
			ReasoningTokens: downgradeProbeIntPtr(1800),
		}
	}
	runner.SetRecentTrafficChecker(func(context.Context, int64, time.Duration) bool { return true })
	state := &OpenAIDowngradeProbeState{
		AccountID: accountID, State: OpenAIDowngradeStateOnDuty,
		ProbeMode: "normal", NextProbeAt: now,
	}

	openAIAbuseRouteSignals.ObserveRealTrafficModelMismatch(
		accountID, "gpt-6-astra", "gpt-5.6-luna", now.Add(-time.Minute))

	require.NoError(t, runner.processStateAtomic(context.Background(), state, now))
	require.True(t, probeRan, "absorbed mismatch signal must pull this probe out of traffic deferral")
	require.NotEmpty(t, store.observed.Events)
	require.Equal(t, OpenAIDowngradeEventRealTrafficModelMismatch, store.observed.Events[0].Type)
	var details map[string]any
	require.NoError(t, json.Unmarshal(store.observed.Events[0].Details, &details))
	require.Equal(t, map[string]any{
		"requested_model": "gpt-6-astra",
		"response_model":  "gpt-5.6-luna",
		"observed_at":     now.Add(-time.Minute).Format(time.RFC3339),
	}, details)
	// 信号已吸收：同账号同窗再跑一轮（无新信号）回到顺延语义。
	probeRan = false
	require.NoError(t, runner.processStateAtomic(context.Background(), state, now))
	require.False(t, probeRan, "deferral must resume once the signal is absorbed")
	require.LessOrEqual(t, state.NextProbeAt.Sub(now), 67*time.Minute+time.Second)
	require.GreaterOrEqual(t, state.NextProbeAt.Sub(now), 22*time.Minute-time.Second)
}

// TestProbeTurnStateDegradedAloneRecordsEventAndAcceleratesRecheck：单 356
// （无降智证据）只记事件 + 加速复查（2.5-7.5min），不摘号、不熔断。
func TestProbeTurnStateDegradedAloneRecordsEventAndAcceleratesRecheck(t *testing.T) {
	const accountID = int64(91006)
	account := &Account{
		ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	now := time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return now }
	runner.nextDelay = func() time.Duration { return time.Hour }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: true,
			ReasoningTokens: downgradeProbeIntPtr(1800),
			TurnStateLen:    356,
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: accountID, State: OpenAIDowngradeStateOnDuty,
		ProbeMode: "normal", NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, []string{OpenAIDowngradeEventTurnStateDegraded}, store.eventTypes)
	require.Equal(t, OpenAIDowngradeStateOnDuty, state.State, "single 356 must not circuit")
	require.Empty(t, repo.schedulableCalls, "single 356 must not touch schedulable")
	delay := state.NextProbeAt.Sub(now)
	require.GreaterOrEqual(t, delay, 5*time.Minute/2-time.Second, "accelerated recheck floor 2.5min")
	require.LessOrEqual(t, delay, 15*time.Minute/2+time.Second, "accelerated recheck cap 7.5min")
}

// TestProbeDualSignalTurnStateAndDegradedCircuitsImmediately：356 与降智证据
// （答错）同针在场 = 双信号，首针即熔断——复用既有熔断路径（事件、摘调度、
// 随机化冷却），不另起摘除逻辑。
func TestProbeDualSignalTurnStateAndDegradedCircuitsImmediately(t *testing.T) {
	const accountID = int64(91007)
	account := &Account{
		ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	now := time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return now }
	runner.nextDelay = func() time.Duration { return time.Hour }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: false,
			ReasoningTokens: downgradeProbeIntPtr(516),
			TurnStateLen:    356,
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: accountID, State: OpenAIDowngradeStateOnDuty,
		ProbeMode: "normal", NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, OpenAIDowngradeStateCircuitOpen, state.State,
		"dual signal on the first probe must circuit immediately")
	require.Equal(t, 1, store.probeCalls, "circuit must come from a single probe")
	require.Equal(t, []string{
		OpenAIDowngradeEventTurnStateDegraded,
		OpenAIDowngradeEventCircuitOpen,
	}, store.eventTypes)
	require.Equal(t, []bool{false}, repo.schedulableCalls, "circuit must pull the account out of scheduling")
	require.NotNil(t, state.CircuitOpenedAt)
	delay := state.NextProbeAt.Sub(now)
	require.GreaterOrEqual(t, delay, 30*time.Minute-time.Second, "cooling floor")
	require.LessOrEqual(t, delay, time.Duration(float64(30*time.Minute)*1.25)+time.Second, "cooling cap")
}

// TestProbeTurnStateHealthyLengthRecordsNoEvent：332（健康态）与无头（0）
// 都不触发相位2路径，排期走常规节奏。
func TestProbeTurnStateHealthyLengthRecordsNoEvent(t *testing.T) {
	for _, turnStateLen := range []int{332, 0} {
		const accountID = int64(91008)
		account := &Account{
			ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: true,
		}
		store := &downgradeProbeStoreStub{}
		repo := &downgradeProbeAccountRepoStub{account: account}
		runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
		now := time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)
		runner.now = func() time.Time { return now }
		runner.nextDelay = func() time.Duration { return time.Hour }
		runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
			return OpenAIDowngradeProbeResult{
				TransportOK: true, AnswerCorrect: true,
				ReasoningTokens: downgradeProbeIntPtr(1800),
				TurnStateLen:    turnStateLen,
			}
		}
		state := &OpenAIDowngradeProbeState{
			AccountID: accountID, State: OpenAIDowngradeStateOnDuty,
			ProbeMode: "normal", NextProbeAt: now,
		}

		require.NoError(t, runner.processState(context.Background(), state, now))
		require.Empty(t, store.eventTypes, "turn_state_len=%d must not record events", turnStateLen)
		require.Equal(t, now.Add(time.Hour), state.NextProbeAt, "regular cadence must be kept")
		require.Equal(t, OpenAIDowngradeStateOnDuty, state.State)
	}
}

// TestProbeDegradedWithoutTurnStateStillNeedsTwoStrikes：无 356 在场的降智
// 证据仍走两连败防误杀（1020 间歇性先例），相位2不改变单证据语义。
func TestProbeDegradedWithoutTurnStateStillNeedsTwoStrikes(t *testing.T) {
	const accountID = int64(91009)
	account := &Account{
		ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	now := time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return now }
	runner.nextDelay = func() time.Duration { return time.Hour }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: false,
			ReasoningTokens: downgradeProbeIntPtr(400),
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: accountID, State: OpenAIDowngradeStateOnDuty,
		ProbeMode: "normal", NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, OpenAIDowngradeStateOnDuty, state.State,
		"degraded evidence without turn-state must still require two strikes")
	require.Equal(t, 1, state.ConsecutiveFailures)
	require.Empty(t, repo.schedulableCalls)
	require.Empty(t, store.eventTypes)
}

// TestProbeDualSignalQualificationFirstStrikeGetsGracePeriod：qualification
// 模式不注入双信号熔断（2026-09-21 裁定，1115/1116 案）：新号无历史基线，
// 首针 356+答错可能是 IP 级暂态——连败走自然节奏，认证失败走既有
// qualification_failed 判死分支（2 连败），不与双信号叠加一针判死。
func TestProbeDualSignalQualificationFirstStrikeGetsGracePeriod(t *testing.T) {
	const accountID = int64(91009)
	proxyID := int64(1)
	account := &Account{
		ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &proxyID,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	now := time.Date(2026, 9, 21, 0, 57, 0, 0, time.UTC)
	runner.now = func() time.Time { return now }
	runner.nextDelay = func() time.Duration { return time.Hour }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		// 1115 首针实况：200/答错/rt670/356。
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: false,
			ReasoningTokens: downgradeProbeIntPtr(670),
			TurnStateLen:    356,
			HTTPStatus:      http.StatusOK,
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: accountID, State: OpenAIDowngradeStateOnDuty,
		ProbeMode: "qualification", CurrentProxyID: &proxyID,
		OriginalProxyID: &proxyID, NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, 1, state.ConsecutiveFailures,
		"qualification first degraded strike must count naturally, not be pushed to circuit threshold")
	require.NotEqual(t, OpenAIDowngradeStatePendingReplace, state.State,
		"qualification must not be judged dead on a single dual-signal strike")
	require.Contains(t, store.eventTypes, OpenAIDowngradeEventTurnStateDegraded,
		"turn_state_degraded event must still be recorded (evidence preserved)")
}
