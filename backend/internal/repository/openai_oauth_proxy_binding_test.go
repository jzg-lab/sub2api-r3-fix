package repository

import (
	"database/sql"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const openAIOAuthHistoryQueryPattern = `(?s)SELECT credentials, extra, proxy_id, deleted_at.*FROM accounts.*FOR UPDATE`

func openAIOAuthCreateFixture() *service.Account {
	proxyID := int64(7)
	return &service.Account{
		Name:        "oauth-create",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		ProxyID:     &proxyID,
		Credentials: map[string]any{
			"email": " User@Example.COM ",
		},
		Extra: map[string]any{"caller_value": true},
	}
}

func openAIOAuthPrepareMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		require.NoError(t, mock.ExpectationsWereMet())
	})
	return db, mock
}

func expectOpenAIOAuthIdentityLock(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs("sub2api:openai-oauth:email:user@example.com").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectOpenAIOAuthHistory(
	mock sqlmock.Sqlmock,
	rows *sqlmock.Rows,
) {
	mock.ExpectQuery(openAIOAuthHistoryQueryPattern).
		WithArgs("email", "user@example.com").
		WillReturnRows(rows)
}

func expectValidOpenAIOAuthProxy(mock sqlmock.Sqlmock, proxyID int64) {
	mock.ExpectQuery(`(?s)SELECT status, expires_at.*FROM proxies.*FOR SHARE`).
		WithArgs(proxyID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "expires_at"}).
			AddRow(service.StatusActive, nil))
}

func expectNonBrowserOpenAIOAuthAccountLock(mock sqlmock.Sqlmock, accountID int64) {
	mock.ExpectQuery(`(?s)SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id.*FOR NO KEY UPDATE`).
		WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "platform", "type", "credentials", "extra", "proxy_id", "parent_account_id",
		}).AddRow(
			accountID,
			service.PlatformOpenAI,
			service.AccountTypeAPIKey,
			[]byte(`{}`),
			[]byte(`{}`),
			nil,
			nil,
		))
}

func expectNonBrowserOpenAIOAuthPATAccountLock(mock sqlmock.Sqlmock, accountID int64) {
	mock.ExpectQuery(`(?s)SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id.*FOR NO KEY UPDATE`).
		WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "platform", "type", "credentials", "extra", "proxy_id", "parent_account_id",
		}).AddRow(
			accountID,
			service.PlatformOpenAI,
			service.AccountTypeOAuth,
			[]byte(`{"auth_mode":"personalAccessToken"}`),
			[]byte(`{}`),
			nil,
			nil,
		))
}

func TestPrepareOpenAIOAuthAccountCreateRequiresStableIdentity(t *testing.T) {
	account := openAIOAuthCreateFixture()
	account.Credentials = map[string]any{}

	err := prepareOpenAIOAuthAccountCreate(t.Context(), nil, account)

	require.ErrorIs(t, err, service.ErrOpenAIOAuthStableIdentityRequired)
}

func TestPrepareOpenAIOAuthAccountCreateRejectsActiveDuplicate(t *testing.T) {
	db, mock := openAIOAuthPrepareMock(t)
	account := openAIOAuthCreateFixture()
	expectOpenAIOAuthIdentityLock(mock)
	expectOpenAIOAuthHistory(mock, sqlmock.NewRows(
		[]string{"credentials", "extra", "proxy_id", "deleted_at"},
	).AddRow(
		[]byte(`{"email":"user@example.com"}`),
		[]byte(`{}`),
		int64(7),
		nil,
	))

	err := prepareOpenAIOAuthAccountCreate(t.Context(), db, account)

	require.ErrorIs(t, err, service.ErrOpenAIOAuthIdentityExists)
}

func TestPrepareOpenAIOAuthAccountCreateRestoresDeletedQualifiedProxy(t *testing.T) {
	db, mock := openAIOAuthPrepareMock(t)
	account := openAIOAuthCreateFixture()
	account.ProxyID = nil
	account.Extra[service.OpenAIOAuthQualifiedProxyExtraKey] = int64(999)
	expectOpenAIOAuthIdentityLock(mock)
	expectOpenAIOAuthHistory(mock, sqlmock.NewRows(
		[]string{"credentials", "extra", "proxy_id", "deleted_at"},
	).AddRow(
		[]byte(`{"email":"user@example.com"}`),
		[]byte(`{"openai_oauth_qualified_proxy_id":7}`),
		int64(7),
		time.Now().Add(-time.Hour),
	))
	expectValidOpenAIOAuthProxy(mock, 7)

	err := prepareOpenAIOAuthAccountCreate(t.Context(), db, account)

	require.NoError(t, err)
	require.Equal(t, int64(7), requireProxyID(t, account.ProxyID))
	require.EqualValues(t, int64(7), account.Extra[service.OpenAIOAuthQualifiedProxyExtraKey])
	require.Equal(t, true, account.Extra[service.OpenAIDowngradeQualificationExtraKey])
	require.Equal(t, true, account.Extra["caller_value"])
	require.False(t, account.Schedulable)
}

func TestPrepareOpenAIOAuthAccountCreateRejectsHistoricalBindingFailures(t *testing.T) {
	tests := []struct {
		name      string
		history   *sqlmock.Rows
		wantError error
	}{
		{
			name: "missing",
			history: sqlmock.NewRows(
				[]string{"credentials", "extra", "proxy_id", "deleted_at"},
			).AddRow(
				[]byte(`{"email":"user@example.com"}`),
				[]byte(`{}`),
				int64(7),
				time.Now().Add(-time.Hour),
			),
			wantError: service.ErrOpenAIOAuthHistoryBindingMissing,
		},
		{
			name: "corrupt",
			history: sqlmock.NewRows(
				[]string{"credentials", "extra", "proxy_id", "deleted_at"},
			).AddRow(
				[]byte(`{"email":"user@example.com"}`),
				[]byte(`{"openai_oauth_qualified_proxy_id":"invalid"}`),
				int64(7),
				time.Now().Add(-time.Hour),
			),
			wantError: service.ErrOpenAIOAuthProxyBindingCorrupt,
		},
		{
			name: "conflict",
			history: sqlmock.NewRows(
				[]string{"credentials", "extra", "proxy_id", "deleted_at"},
			).AddRow(
				[]byte(`{"email":"user@example.com"}`),
				[]byte(`{"openai_oauth_qualified_proxy_id":7}`),
				int64(7),
				time.Now().Add(-2*time.Hour),
			).AddRow(
				[]byte(`{"email":"user@example.com"}`),
				[]byte(`{"openai_oauth_qualified_proxy_id":8}`),
				int64(8),
				time.Now().Add(-time.Hour),
			),
			wantError: service.ErrOpenAIOAuthHistoryBindingConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock := openAIOAuthPrepareMock(t)
			account := openAIOAuthCreateFixture()
			expectOpenAIOAuthIdentityLock(mock)
			expectOpenAIOAuthHistory(mock, tt.history)

			err := prepareOpenAIOAuthAccountCreate(t.Context(), db, account)

			require.ErrorIs(t, err, tt.wantError)
		})
	}
}

func TestPrepareOpenAIOAuthAccountCreateRejectsRequestedProxyMismatch(t *testing.T) {
	db, mock := openAIOAuthPrepareMock(t)
	account := openAIOAuthCreateFixture()
	requestedProxyID := int64(8)
	account.ProxyID = &requestedProxyID
	expectOpenAIOAuthIdentityLock(mock)
	expectOpenAIOAuthHistory(mock, sqlmock.NewRows(
		[]string{"credentials", "extra", "proxy_id", "deleted_at"},
	).AddRow(
		[]byte(`{"email":"user@example.com"}`),
		[]byte(`{"openai_oauth_qualified_proxy_id":7}`),
		int64(7),
		time.Now().Add(-time.Hour),
	))

	err := prepareOpenAIOAuthAccountCreate(t.Context(), db, account)

	require.ErrorIs(t, err, service.ErrOpenAIOAuthProxyMismatch)
}

func TestPrepareOpenAIOAuthAccountCreateRejectsInvalidProxy(t *testing.T) {
	db, mock := openAIOAuthPrepareMock(t)
	account := openAIOAuthCreateFixture()
	expectOpenAIOAuthIdentityLock(mock)
	expectOpenAIOAuthHistory(mock, sqlmock.NewRows(
		[]string{"credentials", "extra", "proxy_id", "deleted_at"},
	))
	mock.ExpectQuery(`(?s)SELECT status, expires_at.*FROM proxies.*FOR SHARE`).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"status", "expires_at"}).
			AddRow(service.StatusDisabled, nil))

	err := prepareOpenAIOAuthAccountCreate(t.Context(), db, account)

	require.ErrorIs(t, err, service.ErrOpenAIOAuthProxyInvalid)
}

func TestPrepareOpenAIOAuthAccountCreateArmsFirstQualification(t *testing.T) {
	db, mock := openAIOAuthPrepareMock(t)
	account := openAIOAuthCreateFixture()
	account.Extra[service.OpenAIOAuthQualifiedProxyExtraKey] = int64(999)
	expectOpenAIOAuthIdentityLock(mock)
	expectOpenAIOAuthHistory(mock, sqlmock.NewRows(
		[]string{"credentials", "extra", "proxy_id", "deleted_at"},
	))
	expectValidOpenAIOAuthProxy(mock, 7)

	err := prepareOpenAIOAuthAccountCreate(t.Context(), db, account)

	require.NoError(t, err)
	_, qualifiedExists := account.Extra[service.OpenAIOAuthQualifiedProxyExtraKey]
	require.False(t, qualifiedExists, "a caller cannot pre-qualify a new authorization proxy")
	require.Equal(t, true, account.Extra[service.OpenAIDowngradeQualificationExtraKey])
	require.False(t, account.Schedulable)
}

func TestPrepareOpenAIOAuthAccountCreateIgnoresNonBrowserOAuthAccounts(t *testing.T) {
	parentID := int64(11)
	tests := []*service.Account{
		{
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeSetupToken,
		},
		{
			Platform:        service.PlatformOpenAI,
			Type:            service.AccountTypeOAuth,
			ParentAccountID: &parentID,
		},
		{
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeOAuth,
			Credentials: map[string]any{
				"openai_auth_mode": "personal_access_token",
			},
		},
		{
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeOAuth,
			Credentials: map[string]any{
				"auth_mode": "agentIdentity",
			},
		},
	}

	for _, account := range tests {
		before := *account
		require.NoError(t, prepareOpenAIOAuthAccountCreate(t.Context(), nil, account))
		require.Equal(t, before, *account)
	}
}

func TestRequiresOpenAIOAuthBulkValidation(t *testing.T) {
	trueValue := true
	falseValue := false
	proxyID := int64(7)
	tests := []struct {
		name    string
		updates service.AccountBulkUpdate
		want    bool
	}{
		{name: "unrelated fields", updates: service.AccountBulkUpdate{Name: stringPtr("renamed")}},
		{name: "ordinary credential refresh", updates: service.AccountBulkUpdate{
			Credentials: map[string]any{"access_token": "replacement"},
		}},
		{name: "pause only", updates: service.AccountBulkUpdate{Schedulable: &falseValue}},
		{name: "proxy", updates: service.AccountBulkUpdate{ProxyID: &proxyID}, want: true},
		{name: "resume", updates: service.AccountBulkUpdate{Schedulable: &trueValue}, want: true},
		{name: "stable account id", updates: service.AccountBulkUpdate{
			Credentials: map[string]any{"chatgpt_account_id": "account"},
		}, want: true},
		{name: "stable email", updates: service.AccountBulkUpdate{
			Credentials: map[string]any{"email": "user@example.com"},
		}, want: true},
		{name: "auth mode", updates: service.AccountBulkUpdate{
			Credentials: map[string]any{"auth_mode": "browser"},
		}, want: true},
		{name: "legacy auth mode", updates: service.AccountBulkUpdate{
			Credentials: map[string]any{"openai_auth_mode": "browser"},
		}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, requiresOpenAIOAuthBulkValidation(tt.updates))
		})
	}
}

func requireProxyID(t *testing.T, proxyID *int64) int64 {
	t.Helper()
	require.NotNil(t, proxyID)
	return *proxyID
}

func stringPtr(value string) *string {
	return &value
}
