//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type authFailureCASRepo struct {
	*unauthorizedRecoveryRepo
	applied int
	fail    bool
}

func (r *authFailureCASRepo) ApplyOpenAIAuthStateIfUnchanged(ctx context.Context, before *Account, change OpenAIAuthStateUpdate) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return false, errors.New("fixture state storage unavailable")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	current := r.account
	if !matchesAuthFailureTestAccount(current, before) {
		return false, nil
	}
	if change.ErrorMessage != nil {
		current.Status = StatusError
		current.ErrorMessage = *change.ErrorMessage
	}
	if change.CooldownUntil != nil {
		current.TempUnschedulableUntil = change.CooldownUntil
		current.TempUnschedulableReason = change.CooldownReason
	}
	r.applied++
	return true, nil
}

func TestOpenAIAuthFailureIgnoresStale401(t *testing.T) {
	for _, mode := range []string{"reauthorized", "proxy", "disabled", "unscheduled", "unchanged", "storage_failure"} {
		t.Run(mode, func(t *testing.T) {
			before, base, _, _ := newUnauthorizedRecoveryFixture()
			repo := &authFailureCASRepo{unauthorizedRecoveryRepo: base}
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
			service := &RateLimitService{accountRepo: repo}
			applied := service.handleOpenAIOAuthUnauthorized(context.Background(), before, []byte(`{"error":{"code":"token_revoked"}}`), "fixture rejected")
			require.Equal(t, mode == "unchanged", applied)
			if mode == "unchanged" {
				require.Equal(t, StatusError, repo.account.Status)
				require.Equal(t, 1, repo.applied)
			} else {
				require.Zero(t, repo.applied)
			}
		})
	}
}

func TestOpenAIAuthFailureCooldownAndTestConsumer(t *testing.T) {
	before, base, _, _ := newUnauthorizedRecoveryFixture()
	repo := &authFailureCASRepo{unauthorizedRecoveryRepo: base}
	service := &RateLimitService{accountRepo: repo}
	start := time.Now()
	require.True(t, service.handleOpenAIOAuthUnauthorized(context.Background(), before, []byte(`{"error":{"code":"token_expired"}}`), ""))
	require.Equal(t, StatusActive, repo.account.Status)
	require.NotNil(t, repo.account.TempUnschedulableUntil)
	require.True(t, repo.account.TempUnschedulableUntil.After(start.Add(9*time.Minute)))

	before, base, _, _ = newUnauthorizedRecoveryFixture()
	repo = &authFailureCASRepo{unauthorizedRecoveryRepo: base}
	repo.account.Credentials["access_token"] = "fixture-new"
	setAccountTestAuthError(context.Background(), repo, before, "fixture late error")
	require.Zero(t, repo.applied)
	current, err := repo.GetByID(context.Background(), before.ID)
	require.NoError(t, err)
	setAccountTestAuthError(context.Background(), repo, current, "fixture current error")
	require.Equal(t, 1, repo.applied)
}

func TestOpenAIAuthFailurePreservesPermanentReason(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		message string
		want    string
	}{
		{"revoked", `{"error":{"code":"token_revoked"}}`, "", "Token revoked (401): account authentication permanently revoked"},
		{"invalidated", `{"error":{"code":"token_invalidated"}}`, "fixture revoked", "Token revoked (401): fixture revoked"},
		{"unauthorized", `{"detail":"Unauthorized"}`, "", "Unauthorized (401): account authentication failed permanently"},
		{"unauthorized_message", `{"detail":"Unauthorized"}`, "fixture unauthorized", "Unauthorized (401): fixture unauthorized"},
		{"missing_refresh", `{"error":{"code":"token_expired"}}`, "", "Authentication failed (401): refresh_token missing, cannot recover"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, base, _, _ := newUnauthorizedRecoveryFixture()
			if tc.name == "missing_refresh" {
				delete(before.Credentials, "refresh_token")
				delete(base.account.Credentials, "refresh_token")
			}
			repo := &authFailureCASRepo{unauthorizedRecoveryRepo: base}
			service := &RateLimitService{accountRepo: repo}
			require.True(t, service.handleOpenAIOAuthUnauthorized(context.Background(), before, []byte(tc.body), tc.message))
			require.Equal(t, StatusError, repo.account.Status)
			require.Equal(t, tc.want, repo.account.ErrorMessage)
			require.Nil(t, repo.account.TempUnschedulableUntil)
		})
	}
}

func TestOpenAIAuthFailureBindsActualAttempt(t *testing.T) {
	for _, authorization := range []string{"Bearer fixture-override", "", "Basic fixture", "Bearer fixture-new"} {
		t.Run(authorization, func(t *testing.T) {
			before, base, _, _ := newUnauthorizedRecoveryFixture()
			repo := &authFailureCASRepo{unauthorizedRecoveryRepo: base}
			repo.account.Credentials["access_token"] = "fixture-new"
			req, err := http.NewRequest(http.MethodPost, "http://fixture.invalid/", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", authorization)
			resp := &http.Response{}
			bindOpenAIResponseAccount(resp, req, before)
			attempted := openAIResponseAccount(resp, before)
			require.NotSame(t, before, attempted)
			require.NotEqual(t, "fixture-new", before.GetOpenAIAccessToken())
			applied := (&RateLimitService{accountRepo: repo}).handleOpenAIOAuthUnauthorized(context.Background(), attempted, []byte(`{"detail":"Unauthorized"}`), "")
			require.Equal(t, authorization == "Bearer fixture-new", applied)
			foreign := &Account{ID: before.ID + 1}
			require.Same(t, foreign, openAIResponseAccount(resp, foreign))
		})
	}
}

func TestOpenAIAuthFailureSecond401UsesRefreshedSnapshot(t *testing.T) {
	before, base, _, gateway := newUnauthorizedRecoveryFixture()
	repo := &authFailureCASRepo{unauthorizedRecoveryRepo: base}
	req, err := http.NewRequest(http.MethodPost, "http://fixture.invalid/", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+before.GetOpenAIAccessToken())
	attempts := 0
	resp, err := doOpenAIUpstreamWithRecovery(context.Background(), req, before.Proxy.URL(), before, gateway.openAITokenProvider, false,
		func(*http.Request, string, *Account) (*http.Response, error) {
			attempts++
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"token_expired"}}`))}, nil
		})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 2, attempts)
	attempted := openAIResponseAccount(resp, before)
	require.NotEqual(t, before.GetOpenAIAccessToken(), attempted.GetOpenAIAccessToken())
	require.Equal(t, repo.account.Credentials, attempted.Credentials)
	require.True(t, (&RateLimitService{accountRepo: repo}).handleOpenAIOAuthUnauthorized(context.Background(), attempted, []byte(`{"detail":"Unauthorized"}`), ""))
	require.Equal(t, 1, repo.applied)
}

func TestOpenAIAuthFailureRequiresCAS(t *testing.T) {
	before, repo, _, _ := newUnauthorizedRecoveryFixture()
	message := "fixture rejected"
	applied, err := applyOpenAIAuthState(context.Background(), repo, before, OpenAIAuthStateUpdate{ErrorMessage: &message})
	require.Error(t, err)
	require.False(t, applied)
	require.Equal(t, StatusActive, repo.account.Status)
}

func TestOpenAIAuthFailureProviderRotationBeforeRequest(t *testing.T) {
	for _, changedInFlight := range []bool{false, true} {
		t.Run(map[bool]string{false: "current_attempt", true: "new_authorization_in_flight"}[changedInFlight], func(t *testing.T) {
			before, base, _, gateway := newUnauthorizedRecoveryFixture()
			repo := &authFailureCASRepo{unauthorizedRecoveryRepo: base}
			repo.account.Credentials["access_token"] = "fixture-rotated-access"
			repo.account.Credentials["refresh_token"] = "fixture-rotated-refresh"
			repo.account.Credentials["_token_version"] = int64(2)
			expected := snapshotOAuthRefreshAccount(repo.account)
			req, err := http.NewRequest(http.MethodPost, "http://fixture.invalid/", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+expected.GetOpenAIAccessToken())
			resp, err := doOpenAIUpstreamWithRecovery(context.Background(), req, before.Proxy.URL(), before,
				gateway.openAITokenProvider, false,
				func(_ *http.Request, _ string, attempted *Account) (*http.Response, error) {
					require.Equal(t, expected.Credentials, attempted.Credentials)
					if changedInFlight {
						repo.account.Credentials["access_token"] = "fixture-reauthorized"
					}
					return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"detail":"Unauthorized"}`))}, nil
				})
			require.NoError(t, err)
			defer resp.Body.Close()
			attempted := openAIResponseAccount(resp, before)
			require.Equal(t, expected.Credentials, attempted.Credentials)
			require.NotEqual(t, expected.Credentials, before.Credentials)
			applied := (&RateLimitService{accountRepo: repo}).HandleUpstreamError(
				context.Background(), attempted, 401, nil, []byte(`{"detail":"Unauthorized"}`))
			require.Equal(t, !changedInFlight, applied)
		})
	}
}

func TestOpenAIAuthFailureShadowCapturesParentBeforeRequest(t *testing.T) {
	for _, mode := range []string{"current", "reauthorized", "missing_attempt", "proxy_changed"} {
		t.Run(mode, func(t *testing.T) {
			parent, base, _, gateway := newUnauthorizedRecoveryFixture()
			repo := &authFailureCASRepo{unauthorizedRecoveryRepo: base}
			shadow := snapshotOAuthRefreshAccount(parent)
			shadow.ID++
			shadow.ParentAccountID = &parent.ID
			shadow.Credentials = map[string]any{}
			req, err := http.NewRequest(http.MethodPost, "http://fixture.invalid/", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+parent.GetOpenAIAccessToken())
			resp, err := doOpenAIUpstreamWithRecovery(context.Background(), req, shadow.Proxy.URL(), shadow,
				gateway.openAITokenProvider, false,
				func(_ *http.Request, _ string, attempted *Account) (*http.Response, error) {
					require.Equal(t, shadow.ID, attempted.ID)
					require.Equal(t, parent.Credentials, openAIAuthAttemptAccount(attempted).Credentials)
					if mode == "reauthorized" {
						repo.account.Credentials["access_token"] = "fixture-parent-reauthorized"
					} else if mode == "proxy_changed" {
						id := int64(999)
						repo.account.ProxyID = &id
					}
					return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"detail":"Unauthorized"}`))}, nil
				})
			require.NoError(t, err)
			defer resp.Body.Close()
			attempted := openAIResponseAccount(resp, shadow)
			if mode == "missing_attempt" {
				attempted = shadow
			}
			applied := (&RateLimitService{accountRepo: repo}).HandleUpstreamError(
				context.Background(), attempted, 401, nil, []byte(`{"detail":"Unauthorized"}`))
			require.Equal(t, mode == "current", applied)
			require.Nil(t, shadow.openAIAuthAttempt)
		})
	}
}

func TestOpenAIAuthFailureShadowRefreshPreservesScheduledIdentity(t *testing.T) {
	parent, repo, executor, gateway := newUnauthorizedRecoveryFixture()
	shadow := snapshotOAuthRefreshAccount(parent)
	shadow.ID++
	shadow.ParentAccountID = &parent.ID
	shadow.Credentials = map[string]any{}
	req, err := http.NewRequest(http.MethodPost, "http://fixture.invalid/", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+parent.GetOpenAIAccessToken())
	calls := 0
	resp, err := doOpenAIUpstreamWithRecovery(context.Background(), req, shadow.Proxy.URL(), shadow,
		gateway.openAITokenProvider, false,
		func(_ *http.Request, _ string, attempted *Account) (*http.Response, error) {
			calls++
			require.Equal(t, shadow.ID, attempted.ID)
			if calls == 2 {
				require.Equal(t, repo.account.Credentials, openAIAuthAttemptAccount(attempted).Credentials)
			}
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"token_expired"}}`))}, nil
		})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 2, calls)
	require.EqualValues(t, 1, executor.calls.Load())
	require.Equal(t, repo.account.Credentials, openAIAuthAttemptAccount(openAIResponseAccount(resp, shadow)).Credentials)
}
