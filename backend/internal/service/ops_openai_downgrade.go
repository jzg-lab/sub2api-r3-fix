package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// GetOpenAIDowngradeDashboard returns a redacted, read-only snapshot for the
// admin Ops view. Probe data is deliberately kept separate from usage logs.
func (s *OpsService) GetOpenAIDowngradeDashboard(ctx context.Context, since time.Time, eventLimit, accountLimit int) (*OpenAIDowngradeDashboard, error) {
	if err := s.RequireMonitoringEnabled(ctx); err != nil {
		return nil, err
	}
	if s == nil || s.openAIDowngradeStore == nil {
		return nil, infraerrors.ServiceUnavailable("OPS_OPENAI_PROBE_UNAVAILABLE", "OpenAI downgrade probe store not available")
	}
	if since.IsZero() {
		since = time.Now().UTC().Add(-24 * time.Hour)
	}
	if eventLimit <= 0 || eventLimit > 200 {
		eventLimit = 100
	}
	if accountLimit <= 0 || accountLimit > 500 {
		accountLimit = 200
	}

	dashboard, err := s.openAIDowngradeStore.ListOpenAIDowngradeDashboard(ctx, since)
	if err != nil {
		return nil, err
	}
	if dashboard == nil {
		dashboard = &OpenAIDowngradeDashboard{}
	}
	events, err := s.openAIDowngradeStore.ListOpenAIDowngradeEvents(ctx, since, eventLimit)
	if err != nil {
		return nil, err
	}
	accounts, err := s.openAIDowngradeStore.ListOpenAIDowngradeAccountStats(ctx, since, accountLimit)
	if err != nil {
		return nil, err
	}
	dashboard.RecentEvents = events
	dashboard.AccountStats = accounts
	for _, item := range accounts {
		if item.State == OpenAIDowngradeStateCircuitOpen ||
			item.State == OpenAIDowngradeStateReprobe ||
			item.State == OpenAIDowngradeStatePendingReplace {
			dashboard.DegradedAccountCount++
		}
	}
	for i := range dashboard.Buckets {
		dashboard.Buckets[i].AtCapacity = dashboard.Buckets[i].Capacity > 0 &&
			len(dashboard.Buckets[i].AccountIDs) >= dashboard.Buckets[i].Capacity
		if dashboard.Buckets[i].AtCapacity {
			dashboard.AtCapacityBucketCount++
		}
	}
	return dashboard, nil
}
