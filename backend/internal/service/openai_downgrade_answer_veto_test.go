package service

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestProbeFailureEventEvidenceClass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		correct bool
		rt, tsl int
		class   string
		hard    bool
	}{
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
