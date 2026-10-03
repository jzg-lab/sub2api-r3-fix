package service

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type downgradeAtomicStoreStub struct {
	*downgradeProbeStoreStub
	accountRepo *downgradeProbeAccountRepoStub
	commitErr   error
	// commitErrs 按次消费(优先于 commitErr),供"首次失配、重试成功"类场景;
	// onCommitFail 在每次提交失败后回调,用于模拟中立键写入者推走账号行代际。
	commitErrs   []error
	onCommitFail func(commits int)
	commits      int
	observed     *OpenAIDowngradeMutation
	events       []OpenAIDowngradeMutationEvent
	afterCommit  func()
	committedAt  time.Time
	paused       bool
	controlErr   error
}

func (s *downgradeAtomicStoreStub) CanRunOpenAIDowngradeProbe(context.Context, int64) (bool, error) {
	return !s.paused, s.controlErr
}

func (s *downgradeAtomicStoreStub) CommitOpenAIDowngradeMutation(_ context.Context, mutation *OpenAIDowngradeMutation) error {
	s.commits++
	s.observed = mutation
	err := s.commitErr
	if len(s.commitErrs) > 0 {
		err = s.commitErrs[0]
		s.commitErrs = s.commitErrs[1:]
	}
	if err != nil {
		if s.onCommitFail != nil {
			s.onCommitFail(s.commits)
		}
		return err
	}
	if !s.committedAt.IsZero() {
		mutation.State.UpdatedAt = s.committedAt
	}
	state := *mutation.State
	s.state = &state
	s.events = append(s.events, mutation.Events...)
	if mutation.Schedulable != nil && s.accountRepo != nil {
		s.accountRepo.account.Schedulable = *mutation.Schedulable
		s.accountRepo.schedulableCalls = append(s.accountRepo.schedulableCalls, *mutation.Schedulable)
	}
	if s.afterCommit != nil {
		s.afterCommit()
	}
	return nil
}

func (s *downgradeAtomicStoreStub) CountOpenAIDowngradeEvents(
	ctx context.Context,
	id int64,
	eventType string,
	since time.Time,
) (int, error) {
	if s.eventCountFn != nil {
		return s.eventCountFn(ctx, id, eventType, since)
	}
	count := 0
	for _, event := range s.events {
		if event.Type == eventType &&
			(event.ObservedAt == nil || !event.ObservedAt.Before(since)) {
			count++
		}
	}
	return count, nil
}

func TestOpenAIProbeAtomicCommitPublishesOnlyAfterSuccess(t *testing.T) {
	for _, commitErr := range []error{nil, ErrOpenAIProbeStale, errors.New("outbox unavailable")} {
		t.Run(errorTestName(commitErr), func(t *testing.T) {
			now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
			proxyID := int64(3)
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, ProxyID: &proxyID, UpdatedAt: now.Add(-time.Hour),
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			base := &downgradeProbeStoreStub{}
			store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, accountRepo: repo, commitErr: commitErr}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.deferCounts[8] = 3
			beforeDeferrals := maps.Clone(runner.deferCounts)
			state := OpenAIDowngradeProbeState{
				AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
				OriginalProxyID: &proxyID, CurrentProxyID: &proxyID, UpdatedAt: now.Add(-time.Minute),
				Consecutive429s: 2,
			}
			before := state
			runner.probeFn = func(_ context.Context, candidate *Account, _ string) OpenAIDowngradeProbeResult {
				require.NotSame(t, account, candidate)
				require.False(t, account.Schedulable)
				require.Zero(t, store.commits)
				return OpenAIDowngradeProbeResult{
					AccountID: 7, ProxyID: &proxyID, HTTPStatus: http.StatusOK,
					TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(1500),
				}
			}
			err := runner.processStateAtomic(context.Background(), &state, now)
			require.ErrorIs(t, err, commitErr)
			require.Equal(t, 1, store.commits)
			require.Zero(t, base.saveCalls, "planning must not call the live writer")
			require.Zero(t, base.probeCalls)
			require.Zero(t, base.eventCalls)
			require.Empty(t, base.proxyChanges)
			require.Equal(t, before.UpdatedAt, store.observed.ExpectedStateUpdatedAt)
			require.Equal(t, account.UpdatedAt, store.observed.ExpectedAccountUpdatedAt)
			require.Len(t, store.observed.Results, 1)
			if commitErr != nil {
				require.Equal(t, before, state)
				require.False(t, account.Schedulable)
				require.Equal(t, beforeDeferrals, runner.deferCounts)
				require.Zero(t, repo.snapshotCalls)
			} else {
				require.Equal(t, "normal", state.ProbeMode)
				require.True(t, account.Schedulable)
				require.Zero(t, state.Consecutive429s)
				require.Equal(t, 1, repo.snapshotCalls)
			}
		})
	}
}

func TestOpenAIProbeQualificationUsesDirectRoute(t *testing.T) {
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	oldProxyID, availableProxyID := int64(3), int64(5)
	for _, tc := range []struct {
		name        string
		schedulable bool
		previous    *int64
		commitErr   error
	}{
		{name: "lost_binding", schedulable: true, previous: &oldProxyID},
		{name: "already_paused", previous: &oldProxyID},
		{name: "stale_generation", schedulable: true, previous: &oldProxyID, commitErr: ErrOpenAIProbeStale},
		{name: "commit_failure", schedulable: true, previous: &oldProxyID, commitErr: errors.New("outbox unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: tc.schedulable, UpdatedAt: now.Add(-time.Hour),
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			base := &downgradeProbeStoreStub{mainProxyID: &availableProxyID}
			store := &downgradeAtomicStoreStub{
				downgradeProbeStoreStub: base, accountRepo: repo, commitErr: tc.commitErr,
			}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.nextDelay = func() time.Duration { return time.Minute }
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				return OpenAIDowngradeProbeResult{AccountID: account.ID}
			}
			state := OpenAIDowngradeProbeState{
				AccountID: account.ID, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
				CurrentProxyID: tc.previous, OriginalProxyID: tc.previous,
				ConsecutiveFailures: 1, ConsecutiveSuccesses: 1,
				NextProbeAt: now, UpdatedAt: now.Add(-time.Minute),
			}
			before := state

			err := runner.processStateAtomic(context.Background(), &state, now)
			require.ErrorIs(t, err, tc.commitErr)
			require.Equal(t, 1, store.commits)
			require.Zero(t, base.mainProxyCalls, "direct traffic does not require proxy selection")
			require.Empty(t, base.proxyChanges)
			require.Equal(t, 1, base.probeCalls)
			require.Zero(t, base.saveCalls)
			require.Zero(t, base.eventCalls, "events must use the same atomic commit")
			require.Nil(t, account.ProxyID)
			require.NotNil(t, store.observed)
			require.False(t, store.observed.ProxyChanged)
			require.Nil(t, store.observed.ExpectedProxyID)
			require.Equal(t, before.UpdatedAt, store.observed.ExpectedStateUpdatedAt)
			require.Equal(t, account.UpdatedAt, store.observed.ExpectedAccountUpdatedAt)
			require.Len(t, store.observed.Results, 1)
			require.Empty(t, store.observed.Events)
			require.Nil(t, store.observed.State.CurrentProxyID)
			require.Nil(t, store.observed.State.OriginalProxyID)
			require.Nil(t, store.observed.Schedulable)
			if tc.commitErr != nil {
				require.Equal(t, before, state)
				require.Equal(t, tc.schedulable, account.Schedulable)
				require.Empty(t, repo.schedulableCalls)
				require.Zero(t, repo.snapshotCalls)
				require.Nil(t, base.state)
			} else {
				require.Equal(t, tc.schedulable, account.Schedulable)
				require.Nil(t, state.CurrentProxyID)
				require.Nil(t, state.OriginalProxyID)
				require.Equal(t, "qualification", state.ProbeMode)
				require.True(t, state.NextProbeAt.After(now))
				require.Zero(t, state.ConsecutiveFailures)
				require.Zero(t, state.ConsecutiveSuccesses)
				require.Zero(t, repo.snapshotCalls)
			}
		})
	}
}

func TestOpenAIProbeQualificationNonBrowserAssignmentPreserved(t *testing.T) {
	for _, mode := range []string{OpenAIAuthModePersonalAccessToken, OpenAIAuthModeAgentIdentity} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			proxyID := int64(3)
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
				ProxyID:     &proxyID,
				Credentials: map[string]any{openAIAuthModeCredentialKey: mode},
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			base := &downgradeProbeStoreStub{mainProxyID: &proxyID}
			store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, accountRepo: repo}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			calls := 0
			runner.probeFn = func(_ context.Context, candidate *Account, _ string) OpenAIDowngradeProbeResult {
				calls++
				require.Equal(t, &proxyID, candidate.ProxyID)
				return OpenAIDowngradeProbeResult{
					AccountID: account.ID, ProxyID: &proxyID, HTTPStatus: http.StatusOK,
					TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(1500),
				}
			}
			state := OpenAIDowngradeProbeState{
				AccountID: account.ID, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
			}

			require.NoError(t, runner.processStateAtomic(context.Background(), &state, now))
			require.Zero(t, base.mainProxyCalls)
			require.Equal(t, 1, calls)
			require.Equal(t, 1, store.commits)
			require.True(t, store.observed.ProxyChanged)
			require.Equal(t, &proxyID, store.observed.ProxyID)
			require.Equal(t, &proxyID, state.CurrentProxyID)
			require.Equal(t, "normal", state.ProbeMode)
			require.True(t, account.Schedulable)
		})
	}
}

func TestOpenAIProbeSnapshotFailurePreservesCommittedGeneration(t *testing.T) {
	now := time.Now()
	account := &Account{ID: 1, Schedulable: true, Status: StatusActive, UpdatedAt: now}
	repo := &downgradeProbeAccountRepoStub{account: account, snapshotErr: errors.New("cache unavailable")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	committedAt := now.Add(time.Microsecond)
	store := &downgradeAtomicStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{}, accountRepo: repo,
		afterCommit: cancel, committedAt: committedAt,
	}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	state := &OpenAIDowngradeProbeState{AccountID: 1, State: OpenAIDowngradeStateOnDuty, UpdatedAt: now}
	err := runner.armQualificationAtomic(ctx, account, state, now)
	require.ErrorIs(t, err, ErrOpenAIProbeSnapshotRefresh)
	require.Equal(t, 1, store.commits)
	require.Equal(t, 1, repo.snapshotCalls)
	require.Equal(t, "qualification", state.ProbeMode)
	require.Equal(t, committedAt, state.UpdatedAt)
	require.False(t, account.Schedulable)
}

func TestOpenAIProbeSnapshotCapabilityRequiredBeforeCommit(t *testing.T) {
	store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{}}
	runner := NewOpenAIDowngradeProbeRunner(store, nil, nil, nil, nil, nil)
	account := &Account{ID: 1}
	state := &OpenAIDowngradeProbeState{AccountID: 1}
	require.ErrorIs(t, runner.armQualificationAtomic(context.Background(), account, state, time.Now()), ErrOpenAIProbeSnapshotStore)
	require.Zero(t, store.commits)
	require.Empty(t, state.ProbeMode)
}

func errorTestName(err error) string {
	if err == nil {
		return "success"
	}
	return err.Error()
}

func TestOpenAIProbeManualPauseBlocksEveryRecoveryTrack(t *testing.T) {
	for _, mode := range []string{"qualification", "normal", "sol_fallback"} {
		for _, stateName := range []string{
			OpenAIDowngradeStateOnDuty, OpenAIDowngradeStateCircuitOpen,
			OpenAIDowngradeStateReprobe, OpenAIDowngradeStatePendingReplace,
		} {
			t.Run(mode+"/"+stateName, func(t *testing.T) {
				account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive}
				base := &downgradeProbeStoreStub{}
				store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, paused: true}
				repo := &downgradeProbeAccountRepoStub{account: account}
				runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
				runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
					t.Fatal("manual pause must be checked before network work")
					return OpenAIDowngradeProbeResult{}
				}
				state := &OpenAIDowngradeProbeState{AccountID: 7, State: stateName, ProbeMode: mode}
				before := *state
				require.NoError(t, runner.processStateAtomic(context.Background(), state, time.Now()))
				require.NoError(t, runner.armQualificationAtomic(context.Background(), account, state, time.Now()))
				require.Equal(t, before, *state)
				require.Zero(t, store.commits)
				require.Zero(t, base.saveCalls)
				require.Zero(t, repo.snapshotCalls)
			})
		}
	}
}

func TestOpenAIProbeControlReadFailureStopsBeforeMutation(t *testing.T) {
	injected := errors.New("control read unavailable")
	store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{}, controlErr: injected}
	runner := NewOpenAIDowngradeProbeRunner(store, nil, nil, nil, nil, nil)
	state := &OpenAIDowngradeProbeState{AccountID: 7}
	require.ErrorIs(t, runner.processStateAtomic(context.Background(), state, time.Now()), injected)
	require.ErrorIs(t, runner.armQualificationAtomic(context.Background(), &Account{ID: 7}, state, time.Now()), injected)
	require.Zero(t, store.commits)
}

func TestOpenAIProbeOwnedErrorOnDutyRequalifies(t *testing.T) {
	for _, mode := range []string{"normal", "qualification", "sol_fallback"} {
		t.Run(mode, func(t *testing.T) {
			proxyID := int64(3)
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusError, ProxyID: &proxyID}
			repo := &downgradeProbeAccountRepoStub{account: account}
			store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{}, accountRepo: repo}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.probeFn = func(_ context.Context, a *Account, probeMode string) OpenAIDowngradeProbeResult {
				require.Equal(t, proxyID, *a.ProxyID)
				if mode == "sol_fallback" {
					require.Equal(t, mode, probeMode)
				} else {
					require.Equal(t, "qualification", probeMode)
				}
				return OpenAIDowngradeProbeResult{
					AccountID: 7, ProxyID: &proxyID, HTTPStatus: http.StatusOK,
					TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(1500),
				}
			}
			state := &OpenAIDowngradeProbeState{AccountID: 7, State: OpenAIDowngradeStateOnDuty,
				ProbeMode: mode, OriginalProxyID: &proxyID, CurrentProxyID: &proxyID}
			require.NoError(t, runner.processStateAtomic(context.Background(), state, time.Now()))
			require.Equal(t, 1, store.commits)
			require.True(t, store.observed.RecoverOwnedError)
			require.NotNil(t, store.observed.Schedulable)
			require.True(t, *store.observed.Schedulable)
			require.False(t, store.observed.ProxyChanged)
		})
	}
}

func TestOpenAIProbeAtomicStoreRequiredBeforeNetwork(t *testing.T) {
	base := &downgradeProbeStoreStub{}
	runner := NewOpenAIDowngradeProbeRunner(base, nil, nil, nil, nil, nil)
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		t.Fatal("must not probe without atomic persistence")
		return OpenAIDowngradeProbeResult{}
	}
	err := runner.processStateAtomic(context.Background(), &OpenAIDowngradeProbeState{AccountID: 1}, time.Now())
	require.ErrorIs(t, err, ErrOpenAIProbeAtomicStore)
	require.Zero(t, base.saveCalls)
}

func TestOpenAIProbeStagingRejectsCrossAccountAndBadEvent(t *testing.T) {
	stage := &openAIProbeStaging{mutation: OpenAIDowngradeMutation{AccountID: 1}}
	ctx := context.Background()
	require.ErrorIs(t, stage.SetSchedulable(ctx, 2, true), ErrOpenAIProbeStale)
	require.ErrorIs(t, stage.SetError(ctx, 2, "fixture"), ErrOpenAIProbeStale)
	require.ErrorIs(t, stage.SetOpenAIAccountProxy(ctx, 2, nil), ErrOpenAIProbeStale)
	require.ErrorIs(t, stage.SetOpenAIDowngradeFallbackMode(ctx, 2, true), ErrOpenAIProbeStale)
	require.ErrorIs(t, stage.SetRateLimitedIfLater(ctx, 2, time.Now()), ErrOpenAIProbeStale)
	require.ErrorIs(t, stage.SaveOpenAIDowngradeState(ctx, &OpenAIDowngradeProbeState{AccountID: 2}), ErrOpenAIProbeStale)
	require.ErrorIs(t, stage.RecordOpenAIDowngradeProbe(ctx, &OpenAIDowngradeProbeResult{AccountID: 2}), ErrOpenAIProbeStale)
	require.ErrorIs(t, stage.AppendOpenAIDowngradeEvent(ctx, 2, nil, "fixture", nil), ErrOpenAIProbeStale)
	require.Error(t, stage.AppendOpenAIDowngradeEvent(ctx, 1, nil, "fixture", map[string]any{"invalid": make(chan int)}))
	require.Empty(t, stage.mutation.Events)
	require.Nil(t, stage.mutation.Schedulable)
}

func TestOpenAIProbeStagingRequalifiesRestoredBrowserOAuthBinding(t *testing.T) {
	proxyID := int64(3)
	wrongProxyID := int64(4)
	passingResult := OpenAIDowngradeProbeResult{
		AccountID: 1, ProxyID: &proxyID, HTTPStatus: http.StatusOK,
		TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(900),
	}
	for _, test := range []struct {
		name    string
		results []OpenAIDowngradeProbeResult
		wantErr error
	}{
		{name: "matching_fresh_pass", results: []OpenAIDowngradeProbeResult{passingResult}},
		{name: "missing_fresh_pass", wantErr: ErrOpenAIOAuthQualificationRequired},
		{
			name: "pass_from_different_proxy",
			results: []OpenAIDowngradeProbeResult{func() OpenAIDowngradeProbeResult {
				result := passingResult
				result.ProxyID = &wrongProxyID
				return result
			}()},
			wantErr: ErrOpenAIOAuthQualificationRequired,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := &Account{
				ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, ProxyID: &proxyID,
				Extra: map[string]any{
					OpenAIOAuthQualifiedProxyExtraKey:    proxyID,
					OpenAIDowngradeQualificationExtraKey: true,
				},
			}
			stage := &openAIProbeStaging{
				account: account,
				mutation: OpenAIDowngradeMutation{
					AccountID: 1,
					Results:   test.results,
				},
			}
			err := stage.SetSchedulable(context.Background(), 1, true)
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
				require.False(t, stage.mutation.CompleteQualification)
				require.Nil(t, stage.mutation.Schedulable)
				return
			}
			require.NoError(t, err)
			require.True(t, stage.mutation.CompleteQualification)
			require.NotNil(t, stage.mutation.Schedulable)
			require.True(t, *stage.mutation.Schedulable)
		})
	}
}

func TestOpenAIProbeStagingKeepsLongestCooldown(t *testing.T) {
	stage := &openAIProbeStaging{mutation: OpenAIDowngradeMutation{AccountID: 1}}
	now := time.Now()
	for _, reset := range []time.Time{now.Add(time.Hour), now, now.Add(2 * time.Hour), now} {
		require.NoError(t, stage.SetRateLimitedIfLater(context.Background(), 1, reset))
	}
	require.Equal(t, now.Add(2*time.Hour), *stage.mutation.RateLimitResetAt)
}

func TestOpenAIProbeQualificationCommitFailurePreservesInputs(t *testing.T) {
	now := time.Now()
	account := &Account{ID: 1, Schedulable: true, Status: StatusActive, UpdatedAt: now}
	state := &OpenAIDowngradeProbeState{AccountID: 1, State: OpenAIDowngradeStateOnDuty, UpdatedAt: now}
	before := *state
	base := &downgradeProbeStoreStub{}
	store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, commitErr: ErrOpenAIProbeStale}
	runner := NewOpenAIDowngradeProbeRunner(store, &downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
	require.ErrorIs(t, runner.armQualificationAtomic(context.Background(), account, state, now), ErrOpenAIProbeStale)
	require.Equal(t, before, *state)
	require.True(t, account.Schedulable)
	require.Zero(t, base.saveCalls)
}

func TestOpenAIProbeDefaultTransportIsNotBoundToLiveWriter(t *testing.T) {
	runner := NewOpenAIDowngradeProbeRunner(nil, nil, nil, nil, nil, nil)
	require.Nil(t, runner.probeFn, "default transport must resolve on the staged receiver")
	stage := newOpenAIProbeStaging(runner, &Account{ID: 1}, &OpenAIDowngradeProbeState{AccountID: 1})
	staged := runner.stagedRunner(stage)
	require.Nil(t, staged.probeFn)
	require.Same(t, stage, staged.accountRepo)
	require.Same(t, stage, staged.store)
}

func TestOpenAIProbeAuthenticationFailureIsStaged(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			now := time.Now()
			proxyID := int64(3)
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, ProxyID: &proxyID, UpdatedAt: now,
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			base := &downgradeProbeStoreStub{}
			store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, commitErr: ErrOpenAIProbeStale}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			state := &OpenAIDowngradeProbeState{
				AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
				CurrentProxyID: &proxyID, OriginalProxyID: &proxyID, UpdatedAt: now,
			}
			before := *state
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				return OpenAIDowngradeProbeResult{AccountID: 7, ProxyID: &proxyID, HTTPStatus: status}
			}
			require.ErrorIs(t, runner.processStateAtomic(context.Background(), state, now), ErrOpenAIProbeStale)
			require.Equal(t, before, *state)
			require.Equal(t, StatusActive, account.Status)
			require.True(t, account.Schedulable)
			require.Empty(t, repo.schedulableCalls)
			require.Zero(t, base.probeCalls)
			require.Equal(t, "OpenAI probe authentication failed", *store.observed.ErrorMessage)
			require.NotNil(t, store.observed.Schedulable)
			require.False(t, *store.observed.Schedulable)
		})
	}
}

// TestOpenAIProbeAtomicCommitRetriesOnceAfterNeutralGenerationBump 回归
// r17c:codex 用量轮询等中立键写入者会在探针网络往返期间推走账号行 updated_at,
// 首次提交以 ErrOpenAIProbeStale 失配后,应以新鲜代际复用已完成的探测结果
// 重提一次且不再重探;状态行被并发推进或账号行实质字段变化时必须让位。
func TestOpenAIProbeAtomicCommitRetriesOnceAfterNeutralGenerationBump(t *testing.T) {
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	proxyID := int64(3)

	newFixture := func() (*OpenAIDowngradeProbeRunner, *downgradeAtomicStoreStub, *downgradeProbeAccountRepoStub, *OpenAIDowngradeProbeState, *int) {
		account := &Account{
			ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, ProxyID: &proxyID, UpdatedAt: now.Add(-time.Hour),
		}
		repo := &downgradeProbeAccountRepoStub{account: account}
		state := &OpenAIDowngradeProbeState{
			AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
			OriginalProxyID: &proxyID, CurrentProxyID: &proxyID, UpdatedAt: now.Add(-time.Minute),
			Consecutive429s: 2,
		}
		// 预置状态行:与探针运行前提一致(UpdatedAt == ExpectedStateUpdatedAt)。
		rowState := *state
		base := &downgradeProbeStoreStub{state: &rowState}
		store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, accountRepo: repo}
		probeRuns := new(int)
		runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
		runner.probeFn = func(_ context.Context, _ *Account, _ string) OpenAIDowngradeProbeResult {
			*probeRuns++
			return OpenAIDowngradeProbeResult{
				AccountID: 7, ProxyID: &proxyID, HTTPStatus: http.StatusOK,
				TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(1500),
			}
		}
		return runner, store, repo, state, probeRuns
	}

	t.Run("neutral_bump_retries_with_fresh_generation", func(t *testing.T) {
		runner, store, repo, state, probeRuns := newFixture()
		store.commitErrs = []error{ErrOpenAIProbeStale, nil}
		neutralBump := now.Add(-time.Minute) // 中立键写入者推走账号行代际
		store.onCommitFail = func(int) { repo.account.UpdatedAt = neutralBump }

		err := runner.processStateAtomic(context.Background(), state, now)
		require.NoError(t, err)
		require.Equal(t, 2, store.commits, "stale commit must be retried exactly once")
		require.Equal(t, 1, *probeRuns, "retry must reuse the completed probe result")
		require.Equal(t, neutralBump, store.observed.ExpectedAccountUpdatedAt)
		require.Equal(t, "normal", state.ProbeMode)
		require.True(t, repo.account.Schedulable)
		require.Zero(t, state.Consecutive429s)
		require.Len(t, store.observed.Results, 1)
		require.Equal(t, 1, repo.snapshotCalls)
	})

	t.Run("concurrent_state_advance_declines_retry", func(t *testing.T) {
		runner, store, repo, state, probeRuns := newFixture()
		store.commitErrs = []error{ErrOpenAIProbeStale, nil}
		store.onCommitFail = func(int) {
			repo.account.UpdatedAt = now.Add(-time.Minute)
			// 竞争者已推进状态行计数器 → 本轮回让位。
			row := *state
			row.UpdatedAt = now.Add(-30 * time.Second)
			row.ConsecutiveSuccesses = 3
			store.downgradeProbeStoreStub.state = &row
		}

		err := runner.processStateAtomic(context.Background(), state, now)
		require.ErrorIs(t, err, ErrOpenAIProbeStale)
		require.Equal(t, 1, store.commits)
		require.Equal(t, 1, *probeRuns)
		require.False(t, repo.account.Schedulable)
	})

	t.Run("material_account_change_declines_retry", func(t *testing.T) {
		runner, store, repo, state, probeRuns := newFixture()
		store.commitErrs = []error{ErrOpenAIProbeStale, nil}
		store.onCommitFail = func(int) {
			repo.account.UpdatedAt = now.Add(-time.Minute)
			repo.account.Status = StatusError // 实质前提变化
		}

		err := runner.processStateAtomic(context.Background(), state, now)
		require.ErrorIs(t, err, ErrOpenAIProbeStale)
		require.Equal(t, 1, store.commits)
		require.Equal(t, 1, *probeRuns)
	})
}

// r17u 回归：合格戳与现桶不一致（运营迁桶后）时，恢复路径不再被
// OPENAI_OAUTH_PROXY_MISMATCH 409 卡死——现桶上有完整健康针即放行并
// 把合格戳随迁到现桶；无健康证据仍拒。
func TestOpenAIProbeStagingMismatchRequalifiedByCurrentBucketHealth(t *testing.T) {
	now := time.Now()
	currentProxy := int64(11)
	rt := 1800
	account := &Account{
		ID: 1116, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &currentProxy, UpdatedAt: now,
		Extra: map[string]any{
			OpenAIOAuthQualifiedProxyExtraKey: float64(6),
		},
	}
	runner := NewOpenAIDowngradeProbeRunner(nil, nil, nil, nil, nil, nil)
	stage := newOpenAIProbeStaging(runner, account, &OpenAIDowngradeProbeState{AccountID: 1116})
	stage.mutation.Results = []OpenAIDowngradeProbeResult{{
		AccountID: 1116, ProxyID: &currentProxy, HTTPStatus: http.StatusOK,
		TransportOK: true, AnswerCorrect: true, ReasoningTokens: &rt,
	}}
	require.NoError(t, stage.SetSchedulable(context.Background(), 1116, true))
	require.False(t, stage.mutation.CompleteQualification, "legacy proxy stamps no longer gate scheduling")
	require.NotNil(t, stage.mutation.Schedulable)
	require.True(t, *stage.mutation.Schedulable)

	// 无健康证据（只有失败结果）→ 仍 409。
	stage2Account := &Account{
		ID: 1116, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &currentProxy, UpdatedAt: now,
		Extra: map[string]any{
			OpenAIOAuthQualifiedProxyExtraKey: float64(6),
		},
	}
	stage2 := newOpenAIProbeStaging(runner, stage2Account, &OpenAIDowngradeProbeState{AccountID: 1116})
	stage2.mutation.Results = []OpenAIDowngradeProbeResult{{
		AccountID: 1116, ProxyID: &currentProxy, HTTPStatus: http.StatusOK,
		TransportOK: true, AnswerCorrect: false, ReasoningTokens: &rt,
	}}
	require.NoError(t, stage2.SetSchedulable(context.Background(), 1116, true))
	require.False(t, stage2.mutation.CompleteQualification)
}

// r17u 回归：stagedRunner 采票直通——staging 必须实现 OpenAICodexTicketStore
// （生产实证：断言失败 → 动态桶 332 针多根票表恒空零日志）。
func TestOpenAIProbeStagingImplementsTicketStore(t *testing.T) {
	var _ OpenAICodexTicketStore = (*openAIProbeStaging)(nil)
	runner := NewOpenAIDowngradeProbeRunner(nil, nil, nil, nil, nil, nil)
	stage := newOpenAIProbeStaging(runner, &Account{ID: 1}, &OpenAIDowngradeProbeState{AccountID: 1})
	staged := runner.stagedRunner(stage)
	ts, ok := staged.store.(OpenAICodexTicketStore)
	require.True(t, ok, "staged runner store must satisfy ticket store for probe-side harvest")
	_ = ts
}

func TestOpenAIProbeStagingAllowsBrowserProxyChanges(t *testing.T) {
	now := time.Now()
	homeID, dynID := int64(5), int64(11)
	newBrowserAccount := func() *Account {
		return &Account{
			ID: 1136, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: false, ProxyID: &dynID, UpdatedAt: now,
			Extra: map[string]any{
				OpenAIOAuthQualifiedProxyExtraKey: float64(homeID),
				"email":                           "fixture@example.com",
			},
		}
	}
	runner := NewOpenAIDowngradeProbeRunner(nil, nil, nil, nil, nil, nil)

	t.Run("harvest_return_to_original_bucket", func(t *testing.T) {
		account := newBrowserAccount()
		stage := newOpenAIProbeStaging(runner, account, &OpenAIDowngradeProbeState{
			AccountID: 1136, State: OpenAIDowngradeStateOnDuty, ProbeMode: "harvest",
			CurrentProxyID: &dynID, OriginalProxyID: &homeID,
		})
		require.NoError(t, stage.SetOpenAIAccountProxy(context.Background(), 1136, &homeID))
		require.True(t, stage.mutation.ProxyChanged)
	})

	t.Run("harvest_to_other_static_bucket", func(t *testing.T) {
		account := newBrowserAccount()
		otherID := int64(7)
		stage := newOpenAIProbeStaging(runner, account, &OpenAIDowngradeProbeState{
			AccountID: 1136, State: OpenAIDowngradeStateOnDuty, ProbeMode: "harvest",
			CurrentProxyID: &dynID, OriginalProxyID: &homeID,
		})
		require.NoError(t, stage.SetOpenAIAccountProxy(context.Background(), 1136, &otherID))
		require.True(t, stage.mutation.ProxyChanged)
	})

	t.Run("normal_mode_proxy_change", func(t *testing.T) {
		account := newBrowserAccount()
		stage := newOpenAIProbeStaging(runner, account, &OpenAIDowngradeProbeState{
			AccountID: 1136, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
			CurrentProxyID: &dynID, OriginalProxyID: &homeID,
		})
		require.NoError(t, stage.SetOpenAIAccountProxy(context.Background(), 1136, &homeID))
		require.True(t, stage.mutation.ProxyChanged)
	})
}

// 回归（2026-09-22，1136 实证）：提交失败（非 Stale）后必须让位重排，
// next_probe_at 不再冻结在过去——否则 ListDue 每分钟捞出重探，形成
// 「冻结+空打」循环（1136 被自己人的 409 打了 740 针）。
func TestOpenAIProbeCommitFailureYieldsSchedule(t *testing.T) {
	now := time.Date(2026, 9, 22, 5, 30, 0, 0, time.UTC)
	proxyID := int64(11)
	account := &Account{
		ID: 1136, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &proxyID, UpdatedAt: now.Add(-time.Hour),
	}
	repo := &downgradeProbeAccountRepoStub{account: account}
	// 预置真库状态行：next_probe_at 在过去（冻结形态），UpdatedAt 与探针前提一致。
	rowState := OpenAIDowngradeProbeState{
		AccountID: 1136, State: OpenAIDowngradeStateOnDuty, ProbeMode: "harvest",
		CurrentProxyID: &proxyID, OriginalProxyID: int64Ptr(5),
		NextProbeAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
	}
	base := &downgradeProbeStoreStub{state: &rowState}
	store := &downgradeAtomicStoreStub{
		downgradeProbeStoreStub: base, accountRepo: repo,
		commitErr: ErrOpenAIOAuthProxyBindingProtected, // 模拟 409 保护闸
	}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.probeFn = func(_ context.Context, _ *Account, _ string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			AccountID: 1136, ProxyID: &proxyID, HTTPStatus: http.StatusOK,
			TransportOK: true, AnswerCorrect: false, ReasoningTokens: downgradeProbeIntPtr(665),
		}
	}
	state := rowState
	err := runner.processStateAtomic(context.Background(), &state, now)
	require.ErrorIs(t, err, ErrOpenAIOAuthProxyBindingProtected)
	require.Equal(t, 1, store.commits)
	// 让位落真库：next_probe_at 推到 half-open 节奏（30 分钟起 spread）。
	require.Equal(t, 1, base.saveCalls, "yield must persist via the live store")
	require.NotNil(t, base.state)
	require.True(t, base.state.NextProbeAt.After(now.Add(25*time.Minute)),
		"yielded schedule must be ~30min out, got %v", base.state.NextProbeAt.Sub(now))
	require.True(t, base.state.NextProbeAt.Before(now.Add(45*time.Minute)))
	// 让位只动排期，不动 state/proxy 实质。
	require.Equal(t, "harvest", base.state.ProbeMode)
	require.Equal(t, proxyID, *base.state.CurrentProxyID)
}

// 回归（2026-09-22）：让位不得覆盖并发赢家的推进——状态行 UpdatedAt 已被
// 他处推进时，让位跳过（不回写旧快照）。
func TestOpenAIProbeCommitFailureYieldRespectsConcurrentAdvance(t *testing.T) {
	now := time.Date(2026, 9, 22, 5, 30, 0, 0, time.UTC)
	proxyID := int64(11)
	account := &Account{
		ID: 1137, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &proxyID, UpdatedAt: now.Add(-time.Hour),
	}
	repo := &downgradeProbeAccountRepoStub{account: account}
	rowState := OpenAIDowngradeProbeState{
		AccountID: 1137, State: OpenAIDowngradeStateOnDuty, ProbeMode: "harvest",
		CurrentProxyID: &proxyID, OriginalProxyID: int64Ptr(5),
		NextProbeAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
	}
	base := &downgradeProbeStoreStub{state: &rowState}
	injected := errors.New("constraint violation")
	store := &downgradeAtomicStoreStub{
		downgradeProbeStoreStub: base, accountRepo: repo,
		commitErr: injected,
	}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.probeFn = func(_ context.Context, _ *Account, _ string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			AccountID: 1137, ProxyID: &proxyID, HTTPStatus: http.StatusOK,
			TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(1500),
		}
	}
	state := rowState
	// 提交失败后、让位读回前，竞争者推进了状态行。
	advanced := rowState
	advanced.UpdatedAt = now.Add(-30 * time.Second)
	advanced.ConsecutiveSuccesses = 2
	base.state = &advanced
	err := runner.processStateAtomic(context.Background(), &state, now)
	require.ErrorIs(t, err, injected)
	require.Zero(t, base.saveCalls, "yield must not overwrite a concurrent advance")
	require.Equal(t, advanced.UpdatedAt, base.state.UpdatedAt)
}
