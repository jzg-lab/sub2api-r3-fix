package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProbeCooldownPersistsEveryExplicitDeadline(t *testing.T) {
	now := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	for _, delay := range []time.Duration{time.Hour, 7 * 24 * time.Hour, 90 * 24 * time.Hour} {
		t.Run(delay.String(), func(t *testing.T) {
			reset := now.Add(delay)
			repo := &downgradeProbeAccountRepoStub{}
			runner := NewOpenAIDowngradeProbeRunner(&downgradeProbeStoreStub{}, repo, nil, nil, nil, nil)
			state := &OpenAIDowngradeProbeState{AccountID: 1}
			handled, err := runner.applyRateLimitDeferral(context.Background(), nil, state,
				OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests, RateLimitResetAt: &reset}, now)
			require.True(t, handled)
			require.NoError(t, err)
			require.Equal(t, []time.Time{reset}, repo.rateLimitedResets)
			require.False(t, state.NextProbeAt.Before(reset))
		})
	}
}

func TestProbeCooldownLongerObservedHoldWins(t *testing.T) {
	now := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	existing := now.Add(7 * 24 * time.Hour)
	shorter := now.Add(time.Hour)
	for _, reset := range []*time.Time{nil, &shorter} {
		repo := &downgradeProbeAccountRepoStub{}
		runner := NewOpenAIDowngradeProbeRunner(&downgradeProbeStoreStub{}, repo, nil, nil, nil, nil)
		state := &OpenAIDowngradeProbeState{AccountID: 1, ConsecutiveFailures: 2, ConsecutiveSuccesses: 1}
		handled, err := runner.applyRateLimitDeferral(context.Background(),
			&Account{ID: 1, RateLimitResetAt: &existing}, state,
			OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests, RateLimitResetAt: reset}, now)
		require.True(t, handled)
		require.NoError(t, err)
		require.False(t, state.NextProbeAt.Before(existing), "a missing or shorter deadline must not bypass an existing hold")
		require.Equal(t, 2, state.ConsecutiveFailures)
		require.Equal(t, 1, state.ConsecutiveSuccesses)
	}
}

func TestProbeCooldownPreflightBlocksEveryMode(t *testing.T) {
	now := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	for _, mode := range []string{"normal", "qualification", "sol_fallback", "half_open", "reprobe"} {
		t.Run(mode, func(t *testing.T) {
			store := &downgradeProbeStoreStub{}
			repo := &downgradeProbeAccountRepoStub{account: &Account{
				ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, RateLimitResetAt: &reset,
			}}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				t.Fatal("cooling accounts must not contact the upstream")
				return OpenAIDowngradeProbeResult{}
			}
			state := &OpenAIDowngradeProbeState{
				AccountID: 1, State: OpenAIDowngradeStateOnDuty, ProbeMode: mode,
				NextProbeAt: now, ConsecutiveFailures: 2, ConsecutiveSuccesses: 1,
			}
			if mode == "half_open" {
				state.State = OpenAIDowngradeStateCircuitOpen
			} else if mode == "reprobe" {
				state.State = OpenAIDowngradeStateReprobe
			}
			require.NoError(t, runner.processState(context.Background(), state, now))
			require.False(t, state.NextProbeAt.Before(reset))
			require.Nil(t, state.LastProbeAt, "a deferred probe is not a completed probe")
			require.Zero(t, store.probeCalls)
			require.Zero(t, store.mainProxyCalls)
			require.Empty(t, store.proxyChanges)
			require.Empty(t, repo.fallbackModes)
			require.Equal(t, 2, state.ConsecutiveFailures)
			require.Equal(t, 1, state.ConsecutiveSuccesses)
		})
	}
}
