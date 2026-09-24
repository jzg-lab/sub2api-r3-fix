package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type scheduleReconciliationStore struct {
	downgradeProbeStoreStub
	calls         []string
	now           time.Time
	reconcileErr  error
	accelerateErr error
}

func (s *scheduleReconciliationStore) ReconcileOpenAIRateLimitProbeSchedules(
	_ context.Context, now time.Time, interval time.Duration,
) (int64, error) {
	s.calls = append(s.calls, "reconcile")
	if !now.Equal(s.now) || interval != openAIDowngradeRateLimitRecheckInterval {
		return 0, errors.New("unexpected sparse recheck policy")
	}
	return 1, s.reconcileErr
}

func (s *scheduleReconciliationStore) AccelerateOpenAIInterruptedProbeRechecks(
	_ context.Context, now time.Time, delay time.Duration,
) (int64, error) {
	s.calls = append(s.calls, "accelerate")
	if !now.Equal(s.now) || delay != openAIDowngradeInterruptedRecheck {
		return 0, errors.New("unexpected interrupted recheck policy")
	}
	return 1, s.accelerateErr
}

func (s *scheduleReconciliationStore) ListDueOpenAIDowngradeStates(
	context.Context, time.Time, int,
) ([]OpenAIDowngradeProbeState, error) {
	s.calls = append(s.calls, "due")
	return nil, nil
}

func TestOpenAIProbeScheduleReconciliationRunOnce(t *testing.T) {
	for _, phase := range []string{"success", "reconcile_failure", "accelerate_failure"} {
		t.Run(phase, func(t *testing.T) {
			store := &scheduleReconciliationStore{now: time.Now()}
			switch phase {
			case "reconcile_failure":
				store.reconcileErr = errors.New("schedule commit failed")
			case "accelerate_failure":
				store.accelerateErr = errors.New("recheck commit failed")
			}
			runner := NewOpenAIDowngradeProbeRunner(store, &downgradeProbeAccountRepoStub{}, nil, nil, nil, nil)
			runner.now = func() time.Time { return store.now }
			err := runner.RunOnce(context.Background())
			switch phase {
			case "reconcile_failure":
				require.ErrorIs(t, err, store.reconcileErr)
				require.Equal(t, []string{"reconcile"}, store.calls)
			case "accelerate_failure":
				require.ErrorIs(t, err, store.accelerateErr)
				require.Equal(t, []string{"reconcile", "accelerate"}, store.calls)
			default:
				require.NoError(t, err)
				require.Equal(t, []string{"reconcile", "accelerate", "due"}, store.calls)
			}
		})
	}
}
