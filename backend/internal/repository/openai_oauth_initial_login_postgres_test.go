package repository

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func initialLoginPostgresFixture(t *testing.T) (*accountRepository, *sql.DB, *service.Account, initialLoginProofContext) {
	t.Helper()
	repo, db, existing := newOpenAIReauthorizationPostgres(t)
	proxy := &service.Proxy{ID: *existing.ProxyID, Protocol: "socks5", Host: "127.0.0.1",
		Port: 17923, Status: service.StatusActive}
	account := &service.Account{
		Name: "initial-login", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, ProxyID: existing.ProxyID, Concurrency: 1,
		Credentials: map[string]any{"access_token": "fixture", "email": "initial@example.test"},
		Extra:       map[string]any{service.OpenAIOAuthLoginExitIPExtraKey: "198.51.100.99"},
	}
	return repo, db, account, initialLoginCommitContext(t, account, proxy)
}

func TestOpenAIInitialLoginPostgresAtomicCreate(t *testing.T) {
	repo, db, account, ctx := initialLoginPostgresFixture(t)
	group, err := repo.client.Group.Create().SetName("initial-login-group").Save(t.Context())
	require.NoError(t, err)
	require.NoError(t, repo.CreateWithAccountGroups(ctx, account,
		[]service.AccountGroup{{GroupID: group.ID, Priority: 1}}))
	current, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "198.51.100.25", current.Extra[service.OpenAIOAuthLoginExitIPExtraKey])
	require.Equal(t, account.ProxyID, current.ProxyID)
	require.False(t, current.Schedulable, "new identities still require qualification")
	require.Equal(t, []int64{group.ID}, current.GroupIDs)
	var events int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM scheduler_outbox WHERE account_id = $1", account.ID).Scan(&events))
	require.Equal(t, 1, events)
}

func TestOpenAIInitialLoginPostgresRejectsStaleProofAndHistory(t *testing.T) {
	for _, mutation := range []string{"route", "inactive proxy", "expired", "credentials", "missing history", "different history", "outbox"} {
		t.Run(mutation, func(t *testing.T) {
			repo, db, account, ctx := initialLoginPostgresFixture(t)
			switch mutation {
			case "route":
				_, err := db.Exec("UPDATE proxies SET port = port + 1 WHERE id = $1", *account.ProxyID)
				require.NoError(t, err)
			case "inactive proxy":
				_, err := db.Exec("UPDATE proxies SET status = 'inactive' WHERE id = $1", *account.ProxyID)
				require.NoError(t, err)
			case "expired":
				ctx.proof.CreatedAt = time.Now().Add(-time.Hour)
			case "credentials":
				account.Credentials["email"] = "another@example.test"
			case "missing history", "different history":
				_, err := db.Exec(`UPDATE accounts SET deleted_at = now(),
					credentials = jsonb_set(credentials, '{email}', '"initial@example.test"'),
					extra = extra - 'openai_oauth_login_exit_ip'`)
				require.NoError(t, err)
				if mutation == "different history" {
					_, err = db.Exec(`UPDATE accounts SET extra = extra ||
						'{"openai_oauth_login_exit_ip":"198.51.100.26"}'::jsonb`)
					require.NoError(t, err)
				}
			case "outbox":
				_, err := db.Exec(`ALTER TABLE scheduler_outbox ADD CONSTRAINT reject_initial_login
					CHECK (event_type <> 'account_changed')`)
				require.NoError(t, err)
			}
			before := *account
			before.Extra = maps.Clone(account.Extra)
			before.Credentials = maps.Clone(account.Credentials)
			err := repo.CreateWithAccountGroups(ctx, account, nil)
			require.Error(t, err)
			switch mutation {
			case "route":
				require.ErrorIs(t, err, service.ErrOpenAIOAuthFixedEgressRequired)
			case "expired", "credentials":
				require.ErrorIs(t, err, service.ErrOpenAIOAuthInitialLoginProofRequired)
			case "missing history", "different history":
				require.ErrorIs(t, err, service.ErrOpenAIOAuthLoginIPChanged)
			}
			require.Equal(t, before, *account, "failed create must not publish staged state")
			var accounts, events int
			require.NoError(t, db.QueryRow("SELECT count(*) FROM accounts WHERE name = 'initial-login'").Scan(&accounts))
			require.NoError(t, db.QueryRow("SELECT count(*) FROM scheduler_outbox").Scan(&events))
			require.Zero(t, accounts)
			require.Zero(t, events)
		})
	}
}

func TestOpenAIInitialLoginPostgresSerializesDuplicateIdentity(t *testing.T) {
	repo, db, account, proof := initialLoginPostgresFixture(t)
	ctx, cancel := context.WithTimeout(proof, 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		candidate := *account
		go func() {
			<-start
			results <- repo.CreateWithAccountGroups(ctx, &candidate, nil)
		}()
	}
	close(start)
	success, duplicate := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			success++
		case errors.Is(err, service.ErrOpenAIOAuthIdentityExists):
			duplicate++
		default:
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, duplicate)
	var accounts, events int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM accounts WHERE name = 'initial-login'").Scan(&accounts))
	require.NoError(t, db.QueryRow("SELECT count(*) FROM scheduler_outbox").Scan(&events))
	require.Equal(t, 1, accounts)
	require.Equal(t, 1, events)
}
