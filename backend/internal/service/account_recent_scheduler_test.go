package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type recentSchedulerCache struct {
	schedulerTestGatewayCache
	stats map[int64]AccountRecentStats
}

func (c *recentSchedulerCache) RecordAccountAttempt(context.Context, int64, AccountAttemptObservation, time.Time) error {
	return nil
}
func (c *recentSchedulerCache) GetAccountRecentStats(context.Context, []int64, time.Time) (map[int64]AccountRecentStats, error) {
	return c.stats, nil
}

func TestRecentAccountStatsDriveNewSelectionAndFailover(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		t.Run(advanced, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			groupID := int64(20)
			accounts := []Account{
				{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{groupID}},
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{groupID}},
				{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{groupID}},
			}
			cache := &recentSchedulerCache{stats: map[int64]AccountRecentStats{1: {Successes: 30, Failures: 20}, 2: {Successes: 99, Failures: 1}, 3: {Successes: 80, Failures: 20}}}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Gateway.OpenAIWS.LBTopK = 1
			cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate = 10
			svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: cfg, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(advanced), concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
			ctx := WithAPIKeyRouteAdmission(context.Background())
			selected, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.EqualValues(t, 2, selected.Account.ID)
			if selected.ReleaseFunc != nil {
				selected.ReleaseFunc()
			}
			FailAPIKeyRouteAccount(ctx, 2)
			selected, _, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.EqualValues(t, 3, selected.Account.ID)
			if selected.ReleaseFunc != nil {
				selected.ReleaseFunc()
			}
		})
	}
}

func TestRecentAccountFailureBreaksAffinityButHealthySlowSessionStays(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(20)
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{groupID}},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{groupID}},
	}
	cache := &recentSchedulerCache{schedulerTestGatewayCache: schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:conversation": 1}}, stats: map[int64]AccountRecentStats{1: {Successes: 20, TTFTSumMs: 1200000, TTFTCount: 20}, 2: {Successes: 100, TTFTSumMs: 10000, TTFTCount: 100}}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.OpenAIWS.LBTopK = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate = 10
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT = 10
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: cfg, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true", "true"), concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
	ctx := WithAPIKeyRouteAdmission(context.Background())
	selected, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "conversation", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.EqualValues(t, 1, selected.Account.ID)
	if selected.ReleaseFunc != nil {
		selected.ReleaseFunc()
	}
	cache.stats[1] = AccountRecentStats{Successes: 20, Failures: 1, ConsecutiveFailures: 1, LastObservedAt: time.Now().Unix()}
	selected, _, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "conversation", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.EqualValues(t, 2, selected.Account.ID)
	if selected.ReleaseFunc != nil {
		selected.ReleaseFunc()
	}
}
