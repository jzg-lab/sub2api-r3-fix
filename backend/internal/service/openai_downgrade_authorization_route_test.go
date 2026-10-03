package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIProbeDirectRouteDoesNotAssignProxy(t *testing.T) {
	for _, mode := range []string{"browser", OpenAIAuthModePersonalAccessToken, OpenAIAuthModeAgentIdentity} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			availableProxy := int64(9)
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, UpdatedAt: now,
				Credentials: map[string]any{"auth_mode": mode},
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			store := &downgradeProbeStoreStub{mainProxyID: &availableProxy}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.nextDelay = func() time.Duration { return time.Minute }
			runner.probeFn = func(_ context.Context, probed *Account, _ string) OpenAIDowngradeProbeResult {
				require.Nil(t, probed.ProxyID)
				return OpenAIDowngradeProbeResult{AccountID: 7}
			}
			oldProxy := int64(3)
			state := &OpenAIDowngradeProbeState{
				AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
				CurrentProxyID: &oldProxy, OriginalProxyID: &oldProxy, NextProbeAt: now,
			}
			require.NoError(t, runner.processState(context.Background(), state, now))
			require.Zero(t, store.mainProxyCalls)
			require.Empty(t, store.proxyChanges)
			require.Equal(t, 1, store.probeCalls)
			require.True(t, account.Schedulable)
			require.Nil(t, account.ProxyID)
			require.Nil(t, state.CurrentProxyID)
			require.Nil(t, state.OriginalProxyID)
		})
	}
}

func TestOpenAIProbeDirectRouteCommitsAtomically(t *testing.T) {
	for _, commitErr := range []error{nil, ErrOpenAIProbeStale, errors.New("outbox unavailable")} {
		t.Run(errorTestName(commitErr), func(t *testing.T) {
			now := time.Now()
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, UpdatedAt: now}
			repo := &downgradeProbeAccountRepoStub{account: account}
			base := &downgradeProbeStoreStub{}
			store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, accountRepo: repo, commitErr: commitErr}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.probeFn = func(_ context.Context, candidate *Account, _ string) OpenAIDowngradeProbeResult {
				require.Nil(t, candidate.ProxyID)
				return OpenAIDowngradeProbeResult{AccountID: 7}
			}
			state := OpenAIDowngradeProbeState{AccountID: 7, State: OpenAIDowngradeStateOnDuty,
				ProbeMode: "normal", NextProbeAt: now, UpdatedAt: now}
			before := state
			err := runner.processStateAtomic(context.Background(), &state, now)
			require.ErrorIs(t, err, commitErr)
			require.Equal(t, 1, store.commits)
			require.False(t, store.observed.ProxyChanged)
			require.Len(t, store.observed.Results, 1)
			require.Nil(t, store.observed.Results[0].ProxyID)
			require.Zero(t, base.mainProxyCalls)
			require.Empty(t, base.proxyChanges)
			require.True(t, account.Schedulable)
			if commitErr != nil {
				require.Equal(t, before, state)
				require.Nil(t, base.state)
			}
		})
	}
}
