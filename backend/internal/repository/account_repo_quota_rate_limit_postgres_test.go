package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/stretchr/testify/require"
)

func TestAccountListQuotaRateLimitPostgres(t *testing.T) {
	db := newProbePostgresWithMigrations(t, nil)
	_, err := db.Exec(`ALTER TABLE accounts ADD COLUMN temp_unschedulable_until TIMESTAMPTZ`)
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := &accountRepository{client: client}
	ctx := context.Background()
	future := time.Now().Add(6 * 24 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for i, extra := range []map[string]any{
		{"codex_7d_used_percent": 100, "codex_7d_reset_at": future},
		{"codex_7d_used_percent": 99.6, "codex_7d_reset_at": future},
		{"codex_7d_used_percent": 100, "codex_7d_reset_at": past},
		{"codex_7d_used_percent": 100, "codex_7d_reset_at": "invalid"},
		{"codex_7d_used_percent": 100, "codex_7d_reset_at": future, "auto_pause_5h_disabled": true, "auto_pause_7d_disabled": true},
		{"codex_7d_used_percent": 100, "codex_7d_reset_at": future, "auto_pause_5h_disabled": "true", "auto_pause_7d_disabled": 1},
		{"codex_7d_used_percent": "100", "codex_7d_reset_at": future, "auto_pause_7d_disabled": true},
		{}, // Actual 429 is applied below.
		{"codex_7d_used_percent": 100},
	} {
		raw, err := json.Marshal(extra)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO accounts(id, extra, updated_at) VALUES($1, $2, NOW())`, i+1, string(raw))
		require.NoError(t, err)
	}
	_, err = db.Exec(`UPDATE accounts SET rate_limit_reset_at = NOW() + INTERVAL '1 hour' WHERE id = 8`)
	require.NoError(t, err)
	for status, want := range map[string][]int64{
		"rate_limited":  {1, 7, 8},
		"active":        {2, 3, 4, 5, 6, 9},
		"unschedulable": nil,
	} {
		t.Run(status, func(t *testing.T) {
			q, err := repo.accountListFilteredQuery(ctx, "", "", status, "", 0, "")
			require.NoError(t, err)
			total, err := q.Clone().Count(ctx)
			require.NoError(t, err)
			require.Equal(t, len(want), total)
			ids, err := q.Order(dbent.Asc(dbaccount.FieldID)).IDs(ctx)
			require.NoError(t, err)
			require.Equal(t, want, ids)
		})
	}
	q, err := repo.accountListFilteredQuery(ctx, "openai", "oauth", "rate_limited", "", 0, "")
	require.NoError(t, err)
	ids, err := q.Order(dbent.Asc(dbaccount.FieldID)).Offset(1).Limit(1).IDs(ctx)
	require.NoError(t, err)
	require.Equal(t, []int64{7}, ids, "pagination applies after quota classification")
	_, err = db.Exec(`UPDATE accounts SET extra = '{}' WHERE id = 1`)
	require.NoError(t, err)
	q, err = repo.accountListFilteredQuery(ctx, "", "", "rate_limited", "", 0, "")
	require.NoError(t, err)
	ids, err = q.Order(dbent.Asc(dbaccount.FieldID)).IDs(ctx)
	require.NoError(t, err)
	require.Equal(t, []int64{7, 8}, ids)
	var stored429 *time.Time
	require.NoError(t, db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = 7`).Scan(&stored429))
	require.Nil(t, stored429, "quota filtering must not write upstream 429 state")
	_, err = db.Exec(`UPDATE accounts SET extra = '{}' WHERE id = 7`)
	require.NoError(t, err)
	q, err = repo.accountListFilteredQuery(ctx, "", "", "active", "", 0, "")
	require.NoError(t, err)
	total, err := q.Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 8, total, "empty quota candidates must not exclude active accounts")
}
