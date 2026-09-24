//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 自动打票线（2026-09-21 相位B，1136 实验号）回归：
//   1. 采到动态票 → 回原静态桶转 qualification 复检（r17y 一击预置）
//   2. 动态上连 5 针无健康票 → 账号级判定 → 回原桶 pending_replace
//   3. 401 → 采票救不了 → 回原桶 pending_replace（停打闸）
//   4. 手动入口：判死号可进、正常号拒绝、无动态桶 409

// harvestPipelineStoreStub 组合桩：探针 store + 票 store + 动态桶 finder。
type harvestPipelineStoreStub struct {
	*downgradeProbeStoreStub
	dynamicBucketID *int64
	ticket          *OpenAICodexTicket
}

func (s *harvestPipelineStoreStub) FindOpenAIDynamicHarvestBucket(context.Context) (*int64, error) {
	return s.dynamicBucketID, nil
}

func (s *harvestPipelineStoreStub) GetOpenAICodexTicket(context.Context, int64, string) (*OpenAICodexTicket, error) {
	return s.ticket, nil
}

func (s *harvestPipelineStoreStub) UpsertOpenAICodexTicket(context.Context, *OpenAICodexTicket) error {
	return nil
}

func (s *harvestPipelineStoreStub) DeleteExpiredOpenAICodexTickets(context.Context, time.Time) (int64, error) {
	return 0, nil
}

// TestHarvestTicketAcquiredReturnsToStatic 采到动态票：回原静态桶 + qualification
// 复检 + r17y 一击预置（复检失败即回判死，不进资格循环反复打）。
func TestHarvestTicketAcquiredReturnsToStatic(t *testing.T) {
	now := time.Date(2026, 9, 21, 23, 30, 0, 0, time.UTC)
	homeID, dynID := int64(5), int64(11)
	account := &Account{
		ID: 96101, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &dynID,
	}
	store := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96101, State: OpenAIDowngradeStateOnDuty,
			ProbeMode: "harvest", CurrentProxyID: &dynID, OriginalProxyID: &homeID,
			HarvestAttempts: 2, NextProbeAt: now,
		},
	}, dynamicBucketID: &dynID}
	// 票表里有一张动态采的活票（292 社区口径）。
	store.ticket = &OpenAICodexTicket{
		AccountID: 96101, Model: "gpt-6-astra", TicketLen: 292,
		IssuedAt: now.Add(-5 * time.Minute), ExpiresAt: now.Add(55 * time.Minute),
		HarvestedMode: OpenAICodexTicketHarvestDynamic,
	}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	result := OpenAIDowngradeProbeResult{
		TransportOK: true, HTTPStatus: http.StatusOK, AnswerCorrect: false,
		TurnStateLen: 356, // 动态针本身降智长度——但票已入库（上一针采的）
	}
	handled, err := runner.processHarvest(context.Background(), store.downgradeProbeStoreStub.state, &result, now)
	require.NoError(t, err)
	require.True(t, handled)
	st := store.downgradeProbeStoreStub.state
	// 回原静态桶 + qualification 复检
	require.Equal(t, "qualification", st.ProbeMode)
	require.NotNil(t, st.CurrentProxyID)
	require.Equal(t, homeID, *st.CurrentProxyID)
	require.Equal(t, 1, st.ConsecutiveFailures, "r17y one-strike preset for recheck")
	// 复检排近刻（资格节奏）
	require.True(t, st.NextProbeAt.After(now))
	require.True(t, st.NextProbeAt.Before(now.Add(10*time.Minute)))
	// 事件落地
	require.Equal(t, 1, len(store.eventTypes))
	require.Equal(t, OpenAIDowngradeEventHarvestTicketAcquired, store.eventTypes[0])
}

// TestHarvestAccountLevelAbandoned 动态上连续无健康票达上限：账号级判定，
// 回原桶判死（社区：账号级 312 永续=换票无解，别硬打）。
func TestHarvestAccountLevelAbandoned(t *testing.T) {
	now := time.Date(2026, 9, 21, 23, 30, 0, 0, time.UTC)
	homeID, dynID := int64(5), int64(11)
	account := &Account{
		ID: 96102, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &dynID,
	}
	store := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96102, State: OpenAIDowngradeStateOnDuty,
			ProbeMode: "harvest", CurrentProxyID: &dynID, OriginalProxyID: &homeID,
			HarvestAttempts: openAIDowngradeHarvestMaxAttempts - 1, NextProbeAt: now,
		},
	}, dynamicBucketID: &dynID}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	result := OpenAIDowngradeProbeResult{
		TransportOK: true, HTTPStatus: http.StatusOK, AnswerCorrect: false,
		TurnStateLen: 356,
	}
	handled, err := runner.processHarvest(context.Background(), store.downgradeProbeStoreStub.state, &result, now)
	require.NoError(t, err)
	require.True(t, handled)
	st := store.downgradeProbeStoreStub.state
	require.Equal(t, OpenAIDowngradeStatePendingReplace, st.State)
	require.Equal(t, "normal", st.ProbeMode)
	require.NotNil(t, st.CurrentProxyID)
	require.Equal(t, homeID, *st.CurrentProxyID, "must return to original static bucket")
	require.Equal(t, openAIDowngradeHarvestMaxAttempts, st.HarvestAttempts)
	// 判死排远（24h 窗口）
	require.True(t, st.NextProbeAt.After(now.Add(23*time.Hour)))
	require.Equal(t, OpenAIDowngradeEventHarvestAbandoned, store.eventTypes[0])
	require.Equal(t, "account_level_degraded", store.eventDetails[0]["reason"])
}

// TestHarvestCredentialsInvalidStops 401：凭据失效采票救不了，停打回原桶判死。
func TestHarvestCredentialsInvalidStops(t *testing.T) {
	now := time.Date(2026, 9, 21, 23, 30, 0, 0, time.UTC)
	homeID, dynID := int64(5), int64(11)
	account := &Account{
		ID: 96103, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &dynID,
	}
	store := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96103, State: OpenAIDowngradeStateOnDuty,
			ProbeMode: "harvest", CurrentProxyID: &dynID, OriginalProxyID: &homeID,
			HarvestAttempts: 1, NextProbeAt: now,
		},
	}, dynamicBucketID: &dynID}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	result := OpenAIDowngradeProbeResult{TransportOK: true, HTTPStatus: http.StatusUnauthorized}
	handled, err := runner.processHarvest(context.Background(), store.downgradeProbeStoreStub.state, &result, now)
	require.NoError(t, err)
	require.True(t, handled)
	st := store.downgradeProbeStoreStub.state
	require.Equal(t, OpenAIDowngradeStatePendingReplace, st.State)
	require.Equal(t, "credentials_invalid", store.eventDetails[0]["reason"])
}

// TestHarvestContinuesAttempts 未达上限且无票：继续换 IP 再采（attempts++）。
func TestHarvestContinuesAttempts(t *testing.T) {
	now := time.Date(2026, 9, 21, 23, 30, 0, 0, time.UTC)
	dynID := int64(11)
	account := &Account{
		ID: 96104, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &dynID,
	}
	store := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96104, State: OpenAIDowngradeStateOnDuty,
			ProbeMode: "harvest", CurrentProxyID: &dynID,
			HarvestAttempts: 1, NextProbeAt: now,
		},
	}, dynamicBucketID: &dynID}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	result := OpenAIDowngradeProbeResult{
		TransportOK: true, HTTPStatus: http.StatusOK, TurnStateLen: 356,
	}
	handled, err := runner.processHarvest(context.Background(), store.downgradeProbeStoreStub.state, &result, now)
	require.NoError(t, err)
	require.True(t, handled)
	st := store.downgradeProbeStoreStub.state
	require.Equal(t, "harvest", st.ProbeMode, "still harvesting")
	require.Equal(t, 2, st.HarvestAttempts)
	require.True(t, st.NextProbeAt.After(now), "next attempt scheduled")
}

// TestPendingReplaceHarvestReachesProbePipeline locks the defensive dispatch
// ordering: a harvest rescue must not be swallowed by the ordinary
// pending_replace terminal guard when state and mode are observed mid-transition.
func TestPendingReplaceHarvestReachesProbePipeline(t *testing.T) {
	now := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	homeID, dynID := int64(5), int64(11)
	account := &Account{
		ID: 96105, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &dynID,
	}
	store := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96105, State: OpenAIDowngradeStatePendingReplace,
			ProbeMode: "harvest", CurrentProxyID: &dynID, OriginalProxyID: &homeID,
			HarvestAttempts: 1, NextProbeAt: now,
		},
	}, dynamicBucketID: &dynID}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	probeCalls := 0
	runner.probeFn = func(_ context.Context, _ *Account, mode string) OpenAIDowngradeProbeResult {
		probeCalls++
		require.Equal(t, "harvest", mode)
		return OpenAIDowngradeProbeResult{
			AccountID: 96105, TransportOK: true, HTTPStatus: http.StatusOK,
			TurnStateLen: 356,
		}
	}

	require.NoError(t, runner.processState(context.Background(), store.downgradeProbeStoreStub.state, now))
	require.Equal(t, 1, probeCalls, "harvest rescue must reach the real probe dispatch")
	require.Equal(t, 1, store.probeCalls, "harvest probe result must be recorded")
	require.Equal(t, "harvest", store.state.ProbeMode)
	require.Equal(t, 2, store.state.HarvestAttempts, "harvest result must reach its dispatcher")
	require.True(t, store.state.NextProbeAt.After(now))
}

// TestStartHarvestEntryGuards 手动入口闸：判死号进线（迁动态桶+事件）、
// 正常号拒绝、无动态桶 409。
func TestStartHarvestEntryGuards(t *testing.T) {
	now := time.Date(2026, 9, 21, 23, 30, 0, 0, time.UTC)
	dynID := int64(11)

	// 判死号进线
	dead := &Account{
		ID: 96201, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false,
		Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken},
	}
	store := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96201, State: OpenAIDowngradeStatePendingReplace,
			ProbeMode: "normal", ConsecutiveFailures: 3,
		},
	}, dynamicBucketID: &dynID}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: dead}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	state, err := runner.StartHarvestOpenAIAccount(context.Background(), 96201)
	require.NoError(t, err)
	require.Equal(t, "harvest", state.ProbeMode)
	require.NotNil(t, state.CurrentProxyID)
	require.Equal(t, dynID, *state.CurrentProxyID, "must move to dynamic bucket")
	require.Zero(t, state.HarvestAttempts)
	require.Equal(t, OpenAIDowngradeEventHarvestStarted, store.eventTypes[0])

	// 正常号拒绝
	healthy := &Account{
		ID: 96202, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken},
	}
	store2 := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96202, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
		},
	}, dynamicBucketID: &dynID}
	runner2 := NewOpenAIDowngradeProbeRunner(store2,
		&downgradeProbeAccountRepoStub{account: healthy}, nil, nil, nil, nil)
	runner2.now = func() time.Time { return now }
	_, err = runner2.StartHarvestOpenAIAccount(context.Background(), 96202)
	require.ErrorIs(t, err, errOpenAIHarvestNotProblem)

	// 无动态桶 409
	store3 := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96203, State: OpenAIDowngradeStatePendingReplace, ProbeMode: "normal",
		},
	}, dynamicBucketID: nil}
	runner3 := NewOpenAIDowngradeProbeRunner(store3,
		&downgradeProbeAccountRepoStub{account: dead}, nil, nil, nil, nil)
	runner3.now = func() time.Time { return now }
	_, err = runner3.StartHarvestOpenAIAccount(context.Background(), 96203)
	require.ErrorIs(t, err, errOpenAIHarvestUnavailable)

	// 浏览器授权号必须永久保留授权出口，不得进入动态 IP 打票线。
	browser := &Account{
		ID: 96204, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: int64Ptr(5),
		Extra: map[string]any{OpenAIOAuthQualifiedProxyExtraKey: int64(5)},
	}
	store4 := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96204, State: OpenAIDowngradeStatePendingReplace,
			ProbeMode: "normal", CurrentProxyID: int64Ptr(5), OriginalProxyID: int64Ptr(5),
		},
	}, dynamicBucketID: &dynID}
	runner4 := NewOpenAIDowngradeProbeRunner(store4,
		&downgradeProbeAccountRepoStub{account: browser}, nil, nil, nil, nil)
	runner4.now = func() time.Time { return now }
	_, err = runner4.StartHarvestOpenAIAccount(context.Background(), 96204)
	require.ErrorIs(t, err, errOpenAIHarvestRouteProtected)
	require.Equal(t, int64(5), *browser.ProxyID)
	require.Empty(t, store4.proxyChanges)
	require.Empty(t, store4.eventTypes)
}

// TestHarvestLabel 打票中标签：harvest 模式 → 紫色 harvesting。
func TestHarvestLabel(t *testing.T) {
	label, color, clickable, reason := LabelOpenAIAccountHealth(OpenAIProbeHealthSnapshot{
		State: OpenAIDowngradeStateOnDuty, ProbeMode: "harvest",
	})
	require.Equal(t, OpenAIHealthLabelHarvesting, label)
	require.Equal(t, OpenAIHealthColorPurple, color)
	require.False(t, clickable)
	require.Equal(t, "harvest", reason)
}

// 自动救援钩子（2026-09-22 用户裁定「都让自动」，推翻 r17x 判死即终态）回归：
//  1. 判死+静默期满 → 自动进打票线（与手动按钮同路径）
//  2. 静默未满 → 不动（社区反滥用冷却窗）
//  3. 非判死态 → 不碰
//  4. 无动态桶 → 让位 +30min（防每分钟空转重试）
func TestMaybeAutoHarvestDeadEntersAfterSilence(t *testing.T) {
	now := time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)
	dynID := int64(11)
	account := &Account{
		ID: 96301, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false,
		Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken},
	}
	// 判死静默已满：next_probe_at 在过去（abandonHarvest 排的 24h 冷却到期）。
	store := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96301, State: OpenAIDowngradeStatePendingReplace,
			ProbeMode: "normal", ConsecutiveFailures: 3,
			NextProbeAt: now.Add(-time.Minute), UpdatedAt: now.Add(-25 * time.Hour),
		},
	}, dynamicBucketID: &dynID}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	require.NoError(t, runner.maybeAutoHarvestDead(context.Background(), store.downgradeProbeStoreStub.state, now))
	st := store.downgradeProbeStoreStub.state
	require.Equal(t, "harvest", st.ProbeMode, "silence elapsed must auto-enter the harvest line")
	require.NotNil(t, st.CurrentProxyID)
	require.Equal(t, dynID, *st.CurrentProxyID)
	require.Equal(t, OpenAIDowngradeEventHarvestStarted, store.eventTypes[0])
}

func TestMaybeAutoHarvestDeadRespectsSilence(t *testing.T) {
	now := time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)
	store := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96302, State: OpenAIDowngradeStatePendingReplace,
			ProbeMode:   "normal",
			NextProbeAt: now.Add(20 * time.Hour), // 冷却窗仍在
			UpdatedAt:   now.Add(-4 * time.Hour),
		},
	}, dynamicBucketID: int64Ptr(11)}
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 96302, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		}}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	before := *store.downgradeProbeStoreStub.state
	require.NoError(t, runner.maybeAutoHarvestDead(context.Background(), store.downgradeProbeStoreStub.state, now))
	require.Equal(t, before, *store.downgradeProbeStoreStub.state, "silence window must hold")
	require.Empty(t, store.eventTypes)
}

func TestMaybeAutoHarvestDeadIgnoresNonDeadStates(t *testing.T) {
	now := time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)
	for _, mode := range []string{"normal", "harvest"} {
		store := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
			state: &OpenAIDowngradeProbeState{
				AccountID: 96303, State: OpenAIDowngradeStateOnDuty,
				ProbeMode: mode, NextProbeAt: now.Add(-time.Minute),
			},
		}, dynamicBucketID: int64Ptr(11)}
		runner := NewOpenAIDowngradeProbeRunner(store,
			&downgradeProbeAccountRepoStub{account: &Account{
				ID: 96303, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
				Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken},
			}}, nil, nil, nil, nil)
		runner.now = func() time.Time { return now }
		require.NoError(t, runner.maybeAutoHarvestDead(context.Background(), store.downgradeProbeStoreStub.state, now))
		require.Equal(t, mode, store.downgradeProbeStoreStub.state.ProbeMode)
		require.Empty(t, store.eventTypes)
	}
}

func TestMaybeAutoHarvestDeadYieldsWhenBucketMissing(t *testing.T) {
	now := time.Date(2026, 9, 22, 16, 0, 0, 0, time.UTC)
	store := &harvestPipelineStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{
		state: &OpenAIDowngradeProbeState{
			AccountID: 96304, State: OpenAIDowngradeStatePendingReplace,
			ProbeMode:   "normal",
			NextProbeAt: now.Add(-time.Minute), UpdatedAt: now.Add(-25 * time.Hour),
		},
	}, dynamicBucketID: nil} // 无动态桶
	runner := NewOpenAIDowngradeProbeRunner(store,
		&downgradeProbeAccountRepoStub{account: &Account{
			ID: 96304, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
			Credentials: map[string]any{"auth_mode": OpenAIAuthModePersonalAccessToken},
		}}, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }

	require.NoError(t, runner.maybeAutoHarvestDead(context.Background(), store.downgradeProbeStoreStub.state, now))
	st := store.downgradeProbeStoreStub.state
	require.Equal(t, OpenAIDowngradeStatePendingReplace, st.State, "failure must not change verdict")
	require.True(t, st.NextProbeAt.After(now.Add(25*time.Minute)),
		"yield must push ~30min out, got %v", st.NextProbeAt.Sub(now))
	require.True(t, st.NextProbeAt.Before(now.Add(45*time.Minute)))
}
