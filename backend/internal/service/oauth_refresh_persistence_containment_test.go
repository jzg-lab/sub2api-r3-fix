//go:build unit

package service

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOAuthRefreshPersistenceFailureNeverReplaysConsumedCredentials(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformAntigravity} {
		for _, unified := range []bool{false, true} {
			for failureName, failure := range map[string]error{
				"stale": ErrOAuthReauthorizationStale, "database unavailable": errors.New("database unavailable"),
			} {
				name := platform + "/compatibility/" + failureName
				if unified {
					name = platform + "/unified/" + failureName
				}
				t.Run(name, func(t *testing.T) {
					account := &Account{
						ID: 71, Platform: platform, Type: AccountTypeOAuth, Status: StatusActive,
						Credentials: map[string]any{"access_token": "fixture-before"},
					}
					original := maps.Clone(account.Credentials)
					repo := &tokenRefreshAccountRepo{updateErr: failure}
					repo.accountsByID = map[int64]*Account{account.ID: account}
					cache := &tokenCacheInvalidatorStub{}
					svc := &TokenRefreshService{
						accountRepo: repo, cacheInvalidator: cache,
						cfg: &config.TokenRefreshConfig{MaxRetries: 3},
					}
					refresher := &tokenRefresherStub{
						credentials: map[string]any{"access_token": "fixture-after"},
					}
					var executor OAuthRefreshExecutor
					if unified {
						svc.refreshAPI = NewOAuthRefreshAPI(repo, nil)
						executor = refresher
					}

					err := svc.refreshWithRetry(t.Context(), account, refresher, executor, time.Minute)
					var contained *providerCycleContainmentRefreshError
					require.ErrorAs(t, err, &contained)
					require.ErrorIs(t, err, errOAuthRefreshCredentialPersist)
					require.ErrorIs(t, err, failure)
					require.Equal(t, 1, refresher.calls)
					require.Equal(t, 1, repo.updateCredentialsCalls)
					require.Zero(t, repo.setErrorCalls)
					require.Zero(t, repo.setTempUnschedCalls)
					require.Zero(t, repo.clearTempCalls)
					require.Zero(t, cache.calls)
					require.Equal(t, original, account.Credentials)
				})
			}
		}
	}
}

type mutatingRefreshExecutor struct {
	refreshAPIExecutorStub
	next map[string]any
}

func (e *mutatingRefreshExecutor) Refresh(_ context.Context, account *Account) (map[string]any, error) {
	e.refreshCalls++
	account.Credentials = maps.Clone(e.next)
	return maps.Clone(e.next), nil
}

type snapshotRefreshRepo struct {
	openAICredentialSnapshotRepo
}

func (r *snapshotRefreshRepo) GetByID(context.Context, int64) (*Account, error) {
	return snapshotOAuthRefreshAccount(r.current), nil
}

func TestOAuthRefreshPersistenceBindsPreProviderSnapshot(t *testing.T) {
	account := openAIOAuthAccountEditFixture()
	account.Status = StatusActive
	repo := &snapshotRefreshRepo{openAICredentialSnapshotRepo: openAICredentialSnapshotRepo{current: account}}
	next := maps.Clone(account.Credentials)
	next["access_token"] = "fixture-after"
	executor := &mutatingRefreshExecutor{
		refreshAPIExecutorStub: refreshAPIExecutorStub{needsRefresh: true},
		next:                   next,
	}
	api := NewOAuthRefreshAPI(repo, nil)

	result, err := api.RefreshIfNeeded(t.Context(), snapshotOAuthRefreshAccount(account), executor, time.Minute)
	require.NoError(t, err)
	require.True(t, result.Refreshed)
	require.Equal(t, 1, executor.refreshCalls)
	require.Equal(t, 1, repo.writes)
	require.Equal(t, next["access_token"], result.Account.Credentials["access_token"])
	require.Equal(t, result.Account.Credentials, account.Credentials)
}

func TestOAuthRefreshPersistenceCompatibilityPreservesSnapshotOnFailure(t *testing.T) {
	account := openAIOAuthAccountEditFixture()
	account.Status = StatusActive
	original := maps.Clone(account.Credentials)
	repo := &snapshotRefreshRepo{openAICredentialSnapshotRepo: openAICredentialSnapshotRepo{
		current: account, failure: errors.New("database unavailable"),
	}}
	next := maps.Clone(account.Credentials)
	next["access_token"] = "fixture-after"
	refresher := &mutatingRefreshExecutor{
		refreshAPIExecutorStub: refreshAPIExecutorStub{needsRefresh: true},
		next:                   next,
	}
	svc := &TokenRefreshService{
		accountRepo: repo, cfg: &config.TokenRefreshConfig{MaxRetries: 3},
	}

	err := svc.refreshWithRetry(t.Context(), account, refresher, nil, time.Minute)
	require.ErrorIs(t, err, errOAuthRefreshCredentialPersist)
	require.Equal(t, 1, refresher.refreshCalls)
	require.Zero(t, repo.writes)
	require.Equal(t, original, account.Credentials)
}
