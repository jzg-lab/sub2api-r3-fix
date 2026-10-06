package prober

import (
	"testing"
	"time"
)

func TestDueHonorsAllSchedulingDeadlines(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		state State
		want  bool
	}{
		{name: "new", want: true},
		{name: "probe", state: State{NextProbeAt: now.Add(time.Second)}},
		{name: "quality_backoff", state: State{BackoffUntil: now.Add(time.Second)}},
		{name: "server_cooldown", state: State{RetryNotBefore: now.Add(time.Second)}},
		{name: "expired_probe_with_server_cooldown", state: State{
			NextProbeAt: now.Add(-time.Hour), BackoffUntil: now.Add(-time.Second),
			RetryNotBefore: now.Add(time.Second),
		}},
		{name: "exact_deadline", state: State{
			NextProbeAt: now, BackoffUntil: now, RetryNotBefore: now,
		}, want: true},
		{name: "expired", state: State{
			NextProbeAt: now.Add(-time.Hour), BackoffUntil: now.Add(-time.Hour),
			RetryNotBefore: now.Add(-time.Second),
		}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.Due(now); got != tc.want {
				t.Fatalf("Due()=%v, want %v", got, tc.want)
			}
		})
	}
}
