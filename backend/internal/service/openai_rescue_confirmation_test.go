package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type rescueConfirmationFixture struct {
	account *Account
	repo    *rescueLaneRepo
	lane    *OpenAIRescueLane
	cfg     OpenAIRescueLaneConfig
	bridge  *PluginBridgeStatus
	host    OpenAIProbeHealthSnapshot
	calls   []int64
}

func newRescueConfirmationFixture(t *testing.T, passes int) *rescueConfirmationFixture {
	t.Helper()
	now := time.Now().UTC()
	account := rescueLaneSweepAccount(81)
	account.GroupIDs, account.Schedulable = []int64{99}, false
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: now.Add(-time.Hour), OrigGroupIDs: []int64{3}, SeedOK: true,
	})
	f := &rescueConfirmationFixture{
		account: account,
		repo:    &rescueLaneRepo{account: account},
		cfg:     rescueLaneEnabledConfig(),
		bridge: &PluginBridgeStatus{
			Running: true, Healthy: true,
			StatusJSON: rescueLaneBridgeJSON(rescueLaneBridgeAccountAtJSON(81, false, passes, 0, false, "", now)),
		},
		host: OpenAIProbeHealthSnapshot{AccountID: 81, State: OpenAIDowngradeStatePendingReplace},
	}
	f.lane = NewOpenAIRescueLane(f.repo, &rescueLaneSink{},
		func() OpenAIRescueLaneConfig { return f.cfg }, nil)
	f.lane.now = func() time.Time { return now }
	f.lane.SetBridgeSource(func(context.Context) *PluginBridgeStatus { return f.bridge })
	f.lane.SetProbeStateSource(func(context.Context, []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
		return map[int64]OpenAIProbeHealthSnapshot{81: f.host}, nil
	})
	f.lane.SetNeedleTrigger(func(_ context.Context, id int64) error {
		f.calls = append(f.calls, id)
		return nil
	})
	t.Cleanup(f.lane.Stop)
	return f
}

func TestRescueConfirmationEarlyNeedleDoesNotGraduate(t *testing.T) {
	f := newRescueConfirmationFixture(t, 3)
	require.NoError(t, f.lane.runConfirmationSweep(t.Context()))
	require.Equal(t, []int64{81}, f.calls)
	require.Equal(t, [][]int64{{81}}, f.repo.getByIDsCalls)
	require.Zero(t, f.repo.listCalls, "fast confirmation must not scan the roster")
	require.False(t, f.account.Schedulable)
	require.Equal(t, []int64{99}, f.account.GroupIDs)
	require.Equal(t, 1, GetOpenAIRescueLaneMarker(f.account).AutoNeedleAttempts)

	f.host = rescueRecoverySnapshot(81, f.lane.now())
	require.NoError(t, f.lane.runConfirmationSweep(t.Context()))
	require.Len(t, f.calls, 1)
	require.False(t, f.account.Schedulable, "host success alone must not lower the six-pass graduation threshold")
	require.NotNil(t, GetOpenAIRescueLaneMarker(f.account))
}

func TestRescueConfirmationGraduatesOnlyWithFullDualEvidence(t *testing.T) {
	for _, hostPass := range []bool{false, true} {
		t.Run(map[bool]string{false: "plugin_only", true: "dual_evidence"}[hostPass], func(t *testing.T) {
			f := newRescueConfirmationFixture(t, 6)
			if hostPass {
				f.host = rescueRecoverySnapshot(81, f.lane.now())
			}
			require.NoError(t, f.lane.runConfirmationSweep(t.Context()))
			require.Equal(t, hostPass, f.account.Schedulable)
			if hostPass {
				require.Nil(t, GetOpenAIRescueLaneMarker(f.account))
				require.Equal(t, []int64{3}, f.account.GroupIDs)
				require.Empty(t, f.calls)
				require.NoError(t, f.lane.runConfirmationSweep(t.Context()))
				require.Len(t, f.repo.binds, 1, "graduation replay is a no-op")
			} else {
				require.NotNil(t, GetOpenAIRescueLaneMarker(f.account))
				require.Equal(t, []int64{99}, f.account.GroupIDs)
				require.Equal(t, []int64{81}, f.calls)
			}
		})
	}
}

func TestRescueConfirmationGates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rescueConfirmationFixture)
	}{
		{"disabled", func(f *rescueConfirmationFixture) { f.cfg.Enabled = false }},
		{"no_group", func(f *rescueConfirmationFixture) { f.cfg.GroupID = 0 }},
		{"offline", func(f *rescueConfirmationFixture) { f.bridge.Offline = true }},
		{"unhealthy", func(f *rescueConfirmationFixture) { f.bridge.Healthy = false }},
		{"stopped_plugin", func(f *rescueConfirmationFixture) { f.bridge.Running = false }},
		{"malformed_bridge", func(f *rescueConfirmationFixture) { f.bridge.StatusJSON = "{}" }},
		{"disabled_account", func(f *rescueConfirmationFixture) { f.account.Status = StatusDisabled }},
		{"paused", func(f *rescueConfirmationFixture) { f.host.ManualPaused = true }},
		{"in_flight", func(f *rescueConfirmationFixture) { f.host.State = OpenAIDowngradeStateOnDuty }},
		{"no_marker", func(f *rescueConfirmationFixture) { delete(f.account.Extra, openAIRescueLaneExtraKey) }},
		{"mixed_binding", func(f *rescueConfirmationFixture) { f.account.GroupIDs = []int64{99, 3} }},
		{"wrong_binding", func(f *rescueConfirmationFixture) { f.account.GroupIDs = []int64{3} }},
		{"expired", func(f *rescueConfirmationFixture) {
			at := f.lane.now().Add(-time.Minute)
			f.account.ExpiresAt, f.account.AutoPauseOnExpired = &at, true
		}},
		{"exiting", func(f *rescueConfirmationFixture) {
			marker := GetOpenAIRescueLaneMarker(f.account)
			marker.ExitReason = "auth_rejected"
			rescueLaneApplyMarker(t, f.account, *marker)
		}},
		{"old_generation", func(f *rescueConfirmationFixture) {
			marker := GetOpenAIRescueLaneMarker(f.account)
			marker.EnteredAt = f.lane.now().Add(time.Second)
			rescueLaneApplyMarker(t, f.account, *marker)
		}},
		{"state_lost_after_pass", func(f *rescueConfirmationFixture) {
			marker := GetOpenAIRescueLaneMarker(f.account)
			marker.Observation = &openAIRescueObservation{StateLosses: []time.Time{f.lane.now().Add(time.Second)}}
			rescueLaneApplyMarker(t, f.account, *marker)
		}},
		{"stale_pass", func(f *rescueConfirmationFixture) {
			f.bridge.StatusJSON = rescueLaneBridgeJSON(rescueLaneBridgeAccountAtJSON(81, false, 6, 0, false, "", f.lane.now().Add(-16*time.Minute)))
		}},
		{"future_pass", func(f *rescueConfirmationFixture) {
			f.bridge.StatusJSON = rescueLaneBridgeJSON(rescueLaneBridgeAccountAtJSON(81, false, 6, 0, false, "", f.lane.now().Add(time.Second)))
		}},
		{"insufficient_passes", func(f *rescueConfirmationFixture) {
			f.bridge.StatusJSON = rescueLaneBridgeJSON(rescueLaneBridgeAccountAtJSON(81, false, 2, 0, false, "", f.lane.now()))
		}},
		{"backoff", func(f *rescueConfirmationFixture) {
			f.bridge.StatusJSON = rescueLaneBridgeJSON(rescueLaneBridgeAccountAtJSON(81, true, 6, 0, false, "", f.lane.now()))
		}},
		{"suspected", func(f *rescueConfirmationFixture) {
			f.bridge.StatusJSON = rescueLaneBridgeJSON(rescueLaneBridgeAccountAtJSON(81, false, 6, 0, true, "", f.lane.now()))
		}},
		{"gate_denied", func(f *rescueConfirmationFixture) {
			f.lane.SetSchedulingGate(func(context.Context, int64) (bool, error) { return false, nil })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRescueConfirmationFixture(t, 6)
			tc.mutate(f)
			require.NoError(t, f.lane.runConfirmationSweep(t.Context()))
			require.Empty(t, f.calls)
			require.Empty(t, f.repo.calls, "ineligible evidence must not mutate state")
			require.False(t, f.account.Schedulable)
		})
	}
}

func TestRescueConfirmationErrorsAndCooldown(t *testing.T) {
	failure := errors.New("confirmation dependency unavailable")
	for _, phase := range []string{"accounts", "states", "gate", "marker", "trigger", "graduation"} {
		t.Run(phase, func(t *testing.T) {
			f := newRescueConfirmationFixture(t, 6)
			switch phase {
			case "accounts":
				f.repo.getErr = failure
			case "states":
				f.lane.SetProbeStateSource(func(context.Context, []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
					return nil, failure
				})
			case "gate":
				f.lane.SetSchedulingGate(func(context.Context, int64) (bool, error) { return false, failure })
			case "marker":
				f.repo.markerErr = ErrOpenAIProbeStale
			case "trigger":
				f.lane.SetNeedleTrigger(func(_ context.Context, id int64) error {
					f.calls = append(f.calls, id)
					return failure
				})
			case "graduation":
				f.host = rescueRecoverySnapshot(81, f.lane.now())
				f.repo.graduateErr = failure
			}
			err := f.lane.runConfirmationSweep(t.Context())
			if phase == "marker" {
				require.ErrorIs(t, err, ErrOpenAIProbeStale)
			} else {
				require.ErrorIs(t, err, failure)
			}
			require.False(t, f.account.Schedulable)
			require.NotNil(t, GetOpenAIRescueLaneMarker(f.account))
			if phase == "trigger" {
				require.NoError(t, f.lane.runConfirmationSweep(t.Context()))
				require.Len(t, f.calls, 1, "failed dispatch must not create a rapid retry loop")
			} else {
				require.Empty(t, f.calls)
			}
		})
	}
}

func TestRescueConfirmationSharesSweepLock(t *testing.T) {
	f := newRescueConfirmationFixture(t, 3)
	f.repo.roster = []Account{*f.account}
	entered, release := make(chan struct{}), make(chan struct{})
	f.lane.SetProbeStateSource(func(context.Context, []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
		close(entered)
		<-release
		return map[int64]OpenAIProbeHealthSnapshot{81: f.host}, nil
	})
	done := make(chan error, 1)
	go func() {
		_, _, _, err := f.lane.RunReconcileSweep(context.Background())
		done <- err
	}()
	<-entered
	require.NoError(t, f.lane.runConfirmationSweep(t.Context()))
	require.Empty(t, f.repo.getByIDsCalls)
	close(release)
	require.NoError(t, <-done)
	require.Equal(t, []int64{81}, f.calls)
}

func TestRescueSweepConfirmsReadyAccountsBeforeReseeding(t *testing.T) {
	f := newRescueConfirmationFixture(t, 3)
	unseeded := rescueLaneSweepAccount(80)
	unseeded.GroupIDs, unseeded.Schedulable = []int64{99}, false
	rescueLaneApplyMarker(t, unseeded, OpenAIRescueLaneMarker{
		EnteredAt: f.lane.now().Add(-time.Hour), OrigGroupIDs: []int64{3},
	})
	f.repo.roster = []Account{*unseeded, *f.account}
	seeds := 0
	f.lane.seed = func(_ context.Context, id int64) error {
		seeds++
		require.Equal(t, int64(80), id)
		require.Equal(t, []int64{81}, f.calls, "ready account must not wait behind an upstream seed")
		return nil
	}
	_, _, _, err := f.lane.RunReconcileSweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, seeds)
	require.True(t, GetOpenAIRescueLaneMarker(f.repo.resolve(80)).SeedOK)
	require.False(t, f.repo.resolve(81).Schedulable, "early confirmation is not graduation")
}

func TestRescueSweepDeferredSeedRetainsRevisionFence(t *testing.T) {
	f := newRescueConfirmationFixture(t, 3)
	unseeded := rescueLaneSweepAccount(80)
	unseeded.GroupIDs, unseeded.Schedulable = []int64{99}, false
	rescueLaneApplyMarker(t, unseeded, OpenAIRescueLaneMarker{
		EnteredAt: f.lane.now().Add(-time.Hour), OrigGroupIDs: []int64{3},
	})
	f.repo.roster = []Account{*unseeded, *f.account}
	seeds := 0
	f.lane.seed = func(context.Context, int64) error {
		seeds++
		return nil
	}
	f.lane.SetNeedleTrigger(func(_ context.Context, _ int64) error {
		// A concurrent reauthorization after the roster read invalidates its seed.
		account := f.repo.resolve(80)
		account.UpdatedAt = account.UpdatedAt.Add(time.Second)
		return nil
	})
	_, _, _, err := f.lane.RunReconcileSweep(t.Context())
	require.NoError(t, err)
	require.Zero(t, seeds, "a stale queued account must be rejected before upstream work")
	require.Zero(t, GetOpenAIRescueLaneMarker(f.repo.resolve(80)).SeedAttempts)
}

func TestRescueLaneConcurrentStartStop(t *testing.T) {
	lane := NewOpenAIRescueLane(&rescueLaneRepo{}, nil, nil, nil)
	var workers sync.WaitGroup
	for i := 0; i < 24; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			if i%2 == 0 {
				lane.Start()
			} else {
				lane.Stop()
			}
		}(i)
	}
	workers.Wait()
	require.ErrorIs(t, lane.runConfirmationSweep(t.Context()), context.Canceled)
	_, _, _, err := lane.RunReconcileSweep(t.Context())
	require.ErrorIs(t, err, context.Canceled)
}

func TestRescueLaneStopCancelsActiveSweep(t *testing.T) {
	f := newRescueConfirmationFixture(t, 3)
	f.cfg.ReconcileInterval = time.Millisecond
	f.repo.roster = []Account{*f.account}
	entered, canceled := make(chan struct{}), make(chan struct{})
	f.lane.SetProbeStateSource(func(ctx context.Context, _ []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	})
	f.lane.Start()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("sweep did not start")
	}
	f.lane.Stop()
	select {
	case <-canceled:
	default:
		t.Fatal("stop returned before the sweep was canceled")
	}
	require.Empty(t, f.calls)
	require.Empty(t, f.repo.calls)
}
