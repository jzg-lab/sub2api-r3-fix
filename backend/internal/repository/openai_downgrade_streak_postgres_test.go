package repository

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIProbePostgresRateLimitStreak(t *testing.T) {
	db := newProbePostgres(t)
	mutation := seedProbePostgres(t, db)
	ctx := context.Background()
	repo := &openAIDowngradeProbeRepository{db: db}
	initial, err := repo.GetOpenAIDowngradeState(ctx, mutation.AccountID)
	require.NoError(t, err)
	require.Zero(t, initial.Consecutive429s, "migration must not invent historical counts")
	initial.Consecutive429s = 5
	require.NoError(t, repo.SaveOpenAIDowngradeState(ctx, initial))

	restarted := &openAIDowngradeProbeRepository{db: db}
	reloaded, err := restarted.GetOpenAIDowngradeState(ctx, mutation.AccountID)
	require.NoError(t, err)
	require.Equal(t, 5, reloaded.Consecutive429s)
	due, err := restarted.ListDueOpenAIDowngradeStates(ctx, initial.NextProbeAt, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, 5, due[0].Consecutive429s)

	require.NoError(t, restarted.CommitOpenAIDowngradeMutation(ctx, mutation))
	committed, err := repo.GetOpenAIDowngradeState(ctx, mutation.AccountID)
	require.NoError(t, err)
	require.Equal(t, 6, committed.Consecutive429s)
	// Result timestamps come from PostgreSQL, not the fixed fixture clock.
	var lastResultAt time.Time
	require.NoError(t, db.QueryRow(
		"SELECT MAX(created_at) FROM openai_downgrade_probe_results WHERE account_id=$1",
		mutation.AccountID).Scan(&lastResultAt))
	nextScanAt := lastResultAt.Add(11 * time.Minute)
	if committed.NextProbeAt.After(nextScanAt) {
		nextScanAt = committed.NextProbeAt
	}
	due, err = repo.ListDueOpenAIDowngradeStates(ctx, nextScanAt, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, 6, due[0].Consecutive429s)

	before := probePostgresSnapshot(t, db)
	require.ErrorIs(t, repo.CommitOpenAIDowngradeMutation(ctx, mutation), service.ErrOpenAIProbeStale)
	require.Equal(t, before, probePostgresSnapshot(t, db))
	committed.Consecutive429s = -1
	require.Error(t, repo.SaveOpenAIDowngradeState(ctx, committed))
	require.Equal(t, before, probePostgresSnapshot(t, db))

	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "240_openai_probe_rate_limit_streak.sql"))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	require.Equal(t, before, probePostgresSnapshot(t, db), "migration replay must preserve the count")
}
