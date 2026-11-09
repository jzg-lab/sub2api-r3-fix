//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type sharedSubscriptionRepo struct {
	userSubRepoNoop
	subs   []UserSubscription
	err    error
	resets int
}

func (r *sharedSubscriptionRepo) ListActiveByUserID(context.Context, int64) ([]UserSubscription, error) {
	return append([]UserSubscription(nil), r.subs...), r.err
}

func (r *sharedSubscriptionRepo) GetByID(_ context.Context, id int64) (*UserSubscription, error) {
	for _, sub := range r.subs {
		if sub.ID == id {
			return &sub, nil
		}
	}
	return nil, ErrSubscriptionNotFound
}

func (r *sharedSubscriptionRepo) ResetDailyUsage(_ context.Context, id int64, _ *time.Time, start time.Time) error {
	for i := range r.subs {
		if r.subs[i].ID == id {
			r.subs[i].DailyUsageUSD = 0
			r.subs[i].DailyWindowStart = &start
			r.resets++
		}
	}
	return nil
}

func (r *sharedSubscriptionRepo) GetActiveByUserIDAndGroupID(_ context.Context, userID, groupID int64) (*UserSubscription, error) {
	for _, sub := range r.subs {
		if sub.UserID == userID && sub.GroupID == groupID {
			return &sub, nil
		}
	}
	return nil, ErrSubscriptionNotFound
}

func sharedSubscriptionFixture(t *testing.T) (*SubscriptionService, *sharedSubscriptionRepo, *User, *Group) {
	t.Helper()
	now := time.Now()
	start := now.Add(-time.Hour)
	daily, weekly, monthly := 10.0, 50.0, 150.0
	quotaGroup := &Group{ID: 11, SubscriptionType: SubscriptionTypeSubscription, DailyLimitUSD: &daily, WeeklyLimitUSD: &weekly, MonthlyLimitUSD: &monthly}
	repo := &sharedSubscriptionRepo{subs: []UserSubscription{{
		ID: 31, UserID: 7, GroupID: quotaGroup.ID, Group: quotaGroup,
		Status: SubscriptionStatusActive, StartsAt: start, ExpiresAt: now.Add(24 * time.Hour),
		DailyWindowStart: &now, WeeklyWindowStart: &start, MonthlyWindowStart: &start,
		DailyUsageUSD: 3, WeeklyUsageUSD: 20, MonthlyUsageUSD: 60,
	}}}
	svc := NewSubscriptionService(nil, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }
	t.Cleanup(svc.Stop)
	return svc, repo, &User{ID: 7, Balance: 100}, &Group{ID: 22, RateMultiplier: 2}
}

func TestSharedSubscriptionPreservesLegacyEntitlementAcrossGroups(t *testing.T) {
	svc, repo, user, group := sharedSubscriptionFixture(t)
	original := repo.subs[0]
	for _, kind := range []string{SubscriptionTypeStandard, SubscriptionTypeSubscription} {
		group.SubscriptionType = kind
		sub, err := svc.ResolveBillingSubscription(context.Background(), user, group)
		require.NoError(t, err)
		require.Equal(t, original, *sub, "conversion must not reset or merge stored allowances")
		require.Equal(t, 7.0, *sub.Group.DailyLimitUSD-sub.DailyUsageUSD)
	}
	require.Zero(t, repo.resets)
}

func TestSharedSubscriptionSelectsEarliestUsableAllowance(t *testing.T) {
	svc, repo, user, group := sharedSubscriptionFixture(t)
	later := repo.subs[0]
	later.ID, later.GroupID = 32, 12
	copyGroup := *later.Group
	copyGroup.ID = later.GroupID
	later.Group = &copyGroup
	later.ExpiresAt = later.ExpiresAt.Add(time.Hour)
	repo.subs = []UserSubscription{later, repo.subs[0]}
	sub, err := svc.ResolveBillingSubscription(context.Background(), user, group)
	require.NoError(t, err)
	require.Equal(t, int64(31), sub.ID)
	for _, window := range []string{"daily", "weekly", "monthly"} {
		repo.subs[1].DailyUsageUSD, repo.subs[1].WeeklyUsageUSD, repo.subs[1].MonthlyUsageUSD = 3, 20, 60
		switch window {
		case "daily":
			repo.subs[1].DailyUsageUSD = 10
		case "weekly":
			repo.subs[1].WeeklyUsageUSD = 50
		case "monthly":
			repo.subs[1].MonthlyUsageUSD = 150
		}
		sub, err = svc.ResolveBillingSubscription(context.Background(), user, group)
		require.NoError(t, err)
		require.Equal(t, later.ID, sub.ID, window)
	}
}

func TestSharedSubscriptionDoesNotSpendWalletWhenExhausted(t *testing.T) {
	svc, repo, user, group := sharedSubscriptionFixture(t)
	repo.subs[0].DailyUsageUSD = 10
	sub, err := svc.ResolveBillingSubscription(context.Background(), user, group)
	require.ErrorIs(t, err, ErrDailyLimitExceeded)
	require.NotNil(t, sub, "usage lookup can still display the exhausted allowance")
	require.Equal(t, 100.0, user.Balance)
}

func TestSharedSubscriptionSelectsAllowanceThatFitsBatchHold(t *testing.T) {
	svc, repo, user, group := sharedSubscriptionFixture(t)
	later := repo.subs[0]
	later.ID++
	later.ExpiresAt = later.ExpiresAt.Add(time.Hour)
	later.DailyUsageUSD = 1
	repo.subs = append(repo.subs, later)
	sub, err := svc.ResolveBillingSubscriptionForCost(t.Context(), user, group, 8)
	require.NoError(t, err)
	require.Equal(t, later.ID, sub.ID)
	_, err = svc.ResolveBillingSubscriptionForCost(t.Context(), user, group, 10)
	require.ErrorIs(t, err, ErrDailyLimitExceeded)
}

func TestSharedSubscriptionUsageDoesNotPersistWindowReset(t *testing.T) {
	svc, repo, user, group := sharedSubscriptionFixture(t)
	past := svc.now().Add(-48 * time.Hour)
	repo.subs[0].DailyWindowStart = &past
	repo.subs[0].DailyUsageUSD = 10
	sub, err := svc.ResolveSubscriptionForUsage(t.Context(), user, group)
	require.NoError(t, err)
	require.Zero(t, sub.DailyUsageUSD)
	require.Zero(t, repo.resets)
	require.Equal(t, 10.0, repo.subs[0].DailyUsageUSD)
	require.Equal(t, past, *repo.subs[0].DailyWindowStart)
}

func TestSharedSubscriptionFiltersInactiveOrForeignAllowances(t *testing.T) {
	for _, mutate := range []func(*UserSubscription, time.Time){
		func(s *UserSubscription, now time.Time) { s.ExpiresAt = now },
		func(s *UserSubscription, now time.Time) { s.StartsAt = now.Add(time.Minute) },
		func(s *UserSubscription, _ time.Time) { s.Status = SubscriptionStatusSuspended },
		func(s *UserSubscription, now time.Time) { s.DeletedAt = &now },
		func(s *UserSubscription, _ time.Time) { s.UserID++ },
	} {
		svc, repo, user, group := sharedSubscriptionFixture(t)
		mutate(&repo.subs[0], svc.now())
		sub, err := svc.ResolveBillingSubscription(t.Context(), user, group)
		require.NoError(t, err)
		require.Nil(t, sub)
	}
}

func TestSharedSubscriptionRefreshesExistingWindowBeforeSelection(t *testing.T) {
	svc, repo, user, group := sharedSubscriptionFixture(t)
	past := svc.now().Add(-48 * time.Hour)
	repo.subs[0].DailyWindowStart = &past
	repo.subs[0].DailyUsageUSD = 10
	expiry, weeklyStart := repo.subs[0].ExpiresAt, *repo.subs[0].WeeklyWindowStart
	sub, err := svc.ResolveBillingSubscription(context.Background(), user, group)
	require.NoError(t, err)
	require.Zero(t, sub.DailyUsageUSD)
	require.Equal(t, 1, repo.resets)
	require.Equal(t, expiry, sub.ExpiresAt)
	require.Equal(t, weeklyStart, *sub.WeeklyWindowStart)
	require.Equal(t, 20.0, sub.WeeklyUsageUSD)
}

func TestSharedSubscriptionUsesPurchasedLimitsNotDestinationLimits(t *testing.T) {
	svc, repo, _, group := sharedSubscriptionFixture(t)
	tiny := 1.0
	group.DailyLimitUSD = &tiny
	_, err := svc.ValidateAndCheckLimits(&repo.subs[0], group)
	require.NoError(t, err)
	repo.subs[0].DailyUsageUSD = 10
	_, err = svc.ValidateAndCheckLimits(&repo.subs[0], &Group{ID: 99})
	require.ErrorIs(t, err, ErrDailyLimitExceeded, "a destination with no limit cannot turn a capped subscription into unlimited usage")
}

func TestSharedSubscriptionKeepsPrivateGroupPermissions(t *testing.T) {
	svc, repo, user, group := sharedSubscriptionFixture(t)
	group.SubscriptionType, group.IsExclusive = SubscriptionTypeSubscription, true
	_, err := svc.ResolveBillingSubscription(context.Background(), user, group)
	require.ErrorIs(t, err, ErrGroupNotAllowed)
	user.AllowedGroups = []int64{group.ID}
	_, err = svc.ResolveBillingSubscription(context.Background(), user, group)
	require.NoError(t, err)
	user.AllowedGroups = nil
	group.ID = repo.subs[0].GroupID
	_, err = svc.ResolveBillingSubscription(context.Background(), user, group)
	require.NoError(t, err, "existing private-group entitlement is retained")
}

func TestSharedSubscriptionRejectsLookupFailureAndPreservesBalanceOnlyUsers(t *testing.T) {
	svc, repo, user, group := sharedSubscriptionFixture(t)
	repo.subs = nil
	sub, err := svc.ResolveBillingSubscription(context.Background(), user, group)
	require.NoError(t, err)
	require.Nil(t, sub)
	group.SubscriptionType = SubscriptionTypeSubscription
	_, err = svc.ResolveBillingSubscription(context.Background(), user, group)
	require.ErrorIs(t, err, ErrSubscriptionNotFound)
	repo.err = errors.New("database failure")
	_, err = svc.ResolveBillingSubscription(context.Background(), user, group)
	require.ErrorIs(t, err, ErrBillingServiceUnavailable)
}

func TestSharedSubscriptionFixedChargeUsesSourceQuotaOnBalanceGroup(t *testing.T) {
	svc, repo, user, group := sharedSubscriptionFixture(t)
	cache := &fixedChargeCacheStub{balance: 0, sub: &SubscriptionCacheData{Status: SubscriptionStatusActive}}
	billing := NewBillingCacheService(cache, nil, repo, nil, nil, nil, &config.Config{}, nil)
	t.Cleanup(billing.Stop)
	sub, err := svc.ResolveBillingSubscription(context.Background(), user, group)
	require.NoError(t, err)
	hold, err := billing.ReserveFixedCharge(context.Background(), user, nil, group, sub, 7)
	require.NoError(t, err)
	hold.Release()
	_, err = billing.ReserveFixedCharge(context.Background(), user, nil, group, sub, 7.1)
	require.ErrorIs(t, err, ErrDailyLimitExceeded)
}

func TestSharedSubscriptionBillsDestinationPriceOnBothGateways(t *testing.T) {
	for _, openAI := range []bool{false, true} {
		var firstCost float64
		for _, rate := range []float64{1, 2} {
			usage, ledger := &openAIRecordUsageLogRepoStub{inserted: true}, &openAIRecordUsageBillingRepoStub{}
			key := &APIKey{ID: 100, GroupID: i64p(22), Group: &Group{ID: 22, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: rate}}
			user, account := &User{ID: 7}, &Account{ID: 200}
			sub := &UserSubscription{ID: 31, GroupID: 11, Group: &Group{ID: 11, RateMultiplier: 99}}
			if openAI {
				svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usage, ledger, nil, nil, nil)
				err := svc.RecordUsage(t.Context(), &OpenAIRecordUsageInput{Result: &OpenAIForwardResult{RequestID: "shared-openai", Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 1000, OutputTokens: 500}}, APIKey: key, User: user, Account: account, Subscription: sub})
				require.NoError(t, err)
			} else {
				svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usage, ledger, nil, nil)
				err := svc.RecordUsage(t.Context(), &RecordUsageInput{Result: &ForwardResult{RequestID: "shared-claude", Model: "claude-sonnet-4", Usage: ClaudeUsage{InputTokens: 1000, OutputTokens: 500}}, APIKey: key, User: user, Account: account, Subscription: sub})
				require.NoError(t, err)
			}
			require.NotNil(t, ledger.lastCmd)
			require.Zero(t, ledger.lastCmd.BalanceCost)
			require.Equal(t, sub.ID, *ledger.lastCmd.SubscriptionID)
			require.Equal(t, key.Group.ID, *usage.lastLog.GroupID)
			require.Equal(t, usage.lastLog.ActualCost, ledger.lastCmd.SubscriptionCost)
			require.Greater(t, ledger.lastCmd.SubscriptionCost, 0.0)
			if rate == 1 {
				firstCost = ledger.lastCmd.SubscriptionCost
			} else {
				require.InDelta(t, 2*firstCost, ledger.lastCmd.SubscriptionCost, 1e-8)
			}
		}
	}
}
