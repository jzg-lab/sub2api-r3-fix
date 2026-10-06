package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newOpenAIAuthStatePostgres(t *testing.T) (*accountRepository, *sql.DB, *service.Account) {
	t.Helper()
	repo, db, original := newOAuthReauthorizationPostgres(t)
	_, err := db.Exec(`UPDATE accounts SET platform='openai', status='active', schedulable=true WHERE id=$1`, original.ID)
	require.NoError(t, err)
	before, err := repo.GetByID(context.Background(), original.ID)
	require.NoError(t, err)
	require.True(t, service.IsOpenAIBrowserOAuthAccount(before))
	return repo, db, before
}

func TestOpenAIAuthStatePostgresCAS(t *testing.T) {
	for _, mutation := range []string{
		"unchanged", "telemetry", "credentials", "proxy", "status", "schedulable", "deleted", "parent",
	} {
		t.Run(mutation, func(t *testing.T) {
			repo, db, original := newOpenAIAuthStatePostgres(t)
			ctx := context.Background()
			_, err := db.Exec(`UPDATE accounts SET status='active', schedulable=true WHERE id=$1`, original.ID)
			require.NoError(t, err)
			before, err := repo.GetByID(ctx, original.ID)
			require.NoError(t, err)
			switch mutation {
			case "telemetry":
				_, err = db.Exec(`UPDATE accounts SET updated_at=NOW(), last_used_at=NOW() WHERE id=$1`, before.ID)
			case "credentials":
				_, err = db.Exec(`UPDATE accounts SET credentials=credentials || '{"expires_at":"9999999999"}'::jsonb WHERE id=$1`, before.ID)
			case "proxy":
				_, err = db.Exec(`UPDATE accounts SET proxy_id=COALESCE(proxy_id,0)+1 WHERE id=$1`, before.ID)
			case "status":
				_, err = db.Exec(`UPDATE accounts SET status='disabled' WHERE id=$1`, before.ID)
			case "schedulable":
				_, err = db.Exec(`UPDATE accounts SET schedulable=false WHERE id=$1`, before.ID)
			case "deleted":
				_, err = db.Exec(`UPDATE accounts SET deleted_at=NOW() WHERE id=$1`, before.ID)
			case "parent":
				_, err = db.Exec(`UPDATE accounts SET parent_account_id=id+1 WHERE id=$1`, before.ID)
			}
			require.NoError(t, err)
			var outboxBefore int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduler_outbox`).Scan(&outboxBefore))
			message := "fixture authentication rejected"
			applied, err := repo.ApplyOpenAIAuthStateIfUnchanged(ctx, before, service.OpenAIAuthStateUpdate{ErrorMessage: &message})
			require.NoError(t, err)
			expected := mutation == "unchanged" || mutation == "telemetry"
			require.Equal(t, expected, applied)
			var status string
			require.NoError(t, db.QueryRow(`SELECT status FROM accounts WHERE id=$1`, before.ID).Scan(&status))
			require.Equal(t, expected, status == service.StatusError)
			var outboxAfter int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM scheduler_outbox`).Scan(&outboxAfter))
			if expected {
				require.Equal(t, outboxBefore+1, outboxAfter)
				applied, err = repo.ApplyOpenAIAuthStateIfUnchanged(ctx, before, service.OpenAIAuthStateUpdate{ErrorMessage: &message})
				require.NoError(t, err)
				require.False(t, applied, "old failure cannot overwrite a committed status change")
			} else {
				require.Equal(t, outboxBefore, outboxAfter)
			}
		})
	}
}

func TestOpenAIAuthStatePostgresCooldownAndRollback(t *testing.T) {
	repo, db, original := newOpenAIAuthStatePostgres(t)
	ctx := context.Background()
	before, err := repo.GetByID(ctx, original.ID)
	require.NoError(t, err)
	long := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	short := long.Add(-time.Minute)
	applied, err := repo.ApplyOpenAIAuthStateIfUnchanged(ctx, before, service.OpenAIAuthStateUpdate{CooldownUntil: &long, CooldownReason: "long"})
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = repo.ApplyOpenAIAuthStateIfUnchanged(ctx, before, service.OpenAIAuthStateUpdate{CooldownUntil: &short, CooldownReason: "short"})
	require.NoError(t, err)
	require.True(t, applied)
	current, err := repo.GetByID(ctx, before.ID)
	require.NoError(t, err)
	require.WithinDuration(t, long, *current.TempUnschedulableUntil, time.Microsecond)
	require.Equal(t, "long", current.TempUnschedulableReason)

	// A failed outbox insertion must roll the account mutation back as well.
	_, err = db.Exec(`ALTER TABLE scheduler_outbox ADD CONSTRAINT reject_auth_fixture CHECK (event_type <> 'account_changed') NOT VALID`)
	require.NoError(t, err)
	message := "fixture rejection"
	applied, err = repo.ApplyOpenAIAuthStateIfUnchanged(ctx, before, service.OpenAIAuthStateUpdate{ErrorMessage: &message})
	require.Error(t, err)
	require.False(t, applied)
	current, err = repo.GetByID(ctx, before.ID)
	require.NoError(t, err)
	require.Equal(t, before.Status, current.Status)
	require.WithinDuration(t, long, *current.TempUnschedulableUntil, time.Microsecond)
}

func TestOpenAIAuthStatePostgresCanceledAndInvalid(t *testing.T) {
	repo, _, before := newOpenAIAuthStatePostgres(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	message := "fixture rejection"
	applied, err := repo.ApplyOpenAIAuthStateIfUnchanged(ctx, before, service.OpenAIAuthStateUpdate{ErrorMessage: &message})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, applied)
	until := time.Now().Add(time.Minute)
	for _, change := range []service.OpenAIAuthStateUpdate{
		{}, {ErrorMessage: &message, CooldownUntil: &until},
	} {
		applied, err = repo.ApplyOpenAIAuthStateIfUnchanged(context.Background(), before, change)
		require.Error(t, err)
		require.False(t, applied)
	}
}
