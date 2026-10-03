package repository

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/migrate"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newOAuthReauthorizationPostgres(t *testing.T) (*accountRepository, *sql.DB, *service.Account) {
	t.Helper()
	db := newProbePostgresWithMigrations(t, nil)
	// The probe harness creates skeletal tables in its private test schema.
	// Replace only those empty fixtures with the real Ent account schema.
	_, err := db.Exec("DROP TABLE usage_logs, accounts, proxies")
	require.NoError(t, err)
	drv := entsql.OpenDB(dialect.Postgres, db)
	require.NoError(t, migrate.Create(t.Context(), migrate.NewSchema(drv), []*schema.Table{
		migrate.ProxiesTable, migrate.AccountsTable, migrate.GroupsTable, migrate.AccountGroupsTable,
	}, migrate.WithForeignKeys(false)))
	for _, name := range []string{
		"036_scheduler_outbox.sql", "152_scheduler_outbox_dedup_key.sql",
		"153_scheduler_outbox_pending_dedup_key_index_notx.sql",
	} {
		migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, err)
		_, err = db.Exec(string(migration))
		require.NoError(t, err)
	}
	client := dbent.NewClient(dbent.Driver(drv))
	account, err := client.Account.Create().
		SetName("reauthorization-test").
		SetPlatform(service.PlatformAnthropic).
		SetType(service.AccountTypeOAuth).
		SetCredentials(map[string]any{
			"access_token": "old-test-access", "model_mapping": map[string]any{"model-a": "model-a"},
			"_token_version": int64(7),
		}).
		SetExtra(map[string]any{"base_rpm": 3}).
		SetStatus(service.StatusError).
		SetSchedulable(false).
		SetErrorMessage("authorization expired").
		SetUpdatedAt(time.Now().Add(-time.Minute)).
		Save(t.Context())
	require.NoError(t, err)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	current, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	return repo, db, current
}

func TestApplyOAuthCredentialsPostgresExactlyOneConcurrentCommit(t *testing.T) {
	repo, db, account := newOAuthReauthorizationPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	type result struct {
		account *service.Account
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			updated, err := repo.ApplyOAuthCredentials(ctx, account.ID, account.UpdatedAt,
				service.AccountTypeOAuth, map[string]any{"access_token": "new-test-access"}, nil)
			results <- result{updated, err}
		}()
	}
	close(start)
	var committed *service.Account
	successes, conflicts := 0, 0
	for range 2 {
		got := <-results
		switch {
		case got.err == nil:
			successes++
			committed = got.account
		case errors.Is(got.err, service.ErrOAuthReauthorizationStale):
			conflicts++
		default:
			require.NoError(t, got.err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
	current, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, committed.Credentials, current.Credentials)
	require.Equal(t, account.Credentials["model_mapping"], current.Credentials["model_mapping"])
	require.Equal(t, account.Extra, current.Extra)
	require.True(t, current.Schedulable)
	require.Equal(t, service.StatusActive, current.Status)
	require.Greater(t, current.GetCredentialAsInt64("_token_version"), int64(7))
	var events int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM scheduler_outbox WHERE account_id = $1", account.ID).Scan(&events))
	require.Equal(t, 1, events)
}

func TestApplyOAuthCredentialsPostgresOutboxFailureRollsBack(t *testing.T) {
	repo, db, account := newOAuthReauthorizationPostgres(t)
	_, err := db.Exec(`ALTER TABLE scheduler_outbox ADD CONSTRAINT reject_reauth_event CHECK (event_type <> 'account_changed')`)
	require.NoError(t, err)
	updated, err := repo.ApplyOAuthCredentials(t.Context(), account.ID, account.UpdatedAt,
		service.AccountTypeOAuth, map[string]any{"access_token": "new-test-access"}, nil)
	require.Error(t, err)
	require.Nil(t, updated)
	current, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, account.Credentials, current.Credentials)
	require.Equal(t, account.Extra, current.Extra)
	require.Equal(t, account.Status, current.Status)
	require.Equal(t, account.Schedulable, current.Schedulable)
	require.True(t, account.UpdatedAt.Equal(current.UpdatedAt))
	var events int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM scheduler_outbox").Scan(&events))
	require.Zero(t, events)
}

func TestApplyOAuthCredentialsPostgresCanceledTransactionDoesNotMutate(t *testing.T) {
	repo, _, account := newOAuthReauthorizationPostgres(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	updated, err := repo.ApplyOAuthCredentials(ctx, account.ID, account.UpdatedAt,
		service.AccountTypeOAuth, map[string]any{"access_token": "new-test-access"}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, updated)
	current, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, account.Credentials, current.Credentials)
	require.True(t, account.UpdatedAt.Equal(current.UpdatedAt))
}
