package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIHealthRateLimitUsesSchedulingDeadline(t *testing.T) {
	now := time.Date(2026, 10, 15, 5, 0, 0, 0, time.FixedZone("CST", 8*3600))
	old := now.Add(-time.Hour)
	future := now.Add(time.Second)
	for _, tc := range []struct {
		name         string
		event, reset *time.Time
		limited      bool
	}{
		{"historical event without hold", &old, nil, false},
		{"expired hold", &old, &old, false},
		{"exact reset boundary", &old, &now, false},
		{"active hold", &old, &future, true},
		{"active hold without event", nil, &future, true},
		{"no history", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := OpenAIProbeHealthSnapshot{State: OpenAIDowngradeStateOnDuty,
				RateLimitedAt: tc.event, RateLimitResetAt: tc.reset}
			want := OpenAIHealthLabelNormal
			if tc.limited {
				want = OpenAIHealthLabelRateLimited
			}
			label, _, _, _ := labelOpenAIAccountHealthAt(s, now)
			require.Equal(t, want, label)
			require.Equal(t, tc.limited, s.rateLimitedAt(now))
			s.State = OpenAIDowngradeStatePendingReplace
			if !tc.limited {
				want = OpenAIHealthLabelProblem
			}
			label, _, _, _ = labelOpenAIAccountHealthAt(s, now)
			require.Equal(t, want, label, "expired history must not hide another real failure")
		})
	}
}

func TestOpenAIHealthListClearsExpiredRateLimitWithoutErasingHistory(t *testing.T) {
	now := time.Date(2026, 10, 15, 5, 0, 0, 0, time.UTC)
	event, reset := now.Add(-time.Hour), now.Add(time.Minute)
	store := &healthListerStore{snapshots: []OpenAIProbeHealthSnapshot{{
		AccountID: 42, State: OpenAIDowngradeStateOnDuty, Schedulable: true,
		RateLimitedAt: &event, RateLimitResetAt: &reset,
	}}}
	runner := NewOpenAIDowngradeProbeRunner(store, nil, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	result, err := runner.ListOpenAIAccountHealth(t.Context(), []int64{42})
	require.NoError(t, err)
	require.True(t, result.Accounts[0].RateLimited)
	require.Equal(t, OpenAIHealthLabelRateLimited, result.Accounts[0].Label)

	now = reset
	result, err = runner.ListOpenAIAccountHealth(t.Context(), []int64{42})
	require.NoError(t, err)
	require.False(t, result.Accounts[0].RateLimited)
	require.Equal(t, OpenAIHealthLabelNormal, result.Accounts[0].Label)
	require.Equal(t, event, *store.snapshots[0].RateLimitedAt)
}
