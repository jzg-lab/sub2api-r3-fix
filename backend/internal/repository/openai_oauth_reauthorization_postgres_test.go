package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newOpenAIReauthorizationPostgres(t *testing.T) (*accountRepository, *sql.DB, *service.Account) {
	t.Helper()
	repo, db, account := newOAuthReauthorizationPostgres(t)
	proxy, err := repo.client.Proxy.Create().
		SetName("reauth-fixture").SetProtocol("socks5").
		SetHost("127.0.0.1").SetPort(17923).Save(t.Context())
	require.NoError(t, err)
	extra, err := json.Marshal(map[string]any{
		service.OpenAIOAuthQualifiedProxyExtraKey: proxy.ID,
		service.OpenAIOAuthLoginExitIPExtraKey:    "198.51.100.25",
		"base_rpm":                                3, "codex_fingerprint_seed": "fixture-identity",
	})
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		UPDATE accounts SET platform = 'openai', proxy_id = $1,
			credentials = credentials || '{"email":"reauth@example.test"}'::jsonb,
			extra = $2::jsonb WHERE id = $3
	`, proxy.ID, string(extra), account.ID)
	require.NoError(t, err)
	account, err = repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	return repo, db, account
}

func openAIPostgresProof(t *testing.T, account *service.Account, credentials map[string]any) reauthProofContext {
	t.Helper()
	ctx := reauthCommitContext(t, account.UpdatedAt, credentials)
	ctx.proof.ReauthorizationAccountID = account.ID
	ctx.proof.ProxyID = *account.ProxyID
	ctx.proof.ReauthorizationAccountRevision = service.OpenAIOAuthAccountRevision(account)
	return ctx
}

func TestOpenAIReauthorizationPostgresAllowsRuntimeUpdatesUnderLock(t *testing.T) {
	repo, db, account := newOpenAIReauthorizationPostgres(t)
	credentials := map[string]any{"access_token": "fixture", "email": "reauth@example.test"}
	proof := openAIPostgresProof(t, account, credentials)
	require.NoError(t, repo.BatchUpdateLastUsed(t.Context(), map[int64]time.Time{account.ID: time.Now()}))
	require.NoError(t, repo.UpdateExtra(t.Context(), account.ID, map[string]any{"runtime_fixture": "retained"}))
	latest, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.False(t, account.UpdatedAt.Equal(latest.UpdatedAt))
	updated, err := repo.ApplyOAuthCredentials(proof, account.ID, account.UpdatedAt,
		service.AccountTypeOAuth, credentials, nil)
	require.NoError(t, err)
	require.Equal(t, "retained", updated.Extra["runtime_fixture"])
	require.Equal(t, latest.LastUsedAt, updated.LastUsedAt)
	require.Equal(t, account.ProxyID, updated.ProxyID)
	require.Equal(t, account.Extra[service.OpenAIOAuthLoginExitIPExtraKey],
		updated.Extra[service.OpenAIOAuthLoginExitIPExtraKey])
	_, err = repo.ApplyOAuthCredentials(proof, account.ID, account.UpdatedAt,
		service.AccountTypeOAuth, credentials, nil)
	require.ErrorIs(t, err, service.ErrOAuthReauthorizationStale)
	var events int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM scheduler_outbox WHERE account_id = $1 AND event_type = 'account_changed'",
		account.ID).Scan(&events))
	require.Positive(t, events)
}

func TestOpenAIReauthorizationPostgresRejectsCredentialChangeWithoutTimestamp(t *testing.T) {
	repo, db, account := newOpenAIReauthorizationPostgres(t)
	credentials := map[string]any{"access_token": "fixture", "email": "reauth@example.test"}
	proof := openAIPostgresProof(t, account, credentials)
	_, err := db.ExecContext(t.Context(), `
		UPDATE accounts SET credentials = jsonb_set(credentials, '{access_token}', '"newer-fixture"') WHERE id = $1
	`, account.ID)
	require.NoError(t, err)
	current, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.True(t, current.UpdatedAt.Equal(account.UpdatedAt))
	updated, err := repo.ApplyOAuthCredentials(proof, account.ID, account.UpdatedAt,
		service.AccountTypeOAuth, credentials, nil)
	require.ErrorIs(t, err, service.ErrOAuthReauthorizationStale)
	require.Nil(t, updated)
	assertOpenAIReauthUnchanged(t, repo, db, current)
}

func assertOpenAIReauthUnchanged(t *testing.T, repo *accountRepository, db *sql.DB, before *service.Account) {
	t.Helper()
	after, err := repo.GetByID(t.Context(), before.ID)
	require.NoError(t, err)
	require.Equal(t, before.Credentials, after.Credentials)
	require.Equal(t, before.Extra, after.Extra)
	require.Equal(t, before.ProxyID, after.ProxyID)
	require.Equal(t, before.Status, after.Status)
	require.Equal(t, before.Schedulable, after.Schedulable)
	require.True(t, before.UpdatedAt.Equal(after.UpdatedAt))
	var events int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM scheduler_outbox WHERE account_id = $1", before.ID).Scan(&events))
	require.Zero(t, events)
}

func TestOpenAIReauthorizationPostgresExactlyOneBoundCommit(t *testing.T) {
	repo, db, account := newOpenAIReauthorizationPostgres(t)
	credentials := map[string]any{"access_token": "fixture", "email": "reauth@example.test"}
	ctx := openAIPostgresProof(t, account, credentials)
	requestContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := repo.ApplyOAuthCredentials(requestContext, account.ID, account.UpdatedAt,
				service.AccountTypeOAuth, credentials, map[string]any{
					service.OpenAIOAuthLoginExitIPExtraKey:    "198.51.100.26",
					service.OpenAIOAuthQualifiedProxyExtraKey: int64(999),
					"codex_fingerprint_seed":                  "untrusted-replacement",
				})
			results <- err
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, service.ErrOAuthReauthorizationStale):
			conflicts++
		default:
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
	current, err := repo.GetByID(t.Context(), account.ID)
	require.NoError(t, err)
	require.Equal(t, credentials["access_token"], current.Credentials["access_token"])
	require.Equal(t, account.Credentials["model_mapping"], current.Credentials["model_mapping"])
	require.Equal(t, account.Extra, current.Extra)
	require.Equal(t, account.ProxyID, current.ProxyID)
	require.Equal(t, service.StatusActive, current.Status)
	require.True(t, current.Schedulable)
	require.Greater(t, current.GetCredentialAsInt64("_token_version"),
		account.GetCredentialAsInt64("_token_version"))
	var events int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM scheduler_outbox WHERE account_id = $1", account.ID).Scan(&events))
	require.Equal(t, 1, events)
}

func TestOpenAIReauthorizationPostgresRejectsUnboundChanges(t *testing.T) {
	for _, mutation := range []string{
		"missing proof", "wrong account", "wrong revision", "wrong proxy", "changed IP",
		"expired proof", "future proof", "changed credentials", "missing history",
	} {
		t.Run(mutation, func(t *testing.T) {
			repo, db, account := newOpenAIReauthorizationPostgres(t)
			credentials := map[string]any{"access_token": "fixture", "email": "reauth@example.test"}
			proof := openAIPostgresProof(t, account, credentials)
			var expected error = service.ErrOpenAIOAuthReauthorizationProofRequired
			switch mutation {
			case "wrong account":
				proof.proof.ReauthorizationAccountID++
			case "wrong revision":
				proof.proof.ReauthorizationRevision = account.UpdatedAt.Add(time.Second).Format(time.RFC3339Nano)
			case "wrong proxy":
				proof.proof.ProxyID++
			case "changed IP":
				proof.proof.ReauthorizationExitIP = "198.51.100.26"
				expected = service.ErrOpenAIOAuthLoginIPChanged
			case "expired proof":
				proof.proof.CreatedAt = time.Now().Add(-time.Hour)
			case "future proof":
				proof.proof.CreatedAt = time.Now().Add(time.Minute)
			case "changed credentials":
				credentials["access_token"] = "fixture-replaced"
			case "missing history":
				_, err := db.ExecContext(t.Context(),
					"UPDATE accounts SET extra = extra - $1 WHERE id = $2",
					service.OpenAIOAuthLoginExitIPExtraKey, account.ID)
				require.NoError(t, err)
				account, err = repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				expected = service.ErrOpenAIOAuthLoginIPChanged
			}
			var ctx context.Context = proof
			if mutation == "missing proof" {
				ctx = t.Context()
			}
			updated, err := repo.ApplyOAuthCredentials(ctx, account.ID, account.UpdatedAt,
				service.AccountTypeOAuth, credentials, nil)
			require.ErrorIs(t, err, expected)
			require.Nil(t, updated)
			assertOpenAIReauthUnchanged(t, repo, db, account)
		})
	}
}

func TestOpenAIReauthorizationPostgresOutboxFailureRollsBack(t *testing.T) {
	repo, db, account := newOpenAIReauthorizationPostgres(t)
	credentials := map[string]any{"access_token": "fixture", "email": "reauth@example.test"}
	_, err := db.ExecContext(t.Context(),
		`ALTER TABLE scheduler_outbox ADD CONSTRAINT reject_reauth CHECK (event_type <> 'account_changed')`)
	require.NoError(t, err)
	updated, err := repo.ApplyOAuthCredentials(openAIPostgresProof(t, account, credentials),
		account.ID, account.UpdatedAt, service.AccountTypeOAuth, credentials, nil)
	require.Error(t, err)
	require.Nil(t, updated)
	assertOpenAIReauthUnchanged(t, repo, db, account)
}

func TestOpenAIReauthorizationPostgresPreservesManualPause(t *testing.T) {
	for _, paused := range []bool{false, true} {
		name := "automatic_error"
		if paused {
			name = "administrator_pause"
		}
		t.Run(name, func(t *testing.T) {
			repo, db, account := newOpenAIReauthorizationPostgres(t)
			_, err := db.ExecContext(t.Context(), `
				INSERT INTO openai_downgrade_probe_controls(account_id, manual_paused)
				VALUES ($1, $2)
			`, account.ID, paused)
			require.NoError(t, err)
			credentials := map[string]any{"access_token": "fixture", "email": "reauth@example.test"}
			updated, err := repo.ApplyOAuthCredentials(openAIPostgresProof(t, account, credentials),
				account.ID, account.UpdatedAt, service.AccountTypeOAuth, credentials, nil)
			require.NoError(t, err)
			require.Equal(t, service.StatusActive, updated.Status)
			require.Equal(t, !paused, updated.Schedulable)
			var stillPaused bool
			require.NoError(t, db.QueryRowContext(t.Context(),
				"SELECT manual_paused FROM openai_downgrade_probe_controls WHERE account_id = $1",
				account.ID).Scan(&stillPaused))
			require.Equal(t, paused, stillPaused)
		})
	}
}
