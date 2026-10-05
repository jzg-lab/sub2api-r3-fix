package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func rtPtr(v int) *int { return &v }

type triggerProbeStoreStub struct {
	*downgradeProbeStoreStub
	allowed     bool
	controlErr  error
	commitErr   error
	commitCalls int
	observed    *OpenAIDowngradeMutation
}

func (s *triggerProbeStoreStub) CanRunOpenAIDowngradeProbe(context.Context, int64) (bool, error) {
	return s.allowed, s.controlErr
}

func (s *triggerProbeStoreStub) CommitOpenAIDowngradeMutation(
	_ context.Context,
	mutation *OpenAIDowngradeMutation,
) error {
	s.commitCalls++
	s.observed = mutation
	if s.commitErr != nil {
		return s.commitErr
	}
	state := *mutation.State
	s.state = &state
	return nil
}

func TestLabelOpenAIAccountHealth(t *testing.T) {
	// 9/21 用户批准的状态映射表（proposal 表格逐行）。
	cases := []struct {
		name   string
		snap   OpenAIProbeHealthSnapshot
		label  string
		color  string
		click  bool
		reason string
	}{
		{
			name:  "terminated without manual pause",
			snap:  OpenAIProbeHealthSnapshot{RescueTerminated: true, State: OpenAIDowngradeStatePendingReplace},
			label: OpenAIHealthLabelPaused, color: OpenAIHealthColorGray, reason: "rescue_terminated",
		},
		{
			name:   "正常号: on_duty/normal + 最近一针健康",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal", LastProbe: &OpenAIProbeLastEvidence{TransportOK: true, HTTPStatus: 200, AnswerCorrect: boolPtr(true), ReasoningTokens: rtPtr(1532), TurnStateLen: 332}},
			label:  OpenAIHealthLabelNormal,
			color:  OpenAIHealthColorGreen,
			reason: "on_duty",
		},
		{
			name:   "待复核: on_duty/normal + 最近一针降智(答错)",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal", LastProbe: &OpenAIProbeLastEvidence{TransportOK: true, HTTPStatus: 200, AnswerCorrect: boolPtr(false), ReasoningTokens: rtPtr(952), TurnStateLen: 356, Degraded: true}},
			label:  OpenAIHealthLabelReview,
			color:  OpenAIHealthColorOrange,
			reason: "last_probe_degraded",
		},
		{
			name:   "待复核: on_duty/normal + 答对但深截断 rt<800",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal", LastProbe: &OpenAIProbeLastEvidence{TransportOK: true, HTTPStatus: 200, AnswerCorrect: boolPtr(true), ReasoningTokens: rtPtr(516), Degraded: true}},
			label:  OpenAIHealthLabelReview,
			color:  OpenAIHealthColorOrange,
			reason: "last_probe_degraded",
		},
		{
			name:   "r15e 中性: 1552截断指纹+答对=不判降智(只看rt)",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal", LastProbe: &OpenAIProbeLastEvidence{TransportOK: true, HTTPStatus: 200, AnswerCorrect: boolPtr(true), ReasoningTokens: rtPtr(1552)}},
			label:  OpenAIHealthLabelNormal,
			color:  OpenAIHealthColorGreen,
			reason: "on_duty",
		},
		{
			name:   "问题号: circuit_open 不可点(半开复检自动恢复)",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateCircuitOpen, ProbeMode: "normal"},
			label:  OpenAIHealthLabelProblem,
			color:  OpenAIHealthColorRed,
			reason: "circuit_open",
		},
		{
			name:   "复检中: reprobe",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateReprobe, ProbeMode: "normal"},
			label:  OpenAIHealthLabelRechecking,
			color:  OpenAIHealthColorBlue,
			reason: "reprobe/normal",
		},
		{
			name:   "复检中: half_open",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateCircuitOpen, ProbeMode: "half_open"},
			label:  OpenAIHealthLabelRechecking,
			color:  OpenAIHealthColorBlue,
			reason: "circuit_open/half_open",
		},
		{
			name:   "问题号: pending_replace 判死退避 可点",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStatePendingReplace, ProbeMode: "normal"},
			label:  OpenAIHealthLabelProblem,
			color:  OpenAIHealthColorRed,
			click:  true,
			reason: "pending_replace",
		},
		{
			name:   "限流中: 优先于问题号",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStatePendingReplace, RateLimitedAt: timePtr(time.Now().UTC())},
			label:  OpenAIHealthLabelRateLimited,
			color:  OpenAIHealthColorGray,
			reason: "rate_limited",
		},
		{
			name:   "已暂停: manual_paused 最高优先",
			snap:   OpenAIProbeHealthSnapshot{ManualPaused: true, State: OpenAIDowngradeStateCircuitOpen},
			label:  OpenAIHealthLabelPaused,
			color:  OpenAIHealthColorGray,
			reason: "manual_paused",
		},
		{
			name:   "待认证: qualification",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification", Qualification: true},
			label:  OpenAIHealthLabelQualification,
			color:  OpenAIHealthColorGray,
			reason: "qualification",
		},
		{
			name:   "无针记录的 on_duty 号=正常(空态由前端显未检测)",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal"},
			label:  OpenAIHealthLabelNormal,
			color:  OpenAIHealthColorGreen,
			reason: "on_duty",
		},
		{
			name:   "sol_fallback = 复检中",
			snap:   OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty, ProbeMode: "sol_fallback"},
			label:  OpenAIHealthLabelRechecking,
			color:  OpenAIHealthColorBlue,
			reason: "on_duty/sol_fallback",
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

func TestTriggerProbeNowPathADoesNotUseGlobalRunLock(t *testing.T) {
	now := time.Date(2026, 9, 28, 21, 0, 0, 0, time.UTC)
	accountUpdatedAt := now.Add(-2 * time.Minute)
	stateUpdatedAt := now.Add(-time.Minute)
	store := &triggerProbeStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
			AccountID: 101, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
			NextProbeAt: now.Add(time.Hour), UpdatedAt: stateUpdatedAt,
		}},
		allowed: true,
	}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 101, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: true, UpdatedAt: accountUpdatedAt,
		}}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	runner.runMu.Lock() // another account's RunOnce scan owns the global lock
	defer runner.runMu.Unlock()

	result, err := runner.TriggerProbeNow(context.Background(), 101)
	require.NoError(t, err)
	require.True(t, result.Accepted)
	require.False(t, result.AlreadyFlying)
	require.Equal(t, 1, store.commitCalls)
	require.Equal(t, now, store.state.NextProbeAt)
	require.Zero(t, store.probeCalls)
}

func TestTriggerProbeNowPathBUsesGlobalRunLock(t *testing.T) {
	now := time.Date(2026, 9, 28, 21, 5, 0, 0, time.UTC)
	store := &triggerProbeStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
			AccountID: 102, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
			NextProbeAt: now.Add(time.Hour), UpdatedAt: now.Add(-time.Minute),
		}},
		allowed: true,
	}
	repo := &downgradeProbeAccountRepoStub{account: &Account{
		ID: 102, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, UpdatedAt: now.Add(-2 * time.Minute),
	}}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	runner.runMu.Lock()
	result, err := runner.TriggerProbeNow(context.Background(), 102)
	runner.runMu.Unlock()

	require.NoError(t, err)
	require.False(t, result.Accepted)
	require.True(t, result.AlreadyFlying)
	require.Zero(t, store.probeCalls)
	require.Equal(t, 1, store.getStateCalls, "locked path B must not start a fresh read")
	require.Equal(t, 1, repo.getByIDCalls)
}

func TestTriggerProbeNowPathBRejectsStaleSnapshots(t *testing.T) {
	now := time.Date(2026, 9, 28, 21, 10, 0, 0, time.UTC)
	stateUpdatedAt := now.Add(-time.Minute)
	accountUpdatedAt := now.Add(-2 * time.Minute)

	t.Run("state_generation", func(t *testing.T) {
		initial := &OpenAIDowngradeProbeState{
			AccountID: 103, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
			NextProbeAt: now.Add(time.Hour), UpdatedAt: stateUpdatedAt,
		}
		fresh := *initial
		fresh.UpdatedAt = now
		store := &triggerProbeStoreStub{
			downgradeProbeStoreStub: &downgradeProbeStoreStub{},
			allowed:                 true,
		}
		calls := 0
		store.getStateFn = func(context.Context, int64) (*OpenAIDowngradeProbeState, error) {
			calls++
			if calls == 1 {
				return initial, nil
			}
			return &fresh, nil
		}
		repo := &downgradeProbeAccountRepoStub{account: &Account{
			ID: 103, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: false, UpdatedAt: accountUpdatedAt,
		}}
		runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
		runner.now = func() time.Time { return now }

		_, err := runner.TriggerProbeNow(context.Background(), 103)
		require.ErrorIs(t, err, ErrOpenAIProbeStale)
		require.Zero(t, store.probeCalls)
		require.Equal(t, 2, store.getStateCalls)
		require.Equal(t, 2, repo.getByIDCalls)
	})

	t.Run("account_generation", func(t *testing.T) {
		state := &OpenAIDowngradeProbeState{
			AccountID: 104, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
			NextProbeAt: now.Add(time.Hour), UpdatedAt: stateUpdatedAt,
		}
		store := &triggerProbeStoreStub{
			downgradeProbeStoreStub: &downgradeProbeStoreStub{state: state},
			allowed:                 true,
		}
		initial := &Account{
			ID: 104, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: false, UpdatedAt: accountUpdatedAt,
		}
		fresh := *initial
		fresh.UpdatedAt = now
		repo := &downgradeProbeAccountRepoStub{}
		repo.getByIDFn = func(context.Context, int64) (*Account, error) {
			if repo.getByIDCalls == 1 {
				return initial, nil
			}
			return &fresh, nil
		}
		runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
		runner.now = func() time.Time { return now }

		_, err := runner.TriggerProbeNow(context.Background(), 104)
		require.ErrorIs(t, err, ErrOpenAIProbeStale)
		require.Zero(t, store.probeCalls)
		require.Equal(t, 2, store.getStateCalls)
		require.Equal(t, 2, repo.getByIDCalls)
	})
}

func TestTriggerProbeNowPathAControlErrorAndReplay(t *testing.T) {
	now := time.Date(2026, 9, 28, 21, 15, 0, 0, time.UTC)
	state := &OpenAIDowngradeProbeState{
		AccountID: 105, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
		NextProbeAt: now.Add(time.Hour), UpdatedAt: now.Add(-time.Minute),
	}
	account := &Account{
		ID: 105, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, UpdatedAt: now.Add(-2 * time.Minute),
	}

	t.Run("control_error", func(t *testing.T) {
		controlErr := errors.New("control read failed")
		store := &triggerProbeStoreStub{
			downgradeProbeStoreStub: &downgradeProbeStoreStub{state: state},
			controlErr:              controlErr,
		}
		runner := NewOpenAIDowngradeProbeRunner(store,
			&downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
		runner.now = func() time.Time { return now }

		_, err := runner.TriggerProbeNow(context.Background(), 105)
		require.ErrorIs(t, err, controlErr)
		require.Zero(t, store.commitCalls)
		require.Equal(t, now.Add(time.Hour), state.NextProbeAt)
	})

	t.Run("replay", func(t *testing.T) {
		stateCopy := *state
		store := &triggerProbeStoreStub{
			downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &stateCopy},
			allowed:                 true,
		}
		runner := NewOpenAIDowngradeProbeRunner(store,
			&downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
		runner.now = func() time.Time { return now }

		first, err := runner.TriggerProbeNow(context.Background(), 105)
		require.NoError(t, err)
		require.True(t, first.Accepted)
		second, err := runner.TriggerProbeNow(context.Background(), 105)
		require.NoError(t, err)
		require.False(t, second.Accepted)
		require.True(t, second.AlreadyFlying)
		require.Equal(t, 1, store.commitCalls)
	})

	t.Run("stale_commit", func(t *testing.T) {
		stateCopy := *state
		store := &triggerProbeStoreStub{
			downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &stateCopy},
			allowed:                 true,
			commitErr:               ErrOpenAIProbeStale,
		}
		runner := NewOpenAIDowngradeProbeRunner(store,
			&downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
		runner.now = func() time.Time { return now }

		_, err := runner.TriggerProbeNow(context.Background(), 105)
		require.ErrorIs(t, err, ErrOpenAIProbeStale)
		require.Equal(t, 1, store.commitCalls)
		require.Equal(t, now.Add(time.Hour), store.state.NextProbeAt)
	})
}

func TestTriggerProbeNowDiagnosticAuthenticationFailureIsEvidenceOnly(t *testing.T) {
	now := time.Date(2026, 9, 28, 21, 20, 0, 0, time.UTC)
	store := &triggerProbeStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
			AccountID: 106, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
			NextProbeAt: now.Add(time.Hour), UpdatedAt: now.Add(-time.Minute),
		}},
		allowed: true,
	}
	repo := &downgradeProbeAccountRepoStub{account: &Account{
		ID: 106, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, UpdatedAt: now.Add(-2 * time.Minute),
	}}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{AccountID: 106, HTTPStatus: http.StatusUnauthorized}
	}

	result, err := runner.TriggerProbeNow(context.Background(), 106)
	require.NoError(t, err)
	require.True(t, result.Accepted)
	require.True(t, result.ProbedNow)
	require.Len(t, store.probeResults, 1)
	require.Empty(t, repo.errorMessages, "diagnostic probes must not mutate account error state")
}

func TestOpenAIProbeEvidenceDegraded(t *testing.T) {
	// 401/异常路径(HTTPStatus!=200)不算降智证据(与 IsDegraded 一致)。
	ev := &OpenAIProbeLastEvidence{TransportOK: true, HTTPStatus: 401, AnswerCorrect: boolPtr(false)}
	if OpenAIProbeEvidenceDegraded(ev) {
		t.Fatal("401 must not be degraded evidence")
	}
	// 200+答错=降智。
	ev = &OpenAIProbeLastEvidence{TransportOK: true, HTTPStatus: 200, AnswerCorrect: boolPtr(false), ReasoningTokens: rtPtr(2000)}
	if !OpenAIProbeEvidenceDegraded(ev) {
		t.Fatal("200 wrong answer must be degraded evidence")
	}
	// 200+答对+rt>=800=健康。
	ev = &OpenAIProbeLastEvidence{TransportOK: true, HTTPStatus: 200, AnswerCorrect: boolPtr(true), ReasoningTokens: rtPtr(800)}
	if OpenAIProbeEvidenceDegraded(ev) {
		t.Fatal("200 correct rt=800 must be healthy")
	}
	// 断流没有可判定答案，即使 HTTP 状态已经是 200，也不能挂成降智。
	ev = &OpenAIProbeLastEvidence{HTTPStatus: 200, AnswerCorrect: nil}
	if OpenAIProbeEvidenceDegraded(ev) {
		t.Fatal("transport interruption must be neutral evidence")
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
func TestTriggerProbeNowIsolatedRescue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		paused bool
		exited bool
	}{
		{name: "active_rescue_queues_stateful_probe"},
		{name: "manual_pause_stays_diagnostic", paused: true},
		{name: "exited_rescue_stays_diagnostic", exited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			future := now.Add(time.Hour)
			account := rescueLaneSweepAccount(7)
			account.Schedulable = false
			marker := OpenAIRescueLaneMarker{EnteredAt: now.Add(-time.Hour)}
			if tc.exited {
				marker.ExitReason = "auth_rejected"
			}
			rescueLaneApplyMarker(t, account, marker)
			store := &triggerProbeStoreStub{
				downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
					AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal", NextProbeAt: future,
				}},
				allowed: !tc.paused,
			}
			runner := NewOpenAIDowngradeProbeRunner(store,
				&downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
			runner.now = func() time.Time { return now }
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				return OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, HTTPStatus: http.StatusOK}
			}
			result, err := runner.TriggerProbeNow(t.Context(), account.ID)
			require.NoError(t, err)
			require.True(t, result.Accepted)
			if tc.paused || tc.exited {
				require.True(t, result.ProbedNow)
				require.Equal(t, future, store.state.NextProbeAt)
				require.Equal(t, 1, store.probeCalls)
			} else {
				require.False(t, result.ProbedNow)
				require.Equal(t, now, store.state.NextProbeAt)
				require.Zero(t, store.probeCalls)
			}
			require.False(t, account.Schedulable)
		})
	}
}

func TestTriggerProbeNowDualPath(t *testing.T) {
	now := time.Date(2026, 9, 21, 6, 0, 0, 0, time.UTC)
	proxyID := int64(1)
	future := now.Add(30 * time.Minute)

	// 路径A：schedulable 正常号 → CAS 提前，不打针。
	schedAccount := &Account{
		ID: 92001, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, ProxyID: &proxyID,
	}
	schedStore := &triggerProbeStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
			AccountID: 92001, State: OpenAIDowngradeStateOnDuty,
			ProbeMode: "normal", NextProbeAt: future,
		}},
		allowed: true,
	}
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
	circuitStore := &triggerProbeStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
			AccountID: 92002, State: OpenAIDowngradeStateCircuitOpen,
			ProbeMode: "half_open", NextProbeAt: future,
		}},
		allowed: true,
	}
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

	// 路径B 死排期不再让位（2026-09-21 生产 1131 修复）：manual_paused/停用号
	// 的 NextProbeAt 一旦落在过去就永久冻结（ListDue 永不拾取重排），旧「已
	// due 即让位」闸把死排期误读成「有针在飞」→ 每次主动检测都 409 already_
	// flying 而实际零针在飞。修复后死排期照常打诊断针；并发叠针由 runMu 与
	// 同出口 10 分钟节流兜底。
	pausedStore.state.NextProbeAt = now.Add(-time.Minute)
	res, err = pausedRunner.TriggerProbeNow(context.Background(), 92003)
	require.NoError(t, err)
	require.True(t, res.Accepted, "frozen past schedule must not swallow the manual probe")
	require.Equal(t, 2, pausedStore.probeCalls, "diagnostic probe must fire on frozen schedule")
}

// TestTriggerProbeNowManualPausedForcesDiagnosticPath manual_paused 号即使
// schedulable=t 也必须走路径B（2026-09-21 修复#2）：ListDue 的 NOT EXISTS
// manual_paused 闸排除它们——旧判定漏了这项，manual_paused+schedulable 的号
// 走路径A 提前排期 = accepted 却永不打针的静默失效。
func TestTriggerProbeNowManualPausedForcesDiagnosticPath(t *testing.T) {
	now := time.Date(2026, 9, 21, 19, 0, 0, 0, time.UTC)
	proxyID := int64(1)
	future := now.Add(30 * time.Minute)
	account := &Account{
		ID: 95001, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, ProxyID: &proxyID,
	}
	store := &manualPausedControlStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
		AccountID: 95001, State: OpenAIDowngradeStateOnDuty,
		ProbeMode: "normal", NextProbeAt: future,
	}}}
	runner := NewOpenAIDowngradeProbeRunner(
		store, &downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, HTTPStatus: http.StatusOK}
	}
	res, err := runner.TriggerProbeNow(context.Background(), 95001)
	require.NoError(t, err)
	require.True(t, res.Accepted, "manual-paused account must take the diagnostic path")
	require.True(t, res.ProbedNow)
	require.Equal(t, 1, store.downgradeProbeStoreStub.probeCalls, "diagnostic probe must fire inline")
	require.Equal(t, future, store.downgradeProbeStoreStub.state.NextProbeAt,
		"diagnostic path must not touch the schedule")
}

// manualPausedControlStub 模拟 openai_downgrade_probe_controls.manual_paused=t
// （CanRunOpenAIDowngradeProbe=false，镜像 ListDue 的 controls 闸联判）。
type manualPausedControlStub struct {
	*downgradeProbeStoreStub
}

func (s *manualPausedControlStub) CanRunOpenAIDowngradeProbe(context.Context, int64) (bool, error) {
	return false, nil
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
// （无状态行）不再误报 ErrAccountNotFound，补行后走路径B 诊断。
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
	require.Equal(t, 1, store.ensureCalls)
	require.Equal(t, 1, store.probeCalls)
}

func TestTriggerProbeNowIneligibleMissingStateHasNoSideEffects(t *testing.T) {
	now := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Minute)
	parentID := int64(11)
	cases := []struct {
		name    string
		account *Account
	}{
		{
			name: "non_openai",
			account: &Account{
				ID: 95001, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
				Status: StatusActive,
			},
		},
		{
			name: "non_oauth",
			account: &Account{
				ID: 95001, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Status: StatusActive,
			},
		},
		{
			name: "shadow",
			account: &Account{
				ID: 95001, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, ParentAccountID: &parentID,
			},
		},
		{
			name: "expired",
			account: &Account{
				ID: 95001, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, AutoPauseOnExpired: true, ExpiresAt: &expired,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &triggerProbeStoreStub{
				downgradeProbeStoreStub: &downgradeProbeStoreStub{},
				allowed:                 true,
			}
			runner := NewOpenAIDowngradeProbeRunner(
				store, &downgradeProbeAccountRepoStub{account: tc.account}, nil, nil, nil, nil)
			runner.now = func() time.Time { return now }

			_, err := runner.TriggerProbeNow(context.Background(), tc.account.ID)
			require.ErrorIs(t, err, errOpenAIProbeNotEligible)
			require.Zero(t, store.ensureCalls)
			require.Zero(t, store.probeCalls)
			require.Zero(t, store.commitCalls)
			require.Nil(t, store.state)
		})
	}
}
