//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// r17x 选项A(2026-09-21 用户裁定):检测通过→正常启用不定期检测;
// 问题号打标签后不再自动检测;手动启用是唯一救援入口。
// 本文件钉「判死即终态 + 手动启用」语义。

// 判死号手动启用:清标签回 qualification、计数归零、排近刻认证针。
func TestReenableOpenAIAccount_FromPendingReplace(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	store.state = &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
		ConsecutiveFailures: 3, ConsecutiveSuccesses: 1, Consecutive429s: 2,
	}

	result, err := runner.ReenableOpenAIAccount(context.Background(), 7)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.ProbeQueued)
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
	require.Equal(t, 1, store.eventCalls)
	require.Equal(t, "manual_reenable", store.eventTypes[0])
	require.Equal(t, OpenAIDowngradeStatePendingReplace, store.eventDetails[0]["from_state"])
}

// 非判死号手动启用:明确拒绝(它们本来就在状态机里自愈)。
func TestReenableOpenAIAccount_RejectsNonDeadStates(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for _, state := range []string{
		OpenAIDowngradeStateOnDuty,
		OpenAIDowngradeStateCircuitOpen,
		OpenAIDowngradeStateReprobe,
	} {
		store := &downgradeProbeStoreStub{}
		store.state = &OpenAIDowngradeProbeState{AccountID: 7, State: state}
		runner := NewOpenAIDowngradeProbeRunner(store,
			&downgradeProbeAccountRepoStub{account: &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
			}}, nil, nil, nil, nil)
		runner.now = func() time.Time { return now }

		_, err := runner.ReenableOpenAIAccount(context.Background(), 7)
		require.Error(t, err, "state=%s 必须拒绝", state)
	}
}

// 判死号点主动检测:409 引导到手动启用,不再静默排期。
func TestTriggerProbeNow_DeadAccountGetsReenableRequired(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	store := &downgradeProbeStoreStub{}
	store.state = &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
		NextProbeAt: now.Add(2 * time.Hour), // 未来:旧语义会提前排期
	}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: false,
		}}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	_, err := runner.TriggerProbeNow(context.Background(), 7)
	require.Error(t, err)
	require.ErrorIs(t, err, errOpenAIReenableRequired)
	// 排期没被提前(静默失效反模式必须杜绝)
	require.Equal(t, now.Add(2*time.Hour), store.state.NextProbeAt)
}

// 状态不存在的号:账号不存在哨兵。
func TestReenableOpenAIAccount_NoStateNotFound(t *testing.T) {
	runner := NewOpenAIDowngradeProbeRunner(&downgradeProbeStoreStub{},
		&downgradeProbeAccountRepoStub{account: nil}, nil, nil, nil, nil)
	_, err := runner.ReenableOpenAIAccount(context.Background(), 99)
	require.ErrorIs(t, err, ErrAccountNotFound)
}

// manual_paused 的判死号 reenable 必须拒绝(2026-09-21 修复#3):静置是用户
// 主动按下的刹车——reenable 拉回 qualification 后 ListDue 的 manual_paused
// 闸会把认证针永远排除(静默失效),且静置中自动打针违反社区救援剧本。
func TestReenableOpenAIAccount_RejectsManualPaused(t *testing.T) {
	now := time.Date(2026, 9, 21, 19, 30, 0, 0, time.UTC)
	store := &manualPausedControlStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{state: &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStatePendingReplace,
		ConsecutiveFailures: 3,
	}}}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: false,
		}}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	_, err := runner.ReenableOpenAIAccount(context.Background(), 7)
	require.ErrorIs(t, err, errOpenAIReenablePaused)
	// 状态没被动:仍是判死终态
	require.Equal(t, OpenAIDowngradeStatePendingReplace, store.downgradeProbeStoreStub.state.State)
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
		ProbeMode: "qualification",
		CurrentProxyID:       &proxyID,
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
		ProbeMode: "qualification",
		CurrentProxyID:       &proxyID,
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
		ProbeMode: "qualification",
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
