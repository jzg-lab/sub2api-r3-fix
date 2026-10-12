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

func TestOpenAIRecoveryClassifiesTerminalAuthentication(t *testing.T) {
	for _, message := range []string{
		"Token revoked (401): fixture rejection",
		"token_invalidated",
		"token_revoked",
		"refresh_token_reused",
		"refresh_token_invalidated",
		"refresh_token_expired",
		"invalid_refresh_token",
		"app_session_terminated",
		"Your refresh token has already been used to generate a new access token.",
		"Your authentication token has been invalidated.",
		"REFRESH_TOKEN_REUSED",
		`{"error":{"code":"token_revoked","message":"fixture"}}`,
	} {
		t.Run(message, func(t *testing.T) {
			require.Equal(t, "permanent", classifyOpenAIRecovery(message))
		})
	}
	for _, tc := range []struct{ message, want string }{
		{"token_expired", "recoverable"},
		{"access_token_expired", "recoverable"},
		{"temporary network failure", "recoverable"},
		{"upstream 503", "recoverable"},
		{"429 too many requests", "rate_limit"},
		{"", "recoverable"},
		{"not_token_revoked", "recoverable"},
		{"token_revoked_extra", "recoverable"},
	} {
		t.Run(tc.message, func(t *testing.T) {
			require.Equal(t, tc.want, classifyOpenAIRecovery(tc.message))
		})
	}
}

type terminalAuthRecoveryRecords struct {
	OpenAIOperationsRepository
	accountID int64
	completed []OpenAIRecoveryRecord
	actions   []string
}

func (r *terminalAuthRecoveryRecords) RecoveryCandidates(context.Context, time.Time, int) ([]int64, error) {
	return []int64{r.accountID}, nil
}

func (r *terminalAuthRecoveryRecords) ClaimRecovery(_ context.Context, account *Account, now time.Time) (*OpenAIRecoveryRecord, error) {
	return &OpenAIRecoveryRecord{AccountID: account.ID, Attempts: 1, LastAttemptAt: now}, nil
}

func (r *terminalAuthRecoveryRecords) CompleteRecovery(_ context.Context, record OpenAIRecoveryRecord, action string) error {
	r.completed = append(r.completed, record)
	r.actions = append(r.actions, action)
	return nil
}

func TestOpenAIRecoveryConsumesHandlerRevocationWithoutRefresh(t *testing.T) {
	for _, code := range []string{
		"token_revoked", "token_invalidated", "refresh_token_reused", "refresh_token_invalidated",
		"refresh_token_expired", "invalid_refresh_token", "invalid_grant", "app_session_terminated",
		"TOKEN_REVOKED",
	} {
		t.Run(code, func(t *testing.T) {
			before, base, executor, gateway := newUnauthorizedRecoveryFixture()
			repo := &authFailureCASRepo{unauthorizedRecoveryRepo: base}
			require.True(t, (&RateLimitService{accountRepo: repo}).handleOpenAIOAuthUnauthorized(
				context.Background(), before, []byte(`{"error":{"code":"`+code+`"}}`), "fixture rejection"))
			records := &terminalAuthRecoveryRecords{accountID: before.ID}
			operations := &OpenAIOperationsService{
				repo: records, accounts: repo,
				refresh: gateway.openAITokenProvider.refreshAPI, executor: executor,
			}
			require.NoError(t, operations.runRecovery(context.Background(), OpenAIRecoverySettings{
				Enabled: true, IntervalMinutes: 5, FailureThreshold: 3, BackoffMinutes: 60, CooldownMinutes: 10,
			}))
			require.Zero(t, executor.calls.Load(), "revoked credentials must not reach the refresh executor")
			require.Equal(t, []string{"skip"}, records.actions)
			require.Len(t, records.completed, 1)
			require.True(t, records.completed[0].Permanent)
			require.Equal(t, "permanent", records.completed[0].Classification)
			require.Equal(t, "skipped", records.completed[0].Outcome)
			require.Equal(t, StatusError, repo.account.Status)
			require.Equal(t, before.Credentials, repo.account.Credentials)
		})
	}
}

type terminalAuthRefreshExecutor struct {
	OAuthRefreshExecutor
	err       error
	calls     int
	onRefresh func()
}

func (*terminalAuthRefreshExecutor) NeedsRefresh(*Account, time.Duration) bool { return true }

func (e *terminalAuthRefreshExecutor) Refresh(context.Context, *Account) (map[string]any, error) {
	e.calls++
	if e.onRefresh != nil {
		e.onRefresh()
	}
	return nil, e.err
}

type terminalAuthBackgroundRepo struct {
	*authFailureCASRepo
	unconditionalErrors    int
	unconditionalCooldowns int
}

func (r *terminalAuthBackgroundRepo) SetError(_ context.Context, _ int64, message string) error {
	r.unconditionalErrors++
	r.account.Status = StatusError
	r.account.ErrorMessage = message
	return nil
}

func (r *terminalAuthBackgroundRepo) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.unconditionalCooldowns++
	r.account.TempUnschedulableUntil = &until
	r.account.TempUnschedulableReason = reason
	return nil
}

func TestOpenAITokenRefreshPermanentFailureUsesAttemptCAS(t *testing.T) {
	testOpenAITokenRefreshFailureUsesAttemptCAS(t, true)
}

func TestOpenAITokenRefreshExhaustedFailureUsesAttemptCAS(t *testing.T) {
	testOpenAITokenRefreshFailureUsesAttemptCAS(t, false)
}

func testOpenAITokenRefreshFailureUsesAttemptCAS(t *testing.T, permanentFailure bool) {
	t.Helper()
	for _, path := range []string{"unified", "legacy"} {
		for _, mode := range []string{"unchanged", "reauthorized", "proxy", "disabled", "unscheduled", "storage_failure"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				before, base, executor, gateway := newUnauthorizedRecoveryFixture()
				repo := &terminalAuthBackgroundRepo{authFailureCASRepo: &authFailureCASRepo{unauthorizedRecoveryRepo: base}}
				invalidator := &tokenCacheInvalidatorStub{}
				blocker := &tokenRefreshRuntimeBlocker{}
				service := &TokenRefreshService{
					accountRepo: repo, cacheInvalidator: invalidator,
					refreshPolicy: DefaultBackgroundRefreshPolicy(),
					cfg:           &config.TokenRefreshConfig{MaxRetries: 1},
				}
				service.SetAccountRuntimeBlocker(blocker)
				if path == "unified" {
					service.refreshAPI = gateway.openAITokenProvider.refreshAPI
				}
				failing := &terminalAuthRefreshExecutor{
					OAuthRefreshExecutor: executor, err: errors.New("token_revoked"),
					onRefresh: func() {
						switch mode {
						case "reauthorized":
							repo.account.Credentials["access_token"] = "fixture-new"
						case "proxy":
							next := int64(987)
							repo.account.ProxyID = &next
						case "disabled":
							repo.account.Status = StatusDisabled
						case "unscheduled":
							repo.account.Schedulable = !before.Schedulable
						case "storage_failure":
							repo.fail = true
						}
					},
				}
				if !permanentFailure {
					failing.err = errors.New("fixture transport unavailable")
				}
				err := service.refreshWithRetry(context.Background(), before, failing, failing, time.Hour)
				require.Error(t, err)
				require.Equal(t, 1, failing.calls)
				require.Zero(t, repo.unconditionalErrors, "a late refresh failure must never bypass credential CAS")
				require.Zero(t, repo.unconditionalCooldowns, "a late cooldown must never bypass credential CAS")
				if mode == "unchanged" {
					if permanentFailure {
						var permanent *accountPermanentRefreshError
						require.ErrorAs(t, err, &permanent)
						require.True(t, permanent.persistentlyBlocked)
						require.Equal(t, StatusError, repo.account.Status)
						require.Equal(t, 1, invalidator.calls)
					} else {
						require.ErrorIs(t, err, failing.err)
						require.Equal(t, StatusActive, repo.account.Status)
						require.NotNil(t, repo.account.TempUnschedulableUntil)
						require.True(t, repo.account.TempUnschedulableUntil.After(time.Now()))
						require.Zero(t, invalidator.calls)
					}
					require.Equal(t, 1, repo.applied)
					require.Equal(t, 1, blocker.blockCalls)
				} else {
					if mode == "storage_failure" {
						var containment *providerCycleContainmentRefreshError
						require.ErrorAs(t, err, &containment)
					} else {
						require.ErrorIs(t, err, errRefreshSkipped)
					}
					require.Zero(t, repo.applied)
					require.Zero(t, invalidator.calls)
					require.Zero(t, blocker.blockCalls)
					require.NotEqual(t, StatusError, repo.account.Status)
					require.Nil(t, repo.account.TempUnschedulableUntil)
				}
			})
		}
	}
}

func TestOpenAITokenRefreshPermanentFailureRequiresCAS(t *testing.T) {
	testOpenAITokenRefreshFailureRequiresCAS(t, errors.New("token_revoked"))
}

func TestOpenAITokenRefreshExhaustedFailureRequiresCAS(t *testing.T) {
	testOpenAITokenRefreshFailureRequiresCAS(t, errors.New("fixture transport unavailable"))
}

func testOpenAITokenRefreshFailureRequiresCAS(t *testing.T, failure error) {
	t.Helper()
	before, base, executor, gateway := newUnauthorizedRecoveryFixture()
	invalidator := &tokenCacheInvalidatorStub{}
	blocker := &tokenRefreshRuntimeBlocker{}
	service := &TokenRefreshService{
		accountRepo: base, cacheInvalidator: invalidator,
		refreshAPI:    gateway.openAITokenProvider.refreshAPI,
		refreshPolicy: DefaultBackgroundRefreshPolicy(),
		cfg:           &config.TokenRefreshConfig{MaxRetries: 1},
	}
	service.SetAccountRuntimeBlocker(blocker)
	failing := &terminalAuthRefreshExecutor{OAuthRefreshExecutor: executor, err: failure}
	err := service.refreshWithRetry(context.Background(), before, failing, failing, time.Hour)
	var containment *providerCycleContainmentRefreshError
	require.ErrorAs(t, err, &containment)
	require.Equal(t, 1, failing.calls)
	require.Equal(t, StatusActive, base.account.Status)
	require.Zero(t, invalidator.calls)
	require.Zero(t, blocker.blockCalls)
}

func TestOpenAIRecoveryConsumesTerminalMessageWithoutRetry(t *testing.T) {
	for _, message := range []string{
		"Your authentication token has been invalidated.",
		"Your refresh token has already been used to generate a new access token.",
		"app_session_terminated",
	} {
		t.Run(message, func(t *testing.T) {
			before, base, _, _ := newUnauthorizedRecoveryFixture()
			repo := &authFailureCASRepo{unauthorizedRecoveryRepo: base}
			require.True(t, (&RateLimitService{accountRepo: repo}).handleOpenAIOAuthUnauthorized(
				context.Background(), before, []byte(`{"error":{"code":"invalid_token"}}`), message))
			require.Equal(t, StatusError, repo.account.Status)
			require.Equal(t, "permanent", classifyOpenAIRecovery(repo.account.ErrorMessage))
			require.Nil(t, repo.account.TempUnschedulableUntil)
		})
	}
}

func TestOpenAIRecoveryRefreshErrorUsesEndpointContext(t *testing.T) {
	for _, tc := range []struct {
		message   string
		permanent bool
	}{
		{"token_expired", true},
		{`refresh failed: 400 {"error":{"code":"token_expired"}}`, true},
		{"TOKEN_EXPIRED", true},
		{"refresh_token_reused", true},
		{"app_session_terminated", true},
		{"invalid_client", false},
		{"unauthorized_client", false},
		{"invalid_scope", false},
		{"access_denied", false},
		{"missing_project_id", false},
		{"access_token_expired", false},
		{"not_token_expired", false},
		{"token_expired_extra", false},
		{"temporary network failure", false},
	} {
		t.Run(tc.message, func(t *testing.T) {
			before, base, executor, gateway := newUnauthorizedRecoveryFixture()
			base.account.Status = StatusError
			base.account.ErrorMessage = "token_expired"
			records := &terminalAuthRecoveryRecords{accountID: before.ID}
			failing := &terminalAuthRefreshExecutor{OAuthRefreshExecutor: executor, err: errors.New(tc.message)}
			operations := &OpenAIOperationsService{
				repo: records, accounts: base, refresh: gateway.openAITokenProvider.refreshAPI, executor: failing,
			}
			require.NoError(t, operations.runRecovery(context.Background(), OpenAIRecoverySettings{
				Enabled: true, IntervalMinutes: 5, FailureThreshold: 3, BackoffMinutes: 60, CooldownMinutes: 10,
			}))
			require.Equal(t, 1, failing.calls, "expired access tokens may enter recovery once")
			require.Len(t, records.completed, 1)
			require.Equal(t, tc.permanent, records.completed[0].Permanent)
			if tc.permanent {
				require.Equal(t, "permanent", records.completed[0].Classification)
			} else {
				require.Equal(t, "recoverable", records.completed[0].Classification)
			}
			require.Equal(t, "failed", records.completed[0].Outcome)
			require.Equal(t, 1, records.completed[0].ConsecutiveFailures)
			require.Equal(t, StatusError, base.account.Status)
			require.Equal(t, before.Credentials, base.account.Credentials)
		})
	}
}

func TestOpenAIRecoveryRereadStopsNewTerminalFailure(t *testing.T) {
	before, base, executor, gateway := newUnauthorizedRecoveryFixture()
	before.Status = StatusError
	before.ErrorMessage = "token_expired"
	base.account.Status = StatusError
	base.account.ErrorMessage = "Token revoked (401): fixture rejection"
	result, err := gateway.openAITokenProvider.refreshAPI.RecoverOpenAIError(context.Background(), before, executor)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Refreshed)
	require.Zero(t, executor.calls.Load())
	require.Equal(t, before.Credentials, base.account.Credentials)
}

func TestOpenAITerminalAuthenticationNeighborConsumers(t *testing.T) {
	for _, message := range []string{
		"Token revoked (401): fixture rejection",
		"token_invalidated", "refresh_token_reused", "app_session_terminated",
		"Your refresh token has already been used to generate a new access token.",
	} {
		t.Run(message, func(t *testing.T) {
			require.True(t, isNonRetryableRefreshError(errors.New(message)))
			require.True(t, rescueSeedAuthRejected(errors.New(message)))
			require.False(t, isRecoverableOpenAIUnauthorized([]byte(
				`{"error":{"code":"invalid_token","message":"`+message+`"}}`)))
		})
	}
	require.False(t, rescueSeedAuthRejected(nil))
	require.False(t, rescueSeedAuthRejected(errors.New("temporary network failure")))
	require.True(t, isRecoverableOpenAIUnauthorized([]byte(`{"error":{"code":"token_expired"}}`)))
}

func TestOpenAIAuthDiagnosticCodeAllowlist(t *testing.T) {
	require.Equal(t, "token_invalidated", openAIAuthDiagnosticCode("token_invalidated"))
	require.Equal(t, "access_token_expired", openAIAuthDiagnosticCode("access_token_expired"))
	for _, code := range []string{"", "fixture-untrusted-field", "token_revoked\nfixture", "Token_revoked"} {
		require.Equal(t, "unclassified", openAIAuthDiagnosticCode(code))
	}
}
