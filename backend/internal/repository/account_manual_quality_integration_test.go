//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestManualQualityPersistenceIsolationAndLease(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	repo := &accountRepository{sql: tx}
	var id int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO accounts (name,platform,type,credentials,extra,status,schedulable)
 VALUES ('quality-test','openai','apikey','{}','{}','active',true) RETURNING id`).Scan(&id))
	var originalExtra string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT extra::text FROM accounts WHERE id=$1`, id).Scan(&originalExtra))
	input := service.ManualQualityRequest{Model: "model-a", ManualQualityTemplate: service.DefaultManualQualityTemplates()[0]}
	first := &service.ManualQualityResult{ManualQualityRequest: input, AccountID: id, RunID: uuid.NewString(), StartedAt: time.Now().UTC(), Status: "running"}
	ok, err := repo.BeginManualQuality(ctx, first)
	require.NoError(t, err)
	require.True(t, ok)
	second := *first
	second.RunID = uuid.NewString()
	ok, err = repo.BeginManualQuality(ctx, &second)
	require.NoError(t, err)
	require.False(t, ok, "database lease rejects a second instance")
	first.Status = "completed"
	first.Answer = "answer"
	first.UpstreamModel = "mapped"
	require.NoError(t, repo.FinishManualQuality(ctx, first))
	stored, err := repo.GetManualQuality(ctx, id, "model-a", "candy")
	require.NoError(t, err)
	require.Equal(t, first.Answer, stored.Answer)
	require.Equal(t, input.Prompt, stored.Prompt)
	missing, err := repo.GetManualQuality(ctx, id, "model-b", "candy")
	require.NoError(t, err)
	require.Nil(t, missing)
	summaries, err := repo.ListManualQuality(ctx, []int64{id})
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	require.Empty(t, summaries[0].Answer)
	require.Empty(t, summaries[0].Prompt)
	ok, err = repo.BeginManualQuality(ctx, &second)
	require.NoError(t, err)
	require.True(t, ok)
	require.ErrorContains(t, repo.FinishManualQuality(ctx, first), "superseded", "old completion cannot overwrite new run")
	_, err = tx.ExecContext(ctx, `UPDATE account_manual_quality_tests SET running_until=NOW()-INTERVAL '1 second' WHERE account_id=$1`, id)
	require.NoError(t, err)
	stored, err = repo.GetManualQuality(ctx, id, "model-a", "candy")
	require.NoError(t, err)
	require.Equal(t, "unknown", stored.Status)
	third := second
	third.RunID = uuid.NewString()
	ok, err = repo.BeginManualQuality(ctx, &third)
	require.NoError(t, err)
	require.True(t, ok, "explicit new click may replace an expired run")
	var status string
	var schedulable bool
	var extra string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT status,schedulable,extra::text FROM accounts WHERE id=$1`, id).Scan(&status, &schedulable, &extra))
	require.Equal(t, "active", status)
	require.True(t, schedulable)
	require.Equal(t, originalExtra, extra)
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET deleted_at=NOW() WHERE id=$1`, id)
	require.NoError(t, err)
	stored, err = repo.GetManualQuality(ctx, id, "model-a", "candy")
	require.NoError(t, err)
	require.Nil(t, stored)
	summaries, err = repo.ListManualQuality(ctx, []int64{id})
	require.NoError(t, err)
	require.Empty(t, summaries)
}
