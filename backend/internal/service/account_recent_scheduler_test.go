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

func TestConversationAffinityIgnoresOtherConversationsFailures(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		for _, smart := range []bool{false, true} {
			for _, kind := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
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
					require.EqualValues(t, 2, selectAccount("", nil), "new conversations still use quality")
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
				if kind == AccountTypeOAuth {
					require.EqualValues(t, 1, selected.Account.ID)
					require.NotNil(t, selected.WaitPlan)
					require.Equal(t, 45*time.Second, selected.WaitPlan.Timeout)
				} else {
					require.EqualValues(t, 2, selected.Account.ID)
					require.Nil(t, selected.WaitPlan)
					require.EqualValues(t, 2, cache.sessionBindings["openai:conversation"])
				}
			})
		}
	}
}

func TestConversationBusyAccountPolicyGenericScheduler(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, kind := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			for _, freeBackup := range []bool{false, true} {
				t.Run(fmt.Sprintf("batch=%v/%s/free=%v", batch, kind, freeBackup), func(t *testing.T) {
					accounts := []Account{
						{ID: 1, Platform: PlatformAnthropic, Type: kind, Status: StatusActive, Schedulable: true, Concurrency: 1},
						{ID: 2, Platform: PlatformAnthropic, Type: kind, Status: StatusActive, Schedulable: true, Concurrency: 1},
					}
					repo := &mockAccountRepoForPlatform{accounts: accounts, accountsByID: map[int64]*Account{1: &accounts[0], 2: &accounts[1]}}
					cache := &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{"conversation": 1}}
					cfg := testConfig()
					cfg.Gateway.Scheduling.LoadBatchEnabled = batch
					cfg.Gateway.Scheduling.StickySessionMaxWaiting = 2
					cfg.Gateway.Scheduling.StickySessionWaitTimeout = 45 * time.Second
					backupLoad := 100
					if freeBackup {
						backupLoad = 0
					}
					svc := &GatewayService{accountRepo: repo, cache: cache, cfg: cfg, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{1: false, 2: freeBackup}, loadMap: map[int64]*AccountLoadInfo{1: {AccountID: 1, LoadRate: 100}, 2: {AccountID: 2, LoadRate: backupLoad}}})}
					selected, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, "conversation", "", nil, "", 0)
					require.NoError(t, err)
					if selected.ReleaseFunc != nil {
						defer selected.ReleaseFunc()
					}
					if kind == AccountTypeAPIKey && freeBackup {
						require.EqualValues(t, 2, selected.Account.ID)
						require.Nil(t, selected.WaitPlan)
						require.EqualValues(t, 2, cache.sessionBindings["conversation"])
					} else {
						require.NotNil(t, selected.WaitPlan)
						if kind == AccountTypeOAuth {
							require.EqualValues(t, 1, selected.Account.ID)
							require.Equal(t, 45*time.Second, selected.WaitPlan.Timeout)
						}
					}
				})
			}
		}
	}
}
