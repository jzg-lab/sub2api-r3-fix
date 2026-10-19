package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestR7AnswerVetoPredicateBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport bool
		status    int
		correct   bool
		rt        *int
		tsl       int
		suspect   bool
		recovered bool
		qualified bool
	}{
		{"1285", true, 200, true, downgradeProbeIntPtr(749), 780, true, false, false},
		{"1202", true, 200, true, downgradeProbeIntPtr(756), 780, true, false, false},
		{"zero status", true, 0, true, downgradeProbeIntPtr(799), 0, true, false, false},
		{"zero reasoning", true, 200, true, downgradeProbeIntPtr(0), 780, true, false, false},
		{"missing reasoning", true, 200, true, nil, 780, false, false, false},
		{"transport failure", false, 200, true, downgradeProbeIntPtr(749), 780, false, false, false},
		{"unauthorized", true, 401, true, downgradeProbeIntPtr(749), 780, false, false, false},
		{"rate limited", true, 429, true, downgradeProbeIntPtr(749), 780, false, false, false},
		{"wrong low", true, 200, false, downgradeProbeIntPtr(749), 780, false, false, false},
		{"356 lower edge", true, 200, true, downgradeProbeIntPtr(749), 336, false, false, false},
		{"356 upper edge", true, 200, true, downgradeProbeIntPtr(749), 376, false, false, false},
		{"outside 356", true, 200, true, downgradeProbeIntPtr(749), 377, true, false, false},
		{"qualification lower edge", true, 200, true, downgradeProbeIntPtr(800), 780, false, false, true},
		{"neutral upper edge", true, 200, true, downgradeProbeIntPtr(1399), 780, false, false, true},
		{"recovery lower edge", true, 200, true, downgradeProbeIntPtr(1400), 780, false, true, true},
		{"fingerprint lower", true, 200, true, downgradeProbeIntPtr(1544), 780, false, false, true},
		{"fingerprint", true, 200, true, downgradeProbeIntPtr(1552), 780, false, false, true},
		{"fingerprint upper", true, 200, true, downgradeProbeIntPtr(1560), 780, false, false, true},
		{"outside fingerprint", true, 200, true, downgradeProbeIntPtr(1561), 780, false, true, true},
		{"356 qualification unchanged", true, 200, true, downgradeProbeIntPtr(1800), 356, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := OpenAIDowngradeProbeResult{
				TransportOK: tc.transport, HTTPStatus: tc.status, AnswerCorrect: tc.correct,
				ReasoningTokens: tc.rt, TurnStateLen: tc.tsl,
			}
			require.Equal(t, tc.suspect, result.IsSuspectLowReasoning())
			require.Equal(t, tc.recovered, result.IsRecovered())
			require.Equal(t, tc.qualified, result.IsQualificationPass())
			if tc.suspect {
				require.True(t, result.IsDegraded())
				require.False(t, result.IsSuspectMiss())
				require.Equal(t, OpenAIProxyOutcomeDegraded, ClassifyOpenAIDowngradeProxyOutcome(&result))
			}
		})
	}
}

func TestR7AnswerVetoTwoStrikesAndRecovery(t *testing.T) {
	now := time.Unix(1791496800, 0)
	low := OpenAIDowngradeProbeResult{
		TransportOK: true, HTTPStatus: 200, AnswerCorrect: true,
		ReasoningTokens: downgradeProbeIntPtr(749), TurnStateLen: 780,
	}
	miss := low
	miss.AnswerCorrect, miss.ReasoningTokens = false, downgradeProbeIntPtr(1800)
	hard := low
	hard.TurnStateLen = 356
	wrong := low
	wrong.AnswerCorrect = false
	clean := low
	clean.ReasoningTokens = downgradeProbeIntPtr(1400)
	for _, stateName := range []string{OpenAIDowngradeStateOnDuty, OpenAIDowngradeStateCircuitOpen, OpenAIDowngradeStateReprobe} {
		t.Run(stateName, func(t *testing.T) {
			state := OpenAIDowngradeProbeState{State: stateName, ProbeMode: "normal", ConsecutiveSuccesses: 1}
			first := ApplyOpenAIDowngradeProbeResult(state, low, now)
			require.False(t, first.Circuit)
			require.False(t, first.NeedsReplacement)
			require.Equal(t, 1, first.State.ConsecutiveFailures)
			require.Zero(t, first.State.ConsecutiveSuccesses)
			for _, next := range []OpenAIDowngradeProbeResult{low, miss, hard, wrong} {
				second := ApplyOpenAIDowngradeProbeResult(first.State, next, now)
				require.Equal(t, stateName == OpenAIDowngradeStateOnDuty, second.Circuit)
				require.Equal(t, stateName != OpenAIDowngradeStateOnDuty, second.NeedsReplacement)
			}
			reset := ApplyOpenAIDowngradeProbeResult(first.State, clean, now)
			require.Zero(t, reset.State.ConsecutiveFailures)
			require.Equal(t, 1, reset.State.ConsecutiveSuccesses)
			require.False(t, reset.Recovered)
			again := ApplyOpenAIDowngradeProbeResult(reset.State, low, now)
			require.False(t, again.Circuit)
			require.False(t, again.NeedsReplacement)
			for _, rt := range []int{800, 1399, 1552} {
				neutral := low
				neutral.ReasoningTokens = downgradeProbeIntPtr(rt)
				next := ApplyOpenAIDowngradeProbeResult(state, neutral, now)
				require.False(t, next.Circuit)
				require.False(t, next.NeedsReplacement)
				require.False(t, next.Recovered)
				if rt == 1552 {
					require.Equal(t, 1, next.State.ConsecutiveSuccesses)
				} else {
					require.Zero(t, next.State.ConsecutiveSuccesses)
				}
			}
		})
	}
	require.True(t, ApplyOpenAIDowngradeProbeResult(OpenAIDowngradeProbeState{}, hard, now).Circuit)
	require.True(t, ApplyOpenAIDowngradeProbeResult(OpenAIDowngradeProbeState{}, wrong, now).Circuit)
}

func TestR7AnswerVetoRunnerRecheckAndEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		correct bool
		rt, tsl int
		class   string
		hard    bool
	}{
		{"low reasoning", true, 749, 780, "suspect_low_reasoning", false},
		{"miss", false, 1800, 780, "suspect_miss", false},
		{"356", true, 749, 356, "hard", true},
		{"wrong", false, 749, 780, "hard", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1791496800, 0)
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
			store := &downgradeProbeStoreStub{}
			repo := &downgradeProbeAccountRepoStub{account: account}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.now = func() time.Time { return now }
			runner.nextDelay = func() time.Duration { return 75 * time.Minute }
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				return OpenAIDowngradeProbeResult{
					TransportOK: true, HTTPStatus: 200, AnswerCorrect: tc.correct,
					ReasoningTokens: downgradeProbeIntPtr(tc.rt), TurnStateLen: tc.tsl,
				}
			}
			state := &OpenAIDowngradeProbeState{AccountID: 1, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal", NextProbeAt: now}
			require.NoError(t, runner.processState(context.Background(), state, now))
			if !tc.hard {
				require.Equal(t, OpenAIDowngradeStateOnDuty, state.State)
				require.True(t, account.Schedulable)
				delay := state.NextProbeAt.Sub(now)
				require.GreaterOrEqual(t, delay, openAIDowngradeSuspectRecheckInterval/2)
				require.LessOrEqual(t, delay, 3*openAIDowngradeSuspectRecheckInterval/2)
				now = state.NextProbeAt
				require.NoError(t, runner.processState(context.Background(), state, now))
			}
			require.Equal(t, OpenAIDowngradeStateCircuitOpen, state.State)
			require.False(t, account.Schedulable)
			require.Equal(t, OpenAIDowngradeEventCircuitOpen, store.eventTypes[len(store.eventTypes)-1])
			details := store.eventDetails[len(store.eventDetails)-1]
			require.Equal(t, "downgrade_probe", details["reason"])
			require.Equal(t, tc.class, details["evidence_class"])
			require.NotContains(t, store.eventTypes, OpenAIDowngradeEventReplaceRequired)
		})
	}
}

func TestR7AnswerVetoReplacementEvidenceAndFallbackParity(t *testing.T) {
	for _, mode := range []string{"qualification", "sol_fallback", "start_sol_fallback"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(1791496800, 0)
			proxyID := int64(10)
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, ProxyID: &proxyID}
			store := &downgradeProbeStoreStub{}
			repo := &downgradeProbeAccountRepoStub{account: account}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.now = func() time.Time { return now }
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				return OpenAIDowngradeProbeResult{
					TransportOK: true, HTTPStatus: 200, AnswerCorrect: true,
					ReasoningTokens: downgradeProbeIntPtr(749), TurnStateLen: 780,
				}
			}
			state := &OpenAIDowngradeProbeState{AccountID: 1, State: OpenAIDowngradeStateOnDuty, ProbeMode: mode, NextProbeAt: now, CurrentProxyID: &proxyID}
			if mode == "start_sol_fallback" {
				require.NoError(t, runner.startSolFallback(context.Background(), account, state, now))
			} else {
				require.NoError(t, runner.processState(context.Background(), state, now))
				if mode == "qualification" {
					require.Equal(t, 1, state.ConsecutiveFailures)
					now = state.NextProbeAt
					require.NoError(t, runner.processState(context.Background(), state, now))
				}
			}
			require.Equal(t, OpenAIDowngradeStatePendingReplace, state.State)
			require.Equal(t, OpenAIDowngradeEventReplaceRequired, store.eventTypes[len(store.eventTypes)-1])
			require.Equal(t, "suspect_low_reasoning", store.eventDetails[len(store.eventDetails)-1]["evidence_class"])
		})
	}
}
