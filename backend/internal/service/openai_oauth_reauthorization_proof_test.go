package service

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func reauthorizationProofFixture(t *testing.T) (*OpenAIOAuthService, *Account, *OpenAITokenInfo, map[string]any) {
	t.Helper()
	svc, account, session, _, _ := reauthorizationIPFixture(t)
	launchReauthorizationFixture(t, svc, session)
	result, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
		SessionID: session.ID, State: session.State, Code: "fixture",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.ReauthorizationProof)
	return svc, account, result, svc.BuildAccountCredentials(result)
}

func TestReauthorizationIPProofCommitAndReplay(t *testing.T) {
	svc, account, result, credentials := reauthorizationProofFixture(t)
	ctx, err := svc.ConsumeReauthorizationProof(t.Context(), result.ReauthorizationProof,
		account.ID, account.UpdatedAt, credentials)
	require.NoError(t, err)
	require.NoError(t, ValidateOpenAIOAuthReauthorizationCommit(ctx, account, credentials))
	require.ErrorIs(t, ValidateOpenAIOAuthReauthorizationCommit(t.Context(), account, credentials),
		ErrOpenAIOAuthReauthorizationProofRequired)
	_, err = svc.ConsumeReauthorizationProof(t.Context(), result.ReauthorizationProof,
		account.ID, account.UpdatedAt, credentials)
	require.ErrorIs(t, err, ErrOpenAIOAuthReauthorizationProofRequired)

	changed := *account
	changed.UpdatedAt = account.UpdatedAt.Add(time.Microsecond)
	require.ErrorIs(t, ValidateOpenAIOAuthReauthorizationCommit(ctx, &changed, credentials),
		ErrOpenAIOAuthReauthorizationProofRequired)
	changed = *account
	changed.Extra = map[string]any{OpenAIOAuthLoginExitIPExtraKey: "198.51.100.26"}
	require.ErrorIs(t, ValidateOpenAIOAuthReauthorizationCommit(ctx, &changed, credentials),
		ErrOpenAIOAuthLoginIPChanged)
	next := maps.Clone(credentials)
	next["access_token"] = "different"
	require.ErrorIs(t, ValidateOpenAIOAuthReauthorizationCommit(ctx, account, next),
		ErrOpenAIOAuthReauthorizationProofRequired)
}

func TestReauthorizationIPProofRejectsMismatchedInputs(t *testing.T) {
	for _, mutation := range []string{
		"account", "revision", "expired", "future", "exit", "access_token",
		"refresh_token", "id_token", "client_id", "email", "chatgpt_account_id",
		"chatgpt_user_id", "organization_id", "auth_mode", "openai_auth_mode", "browser evidence",
	} {
		t.Run(mutation, func(t *testing.T) {
			svc, account, result, credentials := reauthorizationProofFixture(t)
			accountID, revision := account.ID, account.UpdatedAt
			switch mutation {
			case "browser evidence":
				proof, err := svc.sessionStore.Get(t.Context(), result.ReauthorizationProof)
				require.NoError(t, err)
				proof.ReauthorizationBrowserSessionID = ""
			case "account":
				accountID++
			case "revision":
				revision = revision.Add(time.Second)
			case "expired", "future":
				proof, err := svc.sessionStore.Get(t.Context(), result.ReauthorizationProof)
				require.NoError(t, err)
				proof.CreatedAt = time.Now().Add(-openAIReauthorizationProofTTL)
				if mutation == "future" {
					proof.CreatedAt = time.Now().Add(time.Minute)
				}
				require.NoError(t, svc.sessionStore.Create(t.Context(), proof))
			case "exit":
				svc.observeReauthorizationExitIP = func(context.Context, string) (string, error) {
					return "198.51.100.26", nil
				}
			default:
				credentials[mutation] = "different"
			}
			ctx, err := svc.ConsumeReauthorizationProof(t.Context(), result.ReauthorizationProof,
				accountID, revision, credentials)
			require.Error(t, err)
			require.Nil(t, ctx)
		})
	}
}

func TestReauthorizationIPProofCannotActAsAuthorizationSession(t *testing.T) {
	svc, _, result, _ := reauthorizationProofFixture(t)
	_, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
		SessionID: result.ReauthorizationProof, State: "fixture", Code: "fixture",
	})
	require.Error(t, err)
	launcher := &OpenAIAuthBrowserLauncher{
		launcherPath: "/usr/bin/true", sessionStore: svc.sessionStore, now: time.Now,
	}
	_, err = launcher.Launch(t.Context(), result.ReauthorizationProof)
	require.ErrorIs(t, err, ErrOpenAIAuthBrowserSessionInvalid)
}

func TestReauthorizationIPProofRejectsMalformedBinding(t *testing.T) {
	_, _, session, _, _ := reauthorizationIPFixture(t)
	session.ReauthorizationCredentialsHash = "invalid"
	require.Error(t, validateOpenAIOAuthReauthorizationBinding(session))
	session.ReauthorizationCredentialsHash = ""
	session.ReauthorizationAccountID = 0
	require.Error(t, validateOpenAIOAuthReauthorizationBinding(session))
	require.NoError(t, ValidateOpenAIOAuthReauthorizationCommit(t.Context(),
		&Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, nil))
}
