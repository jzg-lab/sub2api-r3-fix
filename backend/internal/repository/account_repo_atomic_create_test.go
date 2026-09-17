package repository

import (
	"errors"
	"maps"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func atomicCreateRepository(t *testing.T) (*accountRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		_ = client.Close()
	})
	return newAccountRepositoryWithSQL(client, db, nil), mock
}

func atomicCreateAccount() *service.Account {
	return &service.Account{
		Name: "atomic-create", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive,
		Concurrency: 1, Extra: map[string]any{"existing": true},
	}
}

func TestCreateWithAccountGroupsPublishesOnlyCommittedState(t *testing.T) {
	for _, failure := range []string{"begin", "account", "groups", "outbox", "commit", "none", "no groups"} {
		t.Run(failure, func(t *testing.T) {
			repo, mock := atomicCreateRepository(t)
			account := atomicCreateAccount()
			before := *account
			before.Extra = maps.Clone(account.Extra)
			originalExtra := account.Extra
			groups := []service.AccountGroup{{GroupID: 9, Priority: 37}, {GroupID: 3, Priority: 12}}
			if failure == "no groups" {
				groups = nil
			}
			originalGroups := append([]service.AccountGroup(nil), groups...)
			injected := errors.New("injected atomic creation failure")

			begin := mock.ExpectBegin()
			if failure == "begin" {
				begin.WillReturnError(injected)
			} else {
				insert := mock.ExpectQuery(`INSERT INTO "accounts"`)
				if failure == "account" {
					insert.WillReturnError(injected)
				} else {
					insert.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(71)))
					if len(groups) > 0 {
						groupInsert := mock.ExpectExec(`INSERT INTO "account_groups"`)
						if failure == "groups" {
							groupInsert.WillReturnError(injected)
						} else {
							groupInsert.WillReturnResult(sqlmock.NewResult(0, 2))
						}
					}
					if failure != "groups" {
						outbox := mock.ExpectExec(`INSERT INTO scheduler_outbox`).
							WithArgs(service.SchedulerOutboxEventAccountChanged, int64(71), nil, sqlmock.AnyArg(), sqlmock.AnyArg())
						if failure == "outbox" {
							outbox.WillReturnError(injected)
						} else {
							outbox.WillReturnResult(sqlmock.NewResult(0, 1))
						}
					}
				}
				switch failure {
				case "account", "groups", "outbox":
					mock.ExpectRollback()
				default:
					commit := mock.ExpectCommit()
					if failure == "commit" {
						commit.WillReturnError(injected)
					}
				}
			}

			err := repo.CreateWithAccountGroups(t.Context(), account, groups)
			require.Equal(t, originalGroups, groups, "caller-owned groups must never be changed")
			require.Equal(t, before.Extra, originalExtra, "normalization must not leak through the input map")
			if failure != "none" && failure != "no groups" {
				require.ErrorIs(t, err, injected)
				require.Equal(t, before, *account, "failed creation must not publish an ID, groups, timestamps or fingerprint")
				return
			}
			require.NoError(t, err)
			require.Equal(t, int64(71), account.ID)
			require.Equal(t, 50, account.Concurrency)
			require.False(t, account.CreatedAt.IsZero())
			require.NotEmpty(t, account.Extra["codex_fingerprint_seed"])
			require.Equal(t, len(groups), len(account.GroupIDs))
			for i, group := range groups {
				require.Equal(t, group.GroupID, account.GroupIDs[i])
				require.Equal(t, group.Priority, account.AccountGroups[i].Priority)
				require.Equal(t, account.ID, account.AccountGroups[i].AccountID)
			}
		})
	}
}

func TestCreateWithAccountGroupsRejectsUncommittedOuterTransaction(t *testing.T) {
	repo, mock := atomicCreateRepository(t)
	mock.ExpectBegin()
	tx, err := repo.client.Tx(t.Context())
	require.NoError(t, err)
	transactional := newAccountRepositoryWithSQL(tx.Client(), nil, nil)
	account := atomicCreateAccount()
	before := *account
	before.Extra = maps.Clone(account.Extra)

	err = transactional.CreateWithAccountGroups(t.Context(), account, nil)
	require.ErrorIs(t, err, dbent.ErrTxStarted)
	require.Equal(t, before, *account)
	// The caller still owns its transaction; rejection must not commit or abort it.
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
}
