//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountConcurrencyPreservesExplicitLimits(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformAnthropic, PlatformGemini, "future-platform"} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			for _, concurrency := range []int{0, 1, 10, 50, 100, MaxAccountConcurrency} {
				t.Run(fmt.Sprintf("%s/%s/%d", platform, accountType, concurrency), func(t *testing.T) {
					account, err := buildAccountForCreate(&CreateAccountInput{
						Platform: platform, Type: accountType, Concurrency: concurrency,
					}, map[string]any{})
					require.NoError(t, err)
					expected := concurrency
					if expected == 0 {
						expected = LocalAccountConcurrency
					}
					require.Equal(t, expected, account.Concurrency)
				})
			}
		}
	}
}

func TestAccountConcurrencyRejectsInvalidBeforeWrite(t *testing.T) {
	svc := &adminServiceImpl{}
	accountSvc := &AccountService{}
	for _, limit := range []int{-1, 0, MaxAccountConcurrency + 1} {
		_, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Concurrency: &limit})
		require.Error(t, err)
		_, err = svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
			AccountIDs: []int64{1}, Concurrency: &limit,
		})
		require.Error(t, err)
		_, err = accountSvc.Update(context.Background(), 1, UpdateAccountRequest{Concurrency: &limit})
		require.Error(t, err)
		if limit != 0 {
			_, err = buildAccountForCreate(&CreateAccountInput{Concurrency: limit}, map[string]any{})
			require.Error(t, err)
			_, err = accountSvc.Create(context.Background(), CreateAccountRequest{Concurrency: limit})
			require.Error(t, err)
			_, err = svc.CreateShadow(context.Background(), 1, ShadowOptions{Concurrency: limit})
			require.Error(t, err)
		}
	}
}

func TestAccountConcurrencyExplicitAdminAndServiceUpdates(t *testing.T) {
	repo := &accountBillingSettingsAdminRepo{
		upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
			1: {ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Concurrency: 7, Status: StatusActive},
		}},
	}
	admin := &adminServiceImpl{accountRepo: repo, accountBillingRepo: repo}
	limit := 2
	updated, err := admin.UpdateAccount(t.Context(), 1, &UpdateAccountInput{Concurrency: &limit})
	require.NoError(t, err)
	require.Equal(t, limit, updated.Concurrency)
	require.Equal(t, limit, repo.accounts[1].Concurrency)
	updated, err = admin.UpdateAccount(t.Context(), 1, &UpdateAccountInput{Name: "renamed"})
	require.NoError(t, err)
	require.Equal(t, limit, updated.Concurrency)

	svc := NewAccountService(repo, nil)
	limit = 4
	updated, err = svc.Update(t.Context(), 1, UpdateAccountRequest{Concurrency: &limit})
	require.NoError(t, err)
	require.Equal(t, limit, updated.Concurrency)
	require.Equal(t, limit, repo.accounts[1].Concurrency)
	require.NoError(t, svc.UpdateStatus(t.Context(), 1, StatusActive, ""))
	require.Equal(t, limit, repo.accounts[1].Concurrency)
}

func TestAccountConcurrencyPartialEditAndBulk(t *testing.T) {
	ctx := context.Background()
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Concurrency: 7, Status: StatusActive},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	limit := 3
	updated, err := svc.UpdateAccount(ctx, 1, &UpdateAccountInput{Concurrency: &limit})
	require.NoError(t, err)
	require.Equal(t, 3, updated.Concurrency)
	updated, err = svc.UpdateAccount(ctx, 1, &UpdateAccountInput{Name: "renamed"})
	require.NoError(t, err)
	require.Equal(t, 3, updated.Concurrency, "unrelated edits must not reset the limit")

	limit = 2
	_, err = svc.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{1}, Concurrency: &limit})
	require.NoError(t, err)
	require.Len(t, repo.bulkUpdates, 1)
	require.NotNil(t, repo.bulkUpdates[0].Concurrency)
	require.Equal(t, limit, *repo.bulkUpdates[0].Concurrency)
}
