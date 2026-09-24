package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newAbuseRouteAtomicRunner(t *testing.T) (*OpenAIDowngradeProbeRunner, *downgradeAtomicStoreStub, *downgradeProbeAccountRepoStub, *OpenAIDowngradeProbeState, time.Time) {
	t.Helper()
	const accountID int64 = 92001
	t.Cleanup(func() { drainAbuseRouteSignal(accountID) })
	drainAbuseRouteSignal(accountID)
	now := time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)
	proxyID := int64(3)
	account := &Account{
		ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, ProxyID: &proxyID,
		UpdatedAt: now.Add(-time.Hour),
		Extra:     map[string]any{OpenAIOAuthQualifiedProxyExtraKey: proxyID},
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: accountID, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
		OriginalProxyID: &proxyID, CurrentProxyID: &proxyID,
		NextProbeAt: now.Add(time.Hour), UpdatedAt: now.Add(-time.Minute),
	}
	repo := &downgradeProbeAccountRepoStub{account: account}
	base := &downgradeProbeStoreStub{state: state}
	store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, accountRepo: repo}
	base.listDueFn = func(_ context.Context, at time.Time, limit int) ([]OpenAIDowngradeProbeState, error) {
		require.Equal(t, 100, limit)
		if store.state.NextProbeAt.After(at) {
			return nil, nil
		}
		return []OpenAIDowngradeProbeState{*store.state}, nil
	}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.nextDelay = func() time.Duration { return time.Hour }
	runner.SetRecentTrafficChecker(func(context.Context, int64, time.Duration) bool { return true })
	runner.probeFn = func(_ context.Context, account *Account, mode string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			AccountID: account.ID, ProxyID: account.ProxyID, Mode: mode,
			TransportOK: true, HTTPStatus: http.StatusOK, AnswerCorrect: true,
			ReasoningTokens: downgradeProbeIntPtr(1800),
		}
	}
	return runner, store, repo, state, now
}

func TestAbuseRouteRunOnceArmsFutureState(t *testing.T) {
	runner, store, _, state, now := newAbuseRouteAtomicRunner(t)
	openAIAbuseRouteSignals.ObserveRealTrafficModelMismatch(state.AccountID, "gpt-6-astra", "gpt-5.6-luna", now)
	probes := 0
	originalProbe := runner.probeFn
	runner.probeFn = func(ctx context.Context, account *Account, mode string) OpenAIDowngradeProbeResult {
		probes++
		return originalProbe(ctx, account, mode)
	}
	require.NoError(t, runner.RunOnce(context.Background()))
	require.Equal(t, 1, probes, "future scheduling must not strand a live mismatch signal")
	require.Equal(t, 2, store.commits, "arming and probe results use separate CAS commits")
	require.Len(t, store.events, 1)
	require.Equal(t, OpenAIDowngradeEventRealTrafficModelMismatch, store.events[0].Type)
	require.NotNil(t, store.events[0].ObservedAt)
	require.Equal(t, now, *store.events[0].ObservedAt)
	require.Nil(t, store.observed.Schedulable, "mismatch alone must not remove an account")
	_, pending := openAIAbuseRouteSignals.PeekRealTrafficSignal(state.AccountID, now)
	require.False(t, pending)
}

func TestAbuseRouteRunOnceRespectsExistingHolds(t *testing.T) {
	for _, reason := range []string{"manual_pause", "unschedulable", "quota_hold", "429_backoff", "cooldown", "qualification", "fallback", "expired", "api_key", "stale_signal", "bucket_spacing"} {
		t.Run(reason, func(t *testing.T) {
			runner, store, repo, state, now := newAbuseRouteAtomicRunner(t)
			switch reason {
			case "manual_pause":
				store.paused = true
			case "unschedulable":
				repo.account.Schedulable = false
			case "quota_hold":
				reset := now.Add(24 * time.Hour)
				repo.account.RateLimitResetAt = &reset
			case "429_backoff":
				state.Consecutive429s = 1
			case "cooldown":
				state.State = OpenAIDowngradeStateCircuitOpen
			case "qualification":
				state.ProbeMode = "qualification"
			case "fallback":
				state.ProbeMode = "sol_fallback"
			case "expired":
				expired := now.Add(-time.Minute)
				repo.account.AutoPauseOnExpired = true
				repo.account.ExpiresAt = &expired
			case "api_key":
				repo.account.Type = AccountTypeAPIKey
			case "bucket_spacing":
				store.listDueFn = func(_ context.Context, _ time.Time, limit int) ([]OpenAIDowngradeProbeState, error) {
					require.Equal(t, 100, limit)
					return nil, nil
				}
			}
			observedAt := now
			if reason == "stale_signal" {
				observedAt = now.Add(-openAIAbuseRouteSignalTTL - time.Second)
			}
			openAIAbuseRouteSignals.ObserveRealTrafficModelMismatch(state.AccountID, "gpt-6-astra", "gpt-5.6-luna", observedAt)
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				t.Fatal("signal must not bypass existing probe admission")
				return OpenAIDowngradeProbeResult{}
			}
			require.NoError(t, runner.RunOnce(context.Background()))
			if reason == "bucket_spacing" {
				require.Equal(t, 1, store.commits, "only scheduling may change while the bucket is held")
			} else {
				require.Zero(t, store.commits)
			}
		})
	}
}

func TestAbuseRouteAcknowledgesOnlyCommittedGeneration(t *testing.T) {
	for _, scenario := range []string{"commit_error", "stale_error", "stale_retry", "new_signal", "snapshot_error"} {
		t.Run(scenario, func(t *testing.T) {
			runner, store, repo, state, now := newAbuseRouteAtomicRunner(t)
			openAIAbuseRouteSignals.ObserveRealTrafficModelMismatch(state.AccountID, "gpt-6-astra", "gpt-5.6-luna", now)
			switch scenario {
			case "commit_error":
				store.commitErr = errors.New("database unavailable")
			case "stale_error":
				store.commitErr = ErrOpenAIProbeStale
			case "stale_retry":
				store.commitErrs = []error{ErrOpenAIProbeStale, nil}
				store.onCommitFail = func(int) { repo.account.UpdatedAt = now }
			case "new_signal":
				store.afterCommit = func() {
					// Same bytes and timestamp are still a new observation.
					openAIAbuseRouteSignals.ObserveRealTrafficModelMismatch(state.AccountID, "gpt-6-astra", "gpt-5.6-luna", now)
				}
			case "snapshot_error":
				repo.snapshotErr = errors.New("cache unavailable")
				originalProbe := runner.probeFn
				runner.probeFn = func(ctx context.Context, account *Account, mode string) OpenAIDowngradeProbeResult {
					result := originalProbe(ctx, account, mode)
					result.AnswerCorrect = false
					result.TurnStateLen = 356
					return result
				}
			}
			err := runner.processStateAtomic(context.Background(), state, now)
			_, pending := openAIAbuseRouteSignals.PeekRealTrafficSignal(state.AccountID, now)
			switch scenario {
			case "commit_error", "stale_error":
				require.Error(t, err)
				require.True(t, pending, "failed database commit must retain the observation")
			case "new_signal":
				require.NoError(t, err)
				require.True(t, pending, "acknowledging the old generation must preserve new traffic")
			case "snapshot_error":
				require.ErrorIs(t, err, ErrOpenAIProbeSnapshotRefresh)
				require.False(t, pending, "snapshot failure does not undo a committed event")
			default:
				require.NoError(t, err)
				require.Equal(t, 2, store.commits)
				require.False(t, pending)
			}
		})
	}
}

func TestAbuseRouteRunOnceRetainsSignalWhenArmingFails(t *testing.T) {
	runner, store, _, state, now := newAbuseRouteAtomicRunner(t)
	before := *state
	store.commitErr = errors.New("database unavailable")
	openAIAbuseRouteSignals.ObserveRealTrafficModelMismatch(state.AccountID, "gpt-6-astra", "gpt-5.6-luna", now)
	require.NoError(t, runner.RunOnce(context.Background()))
	require.Equal(t, before, *state)
	_, pending := openAIAbuseRouteSignals.PeekRealTrafficSignal(state.AccountID, now)
	require.True(t, pending)
	store.commitErr = nil
	require.NoError(t, runner.RunOnce(context.Background()))
	require.Len(t, store.observed.Results, 1)
	_, pending = openAIAbuseRouteSignals.PeekRealTrafficSignal(state.AccountID, now)
	require.False(t, pending)
}

func TestAbuseRouteTurnStateRecheckSurvivesRestartAndThenResumesDeferral(t *testing.T) {
	runner, store, repo, state, now := newAbuseRouteAtomicRunner(t)
	state.NextProbeAt = now
	probes := 0
	probeFn := runner.probeFn
	runner.probeFn = func(ctx context.Context, account *Account, mode string) OpenAIDowngradeProbeResult {
		probes++
		result := probeFn(ctx, account, mode)
		result.TurnStateLen = 356
		return result
	}
	runner.SetRecentTrafficChecker(nil)
	var eventAt time.Time
	store.afterCommit = func() {
		for _, event := range store.observed.Events {
			if event.Type == OpenAIDowngradeEventTurnStateDegraded {
				eventAt = now.Add(time.Second)
			}
		}
	}
	store.eventCountFn = func(_ context.Context, id int64, eventType string, since time.Time) (int, error) {
		require.Equal(t, state.AccountID, id)
		if eventType == OpenAIDowngradeEventRealTrafficModelMismatch {
			return 0, nil
		}
		require.Equal(t, OpenAIDowngradeEventTurnStateDegraded, eventType)
		if !eventAt.IsZero() && !eventAt.Before(since) {
			return 1, nil
		}
		return 0, nil
	}
	require.NoError(t, runner.RunOnce(context.Background()))
	require.Equal(t, 1, probes)
	require.False(t, eventAt.IsZero())
	// 纯单针杀（2026-09-22 用户裁定）：356 单信号即熔断摘调度。
	require.False(t, repo.account.Schedulable, "single-shot circuit must pull the account")

	// Keep only committed state and events, just as after a service restart.
	restarted := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	restarted.now = func() time.Time { return now }
	restarted.nextDelay = func() time.Duration { return time.Hour }
	restarted.SetRecentTrafficChecker(func(context.Context, int64, time.Duration) bool { return true })
	restarted.probeFn = func(ctx context.Context, account *Account, mode string) OpenAIDowngradeProbeResult {
		probes++
		return probeFn(ctx, account, mode)
	}
	now = store.state.NextProbeAt
	require.NoError(t, restarted.RunOnce(context.Background()))
	require.Equal(t, 2, probes, "recent traffic must not defer the committed short recheck")
	// 纯单针杀下 356 针已熔断：重启后 reprobe 恢复需 2 连胜。第二连胜针
	//（probes=3）照打；恢复后 finishRescue 进 accelerated 盯防档（30min 窗
	// 内 5min 级针，不走真实流量顺延——盯防优先于顺延），probes 继续走。
	// 该测试验证的核心（356 事件驱动复查排期跨重启存活）已在 probes=2 处
	// 断言完毕，后续节奏归 accelerated 档管辖。
	for probes == 2 {
		now = store.state.NextProbeAt
		require.NoError(t, restarted.RunOnce(context.Background()))
	}
	require.Equal(t, 3, probes, "second clean recheck must complete the recovery streak")
	require.Equal(t, OpenAIDowngradeStateOnDuty, store.state.State, "recovery must return the account on duty")
}

func TestAbuseRouteRecheckReadFailureDoesNotPublishDeferral(t *testing.T) {
	runner, store, _, state, now := newAbuseRouteAtomicRunner(t)
	lastProbe := now.Add(-5 * time.Minute)
	state.LastProbeAt = &lastProbe
	before := *state
	readErr := errors.New("event store unavailable")
	store.eventCountFn = func(context.Context, int64, string, time.Time) (int, error) { return 0, readErr }
	require.ErrorIs(t, runner.processStateAtomic(context.Background(), state, now), readErr)
	require.Equal(t, before, *state)
	require.Zero(t, store.commits)
	require.Empty(t, runner.deferCounts)
}

func TestAbuseRouteHubExpiryAndOutOfOrderObservations(t *testing.T) {
	hub := &openAIAbuseRouteSignalHub{}
	now := time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)
	hub.ObserveRealTrafficModelMismatch(1, "a", "b", now)
	first, ok := hub.PeekRealTrafficSignal(1, now)
	require.True(t, ok)
	hub.ObserveRealTrafficModelMismatch(1, "a", "b", now)
	hub.AcknowledgeRealTrafficSignal(first)
	newer, ok := hub.PeekRealTrafficSignal(1, now)
	require.True(t, ok)
	require.NotEqual(t, first.generation, newer.generation)
	hub.ObserveRealTrafficModelMismatch(1, "old", "old", now.Add(-time.Minute))
	stillNewer, ok := hub.PeekRealTrafficSignal(1, now)
	require.True(t, ok)
	require.Equal(t, newer, stillNewer)
	_, ok = hub.PeekRealTrafficSignal(1, now.Add(openAIAbuseRouteSignalTTL))
	require.True(t, ok, "TTL is inclusive")
	hub.PurgeExpired(now.Add(openAIAbuseRouteSignalTTL + time.Nanosecond))
	require.Empty(t, hub.signals, "expiration must reclaim entries without a consumer")
	hub.ObserveRealTrafficModelMismatch(2, "a", "b", now)
	hub.ObserveRealTrafficModelMismatch(3, "a", "b", now.Add(2*openAIAbuseRouteSignalTTL))
	require.NotContains(t, hub.signals, int64(2), "producer-only traffic also reclaims expired entries")
}
