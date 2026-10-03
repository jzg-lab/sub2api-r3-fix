package repository

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

func TestLockAndMergeAccountProbeExtraUsesCurrentDatabaseSnapshot(t *testing.T) {
	tests := []struct {
		name              string
		identityUnchanged bool
		databaseEnabled   any
		databaseSnapshot  any
		inputExtra        map[string]any
		wantSnapshot      any
		wantEnabled       any
	}{
		{
			name:              "ordinary edit preserves current enable flag and snapshot created after account load",
			identityUnchanged: true,
			databaseEnabled:   []byte(`true`),
			databaseSnapshot:  []byte(`{"status":"ok"}`),
			inputExtra:        map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false},
			wantSnapshot:      map[string]any{"status": "ok"},
			wantEnabled:       true,
		},
		{
			name:              "identity change clears stale snapshot",
			identityUnchanged: false,
			databaseEnabled:   []byte(`true`),
			databaseSnapshot:  []byte(`{"status":"ok"}`),
			inputExtra: map[string]any{
				service.UpstreamBillingProbeEnabledExtraKey: true,
				service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "stale"},
			},
			wantEnabled: true,
		},
		{
			name:              "current explicit disable clears snapshot",
			identityUnchanged: true,
			databaseEnabled:   []byte(`false`),
			databaseSnapshot:  []byte(`{"status":"ok"}`),
			inputExtra: map[string]any{
				service.UpstreamBillingProbeEnabledExtraKey: true,
				service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "stale"},
			},
			wantEnabled: false,
		},
		{
			name:              "missing database snapshot is not resurrected from stale input",
			identityUnchanged: true,
			databaseEnabled:   []byte(`true`),
			databaseSnapshot:  nil,
			inputExtra: map[string]any{
				service.UpstreamBillingProbeEnabledExtraKey: true,
				service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "stale"},
			},
			wantEnabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })

			mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
				WithArgs(int64(27), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-test"}`, nil).
				WillReturnRows(sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "sol_fallback", "qualification", "model_rate_limits", "allow_overages"}).
					AddRow(tt.identityUnchanged, false, true, tt.databaseEnabled, nil, tt.databaseSnapshot, nil, nil, nil, nil, nil, nil, nil))

			account := &service.Account{
				ID:          27,
				Platform:    service.PlatformOpenAI,
				Type:        service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "sk-test"},
				Extra:       tt.inputExtra,
			}
			got, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, nil, nil)
			require.NoError(t, err)
			if tt.wantSnapshot == nil {
				require.NotContains(t, got, service.UpstreamBillingProbeExtraKey)
			} else {
				require.Equal(t, tt.wantSnapshot, got[service.UpstreamBillingProbeExtraKey])
			}
			require.Equal(t, tt.wantEnabled, got[service.UpstreamBillingProbeEnabledExtraKey])
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func probeBoolPtr(value bool) *bool {
	return &value
}

// The probe switch drives the rate-sync switch and never the other way round:
// syncing depends on probing, so a row where sync is on but the probe key is
// missing must lose the sync flag instead of silently gaining periodic
// outbound calls on the next unrelated edit.
func TestLockAndMergeAccountProbeExtraNeverInfersProbeFromRateSync(t *testing.T) {
	tests := []struct {
		name                 string
		databaseEnabled      any
		databaseRateSync     any
		explicitProbeEnabled *bool
		explicitRateSync     *bool
		wantEnabled          any
		wantRateSync         any
	}{
		{
			name:             "sync on with missing probe key zeroes sync and keeps probing off",
			databaseEnabled:  nil,
			databaseRateSync: []byte(`true`),
			wantEnabled:      nil,
			wantRateSync:     false,
		},
		{
			name:             "sync on with probe off zeroes sync",
			databaseEnabled:  []byte(`false`),
			databaseRateSync: []byte(`true`),
			wantEnabled:      false,
			wantRateSync:     false,
		},
		{
			name:             "both on in database stay on",
			databaseEnabled:  []byte(`true`),
			databaseRateSync: []byte(`true`),
			wantEnabled:      true,
			wantRateSync:     true,
		},
		{
			name:                 "admin enabling both explicitly still turns probing on",
			databaseEnabled:      nil,
			databaseRateSync:     nil,
			explicitProbeEnabled: probeBoolPtr(true),
			explicitRateSync:     probeBoolPtr(true),
			wantEnabled:          true,
			wantRateSync:         true,
		},
		{
			name:                 "explicit probe disable clears sync",
			databaseEnabled:      []byte(`true`),
			databaseRateSync:     []byte(`true`),
			explicitProbeEnabled: probeBoolPtr(false),
			wantEnabled:          false,
			wantRateSync:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })

			mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
				WithArgs(int64(31), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-test"}`, nil).
				WillReturnRows(sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "sol_fallback", "qualification", "model_rate_limits", "allow_overages"}).
					AddRow(true, false, true, tt.databaseEnabled, tt.databaseRateSync, nil, nil, nil, nil, nil, nil, nil, nil))

			account := &service.Account{
				ID:          31,
				Platform:    service.PlatformOpenAI,
				Type:        service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "sk-test"},
			}
			got, err := lockAndMergeAccountProbeExtra(
				context.Background(), client, account, tt.explicitProbeEnabled, tt.explicitRateSync,
			)
			require.NoError(t, err)
			if tt.wantEnabled == nil {
				require.NotContains(t, got, service.UpstreamBillingProbeEnabledExtraKey)
			} else {
				require.Equal(t, tt.wantEnabled, got[service.UpstreamBillingProbeEnabledExtraKey])
			}
			if tt.wantRateSync == nil {
				require.NotContains(t, got, service.UpstreamBillingRateSyncEnabledExtraKey)
			} else {
				require.Equal(t, tt.wantRateSync, got[service.UpstreamBillingRateSyncEnabledExtraKey])
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestLockAndMergeAccountProbeExtraProtectsOllamaManagedFields(t *testing.T) {
	for _, identityUnchanged := range []bool{true, false} {
		t.Run(map[bool]string{true: "same identity keeps snapshot", false: "changed identity clears snapshot"}[identityUnchanged], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })

			mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
				WithArgs(int64(29), service.PlatformAnthropic, service.AccountTypeAPIKey, `{"api_key":"key","base_url":"https://ollama.com"}`, nil).
				WillReturnRows(sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "sol_fallback", "qualification", "model_rate_limits", "allow_overages"}).
					AddRow(identityUnchanged, identityUnchanged, true, nil, nil, nil, []byte(`"local-ciphertext"`), []byte(`true`), []byte(`{"status":"ok"}`), nil, nil, nil, nil))

			account := &service.Account{
				ID: 29, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "key", "base_url": "https://ollama.com"},
				Extra: map[string]any{
					service.OllamaCloudUsageSessionExtraKey:     "forged-ciphertext",
					service.OllamaCloudUsageAutoRefreshExtraKey: false,
					service.OllamaCloudUsageSnapshotExtraKey:    map[string]any{"status": "forged"},
				},
			}
			got, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, nil, nil)
			require.NoError(t, err)
			if identityUnchanged {
				require.Equal(t, "local-ciphertext", got[service.OllamaCloudUsageSessionExtraKey])
				require.Equal(t, true, got[service.OllamaCloudUsageAutoRefreshExtraKey])
				require.Equal(t, map[string]any{"status": "ok"}, got[service.OllamaCloudUsageSnapshotExtraKey])
			} else {
				require.NotContains(t, got, service.OllamaCloudUsageSessionExtraKey)
				require.NotContains(t, got, service.OllamaCloudUsageAutoRefreshExtraKey)
				require.NotContains(t, got, service.OllamaCloudUsageSnapshotExtraKey)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUpdateExtraExplicitProbeDisableRemovesSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* - 'upstream_billing_probe'`).
		WithArgs(`{"upstream_billing_probe_enabled":false}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQL(client, db, nil)

	err = repo.UpdateExtra(context.Background(), 27, map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false})

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateExtraNilProbeRemovesKeyInsteadOfWritingJSONNull(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* - 'upstream_billing_probe'`).
		WithArgs(`{"upstream_billing_probe":null}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQL(client, db, nil)

	err = repo.UpdateExtra(context.Background(), 27, map[string]any{service.UpstreamBillingProbeExtraKey: nil})

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkUpdateNilProbeRemovesKeyInsteadOfWritingJSONNull(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	_, err := repo.BulkUpdate(context.Background(), []int64{27}, service.AccountBulkUpdate{
		Extra: map[string]any{service.UpstreamBillingProbeExtraKey: nil},
	})

	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)
	require.Contains(t, normalizeSQLWhitespace(exec.execQueries[0]), "- 'upstream_billing_probe'")
}

func TestBulkUpdateDisablingProbeRemovesSnapshot(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	_, err := repo.BulkUpdate(context.Background(), []int64{27}, service.AccountBulkUpdate{
		Extra: map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false},
	})

	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)
	require.Contains(t, normalizeSQLWhitespace(exec.execQueries[0]), "- 'upstream_billing_probe'")
	payload, ok := exec.execArgs[0][0].([]byte)
	require.True(t, ok)
	require.Equal(t, `{"upstream_billing_probe_enabled":false}`, string(payload))
}

func TestBulkUpdateProbeEligibilityMismatchRollsBack(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	enabled := true
	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* WHERE id = ANY\(\$2\) AND deleted_at IS NULL AND type = \$3`).
		WithArgs(sqlmock.AnyArg(), `{27,28}`, service.AccountTypeAPIKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	rows, err := repo.BulkUpdate(context.Background(), []int64{27, 28}, service.AccountBulkUpdate{
		ProbeEnabled: &enabled,
	})

	require.ErrorIs(t, err, service.ErrUpstreamBillingProbeAccountInvalid)
	require.Zero(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateCredentialsAtomicallyClearsProbeForOpenAIAPIKeyIdentityChange(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	expectNonBrowserOpenAIOAuthAccountLock(mock, 27)
	mock.ExpectExec(`(?s)UPDATE accounts.*credentials IS DISTINCT FROM \$1::jsonb.*- 'upstream_billing_probe'`).
		WithArgs(`{"api_key":"sk-new"}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQL(client, db, nil)

	err = repo.UpdateCredentials(context.Background(), 27, map[string]any{"api_key": "sk-new"})

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateWithAccountBillingSettingsRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	expectNonBrowserOpenAIOAuthAccountLock(mock, 27)
	mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
		WithArgs(int64(27), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-test"}`, nil).
		WillReturnRows(sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "sol_fallback", "qualification", "model_rate_limits", "allow_overages"}).
			AddRow(true, false, true, []byte(`true`), []byte(`true`), []byte(`{"status":"ok"}`), nil, nil, nil, nil, nil, nil, nil))
	mock.ExpectExec(`(?s)UPDATE .*accounts.*SET.*WHERE .*id.*`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE "id" = \$1`).
		WithArgs(int64(27)).
		WillReturnRows(updatedAccountRows(27, `{"upstream_billing_probe_enabled":false,"upstream_billing_rate_sync_enabled":false}`))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	account := &service.Account{
		ID:          27,
		Name:        "test",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra: map[string]any{
			service.UpstreamBillingProbeExtraKey: map[string]any{"status": "stale"},
		},
		Concurrency: 1,
		Priority:    1,
		Status:      service.StatusActive,
		Schedulable: true,
	}

	probeDisabled := false
	err = repo.UpdateWithAccountBillingSettings(context.Background(), account, &probeDisabled, nil, nil)

	require.EqualError(t, err, "outbox failed")
	require.Equal(t, false, account.Extra[service.UpstreamBillingProbeEnabledExtraKey])
	require.Equal(t, false, account.Extra[service.UpstreamBillingRateSyncEnabledExtraKey])
	require.NotContains(t, account.Extra, service.UpstreamBillingProbeExtraKey)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateExtraRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .* - 'upstream_billing_probe'`).
		WithArgs(`{"upstream_billing_probe_enabled":false}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	err = repo.UpdateExtra(context.Background(), 27, map[string]any{service.UpstreamBillingProbeEnabledExtraKey: false})

	require.EqualError(t, err, "outbox failed")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateCredentialsRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	expectNonBrowserOpenAIOAuthAccountLock(mock, 27)
	mock.ExpectExec(`(?s)UPDATE accounts.*credentials IS DISTINCT FROM \$1::jsonb.*- 'upstream_billing_probe'`).
		WithArgs(`{"api_key":"sk-new"}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	err = repo.UpdateCredentials(context.Background(), 27, map[string]any{"api_key": "sk-new"})

	require.EqualError(t, err, "outbox failed")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBulkUpdateRollsBackWhenOutboxFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	name := "renamed"
	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET name = \$1.*WHERE id = ANY\(\$2\)`).
		WithArgs(name, `{27,28}`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).WillReturnError(errors.New("outbox failed"))
	mock.ExpectRollback()

	repo := newAccountRepositoryWithSQL(client, db, nil)
	rows, err := repo.BulkUpdate(context.Background(), []int64{27, 28}, service.AccountBulkUpdate{Name: &name})

	require.EqualError(t, err, "outbox failed")
	require.Zero(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func updatedAccountRows(id int64, extra string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows(dbaccount.Columns).AddRow(
		id, now, now, nil, "test", nil, service.PlatformOpenAI, service.AccountTypeAPIKey,
		[]byte(`{"api_key":"sk-test"}`), []byte(extra), nil, nil, 1, nil, 1, 1.0,
		service.StatusActive, nil, nil, nil, false, true, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, service.QuotaDimensionGlobal,
	)
}

func TestBulkUpdateDropsManagedSchedulingState(t *testing.T) {
	for _, includeCustom := range []bool{false, true} {
		t.Run(fmt.Sprint(includeCustom), func(t *testing.T) {
			exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
			repo := newAccountRepositoryWithSQL(nil, exec, nil)
			input := map[string]any{
				"model_rate_limits":                          map[string]any{"stale": "limit"},
				service.OpenAIDowngradeSolFallbackExtraKey:   true,
				service.OpenAIDowngradeQualificationExtraKey: nil,
			}
			if includeCustom {
				input["custom"] = "edited"
			}

			rows, err := repo.BulkUpdate(context.Background(), []int64{27}, service.AccountBulkUpdate{Extra: input})

			require.NoError(t, err)
			if includeCustom {
				require.EqualValues(t, 1, rows)
				require.NotEmpty(t, exec.execArgs)
				payload, ok := exec.execArgs[0][0].([]byte)
				require.True(t, ok)
				require.JSONEq(t, `{"custom":"edited"}`, string(payload))
			} else {
				require.Zero(t, rows)
				require.Empty(t, exec.execQueries, "protected-only edits must not touch the database or outbox")
			}
			require.Contains(t, input, "model_rate_limits")
			require.Contains(t, input, service.OpenAIDowngradeSolFallbackExtraKey)
			require.Contains(t, input, service.OpenAIDowngradeQualificationExtraKey)
		})
	}
}

func TestLockAndMergeAccountProbeExtraPreservesCurrentModelLimits(t *testing.T) {
	for _, current := range []any{nil, []byte(`{"current-model":{"reason":"current"}}`)} {
		t.Run(fmt.Sprint(current), func(t *testing.T) {
			repo, mock := atomicCreateRepository(t)
			mock.ExpectQuery(`(?s)SELECT.*extra -> 'model_rate_limits'.*FOR NO KEY UPDATE`).
				WithArgs(int64(77), service.PlatformOpenAI, service.AccountTypeOAuth, "{}", nil).
				WillReturnRows(sqlmock.NewRows([]string{
					"identity", "group", "proxy", "probe", "sync", "snapshot", "session",
					"auto", "usage", "sol_fallback", "qualification", "model_rate_limits", "allow_overages",
				}).AddRow(true, false, true, nil, nil, nil, nil, nil, nil, nil, nil, current, nil))
			account := &service.Account{
				ID: 77, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Extra: map[string]any{"model_rate_limits": map[string]any{"stale-model": true}, "custom": "edited"},
			}

			got, err := lockAndMergeAccountProbeExtra(t.Context(), repo.client, account, nil, nil)

			require.NoError(t, err)
			require.Equal(t, "edited", got["custom"])
			if current == nil {
				require.NotContains(t, got, "model_rate_limits")
			} else {
				require.Equal(t, map[string]any{"current-model": map[string]any{"reason": "current"}}, got["model_rate_limits"])
			}
			require.Equal(t, map[string]any{"stale-model": true}, account.Extra["model_rate_limits"])
		})
	}
}

func TestAccountUpdateDoesNotWriteRuntimeCooldowns(t *testing.T) {
	now := time.Now()
	for _, staleCooldown := range []*time.Time{nil, &now} {
		t.Run(fmt.Sprint(staleCooldown), func(t *testing.T) {
			repo, mock := atomicCreateRepository(t)
			account := atomicCreateAccount()
			account.ID = 71
			account.RateLimitedAt = staleCooldown
			account.RateLimitResetAt = staleCooldown
			mock.ExpectBegin()
			expectNonBrowserOpenAIOAuthPATAccountLock(mock, account.ID)
			mock.ExpectQuery(`(?s)SELECT.*FOR NO KEY UPDATE`).
				WithArgs(int64(71), service.PlatformOpenAI, service.AccountTypeOAuth, `{"auth_mode":"personalAccessToken"}`, nil).
				WillReturnRows(sqlmock.NewRows([]string{
					"identity", "group", "proxy", "probe", "sync", "snapshot", "session",
					"auto", "usage", "sol_fallback", "qualification", "model_rate_limits", "allow_overages",
				}).AddRow(true, false, true, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))
			mock.ExpectRollback()
			stop := errors.New("stop after inspecting update mutation")
			repo.client.Account.Use(func(next dbent.Mutator) dbent.Mutator {
				return dbent.MutateFunc(func(ctx context.Context, mutation dbent.Mutation) (dbent.Value, error) {
					for _, field := range []string{dbaccount.FieldRateLimitedAt, dbaccount.FieldRateLimitResetAt} {
						require.NotContains(t, mutation.Fields(), field)
						require.NotContains(t, mutation.ClearedFields(), field)
					}
					return nil, stop
				})
			})

			require.ErrorIs(t, repo.Update(t.Context(), account), stop)
			require.Equal(t, staleCooldown, account.RateLimitedAt)
			require.Equal(t, staleCooldown, account.RateLimitResetAt)
		})
	}
}

func TestLockAndMergeAccountProbeExtraRetiresBrowserQualification(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectQuery(`(?s)`+regexp.QuoteMeta("SELECT")+`.*`+regexp.QuoteMeta("FOR NO KEY UPDATE")).
		WithArgs(int64(77), service.PlatformOpenAI, service.AccountTypeOAuth, `{"api_key":"sk"}`, nil).
		WillReturnRows(sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "sol_fallback", "qualification", "model_rate_limits", "allow_overages"}).
			AddRow(true, false, true, nil, nil, nil, nil, nil, nil, nil, []byte(`true`), nil, nil))

	// 陈旧表单：带旧 sol_fallback=true（行内已无）、不带 qualification
	account := &service.Account{
		ID:          77,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Credentials: map[string]any{"api_key": "sk"},
		Extra: map[string]any{
			service.OpenAIDowngradeSolFallbackExtraKey: true,
			"note": "edited",
		},
	}
	got, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, nil, nil)
	require.NoError(t, err)
	require.NotContains(t, got, service.OpenAIDowngradeSolFallbackExtraKey,
		"stale form value must not resurrect a system-managed key absent in the row")
	require.NotContains(t, got, service.OpenAIDowngradeQualificationExtraKey,
		"legacy browser qualification must not be restored during an edit")
	require.Equal(t, "edited", got["note"])
}
