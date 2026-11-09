package service

import (
	"context"
	"errors"
	"sort"
	"time"
)

// QuotaGroup keeps the purchased allowance separate from the group pricing a request.
func (s *UserSubscription) QuotaGroup(requestGroup *Group) *Group {
	if s != nil && s.Group != nil {
		return s.Group
	}
	return requestGroup
}

func canBindSubscriptionGroup(user *User, group *Group, subscribedGroups map[int64]bool) bool {
	return len(subscribedGroups) > 0 && (subscribedGroups[group.ID] || user.CanBindGroup(group.ID, group.IsExclusive))
}

func activeSubscriptionGroups(subs []UserSubscription, userID int64, now time.Time) map[int64]bool {
	groups := make(map[int64]bool)
	for _, sub := range subs {
		if sub.UserID == userID && sub.Status == SubscriptionStatusActive && sub.DeletedAt == nil && !sub.StartsAt.After(now) && sub.ExpiresAt.After(now) {
			groups[sub.GroupID] = true
		}
	}
	return groups
}

// ResolveBillingSubscription selects one existing allowance without changing its term or usage.
// An exhausted active subscription does not silently spend the user's wallet balance.
func (s *SubscriptionService) ResolveBillingSubscription(ctx context.Context, user *User, requestGroup *Group) (*UserSubscription, error) {
	return s.resolveSharedSubscription(ctx, user, requestGroup, true, 0)
}

// Batch jobs must fit their complete hold in one allowance before submission.
func (s *SubscriptionService) ResolveBillingSubscriptionForCost(ctx context.Context, user *User, requestGroup *Group, cost float64) (*UserSubscription, error) {
	return s.resolveSharedSubscription(ctx, user, requestGroup, true, cost)
}

// ResolveSubscriptionForUsage does not activate or reset persisted usage windows.
func (s *SubscriptionService) ResolveSubscriptionForUsage(ctx context.Context, user *User, requestGroup *Group) (*UserSubscription, error) {
	return s.resolveSharedSubscription(ctx, user, requestGroup, false, 0)
}

func (s *SubscriptionService) resolveSharedSubscription(ctx context.Context, user *User, requestGroup *Group, maintain bool, cost float64) (*UserSubscription, error) {
	if user == nil || requestGroup == nil || s.userSubRepo == nil {
		return nil, ErrBillingServiceUnavailable
	}
	subs, err := s.userSubRepo.ListActiveByUserID(ctx, user.ID)
	if err != nil {
		return nil, ErrBillingServiceUnavailable.WithCause(err)
	}
	now := s.now()
	groups := make(map[int64]bool)
	active := make([]UserSubscription, 0, len(subs))
	for _, sub := range subs {
		if sub.UserID == user.ID && sub.Status == SubscriptionStatusActive && sub.DeletedAt == nil &&
			!sub.StartsAt.After(now) && sub.ExpiresAt.After(now) {
			active = append(active, sub)
			groups[sub.GroupID] = true
		}
	}
	if requestGroup.IsSubscriptionType() && !canBindSubscriptionGroup(user, requestGroup, groups) {
		if len(active) == 0 {
			return nil, ErrSubscriptionNotFound
		}
		return nil, ErrGroupNotAllowed
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].ExpiresAt.Equal(active[j].ExpiresAt) {
			return active[i].ID < active[j].ID
		}
		return active[i].ExpiresAt.Before(active[j].ExpiresAt)
	})
	var exhausted error
	var firstExhausted *UserSubscription
	for i := range active {
		sub := &active[i]
		if sub.Group == nil && s.groupRepo != nil {
			sub.Group, err = s.groupRepo.GetByID(ctx, sub.GroupID)
			if err != nil {
				return nil, ErrBillingServiceUnavailable.WithCause(err)
			}
		}
		if sub.Group == nil || sub.Group.ID != sub.GroupID {
			return nil, ErrBillingServiceUnavailable
		}
		quotaGroup := sub.Group
		maintenance, checkErr := s.ValidateAndCheckLimits(sub, quotaGroup)
		if maintenance && maintain {
			sub, err = s.EnsureWindowMaintenance(ctx, sub)
			if err != nil {
				return nil, ErrBillingServiceUnavailable.WithCause(err)
			}
			if sub.Group == nil {
				sub.Group = quotaGroup
			}
			_, checkErr = s.ValidateAndCheckLimits(sub, quotaGroup)
		}
		if checkErr == nil && maintain && s.billingCacheService != nil {
			checkErr = s.billingCacheService.checkSubscriptionEligibility(ctx, user.ID, quotaGroup, sub)
		}
		if checkErr == nil && cost > 0 {
			checkErr = s.CheckUsageLimits(ctx, sub, quotaGroup, cost)
		}
		if checkErr == nil {
			return sub, nil
		}
		if !errors.Is(checkErr, ErrDailyLimitExceeded) && !errors.Is(checkErr, ErrWeeklyLimitExceeded) && !errors.Is(checkErr, ErrMonthlyLimitExceeded) {
			return nil, checkErr
		}
		if exhausted == nil {
			exhausted = checkErr
			firstExhausted = sub
		}
	}
	// Read-only usage endpoints may display the exhausted allowance; callers must honor the error.
	return firstExhausted, exhausted
}
