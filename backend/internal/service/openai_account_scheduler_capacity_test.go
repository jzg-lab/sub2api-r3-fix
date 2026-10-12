package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type capacityTrackingConcurrencyCache struct {
	upstreamCostTrackingConcurrencyCache
	rejected map[int64]bool
}

func (c *capacityTrackingConcurrencyCache) AcquireAccountSlot(ctx context.Context, id int64, limit int, requestID string) (bool, error) {
	acquired, err := c.upstreamCostTrackingConcurrencyCache.AcquireAccountSlot(ctx, id, limit, requestID)
	return acquired && !c.rejected[id], err
}

func TestOpenAIAccountSchedulerCapacityOutsideTopK(t *testing.T) {
	for _, tc := range []struct {
		name         string
		knownFull    bool
		rejected     map[int64]bool
		excluded     map[int64]struct{}
		wantID       int64
		wantAcquires int
	}{
		{name: "known full preferred accounts", knownFull: true, wantID: 3, wantAcquires: 1},
		{name: "stale load snapshot", rejected: map[int64]bool{1: true, 2: true}, wantID: 3, wantAcquires: 5},
		{name: "preferred account still available", wantID: 2, wantAcquires: 1},
		{name: "excluded overflow is never used", knownFull: true, excluded: map[int64]struct{}{3: {}}, wantID: 4, wantAcquires: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
			accounts := make([]Account, 0, 4)
			cache := &capacityTrackingConcurrencyCache{
				upstreamCostTrackingConcurrencyCache: upstreamCostTrackingConcurrencyCache{
					loadMap: make(map[int64]*AccountLoadInfo),
				},
				rejected: tc.rejected,
			}
			for id := int64(1); id <= 4; id++ {
				accounts = append(accounts, Account{
					ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Status: StatusActive, Schedulable: true, Concurrency: 50, Priority: int(id),
				})
				cache.loadMap[id] = &AccountLoadInfo{AccountID: id}
				if tc.knownFull && id <= 2 {
					cache.loadMap[id].CurrentConcurrency = 50
					cache.loadMap[id].LoadRate = 100
				}
			}
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.LBTopK = 2
			cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 10
			svc := &OpenAIGatewayService{
				accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
				cfg:                cfg,
				rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
				concurrencyService: NewConcurrencyService(cache),
			}
			groupID := int64(1)
			selection, decision, err := svc.SelectAccountWithScheduler(
				context.Background(), &groupID, "", "capacity-regression", "gpt-test",
				tc.excluded, OpenAIUpstreamTransportAny, false,
			)
			require.NoError(t, err)
			require.NotNil(t, selection)
			if selection.ReleaseFunc != nil {
				defer selection.ReleaseFunc()
			}
			require.True(t, selection.Acquired, "do not queue on a busy shortlist while another eligible account is free")
			require.Nil(t, selection.WaitPlan)
			require.Equal(t, tc.wantID, selection.Account.ID)
			require.Equal(t, 2, decision.TopK)
			require.Equal(t, 4-len(tc.excluded), decision.CandidateCount)
			require.Equal(t, tc.wantAcquires, cache.totalAcquires())
		})
	}
}

func TestOpenAIAccountSchedulerCapacityOverflowRemainsBounded(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
	accounts := make([]*Account, 0, 100)
	for id := int64(1); id <= 100; id++ {
		accounts = append(accounts, &Account{
			ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: true, Concurrency: 50,
		})
	}
	cache := &upstreamCostTrackingConcurrencyCache{rejectAcquire: true}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.LBTopK = 2
	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{
		cfg: cfg, concurrencyService: NewConcurrencyService(cache),
	}}
	budget := newOpenAISelectionProbeBudget()
	attempt := scheduler.trySelectByLoadBalancePool(context.Background(),
		OpenAIAccountScheduleRequest{Platform: PlatformOpenAI}, accounts, nil, budget)
	require.NoError(t, attempt.err)
	require.Nil(t, attempt.result)
	require.True(t, budget.limited)
	require.Equal(t, openAIAccountSelectionProbeLimit, cache.totalAcquires())
}
