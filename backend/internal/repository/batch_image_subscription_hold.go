package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The job lock serializes capture and release, which use different deduplication IDs.
func applyBatchImageSubscriptionHold(ctx context.Context, tx *sql.Tx, cmd *service.BatchImageBalanceHoldCommand, operation string) (*service.BatchImageBalanceHoldResult, error) {
	if cmd.HoldAmount < 0 || cmd.ActualAmount < 0 || math.IsNaN(cmd.HoldAmount) || math.IsNaN(cmd.ActualAmount) || math.IsInf(cmd.HoldAmount, 0) || math.IsInf(cmd.ActualAmount, 0) {
		return nil, service.ErrBatchImageBillingHoldFailed
	}
	var state string
	var data []byte
	var heldAmount float64
	err := tx.QueryRowContext(ctx, `SELECT billing_hold_state, billing_snapshot, COALESCE(hold_amount, estimated_cost)
FROM batch_image_jobs WHERE batch_id = $1 AND user_id = $2 AND api_key_id = $3 FOR UPDATE`, cmd.BatchID, cmd.UserID, cmd.APIKeyID).Scan(&state, &data, &heldAmount)
	if err != nil {
		return nil, err
	}
	var snapshot service.BatchImageBillingSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	want := cmd.BillingSnapshot
	if snapshot.SubscriptionID <= 0 || snapshot.SubscriptionID != want.SubscriptionID || snapshot.QuotaGroupID != want.QuotaGroupID || snapshot.GroupID != want.GroupID || math.Abs(heldAmount-cmd.HoldAmount) > 0.00000001 ||
		!sameBatchQuotaWindow(snapshot.DailyStart, want.DailyStart) || !sameBatchQuotaWindow(snapshot.WeeklyStart, want.WeeklyStart) || !sameBatchQuotaWindow(snapshot.MonthlyStart, want.MonthlyStart) {
		return nil, service.ErrUsageBillingRequestConflict
	}
	var next string
	switch operation {
	case "reserve":
		if state != "" {
			return nil, service.ErrBatchImageBillingHoldFailed
		}
		if err := reserveBatchSubscriptionQuota(ctx, tx, cmd, &snapshot); err != nil {
			return nil, err
		}
		next = "held"
	case "capture":
		if state == "captured" {
			return &service.BatchImageBalanceHoldResult{}, nil
		}
		if state != "held" || cmd.ActualAmount-cmd.HoldAmount > 0.00000001 {
			return nil, service.ErrBatchImageSettlementCostExceedsHold
		}
		if err := refundBatchSubscriptionQuota(ctx, tx, &snapshot, math.Max(0, cmd.HoldAmount-cmd.ActualAmount)); err != nil {
			return nil, err
		}
		next = "captured"
	case "release":
		if state == "captured" || state == "released" {
			return &service.BatchImageBalanceHoldResult{}, nil
		}
		if state == "held" {
			if err := refundBatchSubscriptionQuota(ctx, tx, &snapshot, cmd.HoldAmount); err != nil {
				return nil, err
			}
		}
		next = "released"
	default:
		return nil, service.ErrBatchImageBillingHoldFailed
	}
	_, err = tx.ExecContext(ctx, `UPDATE batch_image_jobs SET billing_hold_state = $1 WHERE batch_id = $2`, next, cmd.BatchID)
	return &service.BatchImageBalanceHoldResult{}, err
}

func reserveBatchSubscriptionQuota(ctx context.Context, tx *sql.Tx, cmd *service.BatchImageBalanceHoldCommand, snapshot *service.BatchImageBillingSnapshot) error {
	var sub service.UserSubscription
	var group service.Group
	err := tx.QueryRowContext(ctx, `SELECT us.status, us.starts_at, us.expires_at,
us.daily_window_start, us.weekly_window_start, us.monthly_window_start,
us.daily_usage_usd, us.weekly_usage_usd, us.monthly_usage_usd,
g.daily_limit_usd, g.weekly_limit_usd, g.monthly_limit_usd
FROM user_subscriptions us JOIN groups g ON g.id = us.group_id
WHERE us.id = $1 AND us.user_id = $2 AND us.group_id = $3
AND us.deleted_at IS NULL AND g.deleted_at IS NULL FOR UPDATE OF us`, snapshot.SubscriptionID, cmd.UserID, snapshot.QuotaGroupID).Scan(
		&sub.Status, &sub.StartsAt, &sub.ExpiresAt, &sub.DailyWindowStart, &sub.WeeklyWindowStart, &sub.MonthlyWindowStart,
		&sub.DailyUsageUSD, &sub.WeeklyUsageUSD, &sub.MonthlyUsageUSD, &group.DailyLimitUSD, &group.WeeklyLimitUSD, &group.MonthlyLimitUSD)
	if err != nil {
		return err
	}
	now := time.Now()
	if sub.Status != service.SubscriptionStatusActive || sub.StartsAt.After(now) || !sub.ExpiresAt.After(now) {
		return service.ErrSubscriptionExpired
	}
	if !sub.IsWindowActivated() || !sameBatchQuotaWindow(sub.DailyWindowStart, snapshot.DailyStart) || !sameBatchQuotaWindow(sub.WeeklyWindowStart, snapshot.WeeklyStart) || !sameBatchQuotaWindow(sub.MonthlyWindowStart, snapshot.MonthlyStart) {
		// A concurrent reset must be retried with a fresh allowance snapshot.
		return service.ErrBatchImageBillingHoldFailed
	}
	if !sub.CheckDailyLimit(&group, cmd.HoldAmount) {
		return service.ErrDailyLimitExceeded
	}
	if !sub.CheckWeeklyLimit(&group, cmd.HoldAmount) {
		return service.ErrWeeklyLimitExceeded
	}
	if !sub.CheckMonthlyLimit(&group, cmd.HoldAmount) {
		return service.ErrMonthlyLimitExceeded
	}
	_, err = tx.ExecContext(ctx, `UPDATE user_subscriptions SET daily_usage_usd = daily_usage_usd + $1,
weekly_usage_usd = weekly_usage_usd + $1, monthly_usage_usd = monthly_usage_usd + $1, updated_at = NOW() WHERE id = $2`, cmd.HoldAmount, snapshot.SubscriptionID)
	return err
}

func refundBatchSubscriptionQuota(ctx context.Context, tx *sql.Tx, snapshot *service.BatchImageBillingSnapshot, amount float64) error {
	if amount <= 0 {
		return nil
	}
	// Each window is independent: a daily reset must not suppress a weekly refund.
	_, err := tx.ExecContext(ctx, `UPDATE user_subscriptions SET
daily_usage_usd = CASE WHEN daily_window_start IS NOT DISTINCT FROM $1 THEN GREATEST(0, daily_usage_usd - $4) ELSE daily_usage_usd END,
weekly_usage_usd = CASE WHEN weekly_window_start IS NOT DISTINCT FROM $2 THEN GREATEST(0, weekly_usage_usd - $4) ELSE weekly_usage_usd END,
monthly_usage_usd = CASE WHEN monthly_window_start IS NOT DISTINCT FROM $3 THEN GREATEST(0, monthly_usage_usd - $4) ELSE monthly_usage_usd END,
updated_at = NOW() WHERE id = $5`, snapshot.DailyStart, snapshot.WeeklyStart, snapshot.MonthlyStart, amount, snapshot.SubscriptionID)
	return err
}

func sameBatchQuotaWindow(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
