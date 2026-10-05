package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIRescueGroupChangesRejectedBeforeWrites(t *testing.T) {
	for _, path := range []string{"admin_single", "account_single", "bulk"} {
		t.Run(path, func(t *testing.T) {
			repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
				1: {ID: 1, Name: "original", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
					GroupIDs: []int64{99}, Extra: map[string]any{openAIRescueLaneExtraKey: "malformed marker still owns groups"}},
			}}}
			groups, name := []int64{}, "edited"
			var err error
			switch path {
			case "admin_single":
				_, err = (&adminServiceImpl{accountRepo: repo}).UpdateAccount(t.Context(), 1, &UpdateAccountInput{Name: name, GroupIDs: &groups})
			case "account_single":
				_, err = NewAccountService(repo, nil).Update(t.Context(), 1, UpdateAccountRequest{Name: &name, GroupIDs: &groups})
			case "bulk":
				_, err = (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(t.Context(), &BulkUpdateAccountsInput{AccountIDs: []int64{1}, GroupIDs: &groups, SkipMixedChannelCheck: true})
			}
			require.ErrorIs(t, err, ErrOpenAIRescueGroupLocked)
			require.Equal(t, "original", repo.accounts[1].Name)
			require.Empty(t, repo.bulkUpdates)
		})
	}
	account := &Account{GroupIDs: []int64{2, 3}, Extra: map[string]any{openAIRescueLaneExtraKey: map[string]any{}}}
	require.NoError(t, ValidateOpenAIRescueGroupEdit(account, []int64{3, 2}))
	require.ErrorIs(t, ValidateOpenAIRescueGroupEdit(account, []int64{2, 2}), ErrOpenAIRescueGroupLocked)
	account.Extra[openAIRescueLaneExtraKey] = nil
	require.NoError(t, ValidateOpenAIRescueGroupEdit(account, []int64{}))
}

func TestOpenAIRescueTerminationSuppressesAutomaticEntry(t *testing.T) {
	account := rescueLaneTestAccount()
	account.Status = StatusActive
	account.Extra[OpenAIRescueTerminatedAtExtraKey] = "stopped"
	repo := &rescueLaneRepo{account: account, roster: []Account{*account}}
	lane := newRescueLaneTestLane(repo, nil)
	require.False(t, ShouldAutoEnterRescueLane(&OpenAIDowngradeMutation{State: &OpenAIDowngradeProbeState{State: OpenAIDowngradeStatePendingReplace}}, account, time.Now()))
	require.ErrorIs(t, lane.EnterRescue(t.Context(), account.ID, OpenAIRescueTriggerAuto), ErrOpenAIRescueTerminated)
	lane.SetProbeStateSource(func(context.Context, []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
		return map[int64]OpenAIProbeHealthSnapshot{account.ID: {State: OpenAIDowngradeStatePendingReplace}}, nil
	})
	entered, _, _, err := lane.RunReconcileSweep(t.Context())
	require.NoError(t, err)
	require.Zero(t, entered)
	require.Empty(t, repo.binds)
}

func TestOpenAIRescueOldSeedCannotChangeRestartedRescue(t *testing.T) {
	for _, authRejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "auth_rejection"}[authRejected], func(t *testing.T) {
			account := rescueLaneTestAccount()
			oldTime := time.Now().UTC()
			account.Extra[openAIRescueLaneExtraKey] = rescueLaneMarkerExtraValue(OpenAIRescueLaneMarker{EnteredAt: oldTime, OrigGroupIDs: []int64{3}})
			repo := &rescueLaneRepo{account: account}
			lane := newRescueLaneTestLane(repo, nil)
			newMarker := rescueLaneMarkerExtraValue(OpenAIRescueLaneMarker{EnteredAt: oldTime.Add(time.Millisecond), OrigGroupIDs: []int64{5}})
			lane.seed = func(context.Context, int64) error {
				account.Extra[openAIRescueLaneExtraKey] = newMarker
				if authRejected {
					return errors.New("API returned 401: revoked")
				}
				return nil
			}
			_ = lane.seedAndAccount(t.Context(), account, OpenAIRescueTriggerManual)
			require.Equal(t, newMarker, account.Extra[openAIRescueLaneExtraKey])
			require.Empty(t, repo.binds)
			require.Len(t, repo.extraSets, 1, "only the old episode attempt reservation precedes restart")
		})
	}
}

func TestOpenAIRescueRestartSynchronizesPluginBeforeSeed(t *testing.T) {
	for _, syncFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "synced", true: "unavailable"}[syncFails], func(t *testing.T) {
			account := rescueLaneTestAccount()
			account.Extra[openAIRescueLaneExtraKey] = rescueLaneMarkerExtraValue(OpenAIRescueLaneMarker{EnteredAt: time.Now().UTC(), OrigGroupIDs: []int64{3}})
			lane := newRescueLaneTestLane(&rescueLaneRepo{account: account}, nil)
			var calls []string
			lane.SetPluginProbePauseSync(func(context.Context) error {
				calls = append(calls, "sync")
				if syncFails {
					return errors.New("plugin unavailable")
				}
				return nil
			})
			lane.seed = func(context.Context, int64) error { calls = append(calls, "seed"); return nil }
			err := lane.seedAndAccount(t.Context(), account, OpenAIRescueTriggerManual)
			if syncFails {
				require.Error(t, err)
				require.Equal(t, []string{"sync"}, calls)
			} else {
				require.NoError(t, err)
				require.Equal(t, []string{"sync", "seed"}, calls)
			}
		})
	}
}
