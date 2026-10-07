package repository

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProbeCooldownPostgresDoesNotOccupyIPQueueHead(t *testing.T) {
	db := newProbePostgres(t)
	seedProbePostgres(t, db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	reset := now.Add(7 * 24 * time.Hour)
	_, err := db.Exec(`
		UPDATE proxies SET exit_ip='192.0.2.1' WHERE id=3;
		INSERT INTO accounts(id, proxy_id, updated_at) VALUES(8, 3, NOW());
		INSERT INTO openai_downgrade_probe_states(account_id, current_proxy_id, next_probe_at)
		VALUES(8, 3, NOW());
	`)
	require.NoError(t, err)
	_, err = db.Exec(`
		UPDATE accounts SET rate_limit_reset_at=$1 WHERE id=7;
	`, reset)
	require.NoError(t, err)
	_, err = db.Exec(`
		UPDATE openai_downgrade_probe_states SET next_probe_at=$1 WHERE account_id=8;
	`, now)
	require.NoError(t, err)
	_, err = db.Exec(`
		UPDATE openai_downgrade_probe_states SET next_probe_at=$1, probe_mode='qualification'
		WHERE account_id=7;
	`, now.Add(-time.Hour))
	require.NoError(t, err)
	repo := &openAIDowngradeProbeRepository{db: db}
	due, err := repo.ListDueOpenAIDowngradeStates(context.Background(), now, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.EqualValues(t, 8, due[0].AccountID, "the cooling account must not starve another account on the same IP")
	due, err = repo.ListDueOpenAIDowngradeStates(context.Background(), reset, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.EqualValues(t, 7, due[0].AccountID, "qualification becomes eligible at, not before, the deadline")
}

func TestProbeCooldownPostgresReconcilesEarlyAndPreservesLaterSchedule(t *testing.T) {
	for _, early := range []bool{true, false} {
		t.Run(map[bool]string{true: "early", false: "already_later"}[early], func(t *testing.T) {
			db := newProbePostgres(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			seedLongProbeSchedule(t, db, now, 48*time.Hour)
			next := now.Add(9 * 24 * time.Hour)
			if early {
				next = now.Add(-time.Hour)
			}
			_, err := db.Exec("UPDATE openai_downgrade_probe_states SET next_probe_at=$1", next)
			require.NoError(t, err)
			before := probePostgresSnapshot(t, db)
			repo := &openAIDowngradeProbeRepository{db: db}
			count, err := repo.ReconcileOpenAIRateLimitProbeSchedules(context.Background(), now, 22*time.Hour)
			require.NoError(t, err)
			if !early {
				require.Zero(t, count)
				require.Equal(t, before, probePostgresSnapshot(t, db))
				return
			}
			require.EqualValues(t, 1, count)
			state, err := repo.GetOpenAIDowngradeState(context.Background(), 7)
			require.NoError(t, err)
			require.False(t, state.NextProbeAt.Before(now.Add(8*24*time.Hour)))
			due, err := repo.ListDueOpenAIDowngradeStates(context.Background(), now.Add(24*time.Hour), 10)
			require.NoError(t, err)
			require.Empty(t, due)
		})
	}
}

func TestProbeCooldownPostgresExpiredLegacyHoldCanReconcile(t *testing.T) {
	db := newProbePostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedLongProbeSchedule(t, db, now, 48*time.Hour)
	_, err := db.Exec("UPDATE accounts SET rate_limit_reset_at=$1", now)
	require.NoError(t, err)
	repo := &openAIDowngradeProbeRepository{db: db}
	count, err := repo.ReconcileOpenAIRateLimitProbeSchedules(context.Background(), now, 22*time.Hour)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	state, err := repo.GetOpenAIDowngradeState(context.Background(), 7)
	require.NoError(t, err)
	require.False(t, state.NextProbeAt.Before(now.Add(15*time.Minute)))
	require.False(t, state.NextProbeAt.After(now.Add(45*time.Minute)))
}
