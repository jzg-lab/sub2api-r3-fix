//go:build unit

package service

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type storedRefreshRepo struct {
	AccountRepository
	mu      sync.Mutex
	account *Account
	saveErr error
	readErr error
	saves   int
	authErr error
	blocks  int
}

func (r *storedRefreshRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return snapshotOAuthRefreshAccount(r.account), r.readErr
}

func (r *storedRefreshRepo) UpdateCredentials(ctx context.Context, _ int64, credentials map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveErr != nil {
		return r.saveErr
	}
	if err := ValidateOpenAIOAuthCredentialSnapshot(ctx, r.account); err != nil {
		return err
	}
	r.account.Credentials = shallowCopyMap(credentials)
	r.saves++
	return nil
}

func (r *storedRefreshRepo) SaveRecoveredOpenAIAccount(_ context.Context, before *Account, credentials map[string]any) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveErr != nil {
		return false, r.saveErr
	}
	if !oauthTokenAccountIdentityMatches(before, r.account) ||
		r.account.Status != StatusError ||
		r.account.ErrorMessage != before.ErrorMessage ||
		!reflect.DeepEqual(before.Credentials, r.account.Credentials) {
		return false, nil
	}
	r.account.Credentials = shallowCopyMap(credentials)
	r.account.Status = StatusActive
	r.account.ErrorMessage = ""
	r.saves++
	return true, nil
}

func (r *storedRefreshRepo) ApplyOpenAIAuthStateIfUnchanged(_ context.Context, before *Account, change OpenAIAuthStateUpdate) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.authErr != nil {
		return false, r.authErr
	}
	if !oauthTokenAccountIdentityMatches(before, r.account) ||
		r.account.Status != before.Status ||
		r.account.Schedulable != before.Schedulable ||
		r.account.ErrorMessage != before.ErrorMessage ||
		!reflect.DeepEqual(before.Credentials, r.account.Credentials) {
		return false, nil
	}
	if change.ErrorMessage != nil {
		r.account.Status = StatusError
		r.account.ErrorMessage = *change.ErrorMessage
		r.blocks++
	}
	return true, nil
}

type storedRefreshExecutor struct {
	calls atomic.Int32
	hook  func()
	err   error
}

func (*storedRefreshExecutor) CanRefresh(a *Account) bool {
	return a != nil && a.IsOpenAI() && a.IsOAuth() && !a.IsCredentialShadow()
}

func (*storedRefreshExecutor) NeedsRefresh(a *Account, window time.Duration) bool {
	return NewOpenAITokenRefresher(nil, nil).NeedsRefresh(a, window)
}

func (*storedRefreshExecutor) CacheKey(a *Account) string { return OpenAITokenCacheKey(a) }

func (e *storedRefreshExecutor) Refresh(_ context.Context, a *Account) (map[string]any, error) {
	e.calls.Add(1)
	if e.hook != nil {
		e.hook()
	}
	if e.err != nil {
		return nil, e.err
	}
	credentials := shallowCopyMap(a.Credentials)
	credentials["access_token"] = "fixture-access-next"
	credentials["refresh_token"] = "fixture-refresh-next"
	credentials["expires_at"] = time.Now().Add(time.Hour).Unix()
	return credentials, nil
}

func storedRefreshFixture() *Account {
	proxyID := int64(31)
	return &Account{
		ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, ProxyID: &proxyID,
		Credentials: map[string]any{
			"access_token": "fixture-access-old", "refresh_token": "fixture-refresh-old",
			"expires_at": time.Now().Add(-time.Minute).Unix(),
		},
	}
}

func TestStoredOpenAIRefreshManualAndBackgroundShareLease(t *testing.T) {
	for _, manualFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "background_first", true: "manual_first"}[manualFirst], func(t *testing.T) {
			account := storedRefreshFixture()
			repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
			api := NewOAuthRefreshAPI(repo, nil)
			entered, proceed := make(chan struct{}), make(chan struct{})
			executor := &storedRefreshExecutor{hook: func() {
				close(entered)
				<-proceed
			}}
			run := func(manual bool) error {
				if manual {
					_, err := api.refreshStoredOpenAIAccount(t.Context(), account, executor, true)
					return err
				}
				_, err := api.RefreshIfNeeded(t.Context(), account, executor, time.Minute)
				return err
			}
			first, second := make(chan error, 1), make(chan error, 1)
			go func() { first <- run(manualFirst) }()
			<-entered
			go func() { second <- run(!manualFirst) }()
			close(proceed)
			require.NoError(t, <-first)
			require.NoError(t, <-second)
			require.EqualValues(t, 1, executor.calls.Load())
			require.Equal(t, 1, repo.saves)
		})
	}
}

func TestStoredOpenAIRefreshConcurrentManualRequestsReuseWinner(t *testing.T) {
	account := storedRefreshFixture()
	repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
	api, executor := NewOAuthRefreshAPI(repo, nil), &storedRefreshExecutor{}
	results := make(chan error, 12)
	for range 12 {
		go func() {
			_, err := api.refreshStoredOpenAIAccount(t.Context(), account, executor, true)
			results <- err
		}()
	}
	for range 12 {
		require.NoError(t, <-results)
	}
	require.EqualValues(t, 1, executor.calls.Load())
	require.Equal(t, 1, repo.saves)
}

func TestStoredOpenAIRefreshImportOnlyRotatesWhenNeeded(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		t.Run(map[bool]string{true: "fresh", false: "expired"}[fresh], func(t *testing.T) {
			account := storedRefreshFixture()
			if fresh {
				account.Credentials["expires_at"] = time.Now().Add(time.Hour).Unix()
			}
			repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
			executor := &storedRefreshExecutor{}
			got, err := NewOAuthRefreshAPI(repo, nil).refreshStoredOpenAIAccount(t.Context(), account, executor, false)
			require.NoError(t, err)
			require.Equal(t, account.ID, got.ID)
			require.EqualValues(t, map[bool]int{true: 0, false: 1}[fresh], executor.calls.Load())
		})
	}
}

func TestStoredOpenAIRefreshFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Account)
	}{
		{"disabled", func(a *Account) { a.Status = "disabled" }},
		{"revoked", func(a *Account) { a.Status, a.ErrorMessage = StatusError, "app_session_terminated" }},
		{"missing_refresh", func(a *Account) { delete(a.Credentials, "refresh_token") }},
		{"blank_refresh", func(a *Account) { a.Credentials["refresh_token"] = " \t " }},
		{"shadow", func(a *Account) { parent := int64(99); a.ParentAccountID = &parent }},
		{"wrong_platform", func(a *Account) { a.Platform = PlatformAnthropic }},
		{"unsaved", func(a *Account) { a.ID = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := storedRefreshFixture()
			tc.change(account)
			executor := &storedRefreshExecutor{}
			_, err := NewOAuthRefreshAPI(&storedRefreshRepo{account: account}, nil).
				refreshStoredOpenAIAccount(t.Context(), account, executor, true)
			require.Error(t, err)
			require.Zero(t, executor.calls.Load())
		})
	}
}

func TestStoredOpenAIRefreshRejectsChangedRouteOrClient(t *testing.T) {
	for _, change := range []func(*Account){
		func(a *Account) { proxyID := int64(99); a.ProxyID = &proxyID },
		func(a *Account) { a.Credentials["client_id"] = "different-client" },
		func(a *Account) { a.Status = "disabled" },
	} {
		selected := storedRefreshFixture()
		current := snapshotOAuthRefreshAccount(selected)
		change(current)
		executor := &storedRefreshExecutor{}
		_, err := NewOAuthRefreshAPI(&storedRefreshRepo{account: current}, nil).
			refreshStoredOpenAIAccount(t.Context(), selected, executor, true)
		require.Error(t, err)
		require.Zero(t, executor.calls.Load())
	}
}

func TestStoredOpenAIRefreshLockFailureOrContentionDoesNotExchange(t *testing.T) {
	for _, lockErr := range []error{nil, errors.New("fixture cache unavailable")} {
		account := storedRefreshFixture()
		cache := &refreshAPICacheStub{lockErr: lockErr}
		executor := &storedRefreshExecutor{}
		_, err := NewOAuthRefreshAPI(&storedRefreshRepo{account: account}, cache).
			refreshStoredOpenAIAccount(t.Context(), account, executor, true)
		require.Error(t, err)
		require.Zero(t, executor.calls.Load())
	}
}

func TestStoredOpenAIRefreshPersistenceFailureDoesNotReplay(t *testing.T) {
	account := storedRefreshFixture()
	repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account), saveErr: errors.New("fixture save failed")}
	executor := &storedRefreshExecutor{}
	_, err := NewOAuthRefreshAPI(repo, nil).refreshStoredOpenAIAccount(t.Context(), account, executor, true)
	require.Error(t, err)
	var containment *providerCycleContainmentRefreshError
	require.ErrorAs(t, err, &containment)
	require.EqualValues(t, 1, executor.calls.Load())
	require.Zero(t, repo.saves)
	require.Equal(t, account.Credentials, repo.account.Credentials)
}

func TestStoredOpenAIRefreshSuccessInvalidatesCache(t *testing.T) {
	account := storedRefreshFixture()
	cache := &refreshAPICacheStub{lockResult: true}
	repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
	_, err := NewOAuthRefreshAPI(repo, cache).
		refreshStoredOpenAIAccount(t.Context(), account, &storedRefreshExecutor{}, true)
	require.NoError(t, err)
	require.Positive(t, cache.deleteCalls)
	require.Equal(t, 1, cache.releaseCalls)
}

func TestStoredOpenAIRefreshMissingCoordinatorFailsClosed(t *testing.T) {
	_, err := NewOpenAIOAuthService(nil, nil).RefreshStoredAccount(t.Context(), storedRefreshFixture(), true)
	require.Error(t, err)
}

func TestStoredOpenAIRefreshRejectsRemovedRefreshCapability(t *testing.T) {
	selected := storedRefreshFixture()
	current := snapshotOAuthRefreshAccount(selected)
	delete(current.Credentials, "refresh_token")
	executor := &storedRefreshExecutor{}
	_, err := NewOAuthRefreshAPI(&storedRefreshRepo{account: current}, nil).
		refreshStoredOpenAIAccount(t.Context(), selected, executor, true)
	require.Error(t, err)
	require.Zero(t, executor.calls.Load())
}

func TestCRSStoredOpenAIRefreshFreshImportDoesNotExchangeOrRepersist(t *testing.T) {
	for _, winner := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh_import", true: "concurrent_winner"}[winner], func(t *testing.T) {
			selected := storedRefreshFixture()
			current := snapshotOAuthRefreshAccount(selected)
			current.Credentials["expires_at"] = time.Now().Add(time.Hour).Unix()
			if !winner {
				selected = snapshotOAuthRefreshAccount(current)
			}
			repo := &storedRefreshRepo{account: current}
			oauth := NewOpenAIOAuthService(nil, nil)
			oauth.refreshAPI = NewOAuthRefreshAPI(repo, nil)
			svc := &CRSSyncService{openaiOAuthService: oauth}
			require.Nil(t, svc.refreshOAuthToken(t.Context(), selected))
			require.Equal(t, current.Credentials, selected.Credentials)
			require.Zero(t, repo.saves)
		})
	}
}

func TestStoredOpenAIRefreshErrorRecoveryReusesWinner(t *testing.T) {
	account := storedRefreshFixture()
	account.Status, account.ErrorMessage = StatusError, "access token expired"
	repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
	api, executor := NewOAuthRefreshAPI(repo, nil), &storedRefreshExecutor{}
	results := make(chan error, 8)
	for range 8 {
		go func() {
			got, err := api.refreshStoredOpenAIAccount(t.Context(), account, executor, true)
			if err == nil && !got.IsActive() {
				err = errors.New("recovery did not return an active durable account")
			}
			results <- err
		}()
	}
	for range 8 {
		require.NoError(t, <-results)
	}
	require.EqualValues(t, 1, executor.calls.Load())
	require.Equal(t, 1, repo.saves)
	require.Equal(t, StatusActive, repo.account.Status)
	require.Empty(t, repo.account.ErrorMessage)
}

func TestStoredOpenAIRefreshReadFailureDoesNotExchange(t *testing.T) {
	account := storedRefreshFixture()
	repo := &storedRefreshRepo{account: account, readErr: errors.New("fixture read failed")}
	executor := &storedRefreshExecutor{}
	_, err := NewOAuthRefreshAPI(repo, nil).refreshStoredOpenAIAccount(t.Context(), account, executor, true)
	require.Error(t, err)
	require.Zero(t, executor.calls.Load())
}

func TestStoredOpenAIRefreshPostPersistStateChangeIsNotPublished(t *testing.T) {
	account := storedRefreshFixture()
	repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
	executor := &storedRefreshExecutor{hook: func() {
		repo.mu.Lock()
		repo.account.Status = "disabled"
		repo.mu.Unlock()
	}}
	_, err := NewOAuthRefreshAPI(repo, nil).refreshStoredOpenAIAccount(t.Context(), account, executor, true)
	require.Error(t, err)
	require.EqualValues(t, 1, executor.calls.Load())
	require.Equal(t, "disabled", repo.account.Status)
}

func TestStoredOpenAIRefreshTerminalFailureStopsQueuedRefreshes(t *testing.T) {
	for _, status := range []string{StatusActive, StatusError} {
		t.Run(status, func(t *testing.T) {
			account := storedRefreshFixture()
			account.Status = status
			if status == StatusError {
				account.ErrorMessage = "access token expired"
			}
			repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
			api := NewOAuthRefreshAPI(repo, nil)
			executor := &storedRefreshExecutor{err: errors.New("refresh_token_reused")}
			results := make(chan error, 8)
			for range 8 {
				go func() {
					_, err := api.refreshStoredOpenAIAccount(t.Context(), account, executor, true)
					results <- err
				}()
			}
			for range 8 {
				require.Error(t, <-results)
			}
			require.EqualValues(t, 1, executor.calls.Load())
			require.Equal(t, 1, repo.blocks)
			require.Equal(t, StatusError, repo.account.Status)
			require.True(t, isTerminalOpenAIAuthFailure(repo.account.ErrorMessage))
			require.Equal(t, account.Credentials, repo.account.Credentials)
		})
	}
}

func TestStoredOpenAIRefreshFailureOnlyQuarantinesExactTerminalGeneration(t *testing.T) {
	for code, terminal := range map[string]bool{
		"invalid_grant": true, "refresh_token_reused": true, "refresh_token_invalidated": true,
		"app_session_terminated": true, "token_revoked": true, "refresh_token_expired": true,
		"token_expired": true, "account_deactivated": true,
		"invalid_client": false, "invalid_scope": false, "rate limit exceeded": false,
		"access token expired": false, "upstream timeout": false,
	} {
		t.Run(code, func(t *testing.T) {
			account := storedRefreshFixture()
			repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
			cache := &refreshAPICacheStub{lockResult: true}
			upstreamErr := errors.New(code)
			_, err := NewOAuthRefreshAPI(repo, cache).refreshStoredOpenAIAccount(t.Context(), account,
				&storedRefreshExecutor{err: upstreamErr}, true)
			require.ErrorIs(t, err, upstreamErr)
			if terminal {
				require.Equal(t, 1, repo.blocks)
				require.Positive(t, cache.deleteCalls)
				require.Equal(t, StatusError, repo.account.Status)
			} else {
				require.Zero(t, repo.blocks)
				require.Zero(t, cache.deleteCalls)
				require.Equal(t, StatusActive, repo.account.Status)
			}
			require.Equal(t, 1, cache.releaseCalls)
			require.Zero(t, repo.saves)
		})
	}
}

func TestStoredOpenAIRefreshTerminalFailurePreservesConcurrentEdits(t *testing.T) {
	for _, change := range []string{"reauthorized", "proxy", "disabled", "scheduling"} {
		t.Run(change, func(t *testing.T) {
			account := storedRefreshFixture()
			repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
			cache := &refreshAPICacheStub{lockResult: true}
			executor := &storedRefreshExecutor{err: errors.New("app_session_terminated"), hook: func() {
				repo.mu.Lock()
				defer repo.mu.Unlock()
				switch change {
				case "reauthorized":
					repo.account.Credentials["refresh_token"] = "fixture-new-authorization"
				case "proxy":
					id := int64(55)
					repo.account.ProxyID = &id
				case "disabled":
					repo.account.Status = "disabled"
				case "scheduling":
					repo.account.Schedulable = !repo.account.Schedulable
				}
			}}
			_, err := NewOAuthRefreshAPI(repo, cache).refreshStoredOpenAIAccount(t.Context(), account, executor, true)
			require.Error(t, err)
			require.Zero(t, repo.blocks)
			require.Zero(t, cache.deleteCalls)
			require.Empty(t, repo.account.ErrorMessage)
		})
	}
}

func TestStoredOpenAIRefreshTerminalFailurePersistenceErrorPreservesCause(t *testing.T) {
	account := storedRefreshFixture()
	stateErr, upstreamErr := errors.New("fixture CAS unavailable"), errors.New("app_session_terminated")
	repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account), authErr: stateErr}
	_, err := NewOAuthRefreshAPI(repo, nil).refreshStoredOpenAIAccount(t.Context(), account,
		&storedRefreshExecutor{err: upstreamErr}, true)
	var containment *providerCycleContainmentRefreshError
	require.ErrorAs(t, err, &containment)
	require.ErrorIs(t, err, upstreamErr)
	require.ErrorIs(t, err, stateErr)
	require.Zero(t, repo.blocks)
	require.Equal(t, StatusActive, repo.account.Status)
}

func TestStoredOpenAIRefreshCanceledFailureCannotQuarantine(t *testing.T) {
	account := storedRefreshFixture()
	repo := &storedRefreshRepo{account: snapshotOAuthRefreshAccount(account)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := NewOAuthRefreshAPI(repo, nil).refreshStoredOpenAIAccount(ctx, account,
		&storedRefreshExecutor{err: errors.New("app_session_terminated"), hook: cancel}, true)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, repo.blocks)
	require.Equal(t, StatusActive, repo.account.Status)
}
