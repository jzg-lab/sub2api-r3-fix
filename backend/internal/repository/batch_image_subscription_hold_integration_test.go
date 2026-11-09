//go:build integration

package repository

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func batchSubscriptionFixture(t *testing.T, amount float64) (*service.BatchImageBalanceHoldCommand, service.UsageBillingRepository) {
	t.Helper()
	client := testEntClient(t)
	user := mustCreateUser(t, client, &service.User{Email: uuid.NewString() + "@example.com", PasswordHash: "hash", Balance: 100})
	var groupIDs []int64
	t.Cleanup(func() {
		ctx := context.Background() // The test context is canceled before cleanup runs.
		for _, query := range []string{
			`DELETE FROM batch_image_jobs WHERE user_id=$1`,
			`DELETE FROM usage_billing_dedup WHERE api_key_id IN (SELECT id FROM api_keys WHERE user_id=$1)`,
			`DELETE FROM users WHERE id=$1`,
		} {
			_, err := integrationDB.ExecContext(ctx, query, user.ID)
			require.NoError(t, err, "clean up batch subscription fixture")
		}
		for _, id := range groupIDs {
			_, err := integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id=$1`, id)
			require.NoError(t, err, "clean up batch subscription group")
		}
	})
	limit := 10.0
	group := mustCreateGroup(t, client, &service.Group{Name: uuid.NewString(), SubscriptionType: service.SubscriptionTypeSubscription, DailyLimitUSD: &limit, WeeklyLimitUSD: &limit, MonthlyLimitUSD: &limit})
	groupIDs = append(groupIDs, group.ID)
	destination := mustCreateGroup(t, client, &service.Group{Name: uuid.NewString(), Platform: service.PlatformGemini})
	groupIDs = append(groupIDs, destination.ID)
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, GroupID: &destination.ID, Key: uuid.NewString(), Name: "batch-subscription"})
	sub := mustCreateSubscription(t, client, &service.UserSubscription{UserID: user.ID, GroupID: group.ID})
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	_, err := integrationDB.ExecContext(t.Context(), `UPDATE user_subscriptions SET daily_window_start=$1, weekly_window_start=$1, monthly_window_start=$1 WHERE id=$2`, start, sub.ID)
	require.NoError(t, err)
	snapshot := &service.BatchImageBillingSnapshot{SubscriptionID: sub.ID, QuotaGroupID: group.ID, GroupID: destination.ID, DailyStart: &start, WeeklyStart: &start, MonthlyStart: &start}
	batchID, err := service.NewBatchImageID()
	require.NoError(t, err)
	job, err := NewBatchImageRepository(integrationDB).CreateBatchImageJob(t.Context(), service.CreateBatchImageJobParams{
		BatchID: batchID, UserID: user.ID, APIKeyID: &key.ID, Provider: service.BatchImageProviderGeminiAPI,
		Model: "gemini-2.5-flash-image", ItemCount: 1, HoldAmount: &amount, BillingSnapshot: snapshot,
	})
	require.NoError(t, err)
	require.Equal(t, snapshot, job.BillingSnapshot, "the quota source and windows survive database round trips")
	return &service.BatchImageBalanceHoldCommand{BatchID: batchID, RequestID: service.BatchImageHoldRequestID(batchID), UserID: user.ID, APIKeyID: key.ID, HoldAmount: amount, BillingSnapshot: snapshot}, NewUsageBillingRepository(client, integrationDB)
}

func assertBatchSubscriptionUsage(t *testing.T, cmd *service.BatchImageBalanceHoldCommand, daily, weekly, monthly float64) {
	t.Helper()
	var d, w, m, balance float64
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT daily_usage_usd, weekly_usage_usd, monthly_usage_usd FROM user_subscriptions WHERE id=$1`, cmd.BillingSnapshot.SubscriptionID).Scan(&d, &w, &m))
	require.InDelta(t, daily, d, 1e-8)
	require.InDelta(t, weekly, w, 1e-8)
	require.InDelta(t, monthly, m, 1e-8)
	require.NoError(t, integrationDB.QueryRowContext(t.Context(), `SELECT balance FROM users WHERE id=$1`, cmd.UserID).Scan(&balance))
	require.Equal(t, 100.0, balance, "subscription jobs never spend the wallet")
}

func TestBatchSubscriptionHoldCaptureAndReleaseAreIdempotent(t *testing.T) {
	cmd, repo := batchSubscriptionFixture(t, 3)
	_, err := repo.ReserveBatchImageBalance(t.Context(), cmd)
	require.NoError(t, err)
	_, err = repo.ReserveBatchImageBalance(t.Context(), cmd)
	require.NoError(t, err)
	assertBatchSubscriptionUsage(t, cmd, 3, 3, 3)
	capture := *cmd
	capture.RequestID, capture.RequestFingerprint, capture.ActualAmount = service.BatchImageCaptureRequestID(cmd.BatchID), "", 1
	_, err = repo.CaptureBatchImageBalance(t.Context(), &capture)
	require.NoError(t, err)
	_, err = repo.CaptureBatchImageBalance(t.Context(), &capture)
	require.NoError(t, err)
	release := *cmd
	release.RequestID, release.RequestFingerprint = service.BatchImageReleaseRequestID(cmd.BatchID), ""
	_, err = repo.ReleaseBatchImageBalance(t.Context(), &release)
	require.NoError(t, err, "a release after capture cannot refund a settled charge")
	assertBatchSubscriptionUsage(t, cmd, 1, 1, 1)
}

func TestBatchSubscriptionRefundDoesNotCreditResetWindows(t *testing.T) {
	for _, reset := range []string{"daily", "weekly", "monthly", "all", "expired"} {
		t.Run(reset, func(t *testing.T) {
			cmd, repo := batchSubscriptionFixture(t, 3)
			_, err := repo.ReserveBatchImageBalance(t.Context(), cmd)
			require.NoError(t, err)
			expected := []float64{1, 1, 1}
			for i, window := range []string{"daily", "weekly", "monthly"} {
				if reset == window || reset == "all" {
					_, err := integrationDB.ExecContext(t.Context(), `UPDATE user_subscriptions SET `+window+`_window_start=NOW(), `+window+`_usage_usd=4 WHERE id=$1`, cmd.BillingSnapshot.SubscriptionID)
					require.NoError(t, err)
					expected[i] = 4
				}
			}
			if reset == "expired" {
				_, err := integrationDB.ExecContext(t.Context(), `UPDATE user_subscriptions SET expires_at=NOW()-INTERVAL '1 second' WHERE id=$1`, cmd.BillingSnapshot.SubscriptionID)
				require.NoError(t, err)
			}
			capture := *cmd
			capture.RequestID, capture.RequestFingerprint, capture.ActualAmount = service.BatchImageCaptureRequestID(cmd.BatchID), "", 1
			_, err = repo.CaptureBatchImageBalance(t.Context(), &capture)
			require.NoError(t, err)
			assertBatchSubscriptionUsage(t, cmd, expected[0], expected[1], expected[2])
		})
	}
}

func TestBatchSubscriptionReleaseBeforeReserveDoesNotRefundUsage(t *testing.T) {
	cmd, repo := batchSubscriptionFixture(t, 3)
	release := *cmd
	release.RequestID = service.BatchImageReleaseRequestID(cmd.BatchID)
	_, err := repo.ReleaseBatchImageBalance(t.Context(), &release)
	require.NoError(t, err)
	_, err = repo.ReserveBatchImageBalance(t.Context(), cmd)
	require.Error(t, err, "stale submit cannot reserve after recovery has released the job")
	assertBatchSubscriptionUsage(t, cmd, 0, 0, 0)
}

func TestBatchSubscriptionReleaseAndConcurrentCaptureDoNotDoubleRefund(t *testing.T) {
	cmd, repo := batchSubscriptionFixture(t, 3)
	_, err := repo.ReserveBatchImageBalance(t.Context(), cmd)
	require.NoError(t, err)
	capture, release := *cmd, *cmd
	capture.RequestID, capture.RequestFingerprint, capture.ActualAmount = service.BatchImageCaptureRequestID(cmd.BatchID), "", 1
	release.RequestID, release.RequestFingerprint = service.BatchImageReleaseRequestID(cmd.BatchID), ""
	var captureErr, releaseErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, captureErr = repo.CaptureBatchImageBalance(t.Context(), &capture) }()
	go func() { defer wg.Done(); _, releaseErr = repo.ReleaseBatchImageBalance(t.Context(), &release) }()
	wg.Wait()
	require.NoError(t, releaseErr)
	if captureErr == nil {
		assertBatchSubscriptionUsage(t, cmd, 1, 1, 1)
	} else {
		require.ErrorIs(t, captureErr, service.ErrBatchImageSettlementCostExceedsHold)
		assertBatchSubscriptionUsage(t, cmd, 0, 0, 0)
	}
}

func TestBatchSubscriptionConcurrentHoldsCannotOverspend(t *testing.T) {
	cmd, repo := batchSubscriptionFixture(t, 3)
	commands := []*service.BatchImageBalanceHoldCommand{cmd}
	for i := 1; i < 8; i++ {
		copy := *cmd
		id, err := service.NewBatchImageID()
		require.NoError(t, err)
		_, err = NewBatchImageRepository(integrationDB).CreateBatchImageJob(t.Context(), service.CreateBatchImageJobParams{
			BatchID: id, UserID: cmd.UserID, APIKeyID: &cmd.APIKeyID, Provider: service.BatchImageProviderGeminiAPI,
			Model: "gemini-2.5-flash-image", ItemCount: 1, HoldAmount: &cmd.HoldAmount, BillingSnapshot: cmd.BillingSnapshot,
		})
		require.NoError(t, err)
		copy.BatchID, copy.RequestID = id, service.BatchImageHoldRequestID(id)
		commands = append(commands, &copy)
	}
	errs := make([]error, len(commands))
	var wg sync.WaitGroup
	for i := range commands {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = repo.ReserveBatchImageBalance(t.Context(), commands[i]) }()
	}
	wg.Wait()
	passed := 0
	for _, err := range errs {
		if err == nil {
			passed++
		} else {
			require.True(t, errors.Is(err, service.ErrDailyLimitExceeded), "%v", err)
		}
	}
	require.Equal(t, 3, passed)
	assertBatchSubscriptionUsage(t, cmd, 9, 9, 9)
}

func TestBatchSubscriptionZeroCostAndWindowRace(t *testing.T) {
	cmd, repo := batchSubscriptionFixture(t, 0)
	_, err := repo.ReserveBatchImageBalance(t.Context(), cmd)
	require.NoError(t, err)
	capture := *cmd
	capture.RequestID, capture.RequestFingerprint = service.BatchImageCaptureRequestID(cmd.BatchID), ""
	_, err = repo.CaptureBatchImageBalance(t.Context(), &capture)
	require.NoError(t, err)
	assertBatchSubscriptionUsage(t, cmd, 0, 0, 0)
	cmd, repo = batchSubscriptionFixture(t, 3)
	_, err = integrationDB.ExecContext(t.Context(), `UPDATE user_subscriptions SET daily_window_start=NOW() WHERE id=$1`, cmd.BillingSnapshot.SubscriptionID)
	require.NoError(t, err)
	_, err = repo.ReserveBatchImageBalance(t.Context(), cmd)
	require.ErrorIs(t, err, service.ErrBatchImageBillingHoldFailed)
	assertBatchSubscriptionUsage(t, cmd, 0, 0, 0)
}

func TestBatchSubscriptionRollbackCheckBlocksUnsettledAccounting(t *testing.T) {
	cmd, repo := batchSubscriptionFixture(t, 3)
	_, err := repo.ReserveBatchImageBalance(t.Context(), cmd)
	require.NoError(t, err)
	check, err := os.ReadFile("../../migrations/rollback/266_batch_image_subscription_billing_check.sql")
	require.NoError(t, err)
	tx := testTx(t)
	_, err = tx.ExecContext(t.Context(), "SAVEPOINT rollback_check")
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), string(check))
	require.ErrorContains(t, err, "subscription batch accounting is still pending")
	_, err = tx.ExecContext(t.Context(), "ROLLBACK TO SAVEPOINT rollback_check")
	require.NoError(t, err)
	// Mark only disposable fixtures closed inside the transaction; cleanup rolls this back.
	_, err = tx.ExecContext(t.Context(), `UPDATE batch_image_jobs SET billing_hold_state='released', status='failed' WHERE billing_snapshot IS NOT NULL`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), string(check))
	require.NoError(t, err)
}
