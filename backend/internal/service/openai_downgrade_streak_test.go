package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProbe429StormSurvivesRunnerRestart(t *testing.T) {
	for _, track := range []struct {
		state string
		mode  string
	}{
		{OpenAIDowngradeStateOnDuty, "normal"},
		{OpenAIDowngradeStateOnDuty, "qualification"},
		{OpenAIDowngradeStateCircuitOpen, "half_open"},
		{OpenAIDowngradeStateCircuitOpen, "sol_fallback"},
		{OpenAIDowngradeStateReprobe, "accelerated"},
	} {
		t.Run(track.mode, func(t *testing.T) {
			now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
			proxyID := int64(3)
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, ProxyID: &proxyID, UpdatedAt: now,
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			store := &downgradeAtomicStoreStub{
				downgradeProbeStoreStub: &downgradeProbeStoreStub{}, accountRepo: repo,
			}
			deadline := now.Add(24 * time.Hour)
			state := OpenAIDowngradeProbeState{
				AccountID: 7, State: track.state, ProbeMode: track.mode,
				OriginalProxyID: &proxyID, CurrentProxyID: &proxyID,
				RecoveryDeadline: &deadline, NextProbeAt: now, UpdatedAt: now,
			}
			for attempt := 1; attempt <= openAIDowngrade429StreakThreshold+1; attempt++ {
				runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
				runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
					return OpenAIDowngradeProbeResult{
						AccountID: 7, ProxyID: &proxyID, HTTPStatus: http.StatusTooManyRequests,
					}
				}
				require.NoError(t, runner.processStateAtomic(context.Background(), &state, now))
				require.NotNil(t, store.state)
				encoded, err := json.Marshal(store.state)
				require.NoError(t, err)
				state = OpenAIDowngradeProbeState{}
				require.NoError(t, json.Unmarshal(encoded, &state))
				require.Equal(t, min(attempt, openAIDowngrade429StreakThreshold), state.Consecutive429s)
				delay := state.NextProbeAt.Sub(now)
				if attempt < openAIDowngrade429StreakThreshold {
					require.Less(t, delay, openAIDowngrade429StreakBackoff)
				} else {
					require.GreaterOrEqual(t, delay, openAIDowngrade429StreakBackoff,
						"restarting a runner must not erase the storm threshold")
				}
				now = state.NextProbeAt
			}
		})
	}
}

func TestProbe429StreakResetAndSaturation(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	for _, tc := range []struct {
		name    string
		before  int
		result  OpenAIDowngradeProbeResult
		want    int
		handled bool
	}{
		{"success", 6, OpenAIDowngradeProbeResult{HTTPStatus: http.StatusOK}, 0, false},
		{"transport_error", 6, OpenAIDowngradeProbeResult{}, 0, false},
		{"explicit_reset", 6, OpenAIDowngradeProbeResult{
			HTTPStatus: http.StatusTooManyRequests, RateLimitResetAt: &resetAt,
		}, 0, true},
		{"saturated", 6, OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests}, 6, true},
		{"overflow", math.MaxInt, OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests}, 6, true},
		{"negative", -1, OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &downgradeProbeStoreStub{}
			runner := NewOpenAIDowngradeProbeRunner(store, nil, nil, nil, nil, nil)
			state := &OpenAIDowngradeProbeState{AccountID: 7, Consecutive429s: tc.before}
			handled, err := runner.applyRateLimitDeferral(context.Background(), nil, state, tc.result, now)
			require.NoError(t, err)
			require.Equal(t, tc.handled, handled)
			require.Equal(t, tc.want, state.Consecutive429s)
			if tc.handled {
				require.NotNil(t, store.state)
				require.Equal(t, tc.want, store.state.Consecutive429s)
			}
		})
	}
}

func TestProbe429StreakCommitFailureDoesNotPublish(t *testing.T) {
	for _, commitErr := range []error{ErrOpenAIProbeStale, errors.New("outbox unavailable")} {
		t.Run(errorTestName(commitErr), func(t *testing.T) {
			now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
			proxyID := int64(3)
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, ProxyID: &proxyID, UpdatedAt: now,
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			store := &downgradeAtomicStoreStub{
				downgradeProbeStoreStub: &downgradeProbeStoreStub{},
				accountRepo:             repo, commitErr: commitErr,
			}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				return OpenAIDowngradeProbeResult{AccountID: 7, HTTPStatus: http.StatusTooManyRequests}
			}
			state := OpenAIDowngradeProbeState{
				AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
				CurrentProxyID: &proxyID, Consecutive429s: 5, NextProbeAt: now, UpdatedAt: now,
			}
			before := state
			require.ErrorIs(t, runner.processStateAtomic(context.Background(), &state, now), commitErr)
			require.Equal(t, before, state)
			require.Nil(t, store.state)
			require.Equal(t, 6, store.observed.State.Consecutive429s)
			require.Zero(t, repo.snapshotCalls)
		})
	}
}
