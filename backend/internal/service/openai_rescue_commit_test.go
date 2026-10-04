package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIRescueQualificationCommitsBeforeGraduation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		newAccount bool
		noLane     bool
		passes     int
		hostWrong  bool
		commitErr  error
		resumeMode string
		wantUnlock bool
		wantHeld   bool
	}{
		{name: "new_account_keeps_single_host_pass", newAccount: true, noLane: true, wantUnlock: true},
		{name: "rescue_without_lane"},
		{name: "rescue_without_plugin", noLane: true},
		{name: "single_plugin_pass", passes: 1},
		{name: "host_failed", passes: 2, hostWrong: true},
		{name: "early_host_pass_stays_isolated", passes: 3, wantHeld: true},
		{name: "both_pass", passes: 6, wantUnlock: true},
		{name: "failed_commit_cannot_graduate", passes: 2, commitErr: errors.New("commit unavailable")},
		{name: "post_commit_restart_requalifies", passes: 6, resumeMode: "normal", wantUnlock: true},
		{name: "post_commit_restart_without_plugin_stays_held", resumeMode: "normal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			proxyID := int64(3)
			account := rescueLaneSweepAccount(7)
			account.ProxyID, account.UpdatedAt = &proxyID, now.Add(-time.Hour)
			account.GroupIDs = []int64{99}
			account.Schedulable = false
			if !tc.newAccount {
				rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
					EnteredAt: now.Add(-time.Hour), OrigGroupIDs: []int64{3},
				})
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			store := &downgradeAtomicStoreStub{
				downgradeProbeStoreStub: &downgradeProbeStoreStub{},
				accountRepo:             repo, commitErr: tc.commitErr,
			}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.now = func() time.Time { return now }
			runner.probeFn = func(_ context.Context, _ *Account, mode string) OpenAIDowngradeProbeResult {
				require.Equal(t, "qualification", mode)
				return OpenAIDowngradeProbeResult{
					AccountID: 7, ProxyID: &proxyID, HTTPStatus: 200,
					TransportOK: true, AnswerCorrect: !tc.hostWrong,
					ReasoningTokens: downgradeProbeIntPtr(1400),
				}
			}
			rescueRepo := &rescueLaneRepo{account: account}
			lane := newRescueLaneTestLane(rescueRepo, &rescueLaneSink{})
			lane.now = runner.now
			if tc.passes > 0 {
				lane.SetBridgeSource(func(context.Context) *PluginBridgeStatus {
					return &PluginBridgeStatus{Running: true, Healthy: true,
						StatusJSON: rescueLaneBridgeJSON(rescueLaneBridgeAccountAtJSON(7, false, tc.passes, 0, false, "", now))}
				})
			}
			lane.SetProbeStateSource(func(context.Context, []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
				require.Positive(t, store.commits, "graduation cannot read live evidence before commit")
				require.Nil(t, tc.commitErr, "a failed commit cannot reach graduation")
				require.Equal(t, "normal", store.state.ProbeMode)
				return map[int64]OpenAIProbeHealthSnapshot{7: rescueRecoverySnapshot(7, now)}, nil
			})
			if !tc.noLane {
				runner.SetRescueLane(lane)
			}
			state := OpenAIDowngradeProbeState{
				AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
				OriginalProxyID: &proxyID, CurrentProxyID: &proxyID, UpdatedAt: now.Add(-time.Minute),
			}
			if tc.resumeMode != "" {
				state.ProbeMode = tc.resumeMode
			}
			require.ErrorIs(t, runner.processStateAtomic(context.Background(), &state, now), tc.commitErr)
			require.Equal(t, tc.wantUnlock, account.Schedulable)
			if tc.wantUnlock && !tc.newAccount {
				require.Nil(t, GetOpenAIRescueLaneMarker(account))
				require.Equal(t, [][]int64{{3}}, rescueRepo.binds)
			} else {
				require.Empty(t, rescueRepo.binds)
				require.Empty(t, rescueRepo.extraSets)
				if !tc.newAccount {
					require.NotNil(t, GetOpenAIRescueLaneMarker(account))
				}
			}
			if tc.wantHeld {
				require.Equal(t, OpenAIDowngradeStateOnDuty, state.State)
				require.Equal(t, "normal", state.ProbeMode)
			} else if !tc.wantUnlock && tc.commitErr == nil && !tc.hostWrong {
				require.Equal(t, "qualification", state.ProbeMode)
				require.Zero(t, state.ConsecutiveSuccesses)
			}
		})
	}
}

func TestOpenAIProbeStaleRetryPreservesIdentityAndRescueEpisode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*Account)
		retry bool
	}{
		{name: "neutral_usage", retry: true, edit: func(a *Account) {
			a.Extra["codex_5h_used_percent"] = 17
		}},
		{name: "credentials_changed", edit: func(a *Account) {
			a.Credentials = map[string]any{"model_mapping": map[string]any{"model": "different"}}
		}},
		{name: "rescue_reentered", edit: func(a *Account) {
			a.Extra[openAIRescueLaneExtraKey] = map[string]any{"entered_at": time.Now().UTC().Format(time.RFC3339Nano)}
		}},
		{name: "new_suspicion", edit: func(a *Account) {
			a.Extra[openAIRescueSuspectedExtraKey] = true
		}},
		{name: "qualification_changed", edit: func(a *Account) {
			a.Extra[OpenAIDowngradeQualificationExtraKey] = true
		}},
		{name: "oauth_binding_changed", edit: func(a *Account) {
			a.Extra[OpenAIOAuthQualifiedProxyExtraKey] = int64(99)
		}},
		{name: "groups_changed", edit: func(a *Account) { a.GroupIDs = []int64{99} }},
		{name: "platform_changed", edit: func(a *Account) { a.Platform = PlatformAnthropic }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			account := rescueLaneSweepAccount(7)
			account.UpdatedAt = now.Add(-time.Hour)
			state := &OpenAIDowngradeProbeState{
				AccountID: 7, State: OpenAIDowngradeStateOnDuty,
				ProbeMode: "normal", UpdatedAt: now.Add(-time.Minute),
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			store := &downgradeAtomicStoreStub{
				downgradeProbeStoreStub: &downgradeProbeStoreStub{state: state},
				accountRepo:             repo,
			}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			stage := newOpenAIProbeStaging(runner, account, state)
			stage.mutation.State = state
			tc.edit(account)
			account.UpdatedAt = now
			committed, err, attempted := runner.retryStaleCommit(context.Background(), store, &stage.mutation)
			require.NoError(t, err)
			require.Equal(t, tc.retry, attempted)
			require.Equal(t, tc.retry, committed)
			if !tc.retry {
				require.Zero(t, store.commits)
			}
		})
	}
}

func TestRescueIsolatedAccountRemainsProbeEligible(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "isolated", true: "manual_paused"}[paused], func(t *testing.T) {
			now := time.Now().UTC()
			proxyID := int64(3)
			account := rescueLaneSweepAccount(7)
			account.ProxyID, account.Schedulable = &proxyID, false
			rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{EnteredAt: now.Add(-time.Hour)})
			repo := &downgradeProbeAccountRepoStub{account: account}
			store := &downgradeAtomicStoreStub{
				downgradeProbeStoreStub: &downgradeProbeStoreStub{}, accountRepo: repo, paused: paused,
			}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.now = func() time.Time { return now }
			probes := 0
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				probes++
				return OpenAIDowngradeProbeResult{AccountID: 7, HTTPStatus: 200,
					TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(1400)}
			}
			state := &OpenAIDowngradeProbeState{
				AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
				OriginalProxyID: &proxyID, CurrentProxyID: &proxyID, NextProbeAt: now,
			}
			require.NoError(t, runner.processStateAtomic(t.Context(), state, now))
			require.Equal(t, map[bool]int{false: 1, true: 0}[paused], probes)
			require.False(t, account.Schedulable)
			require.NotNil(t, GetOpenAIRescueLaneMarker(account))
		})
	}
}
