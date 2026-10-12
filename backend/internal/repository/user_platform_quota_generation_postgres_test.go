package repository

import (
	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
	"time"
)

func TestQuotaSnapshotCannotRestoreClearedGeneration(t *testing.T) {
	db := newProbePostgresWithMigrations(t, nil)
	_, err := db.Exec(`CREATE TABLE users (id BIGINT PRIMARY KEY); INSERT INTO users VALUES (1)`)
	require.NoError(t, err)
	migration, err := os.ReadFile("../../migrations/142_user_platform_quotas.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := NewUserPlatformQuotaRepository(client)
	ctx := t.Context()
	limit := 10.0
	records := []UserPlatformQuotaRecord{{Platform: "openai", DailyLimitUSD: &limit}}
	require.NoError(t, repo.UpsertForUser(ctx, 1, records))
	old, err := repo.GetByUserPlatform(ctx, 1, "openai")
	require.NoError(t, err)
	require.NotZero(t, old.QuotaID)
	snapshot := UserPlatformQuotaSnapshot{QuotaID: old.QuotaID, UserID: 1, Platform: "openai", DailyUsageUSD: 7, DailyWindowStart: time.Now(), WeeklyWindowStart: time.Now(), MonthlyWindowStart: time.Now()}
	require.NoError(t, repo.BatchSnapshotUsage(ctx, []UserPlatformQuotaSnapshot{snapshot}, time.Now()))
	old, err = repo.GetByUserPlatform(ctx, 1, "openai")
	require.NoError(t, err)
	require.Equal(t, 7.0, old.DailyUsageUSD)
	require.NoError(t, repo.UpsertForUser(ctx, 1, nil))
	require.NoError(t, repo.UpsertForUser(ctx, 1, records))
	current, err := repo.GetByUserPlatform(ctx, 1, "openai")
	require.NoError(t, err)
	require.NotEqual(t, old.QuotaID, current.QuotaID)
	require.NoError(t, repo.BatchSnapshotUsage(ctx, []UserPlatformQuotaSnapshot{snapshot}, time.Now()))
	current, err = repo.GetByUserPlatform(ctx, 1, "openai")
	require.NoError(t, err)
	require.Zero(t, current.DailyUsageUSD, "old flusher must not restore pre-reset usage")
	snapshot.QuotaID = current.QuotaID
	require.NoError(t, repo.BatchSnapshotUsage(ctx, []UserPlatformQuotaSnapshot{snapshot}, time.Now()))
	current, err = repo.GetByUserPlatform(ctx, 1, "openai")
	require.NoError(t, err)
	require.Equal(t, 7.0, current.DailyUsageUSD, "current generation still flushes")
}
