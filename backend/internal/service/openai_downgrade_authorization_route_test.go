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

func TestOpenAIProbeFreshUploadAutoBindsBucket(t *testing.T) {
	// 用户裁定（2026-09-17）：从未绑过桶的新上传号必须自动分桶（r15b 行为），
	// authorization_proxy_missing 只拦「绑过又丢」的号。判据 = state 里
	// 从未记录过 original 绑定（OriginalProxyID == nil）。
	availableProxy := int64(9)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, UpdatedAt: now,
	}
	repo := &downgradeProbeAccountRepoStub{account: account}
	store := &downgradeProbeStoreStub{mainProxyID: &availableProxy}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.probeFn = func(_ context.Context, acct *Account, _ string) OpenAIDowngradeProbeResult {
		require.Equal(t, &availableProxy, acct.ProxyID, "probe must run on the auto-assigned bucket")
		return OpenAIDowngradeProbeResult{AccountID: 7}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
		NextProbeAt: now, UpdatedAt: now,
	}
	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, []*int64{&availableProxy}, store.proxyChanges, "fresh upload must auto-bind")
	require.Equal(t, 1, store.probeCalls)
	require.Equal(t, &availableProxy, account.ProxyID)
	require.Equal(t, &availableProxy, state.OriginalProxyID)
	require.Equal(t, &availableProxy, state.CurrentProxyID)
	require.True(t, account.Schedulable, "fresh upload must not be force-paused by the lost-route guard")
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

func TestOpenAIProbeFreshUploadAtomicAutoBind(t *testing.T) {
	// 生产 1070/1071 场景（2026-09-17）：原子路径下从未绑桶的新上传号
	// 自动分桶后发认证针——绑桶与探针结果同事务提交。
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
	probed := false
	runner.probeFn = func(_ context.Context, acct *Account, _ string) OpenAIDowngradeProbeResult {
		probed = true
		require.Equal(t, &availableProxy, acct.ProxyID, "probe must run on the auto-assigned bucket")
		return OpenAIDowngradeProbeResult{AccountID: 7}
	}
	state := OpenAIDowngradeProbeState{
		AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
		NextProbeAt: now, UpdatedAt: now.Add(-time.Minute),
	}
	require.NoError(t, runner.processStateAtomic(context.Background(), &state, now))
	require.True(t, probed)
	require.Equal(t, 1, store.commits)
	require.Equal(t, 1, base.mainProxyCalls, "fresh upload must trigger proxy selection")
	require.NotNil(t, store.observed)
	require.True(t, store.observed.ProxyChanged)
	require.Nil(t, store.observed.ExpectedProxyID, "CAS precondition: auto-bind commits only from unbound")
	require.Equal(t, &availableProxy, store.observed.ProxyID)
	require.Equal(t, &availableProxy, store.observed.State.CurrentProxyID)
	require.Len(t, store.observed.Results, 1)
}
