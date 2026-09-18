package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIProbeMissingAuthorizationRouteCannotAssignNewProxy(t *testing.T) {
	oldProxy, availableProxy := int64(3), int64(9)
	for _, tc := range []struct {
		name      string
		oldProxy  *int64
		commitErr error
	}{
		{name: "lost_assignment", oldProxy: &oldProxy},
		{name: "commit_failure", oldProxy: &oldProxy, commitErr: errors.New("outbox unavailable")},
		{name: "stale_probe", oldProxy: &oldProxy, commitErr: ErrOpenAIProbeStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, UpdatedAt: now,
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			base := &downgradeProbeStoreStub{mainProxyID: &availableProxy}
			store := &downgradeAtomicStoreStub{
				downgradeProbeStoreStub: base, accountRepo: repo, commitErr: tc.commitErr,
			}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.nextDelay = func() time.Duration { return 30 * time.Minute }
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				t.Fatal("missing browser authorization route must not reach the network")
				return OpenAIDowngradeProbeResult{}
			}
			state := OpenAIDowngradeProbeState{
				AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
				OriginalProxyID: tc.oldProxy, CurrentProxyID: tc.oldProxy,
				ConsecutiveSuccesses: 1, NextProbeAt: now, UpdatedAt: now,
			}
			before := state
			err := runner.processStateAtomic(context.Background(), &state, now)
			require.ErrorIs(t, err, tc.commitErr)
			require.NotNil(t, store.observed)
			require.False(t, store.observed.ProxyChanged)
			require.Empty(t, store.observed.Results)
			require.Len(t, store.observed.Events, 1)
			require.Equal(t, OpenAIDowngradeEventQualificationBlocked, store.observed.Events[0].Type)
			details := map[string]any{}
			require.NoError(t, json.Unmarshal(store.observed.Events[0].Details, &details))
			require.Equal(t, "authorization_proxy_missing", details["reason"])
			require.Empty(t, base.proxyChanges)
			require.Nil(t, account.ProxyID)
			if tc.commitErr != nil {
				require.Equal(t, before, state)
				require.True(t, account.Schedulable)
				require.Nil(t, store.state)
				require.Zero(t, repo.snapshotCalls)
				return
			}
			require.False(t, account.Schedulable)
			require.Equal(t, tc.oldProxy, state.OriginalProxyID)
			require.Equal(t, tc.oldProxy, state.CurrentProxyID)
			require.Zero(t, state.ConsecutiveSuccesses)
			require.Equal(t, now.Add(30*time.Minute), state.NextProbeAt)
			require.Equal(t, 1, repo.snapshotCalls)
		})
	}
}

func TestOpenAIProbeFreshUploadWithoutAuthorizationProxyFailsClosed(t *testing.T) {
	availableProxy := int64(9)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, UpdatedAt: now,
	}
	repo := &downgradeProbeAccountRepoStub{account: account}
	store := &downgradeProbeStoreStub{mainProxyID: &availableProxy}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.nextDelay = func() time.Duration { return time.Minute }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		t.Fatal("a browser OAuth account without its authorization proxy must not probe")
		return OpenAIDowngradeProbeResult{}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
		NextProbeAt: now, UpdatedAt: now,
	}
	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Zero(t, store.mainProxyCalls)
	require.Empty(t, store.proxyChanges)
	require.Zero(t, store.probeCalls)
	require.Equal(t, []bool{false}, repo.schedulableCalls)
	require.False(t, account.Schedulable)
	require.Nil(t, account.ProxyID)
	require.Nil(t, state.OriginalProxyID)
	require.Nil(t, state.CurrentProxyID)
	require.Equal(t, now.Add(time.Minute), state.NextProbeAt)
	require.Equal(t, []string{OpenAIDowngradeEventQualificationBlocked}, store.eventTypes)
	require.Equal(t, []map[string]any{{"reason": "authorization_proxy_missing"}}, store.eventDetails)
}

func TestOpenAIProbeNonBrowserQualificationKeepsProxyAssignment(t *testing.T) {
	for _, mode := range []string{OpenAIAuthModePersonalAccessToken, OpenAIAuthModeAgentIdentity} {
		t.Run(mode, func(t *testing.T) {
			proxyID := int64(3)
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
				Credentials: map[string]any{"auth_mode": mode},
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			store := &downgradeProbeStoreStub{mainProxyID: &proxyID}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.probeFn = func(_ context.Context, account *Account, _ string) OpenAIDowngradeProbeResult {
				require.Equal(t, &proxyID, account.ProxyID)
				return OpenAIDowngradeProbeResult{AccountID: 7}
			}
			state := &OpenAIDowngradeProbeState{
				AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
			}
			require.NoError(t, runner.processState(context.Background(), state, time.Now()))
			require.Equal(t, []*int64{&proxyID}, store.proxyChanges)
			require.Equal(t, 1, store.probeCalls)
		})
	}
}

func TestOpenAIProbeFreshUploadAtomicWithoutAuthorizationProxyFailsClosed(t *testing.T) {
	availableProxy := int64(5)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, UpdatedAt: now.Add(-time.Hour),
	}
	repo := &downgradeProbeAccountRepoStub{account: account}
	base := &downgradeProbeStoreStub{mainProxyID: &availableProxy}
	store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, accountRepo: repo}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.nextDelay = func() time.Duration { return time.Minute }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		t.Fatal("a browser OAuth account without its authorization proxy must not probe")
		return OpenAIDowngradeProbeResult{}
	}
	state := OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
		NextProbeAt: now, UpdatedAt: now.Add(-time.Minute),
	}
	require.NoError(t, runner.processStateAtomic(context.Background(), &state, now))
	require.Equal(t, 1, store.commits)
	require.Zero(t, base.mainProxyCalls)
	require.Zero(t, base.probeCalls)
	require.Empty(t, base.proxyChanges)
	require.NotNil(t, store.observed)
	require.False(t, store.observed.ProxyChanged)
	require.Nil(t, store.observed.ProxyID)
	require.Empty(t, store.observed.Results)
	require.Len(t, store.observed.Events, 1)
	require.Equal(t, OpenAIDowngradeEventQualificationBlocked, store.observed.Events[0].Type)
	require.JSONEq(t, `{"reason":"authorization_proxy_missing"}`, string(store.observed.Events[0].Details))
	require.NotNil(t, store.observed.Schedulable)
	require.False(t, *store.observed.Schedulable)
	require.Nil(t, state.CurrentProxyID)
	require.Nil(t, state.OriginalProxyID)
	require.Equal(t, now.Add(time.Minute), state.NextProbeAt)
	require.False(t, account.Schedulable)
}
