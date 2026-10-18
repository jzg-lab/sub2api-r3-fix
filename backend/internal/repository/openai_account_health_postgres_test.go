package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIHealthPostgresReadsRateLimitDeadline(t *testing.T) {
	db := newProbePostgres(t)
	seedProbePostgres(t, db)
	repo := &openAIDowngradeProbeRepository{db: db}
	event := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for _, reset := range []*time.Time{nil, &event} {
		_, err := db.Exec(`UPDATE accounts SET rate_limited_at=$1, rate_limit_reset_at=$2 WHERE id=7`, event, reset)
		require.NoError(t, err)
		rows, err := repo.ListOpenAIProbeHealthSnapshots(t.Context(), []int64{7})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.True(t, event.Equal(*rows[0].RateLimitedAt))
		require.Equal(t, reset == nil, rows[0].RateLimitResetAt == nil)
		label, _, _, _ := service.LabelOpenAIAccountHealth(rows[0])
		require.NotEqual(t, service.OpenAIHealthLabelRateLimited, label)
	}
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	_, err := db.Exec(`UPDATE accounts SET rate_limit_reset_at=$1 WHERE id=7`, reset)
	require.NoError(t, err)
	rows, err := repo.ListOpenAIProbeHealthSnapshots(t.Context(), []int64{7})
	require.NoError(t, err)
	require.True(t, reset.Equal(*rows[0].RateLimitResetAt))
	label, _, _, _ := service.LabelOpenAIAccountHealth(rows[0])
	require.Equal(t, service.OpenAIHealthLabelRateLimited, label)
}
