package repository

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type reauthJSONCheck func(map[string]any) bool

func (f reauthJSONCheck) Match(value driver.Value) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	var decoded map[string]any
	return json.Unmarshal([]byte(text), &decoded) == nil && f(decoded)
}

func expectReauthLock(mock sqlmock.Sqlmock, stamp time.Time, platform, credentials, extra string) {
	mock.ExpectQuery(`(?s)SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id,.*FOR NO KEY UPDATE`).
		WithArgs(int64(71)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "platform", "type", "credentials", "extra", "proxy_id",
			"parent_account_id", "status", "schedulable", "updated_at",
		}).AddRow(71, platform, service.AccountTypeOAuth, credentials, extra,
			nil, nil, service.StatusError, false, stamp))
}

func TestMergeReauthorizedCredentialsPreservesConfiguration(t *testing.T) {
	before := map[string]any{
		"access_token": "old", "refresh_token": "old-refresh", "id_token": "old-id",
		"expires_at": "old-expiry", "scope": "old-scope", "cookie": "discard",
		"model_mapping": map[string]any{"allowed": "allowed"},
		"auth_mode":     "oauth", "email": "account@example.test",
		"_token_version": time.Now().Add(time.Hour).UnixMilli(),
	}
	incoming := map[string]any{"access_token": "replacement", "password": "discard"}
	original, input := maps.Clone(before), maps.Clone(incoming)
	got := mergeReauthorizedCredentials(before, incoming)
	require.Equal(t, original, before)
	require.Equal(t, input, incoming)
	require.Equal(t, before["model_mapping"], got["model_mapping"])
	require.Equal(t, before["email"], got["email"])
	require.Equal(t, "replacement", got["access_token"])
	require.Greater(t, got["_token_version"], before["_token_version"])
	for _, key := range []string{"refresh_token", "id_token", "expires_at", "scope", "cookie", "password"} {
		require.NotContains(t, got, key)
	}
}

func TestApplyOAuthCredentialsRejectsInvalidInputBeforeTransaction(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stamp time.Time
		kind  string
		creds map[string]any
	}{
		{"missing revision", time.Time{}, service.AccountTypeOAuth, map[string]any{"access_token": "new"}},
		{"wrong type", time.Now(), service.AccountTypeAPIKey, map[string]any{"access_token": "new"}},
		{"empty token", time.Now(), service.AccountTypeOAuth, map[string]any{"access_token": "  "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := atomicCreateRepository(t)
			_, err := repo.ApplyOAuthCredentials(t.Context(), 71, tc.stamp, tc.kind, tc.creds, nil)
			require.Error(t, err)
		})
	}
}

func TestApplyOAuthCredentialsStaleCallbackRollsBack(t *testing.T) {
	repo, mock := atomicCreateRepository(t)
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	mock.ExpectBegin()
	expectReauthLock(mock, stamp, service.PlatformAnthropic, `{}`, `{}`)
	mock.ExpectRollback()
	_, err := repo.ApplyOAuthCredentials(t.Context(), 71, stamp.Add(-time.Microsecond),
		service.AccountTypeOAuth, map[string]any{"access_token": "new"}, nil)
	require.ErrorIs(t, err, service.ErrOAuthReauthorizationStale)
}

func TestApplyOAuthCredentialsTransactionFailures(t *testing.T) {
	for _, failure := range []string{"begin", "lock", "marshal", "update", "cas", "outbox", "snapshot"} {
		t.Run(failure, func(t *testing.T) {
			repo, mock := atomicCreateRepository(t)
			stamp := time.Now().UTC().Truncate(time.Microsecond)
			injected := errors.New("injected reauthorization transaction failure")
			creds := map[string]any{"access_token": "new"}
			begin := mock.ExpectBegin()
			if failure == "begin" {
				begin.WillReturnError(injected)
			} else {
				if failure == "lock" {
					mock.ExpectQuery(`(?s)SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id,.*FOR NO KEY UPDATE`).
						WillReturnError(injected)
				} else {
					expectReauthLock(mock, stamp, service.PlatformAnthropic,
						`{"refresh_token":"obsolete","model_mapping":{"only":"only"}}`,
						`{"codex_fingerprint_seed":"kept","model_rate_limits":{"old":true}}`)
					if failure == "marshal" {
						creds["invalid"] = make(chan int)
					} else {
						update := mock.ExpectExec(`(?s)UPDATE accounts.*updated_at = NOW\(\).*AND updated_at = \$7`).
							WithArgs(service.AccountTypeOAuth,
								reauthJSONCheck(func(m map[string]any) bool {
									_, hasOldRefresh := m["refresh_token"]
									mapping, ok := m["model_mapping"].(map[string]any)
									return m["access_token"] == "new" && !hasOldRefresh && ok && mapping["only"] == "only"
								}),
								reauthJSONCheck(func(m map[string]any) bool {
									_, stale := m["model_rate_limits"]
									return m["codex_fingerprint_seed"] == "kept" && !stale
								}), service.StatusActive, true, int64(71), stamp)
						switch failure {
						case "update":
							update.WillReturnError(injected)
						case "cas":
							update.WillReturnResult(sqlmock.NewResult(0, 0))
						default:
							update.WillReturnResult(sqlmock.NewResult(0, 1))
							outbox := mock.ExpectExec(`INSERT INTO scheduler_outbox`)
							if failure == "outbox" {
								outbox.WillReturnError(injected)
							} else {
								outbox.WillReturnResult(sqlmock.NewResult(1, 1))
								mock.ExpectQuery(`SELECT .* FROM "accounts"`).WillReturnError(injected)
							}
						}
					}
				}
				mock.ExpectRollback()
			}
			got, err := repo.ApplyOAuthCredentials(t.Context(), 71, stamp,
				service.AccountTypeOAuth, creds, map[string]any{
					"codex_fingerprint_seed": "overwrite", "model_rate_limits": map[string]any{"stale": true},
				})
			require.Nil(t, got)
			switch failure {
			case "cas":
				require.ErrorIs(t, err, service.ErrOAuthReauthorizationStale)
			case "marshal":
				var unsupported *json.UnsupportedTypeError
				require.ErrorAs(t, err, &unsupported)
			default:
				require.ErrorIs(t, err, injected)
			}
		})
	}
}

func expectReauthSnapshot(mock sqlmock.Sqlmock, stamp time.Time, schedulable bool) {
	mock.ExpectQuery(`SELECT .* FROM "accounts"`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "platform", "type", "credentials", "extra", "status", "schedulable", "updated_at",
		}).AddRow(71, service.PlatformOpenAI, service.AccountTypeOAuth,
			`{"access_token":"new"}`, `{}`, service.StatusActive, schedulable, stamp))
	mock.ExpectQuery(`SELECT .* FROM "account_groups"`).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id", "priority"}))
	mock.ExpectQuery(`SELECT .* FROM "accounts"`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "updated_at"}).AddRow(71, stamp))
}

func TestApplyOAuthCredentialsReadsSnapshotBeforeCommit(t *testing.T) {
	for _, commitFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "commit failure"}[commitFails], func(t *testing.T) {
			repo, mock := atomicCreateRepository(t)
			stamp := time.Now().UTC().Truncate(time.Microsecond)
			mock.ExpectBegin()
			expectReauthLock(mock, stamp, service.PlatformAnthropic, `{}`, `{}`)
			mock.ExpectExec(`(?s)UPDATE accounts.*AND updated_at = \$7`).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`INSERT INTO scheduler_outbox`).
				WillReturnResult(sqlmock.NewResult(1, 1))
			nextStamp := stamp.Add(time.Second)
			expectReauthSnapshot(mock, nextStamp, true)
			commit := mock.ExpectCommit()
			injected := errors.New("injected commit failure")
			if commitFails {
				commit.WillReturnError(injected)
			}
			got, err := repo.ApplyOAuthCredentials(t.Context(), 71, stamp,
				service.AccountTypeOAuth, map[string]any{"access_token": "new"}, nil)
			if commitFails {
				require.ErrorIs(t, err, injected)
				require.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, nextStamp, got.UpdatedAt)
				require.Equal(t, "new", got.Credentials["access_token"])
			}
		})
	}
}

func TestApplyOAuthCredentialsOptionalProxyValidation(t *testing.T) {
	injected := errors.New("qualification database unavailable")
	for _, tc := range []struct {
		name, extra string
		proxyID     any
		proxyStatus string
		schedulable bool
		wantError   error
	}{
		{"direct without qualification", `{}`, nil, "", true, nil},
		{"proxy without qualification", `{}`, int64(7), service.StatusActive, true, nil},
		{"inactive proxy", `{"openai_oauth_qualified_proxy_id":7}`, int64(7), "inactive", false, nil},
		{"database error", `{"openai_oauth_qualified_proxy_id":7}`, int64(7), "error", false, injected},
		{"retired corrupt binding", `{"openai_oauth_qualified_proxy_id":"corrupt"}`, int64(7), service.StatusActive, true, nil},
		{"retired mismatched binding", `{"openai_oauth_qualified_proxy_id":8}`, int64(7), service.StatusActive, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, mock := atomicCreateRepository(t)
			stamp := time.Now().UTC().Truncate(time.Microsecond)
			mock.ExpectBegin()
			mock.ExpectQuery(`(?s)SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id,.*FOR NO KEY UPDATE`).
				WithArgs(int64(71)).
				WillReturnRows(sqlmock.NewRows([]string{
					"id", "platform", "type", "credentials", "extra", "proxy_id",
					"parent_account_id", "status", "schedulable", "updated_at",
				}).AddRow(71, service.PlatformOpenAI, service.AccountTypeOAuth,
					`{"email":"account@example.test"}`, tc.extra, tc.proxyID, nil, service.StatusError, false, stamp))
			if tc.proxyStatus != "" {
				query := mock.ExpectQuery(`(?s)SELECT EXISTS.*FROM proxies.*expires_at > NOW`).
					WithArgs(tc.proxyID, service.StatusActive)
				if tc.proxyStatus == "error" {
					query.WillReturnError(injected)
				} else {
					query.WillReturnRows(sqlmock.NewRows([]string{"exists"}).
						AddRow(tc.proxyStatus == service.StatusActive))
				}
			}
			if tc.wantError != nil {
				mock.ExpectRollback()
			} else {
				mock.ExpectExec(`(?s)UPDATE accounts.*AND updated_at = \$7`).
					WithArgs(service.AccountTypeOAuth, sqlmock.AnyArg(), sqlmock.AnyArg(),
						service.StatusActive, tc.schedulable, int64(71), stamp).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`INSERT INTO scheduler_outbox`).WillReturnResult(sqlmock.NewResult(1, 1))
				expectReauthSnapshot(mock, stamp.Add(time.Second), tc.schedulable)
				mock.ExpectCommit()
			}
			got, err := repo.ApplyOAuthCredentials(t.Context(), 71, stamp,
				service.AccountTypeOAuth, map[string]any{"access_token": "new"},
				map[string]any{service.OpenAIOAuthQualifiedProxyExtraKey: 999})
			if tc.wantError != nil {
				require.ErrorIs(t, err, tc.wantError)
				require.Nil(t, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.schedulable, got.Schedulable)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestApplyOAuthCredentialsPreservesRescueIsolation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extra       string
		schedulable bool
	}{
		{"ordinary account", `{"openai_oauth_qualified_proxy_id":7}`, true},
		{"rescue member", `{"openai_oauth_qualified_proxy_id":7,"openai_rescue_lane":{"entered_at":"2026-10-01T00:00:00Z"}}`, false},
		{"withdrawn account", `{"openai_oauth_qualified_proxy_id":7,"openai_rescue_suspected":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, mock := atomicCreateRepository(t)
			stamp := time.Now().UTC().Truncate(time.Microsecond)
			mock.ExpectBegin()
			mock.ExpectQuery(`(?s)SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id,.*FOR NO KEY UPDATE`).
				WithArgs(int64(71)).
				WillReturnRows(sqlmock.NewRows([]string{
					"id", "platform", "type", "credentials", "extra", "proxy_id",
					"parent_account_id", "status", "schedulable", "updated_at",
				}).AddRow(71, service.PlatformOpenAI, service.AccountTypeOAuth,
					`{"email":"account@example.test"}`, tc.extra, 7, nil, service.StatusError, false, stamp))
			mock.ExpectQuery(`(?s)SELECT EXISTS.*FROM proxies.*expires_at > NOW`).
				WithArgs(int64(7), service.StatusActive).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
			var original map[string]any
			require.NoError(t, json.Unmarshal([]byte(tc.extra), &original))
			mock.ExpectExec(`(?s)UPDATE accounts.*AND updated_at = \$7`).
				WithArgs(service.AccountTypeOAuth, sqlmock.AnyArg(), reauthJSONCheck(func(extra map[string]any) bool {
					if len(extra) != len(original) {
						return false
					}
					return reflect.DeepEqual(original, extra)
				}), service.StatusActive, tc.schedulable, int64(71), stamp).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`INSERT INTO scheduler_outbox`).WillReturnResult(sqlmock.NewResult(1, 1))
			expectReauthSnapshot(mock, stamp.Add(time.Second), tc.schedulable)
			mock.ExpectCommit()
			got, err := repo.ApplyOAuthCredentials(t.Context(), 71, stamp,
				service.AccountTypeOAuth, map[string]any{"access_token": "new"},
				map[string]any{
					"openai_rescue_lane":             nil,
					"openai_rescue_suspected":        false,
					"openai_rescue_rescued_at":       "2026-10-02T00:00:00Z",
					"openai_rescue_auth_rejected_at": nil,
				})
			require.NoError(t, err)
			require.Equal(t, tc.schedulable, got.Schedulable)
		})
	}
}
