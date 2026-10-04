//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// r17x 选项A(2026-09-21 用户裁定):检测通过→正常启用不定期检测;
// 问题号打标签后不再自动检测;手动启用是唯一救援入口。
// 本文件钉「判死即终态 + 手动启用」语义。

type accountReenableStoreStub struct {
	*downgradeProbeStoreStub
	allowed      bool
	controlErr   error
	commitErr    error
	unpaused     bool
	commits      int
	observed     *OpenAIAccountReenableMutation
	beforeCommit func()
}

func (s *accountReenableStoreStub) CanRunOpenAIDowngradeProbe(context.Context, int64) (bool, error) {
	return s.allowed, s.controlErr
}

func (s *accountReenableStoreStub) CommitOpenAIAccountReenable(
	_ context.Context,
	mutation *OpenAIAccountReenableMutation,
) (bool, error) {
	s.commits++
	s.observed = mutation
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	if s.commitErr != nil {
		return false, s.commitErr
	}
	state := *mutation.State
	s.state = &state
	return s.unpaused, nil
}

// 判死号手动启用:清标签回 qualification、计数归零、排近刻认证针。
func TestReenableOpenAIAccount_FromPendingReplace(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	accountUpdatedAt := now.Add(-2 * time.Hour)
	stateUpdatedAt := now.Add(-time.Hour)
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, UpdatedAt: accountUpdatedAt,
	}
	store := &accountReenableStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{},
		allowed:                 true,
	}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	store.state = &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
		ConsecutiveFailures: 3, ConsecutiveSuccesses: 1, Consecutive429s: 2,
		UpdatedAt: stateUpdatedAt,
	}

	result, err := runner.ReenableOpenAIAccount(context.Background(), 7, false)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.ProbeQueued)
	require.False(t, result.Unpaused)
	require.Equal(t, OpenAIDowngradeStateOnDuty, store.state.State)
	require.Equal(t, "qualification", store.state.ProbeMode)
	// r17y 一击退出：连败预置 1,结论针失败即推到熔断阈值回判死
	require.Equal(t, 1, store.state.ConsecutiveFailures)
	require.Zero(t, store.state.ConsecutiveSuccesses)
	require.Zero(t, store.state.Consecutive429s)
	require.Nil(t, store.state.RecoveryDeadline)
	// 认证针近刻(5 分钟档 jitter)
	require.True(t, store.state.NextProbeAt.After(now))
	require.True(t, store.state.NextProbeAt.Before(now.Add(10*time.Minute)))
	// 审计事件落地(桩记录在 eventTypes/eventDetails)
	require.Equal(t, 1, store.commits)
	require.NotNil(t, store.observed)
	require.Equal(t, stateUpdatedAt, store.observed.ExpectedStateUpdatedAt)
	require.Equal(t, accountUpdatedAt, store.observed.ExpectedAccountUpdatedAt)
	require.Zero(t, store.eventCalls, "service must not publish events outside the transaction")
}

func TestReenableOpenAIAccount_ControlErrorFailsClosed(t *testing.T) {
	controlErr := errors.New("control read failed")
	state := &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
	}
	store := &accountReenableStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{state: state},
		controlErr:              controlErr,
	}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive,
		}}, nil, nil, nil, nil)

	_, err := runner.ReenableOpenAIAccount(context.Background(), 7, false)
	require.ErrorIs(t, err, controlErr)
	require.Zero(t, store.commits)
	require.Same(t, state, store.state)
	require.Equal(t, OpenAIDowngradeStatePendingReplace, store.state.State)
}

func TestReenableOpenAIAccount_RequiresAtomicStore(t *testing.T) {
	store := &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
	}}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive,
		}}, nil, nil, nil, nil)

	_, err := runner.ReenableOpenAIAccount(context.Background(), 7, false)
	require.ErrorIs(t, err, ErrOpenAIProbeAtomicStore)
	require.Equal(t, OpenAIDowngradeStatePendingReplace, store.state.State)
	require.Zero(t, store.saveCalls)
}

func TestReenableOpenAIAccount_StaleCommitPreservesOriginalState(t *testing.T) {
	state := &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
		ConsecutiveFailures: 3,
	}
	before := *state
	store := &accountReenableStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{state: state},
		allowed:                 true,
		commitErr:               ErrOpenAIProbeStale,
	}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive,
		}}, nil, nil, nil, nil)

	_, err := runner.ReenableOpenAIAccount(context.Background(), 7, false)
	require.ErrorIs(t, err, ErrOpenAIProbeStale)
	require.Equal(t, before, *state)
	require.Equal(t, before, *store.state)
	require.NotSame(t, state, store.observed.State)
}

func TestReenableOpenAIAccount_ReplayDoesNotOverwriteWinner(t *testing.T) {
	store := &accountReenableStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
			AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
		}},
		allowed: true,
	}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive,
		}}, nil, nil, nil, nil)

	_, err := runner.ReenableOpenAIAccount(context.Background(), 7, false)
	require.NoError(t, err)
	winner := *store.state

	_, err = runner.ReenableOpenAIAccount(context.Background(), 7, false)
	require.ErrorIs(t, err, errOpenAIReenableNotDead)
	require.Equal(t, 1, store.commits)
	require.Equal(t, winner, *store.state)
}

// 非判死号手动启用:明确拒绝(它们本来就在状态机里自愈)。
func TestReenableOpenAIAccount_RejectsNonDeadStates(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for _, state := range []string{
		OpenAIDowngradeStateOnDuty,
		OpenAIDowngradeStateCircuitOpen,
		OpenAIDowngradeStateReprobe,
	} {
		store := &accountReenableStoreStub{
			downgradeProbeStoreStub: &downgradeProbeStoreStub{},
			allowed:                 true,
		}
		store.state = &OpenAIDowngradeProbeState{AccountID: 7, State: state}
		runner := NewOpenAIDowngradeProbeRunner(store,
			&downgradeProbeAccountRepoStub{account: &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
			}}, nil, nil, nil, nil)
		runner.now = func() time.Time { return now }

		_, err := runner.ReenableOpenAIAccount(context.Background(), 7, false)
		require.Error(t, err, "state=%s 必须拒绝", state)
	}
}

// r17an(2026-09-28 用户裁定「被判死的号也要可以主动检测」):普通判死号
// 点主动检测 → 路径B 同步诊断针——只落证据行,不动状态机/排期/调度;
// 判死语义不变,复活唯一入口仍是 reenable 认证针。
func TestTriggerProbeNow_DeadAccountRunsDiagnostic(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour) // 未来:若误走排期提前路径会破坏该值
	store := &downgradeProbeStoreStub{}
	store.state = &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
		NextProbeAt: future,
	}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: false,
		}}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, HTTPStatus: http.StatusOK,
			AnswerCorrect: false, ReasoningTokens: downgradeProbeIntPtr(500),
		}
	}

	res, err := runner.TriggerProbeNow(context.Background(), 7)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.Accepted)
	require.True(t, res.ProbedNow, "dead account probe must run inline (diagnostic)")
	// 诊断针只落证据:状态机不动(仍判死)、排期不动(不提前)
	require.Equal(t, 1, store.probeCalls)
	require.Equal(t, OpenAIDowngradeStatePendingReplace, store.state.State)
	require.Equal(t, future, store.state.NextProbeAt)
	require.Zero(t, store.saveCalls)
}


// 状态不存在的号:账号不存在哨兵。
func TestReenableOpenAIAccount_NoStateNotFound(t *testing.T) {
	runner := NewOpenAIDowngradeProbeRunner(&accountReenableStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{},
		allowed:                 true,
	},
		&downgradeProbeAccountRepoStub{account: nil}, nil, nil, nil, nil)
	_, err := runner.ReenableOpenAIAccount(context.Background(), 99, false)
	require.ErrorIs(t, err, ErrAccountNotFound)
}

// manual_paused 的判死号 reenable 必须拒绝(2026-09-21 修复#3):静置是用户
// 主动按下的刹车——reenable 拉回 qualification 后 ListDue 的 manual_paused
// 闸会把认证针永远排除(静默失效),且静置中自动打针违反社区救援剧本。
func TestReenableOpenAIAccount_RejectsManualPaused(t *testing.T) {
	now := time.Date(2026, 9, 21, 19, 30, 0, 0, time.UTC)
	store := &accountReenableStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
			AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
			ConsecutiveFailures: 3,
		}},
		commitErr: ErrOpenAIReenablePaused,
	}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: false,
		}}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	_, err := runner.ReenableOpenAIAccount(context.Background(), 7, false)
	require.ErrorIs(t, err, errOpenAIReenablePaused)
	// 状态没被动:仍是判死终态
	require.Equal(t, OpenAIDowngradeStatePendingReplace, store.state.State)
	require.Equal(t, 1, store.commits)
}

// r17an(2026-09-28 用户裁定):显式 unpause 的 reenable 解除刹车继续走认证针
// ——专用解暂停不动 schedulable(避开死号直回流量池的暗雷),解除与启用
// 各落一条审计事件。
func TestReenableOpenAIAccount_UnpauseClearsBrake(t *testing.T) {
	now := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	store := &accountReenableStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
			AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
			ConsecutiveFailures: 3,
		}},
		unpaused: true,
	}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: false,
		}}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	result, err := runner.ReenableOpenAIAccount(context.Background(), 7, true)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Unpaused)
	require.True(t, result.ProbeQueued)
	require.True(t, store.observed.Unpause)
	require.Zero(t, store.eventCalls, "repository owns both audit events")
	// 认证态就位(r17y 一击退出预置不变)
	require.Equal(t, OpenAIDowngradeStateOnDuty, store.state.State)
	require.Equal(t, "qualification", store.state.ProbeMode)
	require.Equal(t, 1, store.state.ConsecutiveFailures)
}

// 解除刹车后其它闸仍挡(status/过期/影子):明确拒绝,绝不静默失效
// (ListDue 同闸会把认证针永远排除)。
func TestReenableOpenAIAccount_UnpauseStillBlocked(t *testing.T) {
	now := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	store := &accountReenableStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
			AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
		}},
		commitErr: ErrOpenAIReenableBlocked,
	}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: false,
		}}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	_, err := runner.ReenableOpenAIAccount(context.Background(), 7, true)
	require.ErrorIs(t, err, errOpenAIReenableBlocked)
	// 刹车已解但状态没动:仍是判死终态
	require.Equal(t, OpenAIDowngradeStatePendingReplace, store.state.State)
}

// r17y 一击退出端到端:reenable 后单针降智证据(答错/低rt)→ ConsecutiveFailures
// 1→2 直达熔断阈值 → qualification_failed 既有分支回判死终态。不进资格循环。
func TestReenableQualificationSingleStrike(t *testing.T) {
	now := time.Date(2026, 9, 21, 13, 0, 0, 0, time.UTC)
	proxyID := int64(5)
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &proxyID,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		// 降智证据:200+答错(答错即降智,无需凑 rt)
		return OpenAIDowngradeProbeResult{
			TransportOK: true, HTTPStatus: http.StatusOK,
			AnswerCorrect: false,
		}
	}

	// reenable 落下的状态(ReenableOpenAIAccount 的输出形态)
	state := &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStateOnDuty,
		ProbeMode:           "qualification",
		CurrentProxyID:      &proxyID,
		ConsecutiveFailures: 1, // r17y 预置
		NextProbeAt:         now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, 1, store.probeCalls)
	// 一击退出:直接回判死终态,不留在资格循环
	require.Equal(t, OpenAIDowngradeStatePendingReplace, state.State)
	require.Equal(t, "normal", state.ProbeMode)
	// 排期到 24h 判死窗口(既有 openAIDowngradeReplacementWindow spread)
	require.True(t, state.NextProbeAt.After(now.Add(23*time.Hour)),
		"next probe %v should defer into replacement window", state.NextProbeAt)
	// 判死分支既有副作用:摘调度 + qualification_failed 事件
	require.False(t, repo.account.Schedulable)
	require.Equal(t, 1, len(store.eventTypes))
	require.Equal(t, OpenAIDowngradeEventReplaceRequired, store.eventTypes[0])
	require.Equal(t, "qualification_failed", store.eventDetails[0]["reason"])
}

// 对称面:reenable 后一针通过 → 计数清零上岗(语义不变,防一击退出误伤)。
func TestReenableQualificationSinglePass(t *testing.T) {
	now := time.Date(2026, 9, 21, 13, 0, 0, 0, time.UTC)
	proxyID := int64(5)
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &proxyID,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, HTTPStatus: http.StatusOK, AnswerCorrect: true,
			ReasoningTokens: downgradeProbeIntPtr(1600),
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStateOnDuty,
		ProbeMode:           "qualification",
		CurrentProxyID:      &proxyID,
		ConsecutiveFailures: 1, // r17y 预置
		NextProbeAt:         now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	// 通过针:上岗+调度恢复+计数清零
	require.Equal(t, OpenAIDowngradeStateOnDuty, state.State)
	require.Equal(t, "normal", state.ProbeMode)
	require.True(t, repo.account.Schedulable)
	require.Zero(t, state.ConsecutiveFailures)
}

// 边界:无结论针(401/传输故障)不烧掉唯一一击——计数不动,留在资格节奏重试。
func TestReenableQualificationInconclusiveKeepsStrike(t *testing.T) {
	now := time.Date(2026, 9, 21, 13, 0, 0, 0, time.UTC)
	proxyID := int64(5)
	state := &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStateOnDuty,
		ProbeMode:           "qualification",
		CurrentProxyID:      &proxyID,
		ConsecutiveFailures: 1,
		NextProbeAt:         now,
	}
	runner := NewOpenAIDowngradeProbeRunner(&downgradeProbeStoreStub{},
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, ProxyID: &proxyID,
		}}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{TransportOK: false} // 传输失败=无结论
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, OpenAIDowngradeStateOnDuty, state.State)
	require.Equal(t, "qualification", state.ProbeMode)
	require.Equal(t, 1, state.ConsecutiveFailures, "inconclusive keeps the strike")
}
