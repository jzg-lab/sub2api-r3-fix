//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeAccountConcurrencyUsesDefaultOnlyWhenOmitted(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformAnthropic, PlatformGemini, "future-platform"} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			for _, concurrency := range []int{0, 1, 10, 50, 100, 1000} {
				t.Run(fmt.Sprintf("%s/%s/%d", platform, accountType, concurrency), func(t *testing.T) {
					want := concurrency
					if want == 0 {
						want = LocalAccountConcurrency
					}
					require.Equal(t, want, normalizeAccountConcurrency(platform, accountType, concurrency))
				})
			}
		}
	}
}

func TestAccountSchedulingCreatePreservesValues(t *testing.T) {
	for _, concurrency := range []int{0, 1, 75, 1000} {
		account, err := buildAccountForCreate(&CreateAccountInput{Concurrency: concurrency, Priority: 0}, nil)
		require.NoError(t, err)
		want := concurrency
		if want == 0 {
			want = LocalAccountConcurrency
		}
		require.Equal(t, want, account.Concurrency)
		require.Zero(t, account.Priority)
	}
	_, err := buildAccountForCreate(&CreateAccountInput{Concurrency: -1}, nil)
	requireApplicationErrorReason(t, err, "INVALID_ACCOUNT_CONCURRENCY")
}

func TestAccountSchedulingUpdatePreservesOmittedSettings(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Concurrency: 7, Priority: 8, Schedulable: false}
	repo := &updateAccountOveragesRepoStub{account: account}
	svc := &adminServiceImpl{accountRepo: repo}
	updated, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Name: "renamed"})
	require.NoError(t, err)
	require.Equal(t, 7, updated.Concurrency)
	require.Equal(t, 8, updated.Priority)
	concurrency, priority := 75, 0
	updated, err = svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Concurrency: &concurrency, Priority: &priority})
	require.NoError(t, err)
	require.Equal(t, concurrency, updated.Concurrency)
	require.Equal(t, priority, updated.Priority)
	require.False(t, updated.Schedulable)
}

func TestAccountSchedulingBulkPreservesValues(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{}
	svc := &adminServiceImpl{accountRepo: repo}
	concurrency, priority := 75, 0
	_, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{1, 2}, Concurrency: &concurrency, Priority: &priority})
	require.NoError(t, err)
	require.Equal(t, concurrency, *repo.lastBulkUpdate.Concurrency)
	require.Equal(t, priority, *repo.lastBulkUpdate.Priority)
	_, err = svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{1}, Name: "renamed"})
	require.NoError(t, err)
	require.Nil(t, repo.lastBulkUpdate.Concurrency)
	require.Nil(t, repo.lastBulkUpdate.Priority)
}

func TestAccountSchedulingRejectsInvalidUpdatesBeforeAccess(t *testing.T) {
	svc := &adminServiceImpl{}
	for _, concurrency := range []int{-1, 0} {
		_, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Concurrency: &concurrency})
		requireApplicationErrorReason(t, err, "INVALID_ACCOUNT_CONCURRENCY")
		_, err = svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{1}, Concurrency: &concurrency})
		requireApplicationErrorReason(t, err, "INVALID_ACCOUNT_CONCURRENCY")
	}
	priority := -1
	_, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Priority: &priority})
	requireApplicationErrorReason(t, err, "INVALID_ACCOUNT_PRIORITY")
	_, err = svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{1}, Priority: &priority})
	requireApplicationErrorReason(t, err, "INVALID_ACCOUNT_PRIORITY")
}
