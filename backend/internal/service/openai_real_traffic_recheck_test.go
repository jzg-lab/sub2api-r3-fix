package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRealTrafficRecheckArmedBypassesTrafficDeferralOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		armed     bool
		wantProbe bool
	}{
		{name: "armed", armed: true, wantProbe: true},
		{name: "not_armed", armed: false, wantProbe: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
			lastProbeAt := now.Add(-10 * time.Minute)
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, UpdatedAt: now.Add(-time.Hour),
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			base := &downgradeProbeStoreStub{}
			seenEvents := make([]string, 0, 3)
			base.eventCountFn = func(
				_ context.Context, accountID int64, eventType string, since time.Time,
			) (int, error) {
				require.Equal(t, int64(7), accountID)
				require.Equal(t, lastProbeAt, since)
				seenEvents = append(seenEvents, eventType)
				if tc.armed && eventType == OpenAIDowngradeEventRealTrafficRecheckArmed {
					return 1, nil
				}
				return 0, nil
			}
			store := &downgradeAtomicStoreStub{
				downgradeProbeStoreStub: base,
				accountRepo:             repo,
			}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.SetRecentTrafficChecker(
				func(context.Context, int64, time.Duration) bool { return true },
			)
			probeRan := false
			runner.probeFn = func(
				context.Context, *Account, string,
			) OpenAIDowngradeProbeResult {
				probeRan = true
				return OpenAIDowngradeProbeResult{
					AccountID: 7, TransportOK: true, AnswerCorrect: true,
					HTTPStatus:      http.StatusOK,
					ReasoningTokens: downgradeProbeIntPtr(1800),
				}
			}
			state := &OpenAIDowngradeProbeState{
				AccountID: 7, State: OpenAIDowngradeStateOnDuty,
				ProbeMode: "normal", LastProbeAt: &lastProbeAt,
				NextProbeAt: now, UpdatedAt: now.Add(-time.Minute),
			}

			require.NoError(t, runner.processStateAtomic(context.Background(), state, now))
			require.Equal(t, tc.wantProbe, probeRan)
			require.Equal(t, []string{
				OpenAIDowngradeEventRealTrafficModelMismatch,
				OpenAIDowngradeEventTurnStateDegraded,
				OpenAIDowngradeEventRealTrafficRecheckArmed,
			}, seenEvents)
			if tc.armed {
				require.Zero(t, runner.deferCounts[state.AccountID])
			} else {
				require.Greater(t, state.NextProbeAt, now)
			}
		})
	}
}
