package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIProbeCooldownUsesAtomicEntry(t *testing.T) {
	for _, track := range []struct{ state, mode string }{
		{OpenAIDowngradeStateOnDuty, "normal"},
		{OpenAIDowngradeStateOnDuty, "qualification"},
		{OpenAIDowngradeStateCircuitOpen, "half_open"},
		{OpenAIDowngradeStateCircuitOpen, "sol_fallback"},
		{OpenAIDowngradeStateReprobe, "accelerated"},
	} {
		for _, commitErr := range []error{nil, ErrOpenAIProbeStale, errors.New("outbox unavailable")} {
			t.Run(track.mode+"/"+errorTestName(commitErr), func(t *testing.T) {
				now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
				limitedAt, resetAt := now.Add(-time.Hour), now.Add(7*24*time.Hour)
				proxyID := int64(3)
				account := &Account{
					ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Status: StatusActive, Schedulable: true, ProxyID: &proxyID,
					UpdatedAt: now, RateLimitedAt: &limitedAt, RateLimitResetAt: &resetAt,
				}
				repo := &downgradeProbeAccountRepoStub{account: account}
				base := &downgradeProbeStoreStub{}
				store := &downgradeAtomicStoreStub{
					downgradeProbeStoreStub: base, accountRepo: repo, commitErr: commitErr,
				}
				store.afterCommit = func() {
					require.Nil(t, store.observed.RateLimitClear)
					require.Zero(t, repo.snapshotCalls)
				}
				runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
				runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
					t.Fatal("a persisted cooldown cannot be cleared by an early probe")
					return OpenAIDowngradeProbeResult{}
				}
				deadline := now.Add(time.Hour)
				state := OpenAIDowngradeProbeState{
					AccountID: 7, State: track.state, ProbeMode: track.mode,
					OriginalProxyID: &proxyID, CurrentProxyID: &proxyID,
					RecoveryDeadline: &deadline, NextProbeAt: now, UpdatedAt: now,
				}
				before := state
				require.ErrorIs(t, runner.processStateAtomic(context.Background(), &state, now), commitErr)
				require.Equal(t, 1, store.commits)
				require.Nil(t, store.observed.RateLimitClear)
				require.False(t, store.observed.ChangesAccount())
				require.Nil(t, store.observed.RateLimitResetAt)
				require.Empty(t, repo.openAIRateLimitClears, "never call the live nontransactional releaser")
				require.Zero(t, base.eventCalls)
				require.Zero(t, base.saveCalls)
				recovered := 0
				for _, event := range store.observed.Events {
					if event.Type == OpenAIDowngradeEventRateLimitRecheckRecovered {
						recovered++
					}
				}
				require.Zero(t, recovered)
				require.Equal(t, &limitedAt, account.RateLimitedAt)
				require.Equal(t, &resetAt, account.RateLimitResetAt)
				require.Zero(t, repo.snapshotCalls)
				if commitErr != nil {
					require.Equal(t, before, state)
				} else {
					require.False(t, state.NextProbeAt.Before(resetAt))
					require.Nil(t, state.LastProbeAt)
				}
			})
		}
	}
}

func TestOpenAIProbeRecoveryStagingGuards(t *testing.T) {
	now := time.Now().UTC()
	limitedAt, resetAt := now.Add(-time.Hour), now.Add(time.Hour)
	account := &Account{ID: 7, Platform: PlatformOpenAI, RateLimitedAt: &limitedAt, RateLimitResetAt: &resetAt}
	for _, tc := range []struct {
		name string
		id   int64
		at   time.Time
		end  time.Time
		err  error
		want bool
	}{
		{"match", 7, limitedAt, resetAt, nil, true},
		{"other_account", 8, limitedAt, resetAt, ErrOpenAIProbeStale, false},
		{"rearmed", 7, limitedAt.Add(time.Second), resetAt, nil, false},
		{"extended", 7, limitedAt, resetAt.Add(time.Hour), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage := newOpenAIProbeStaging(&OpenAIDowngradeProbeRunner{}, account,
				&OpenAIDowngradeProbeState{AccountID: 7})
			var releaser OpenAIDowngradeRateLimitReleaser = stage
			cleared, err := releaser.ClearOpenAIRateLimitIfObserved(context.Background(), tc.id, tc.at, tc.end)
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, tc.want, cleared)
			require.Equal(t, tc.want, stage.mutation.ChangesAccount())
			require.Equal(t, &limitedAt, account.RateLimitedAt)
			require.Equal(t, &resetAt, account.RateLimitResetAt)
		})
	}
}

func TestOpenAIProbeRecoveryRequiresAcceptedResult(t *testing.T) {
	now := time.Now().UTC()
	limitedAt, resetAt := now.Add(-time.Hour), now.Add(time.Hour)
	for _, tc := range []struct {
		name      string
		status    int
		transport bool
		resetAt   *time.Time
		want      bool
	}{
		{"accepted", 200, true, &resetAt, true},
		{"accepted_2xx", 204, true, &resetAt, true},
		{"transport_failure", 200, false, &resetAt, false},
		{"unauthorized", 401, true, &resetAt, false},
		{"rate_limited", 429, true, &resetAt, false},
		{"server_error", 503, true, &resetAt, false},
		{"expired", 200, true, &now, false},
		{"missing_reset", 200, true, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 7, Platform: PlatformOpenAI, RateLimitedAt: &limitedAt, RateLimitResetAt: tc.resetAt}
			state := &OpenAIDowngradeProbeState{AccountID: 7}
			stage := newOpenAIProbeStaging(&OpenAIDowngradeProbeRunner{}, account, state)
			runner := (&OpenAIDowngradeProbeRunner{}).stagedRunner(stage)
			runner.releaseObservedRateLimitOnAcceptedProbe(context.Background(), account, state,
				OpenAIDowngradeProbeResult{HTTPStatus: tc.status, TransportOK: tc.transport}, now)
			require.Equal(t, tc.want, stage.mutation.RateLimitClear != nil)
			if tc.want {
				require.Len(t, stage.mutation.Events, 1)
			} else {
				require.Empty(t, stage.mutation.Events)
			}
		})
	}
}

func TestOpenAIProbeRecoveryCacheFailurePreservesCommit(t *testing.T) {
	now := time.Now().UTC()
	repo := &downgradeProbeAccountRepoStub{snapshotErr: errors.New("cache unavailable")}
	store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{}}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	mutation := &OpenAIDowngradeMutation{
		AccountID: 7, State: &OpenAIDowngradeProbeState{AccountID: 7},
		RateLimitClear: &OpenAIDowngradeRateLimitObservation{LimitedAt: now, ResetAt: now.Add(time.Hour)},
	}
	committed, err := runner.commitOpenAIProbeMutation(context.Background(), store, mutation)
	require.True(t, committed)
	require.ErrorIs(t, err, ErrOpenAIProbeSnapshotRefresh)
	require.Equal(t, 1, store.commits)
	require.Equal(t, 1, repo.snapshotCalls)

	runner.accountRepo = struct{ AccountRepository }{repo}
	committed, err = runner.commitOpenAIProbeMutation(context.Background(), store, mutation)
	require.False(t, committed)
	require.ErrorIs(t, err, ErrOpenAIProbeSnapshotStore)
	require.Equal(t, 1, store.commits, "missing consumer cannot start another commit")
}
