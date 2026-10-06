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
		Credentials: map[string]any{
			"auth_mode": service.OpenAIAuthModePersonalAccessToken,
		},
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
						// 绑组去重探测(无触发器绑定时返回空)。
						mock.ExpectQuery(`SELECT .* FROM "account_groups"`).
							WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id"}))
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
			require.Equal(t, before.Concurrency, account.Concurrency)
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

// TestCreateWithAccountGroupsSkipsTriggerBoundPoolGroup 复现 2026-09-21 08:19
// 生产 500: 浏览器 OAuth 建号(无显式组→默认组 openai-default)时,DB 侧
// accounts_auto_bind_openai_pool_group AFTER INSERT 触发器已按 plan_type 落了
// 池组绑定;应用侧 openai-default 行经 account_groups_prepare_pool_binding
// BEFORE 触发器改写成同一池组 → account_groups_pkey 撞键。修复=读触发器已
// 落的绑定,同组重放跳过(池组 priority 保留触发器的 max+1 池语义)。
// sqlmock 无法模拟 DB 触发器,这里以「account_groups 已有一行池组绑定」的
// 查询结果模拟触发器效果,断言应用侧不再 INSERT 撞键行。
func TestCreateWithAccountGroupsSkipsTriggerBoundPoolGroup(t *testing.T) {
	repo, mock := atomicCreateRepository(t)
	account := atomicCreateAccount()
	// PAT 账号不走 prepareOpenAIOAuthAccountCreate 的行锁查询,聚焦绑组去重。
	groups := []service.AccountGroup{{GroupID: 3, Priority: 1}} // openai-default

	mock.ExpectBegin()
	// createAccountRecord 的 INSERT(触发器在此后已绑 Team=71)。
	mock.ExpectQuery(`INSERT INTO "accounts"`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(72)))
	// 绑组去重探测①:触发器已落的池组绑定行。
	mock.ExpectQuery(`SELECT .* FROM "account_groups"`).
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id"}).
			AddRow(int64(72), int64(71)))
	// 绑组去重探测②:组名解析(Team=池组)。
	mock.ExpectQuery(`SELECT .* FROM "groups"`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).AddRow(int64(71), "Team"))
	// openai-default 组解析(改写预判)。
	mock.ExpectQuery(`SELECT .* FROM "groups"`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(3)))
	// 触发器已绑 → 应用侧不再 INSERT account_groups(此处无 ExpectExec)。
	mock.ExpectExec(`INSERT INTO scheduler_outbox`).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(72), nil, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := repo.CreateWithAccountGroups(t.Context(), account, groups)
	require.NoError(t, err)
	require.Equal(t, []int64{3}, account.GroupIDs, "outbox 载荷仍登记请求组")
}
