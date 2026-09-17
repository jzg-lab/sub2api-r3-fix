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
	calls []string
	now   time.Time
	err   error
}

func (s *scheduleReconciliationStore) ReconcileOpenAIRateLimitProbeSchedules(
	_ context.Context, now time.Time, interval time.Duration,
) (int64, error) {
	s.calls = append(s.calls, "reconcile")
	if !now.Equal(s.now) || interval != openAIDowngradeRateLimitRecheckInterval {
		return 0, errors.New("unexpected sparse recheck policy")
	}
	return 1, s.err
}

func (s *scheduleReconciliationStore) ListDueOpenAIDowngradeStates(
	context.Context, time.Time, int,
) ([]OpenAIDowngradeProbeState, error) {
	s.calls = append(s.calls, "due")
	return nil, nil
}

func TestOpenAIProbeScheduleReconciliationRunOnce(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			store := &scheduleReconciliationStore{now: time.Now()}
			if failed {
				store.err = errors.New("schedule commit failed")
			}
			runner := NewOpenAIDowngradeProbeRunner(store, &downgradeProbeAccountRepoStub{}, nil, nil, nil, nil)
			runner.now = func() time.Time { return store.now }
			err := runner.RunOnce(context.Background())
			if failed {
				require.ErrorIs(t, err, store.err)
				require.Equal(t, []string{"reconcile"}, store.calls)
			} else {
				require.NoError(t, err)
				require.Equal(t, []string{"reconcile", "due"}, store.calls)
			}
		})
	}
}
