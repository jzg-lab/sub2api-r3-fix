package repository

import (
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthLoginIPCreateCannotSeedCallerEvidence(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		t.Run(map[bool]string{false: "unassigned", true: "assigned"}[assigned], func(t *testing.T) {
			db, mock := openAIOAuthPrepareMock(t)
			account := openAIOAuthCreateFixture()
			if !assigned {
				account.ProxyID = nil
			}
			account.Extra[service.OpenAIOAuthLoginExitIPExtraKey] = "198.51.100.99"
			account.Extra[service.OpenAIOAuthQualifiedProxyExtraKey] = int64(99)
			expectOpenAIOAuthIdentityLock(mock)
			expectOpenAIOAuthHistory(mock, sqlmock.NewRows([]string{"credentials", "extra", "proxy_id", "deleted_at"}))
			if assigned {
				expectValidOpenAIOAuthProxy(mock, 7)
			}
			require.NoError(t, prepareOpenAIOAuthAccountCreate(t.Context(), db, account))
			require.NotContains(t, account.Extra, service.OpenAIOAuthLoginExitIPExtraKey)
			require.NotContains(t, account.Extra, service.OpenAIOAuthQualifiedProxyExtraKey)
			require.True(t, account.Extra["caller_value"].(bool))
		})
	}
}

func TestOpenAIOAuthLoginIPRestoredFromDeletedIdentityOnly(t *testing.T) {
	db, mock := openAIOAuthPrepareMock(t)
	account := openAIOAuthCreateFixture()
	account.Extra[service.OpenAIOAuthLoginExitIPExtraKey] = "198.51.100.99"
	expectOpenAIOAuthIdentityLock(mock)
	expectOpenAIOAuthHistory(mock, sqlmock.NewRows([]string{"credentials", "extra", "proxy_id", "deleted_at"}).
		AddRow([]byte(`{"email":"user@example.com"}`),
			[]byte(`{"openai_oauth_qualified_proxy_id":7,"openai_oauth_login_exit_ip":"198.51.100.25"}`),
			int64(7), time.Now().Add(-time.Hour)))
	expectValidOpenAIOAuthProxy(mock, 7)
	require.NoError(t, prepareOpenAIOAuthAccountCreate(t.Context(), db, account))
	require.Equal(t, "198.51.100.25", account.Extra[service.OpenAIOAuthLoginExitIPExtraKey])
}

func TestOpenAIOAuthLoginIPDeletedHistoryConflictOrCorruptionStopsCreate(t *testing.T) {
	for _, extra := range []string{
		`{"openai_oauth_qualified_proxy_id":7,"openai_oauth_login_exit_ip":"198.51.100.99"}`,
		`{"openai_oauth_qualified_proxy_id":7,"openai_oauth_login_exit_ip":123}`,
		`{"openai_oauth_qualified_proxy_id":7,"openai_oauth_login_exit_ip":"127.0.0.1"}`,
	} {
		db, mock := openAIOAuthPrepareMock(t)
		account := openAIOAuthCreateFixture()
		expectOpenAIOAuthIdentityLock(mock)
		expectOpenAIOAuthHistory(mock, sqlmock.NewRows([]string{"credentials", "extra", "proxy_id", "deleted_at"}).
			AddRow([]byte(`{"email":"user@example.com"}`),
				[]byte(`{"openai_oauth_qualified_proxy_id":7,"openai_oauth_login_exit_ip":"198.51.100.25"}`),
				int64(7), time.Now().Add(-2*time.Hour)).
			AddRow([]byte(`{"email":"user@example.com"}`), []byte(extra), int64(7), time.Now().Add(-time.Hour)))
		require.Error(t, prepareOpenAIOAuthAccountCreate(t.Context(), db, account))
	}
}

func TestOpenAIOAuthLoginIPAccountUpdatePreservesHistoricalEvidence(t *testing.T) {
	for _, original := range []string{`{}`, `{"openai_oauth_login_exit_ip":"198.51.100.25"}`} {
		db, mock := openAIOAuthPrepareMock(t)
		account := openAIOAuthCreateFixture()
		account.ID = 42
		account.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
		account.Schedulable = false
		account.Extra[service.OpenAIOAuthLoginExitIPExtraKey] = "198.51.100.99"
		mock.ExpectQuery(`(?s)SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id.*FOR NO KEY UPDATE`).
			WithArgs(account.ID).
			WillReturnRows(sqlmock.NewRows([]string{"id", "platform", "type", "credentials", "extra", "proxy_id", "parent_account_id",
				"status", "schedulable", "updated_at"}).
				AddRow(account.ID, service.PlatformOpenAI, service.AccountTypeOAuth,
					[]byte(`{"email":"user@example.com"}`), []byte(original), int64(7), nil,
					service.StatusActive, false, account.UpdatedAt))
		require.NoError(t, validateOpenAIOAuthAccountUpdate(t.Context(), db, account))
		if original == `{}` {
			require.NotContains(t, account.Extra, service.OpenAIOAuthLoginExitIPExtraKey)
		} else {
			require.Equal(t, "198.51.100.25", account.Extra[service.OpenAIOAuthLoginExitIPExtraKey])
		}
	}
}
