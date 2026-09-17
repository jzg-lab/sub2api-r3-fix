package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// r17c 回归:纯调度器中立键(codex 用量倒计时等,~30s 周期必变的派生值)的
// extra 写入不得推 updated_at——否则账号行代际被后台轮询持续推走,资格探针
// 的 CAS 提交(CommitOpenAIDowngradeMutation 以 updated_at 为代际)几乎必然
// 失配,新号永远无法通过资格检测。
func TestUpdateExtraNeutralKeysPreserveAccountGeneration(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	// 精确匹配无 updated_at 子句的 UPDATE;实现若回退为推代际,该期望失配。
	mock.ExpectExec(regexp.QuoteMeta(
		"UPDATE accounts SET extra = COALESCE(extra, '{}'::jsonb) || $1::jsonb WHERE id = $2 AND deleted_at IS NULL",
	)).
		WithArgs(`{"codex_7d_used_percent":3}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	repo := newAccountRepositoryWithSQL(client, db, nil)

	require.NoError(t, repo.UpdateExtra(context.Background(), 27, map[string]any{
		"codex_7d_used_percent": 3,
	}))
	require.NoError(t, mock.ExpectationsWereMet())
}

// 非中立键(实质配置变化)仍必须推 updated_at 并走调度器 outbox 事务。
func TestUpdateExtraMaterialKeysBumpAccountGeneration(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = .*, updated_at = NOW\(\) WHERE id = \$2 AND deleted_at IS NULL`).
		WithArgs(`{"codex_7d_used_percent":3,"display_note":"x"}`, int64(27)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	repo := newAccountRepositoryWithSQL(client, db, nil)

	require.NoError(t, repo.UpdateExtra(context.Background(), 27, map[string]any{
		"codex_7d_used_percent": 3,
		"display_note":          "x",
	}))
	require.NoError(t, mock.ExpectationsWereMet())
}
