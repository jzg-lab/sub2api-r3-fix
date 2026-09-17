package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func assertLocalConcurrencyMutation(t *testing.T, repo *accountRepository) *bool {
	t.Helper()
	seen := false
	repo.client.Account.Use(func(next dbent.Mutator) dbent.Mutator {
		return dbent.MutateFunc(func(ctx context.Context, mutation dbent.Mutation) (dbent.Value, error) {
			value, ok := mutation.(*dbent.AccountMutation).Concurrency()
			require.True(t, ok)
			require.Equal(t, 50, value)
			seen = true
			return next.Mutate(ctx, mutation)
		})
	})
	return &seen
}

func TestAccountRepositoryCreateUsesLocalConcurrency(t *testing.T) {
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformAnthropic, service.PlatformGrok, "future-platform"} {
		for _, requested := range []int{-1, 0, 1, 50, 1000} {
			t.Run(fmt.Sprintf("%s/%d", platform, requested), func(t *testing.T) {
				repo, mock := atomicCreateRepository(t)
				seen := assertLocalConcurrencyMutation(t, repo)
				account := atomicCreateAccount()
				account.Platform = platform
				account.Concurrency = requested
				mock.ExpectQuery(`INSERT INTO "accounts"`).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(71)))
				mock.ExpectExec(`INSERT INTO scheduler_outbox`).
					WithArgs(service.SchedulerOutboxEventAccountChanged, int64(71), nil, sqlmock.AnyArg(), sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(0, 1))

				require.NoError(t, repo.Create(t.Context(), account))
				require.True(t, *seen)
				require.Equal(t, 50, account.Concurrency, "the returned account must match persistence")
			})
		}
	}
}

func TestAccountRepositoryCreateFailureKeepsRequestedConcurrency(t *testing.T) {
	repo, mock := atomicCreateRepository(t)
	account := atomicCreateAccount()
	mock.ExpectQuery(`INSERT INTO "accounts"`).WillReturnError(errors.New("insert failed"))
	require.EqualError(t, repo.Create(t.Context(), account), "insert failed")
	require.Equal(t, 1, account.Concurrency)
	require.Zero(t, account.ID)
}

func TestAccountRepositoryUpdateUsesLocalConcurrency(t *testing.T) {
	for _, failure := range []string{"none", "outbox", "commit"} {
		t.Run(failure, func(t *testing.T) {
			repo, mock := atomicCreateRepository(t)
			seen := assertLocalConcurrencyMutation(t, repo)
			account := atomicCreateAccount()
			account.ID = 71
			mock.ExpectBegin()
			mock.ExpectQuery(`(?s)SELECT.*FOR NO KEY UPDATE`).
				WithArgs(int64(71), service.PlatformOpenAI, service.AccountTypeOAuth, "{}", nil).
				WillReturnRows(sqlmock.NewRows([]string{"identity", "group", "proxy", "probe", "sync", "snapshot", "session", "auto", "usage", "sol_fallback", "qualification", "model_rate_limits", "allow_overages"}).
					AddRow(true, false, true, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))
			mock.ExpectExec(`UPDATE "accounts"`).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE "id" = \$1`).
				WithArgs(int64(71)).
				WillReturnRows(sqlmock.NewRows([]string{"id", "concurrency"}).AddRow(int64(71), 50))
			injected := errors.New("injected " + failure)
			outbox := mock.ExpectExec(`INSERT INTO scheduler_outbox`)
			if failure == "outbox" {
				outbox.WillReturnError(injected)
				mock.ExpectRollback()
			} else {
				outbox.WillReturnResult(sqlmock.NewResult(0, 1))
				commit := mock.ExpectCommit()
				if failure == "commit" {
					commit.WillReturnError(injected)
				}
			}

			err := repo.Update(t.Context(), account)
			require.True(t, *seen)
			if failure == "none" {
				require.NoError(t, err)
				require.Equal(t, 50, account.Concurrency)
			} else {
				require.ErrorIs(t, err, injected)
				require.Equal(t, 1, account.Concurrency, "an uncommitted limit must not be published")
			}
		})
	}
}

func TestAccountRepositoryBulkConcurrencyUsesLocalPolicy(t *testing.T) {
	for _, requested := range []int{-1, 0, 1, 50, 1000} {
		t.Run(fmt.Sprint(requested), func(t *testing.T) {
			repo, mock := atomicCreateRepository(t)
			mock.ExpectBegin()
			mock.ExpectExec(`UPDATE accounts SET concurrency = \$1, updated_at = NOW\(\) WHERE id = ANY\(\$2\) AND deleted_at IS NULL`).
				WithArgs(50, "{71,72}").WillReturnResult(sqlmock.NewResult(0, 2))
			mock.ExpectExec(`INSERT INTO scheduler_outbox`).
				WithArgs(service.SchedulerOutboxEventAccountBulkChanged, nil, nil, []byte(`{"account_ids":[71,72]}`)).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			input := requested

			rows, err := repo.BulkUpdate(t.Context(), []int64{71, 72}, service.AccountBulkUpdate{Concurrency: &input})
			require.NoError(t, err)
			require.EqualValues(t, 2, rows)
			require.Equal(t, requested, input, "the caller's pointer must not be mutated")
		})
	}
}

func TestAccountRepositoryBulkOmittedConcurrencyKeepsPartialUpdate(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	name := "renamed"
	_, err := repo.BulkUpdate(t.Context(), []int64{71}, service.AccountBulkUpdate{Name: &name})
	require.NoError(t, err)
	require.NotContains(t, exec.execQueries[0], "concurrency")
}
