package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIProbeAccountUnchanged(t *testing.T) {
	proxyA, proxyB := int64(1), int64(2)
	base := &Account{ID: 1, Status: StatusActive, Schedulable: true, ProxyID: &proxyA}

	require.True(t, openAIProbeAccountUnchanged(base, &Account{ID: 1, Status: StatusActive, Schedulable: true, ProxyID: &proxyA}))
	require.False(t, openAIProbeAccountUnchanged(base, &Account{ProxyID: &proxyB}), "proxy rebinding must invalidate the snapshot")
	require.False(t, openAIProbeAccountUnchanged(base, &Account{Schedulable: false}), "schedulable flip must invalidate the snapshot")
	require.False(t, openAIProbeAccountUnchanged(base, &Account{Status: StatusError}), "status change must invalidate the snapshot")
	require.False(t, openAIProbeAccountUnchanged(base, nil))
	require.True(t, openAIProbeAccountUnchanged(base, &Account{ID: 1, Status: StatusActive, Schedulable: true, ProxyID: &proxyA, Extra: map[string]any{"renamed": true}}),
		"cosmetic Extra edits are deliberately out of scope")
}

func TestOpenAIDowngradeProbeAuthStrikePausesBeforeKill(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{HTTPStatus: http.StatusForbidden, ErrorMessage: "probe upstream returned HTTP 403"}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateOnDuty, NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, 1, state.AuthConsecutiveFailures, "first strike must be counted, not fatal")
	require.Equal(t, []bool{false}, repo.schedulableCalls, "first strike pauses scheduling")
	require.Empty(t, repo.errorMessages, "a single 403 must not kill the account")
	require.Equal(t, []string{OpenAIDowngradeEventProbeAuthStrike}, store.eventTypes)
	require.Equal(t, now.Add(openAIDowngradeAuthRetryInterval), state.NextProbeAt)
	require.Equal(t, OpenAIDowngradeStateOnDuty, state.State, "auth strikes are not degradation evidence")

	next := now.Add(openAIDowngradeAuthRetryInterval)
	require.NoError(t, runner.processState(context.Background(), state, next))
	require.Equal(t, []string{"OpenAI probe authentication failed"}, repo.errorMessages, "second consecutive strike graduates to error")
	require.Equal(t, []string{OpenAIDowngradeEventProbeAuthStrike, OpenAIDowngradeEventProbeAuthTerminal}, store.eventTypes)
	require.Zero(t, state.AuthConsecutiveFailures, "terminal graduation resets the strike counter")
	require.Equal(t, next.Add(openAIDowngradeReplacementWindow), state.NextProbeAt)
}

func TestOpenAIDowngradeProbeAuthLimboStillProbesAndClears(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: true,
			ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateOnDuty,
		AuthConsecutiveFailures: 1, NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, 1, store.probeCalls, "auth limbo must keep probing a paused account")
	require.Equal(t, []bool{true}, repo.schedulableCalls, "an accepted credential clears the strike pause")
	require.Zero(t, state.AuthConsecutiveFailures)
	require.Contains(t, store.eventTypes, OpenAIDowngradeEventProbeAuthCleared)
	require.True(t, account.Schedulable)
}

func TestOpenAIDowngradeProbeStaleAccountSkipsMutations(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	proxyA, proxyB := int64(1), int64(2)
	before := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, ProxyID: &proxyA,
	}
	after := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, ProxyID: &proxyB,
	}
	store := &downgradeProbeStoreStub{}
	getSeq := []*Account{before, after}
	repo := &downgradeProbeAccountRepoStub{account: before, getByIDFn: func(context.Context, int64) (*Account, error) {
		next := getSeq[0]
		getSeq = getSeq[1:]
		return next, nil
	}}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: false,
			ReasoningTokens: downgradeProbeIntPtr(516),
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateOnDuty, NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, 1, store.probeCalls, "telemetry is recorded even for a stale result")
	require.Empty(t, repo.schedulableCalls, "a result attributed to a swapped binding must not open circuits")
	require.Empty(t, repo.errorMessages)
	require.Zero(t, state.ConsecutiveFailures, "counters must not advance on a stale snapshot")
	require.Equal(t, OpenAIDowngradeStateOnDuty, state.State)
	require.Equal(t, []string{OpenAIDowngradeEventProbeSkippedStale}, store.eventTypes)
	require.NotNil(t, state.LastProbeAt)
}
