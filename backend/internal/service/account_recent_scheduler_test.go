package service

import (
	"context"
	"fmt"
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
			counts := map[int64]int{}
			for range 120 {
				ctx := WithAPIKeyRouteAdmission(context.Background())
				selected, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				counts[selected.Account.ID]++
				if selected.ReleaseFunc != nil {
					selected.ReleaseFunc()
				}
				failed := selected.Account.ID
				FailAPIKeyRouteAccount(ctx, failed)
				selected, _, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.NotEqual(t, failed, selected.Account.ID)
				if selected.ReleaseFunc != nil {
					selected.ReleaseFunc()
				}
			}
			if advanced == "true" {
				require.Equal(t, 120, counts[2], "explicit top-K=1 retains the configured scorer behavior")
			} else {
				require.Len(t, counts, 3, "quality must not hard-filter weaker eligible accounts")
			}
		})
	}
}

func TestConversationAffinityIgnoresOtherConversationsFailures(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		for _, smart := range []bool{false, true} {
			for _, kind := range []string{AccountTypeAPIKey} {
				t.Run(fmt.Sprintf("%s/smart=%v/%s", advanced, smart, kind), func(t *testing.T) {
					resetOpenAIAdvancedSchedulerSettingCacheForTest()
					groupID := int64(20)
					accounts := []Account{
						{ID: 1, Platform: PlatformOpenAI, Type: kind, Status: StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{groupID}},
						{ID: 2, Platform: PlatformOpenAI, Type: kind, Status: StatusActive, Schedulable: true, Concurrency: 5, GroupIDs: []int64{groupID}},
					}
					cache := &recentSchedulerCache{schedulerTestGatewayCache: schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:healthy": 1, "openai:failing": 1}}, stats: map[int64]AccountRecentStats{1: {Failures: 20, ConsecutiveFailures: 1, LastObservedAt: time.Now().Unix()}, 2: {Successes: 100}}}
					cfg := &config.Config{RunMode: config.RunModeSimple}
					cfg.Gateway.OpenAIWS.LBTopK = 1
					cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate = 10
					cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
					svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: cfg, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(advanced, "true"), concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
					ctx := context.Background()
					if smart {
						ctx = WithAPIKeyRouteAdmission(ctx)
					}
					selectAccount := func(session string, excluded map[int64]struct{}) int64 {
						selected, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", session, "gpt-5.1", excluded, OpenAIUpstreamTransportAny, false)
						require.NoError(t, err)
						require.NotNil(t, selected)
						if selected.ReleaseFunc != nil {
							selected.ReleaseFunc()
						}
						return selected.Account.ID
					}
					require.EqualValues(t, 1, selectAccount("healthy", nil))
					if advanced == "true" {
						require.EqualValues(t, 2, selectAccount("", nil), "configured top-K=1 chooses the higher quality candidate")
					}
					require.EqualValues(t, 2, selectAccount("failing", map[int64]struct{}{1: {}}))
					require.EqualValues(t, 2, selectAccount("failing", nil), "recovered conversation retains its new account")
					require.EqualValues(t, 1, selectAccount("healthy", nil), "another conversation must retain its original account")
				})
			}
		}
	}
}

func TestConversationBusyAccountPolicy(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		for _, kind := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			t.Run(advanced+"/"+kind, func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				groupID := int64(20)
				accounts := []Account{
					{ID: 1, Platform: PlatformOpenAI, Type: kind, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}},
					{ID: 2, Platform: PlatformOpenAI, Type: kind, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}},
				}
				cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:conversation": 1}}
				cfg := &config.Config{RunMode: config.RunModeSimple}
				cfg.Gateway.Scheduling.StickySessionMaxWaiting = 2
				cfg.Gateway.Scheduling.StickySessionWaitTimeout = 45 * time.Second
				svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: cfg, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(advanced), concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{1: false, 2: true}, loadMap: map[int64]*AccountLoadInfo{1: {AccountID: 1, LoadRate: 100}, 2: {AccountID: 2}}})}
				selected, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "conversation", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				if selected.ReleaseFunc != nil {
					defer selected.ReleaseFunc()
				}
				if kind == AccountTypeOAuth && advanced == "false" {
					require.EqualValues(t, 1, selected.Account.ID)
					require.NotNil(t, selected.WaitPlan)
					require.Equal(t, 45*time.Second, selected.WaitPlan.Timeout)
				} else {
					require.EqualValues(t, 2, selected.Account.ID)
					require.Nil(t, selected.WaitPlan)
					if kind == AccountTypeOAuth {
						require.EqualValues(t, 1, cache.sessionBindings["openai:conversation"], "reference escape preserves the old binding")
					} else {
						require.EqualValues(t, 2, cache.sessionBindings["openai:conversation"])
					}
				}
			})
		}
	}
}

func TestRecentQualityPreservesOAuthScoresAndMixedPoolCandidates(t *testing.T) {
	accounts := []*Account{{ID: 1, Type: AccountTypeOAuth}, {ID: 2, Type: AccountTypeAPIKey}, {ID: 3, Type: AccountTypeAPIKey}}
	stats := newOpenAIAccountRuntimeStats()
	ttft := 100
	stats.report(1, false, &ttft)
	cache := &recentSchedulerCache{stats: map[int64]AccountRecentStats{1: {Successes: 1000, TTFTCount: 1, TTFTSumMs: 9000}, 2: {Successes: 99, Failures: 1}, 3: {Successes: 98, Failures: 2}}}
	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{cache: cache, cfg: &config.Config{}}, stats: stats}
	plan := scheduler.buildOpenAIAccountLoadPlan(context.Background(), OpenAIAccountScheduleRequest{}, accounts, nil)
	require.Len(t, plan.allCandidates, 3)
	for _, candidate := range plan.allCandidates {
		if candidate.account.ID == 1 {
			require.InDelta(t, 0.2, candidate.errorRate, 0.0001)
			require.Equal(t, float64(100), candidate.ttft)
		}
		if candidate.account.ID == 2 {
			require.InDelta(t, 1-cache.stats[2].Reliability(time.Now()), candidate.errorRate, 0.0001)
		}
	}
	ctx := context.WithValue(context.Background(), recentAccountStatsKey{}, cache.stats)
	oauthOnly := []*Account{{ID: 1, Type: AccountTypeOAuth}, {ID: 4, Type: AccountTypeOAuth}}
	require.False(t, sortByRecentQuality(ctx, oauthOnly))
	require.EqualValues(t, 1, oauthOnly[0].ID)
	counts := map[int64]int{}
	for range 300 {
		ordered := append([]*Account(nil), accounts...)
		require.True(t, sortByRecentQuality(ctx, ordered))
		counts[ordered[0].ID]++
	}
	require.Len(t, counts, 3, "mixed pools retain both account types and near-equal API keys")
}

func TestRecentQualityLeavesUnscoredPriorityTierInOriginalOrder(t *testing.T) {
	ctx := context.WithValue(context.Background(), recentAccountStatsKey{}, map[int64]AccountRecentStats{3: {Successes: 100}})
	for range 30 {
		accounts := []*Account{{ID: 1, Type: AccountTypeOAuth, Priority: 0}, {ID: 2, Type: AccountTypeOAuth, Priority: 0}, {ID: 3, Type: AccountTypeAPIKey, Priority: 1}}
		sortByRecentQuality(ctx, accounts)
		require.EqualValues(t, 1, accounts[0].ID, "a lower-priority API key must not change the OAuth-only tier")
		require.EqualValues(t, 2, accounts[1].ID)
	}
}
