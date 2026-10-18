package service

import (
	"context"
	"maps"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

func explicitReloginFixture(t *testing.T) (*OpenAIOAuthService, *Account, *OpenAIOAuthSession) {
	t.Helper()
	svc, account, _, _, _ := reauthorizationIPFixture(t)
	account.Credentials = map[string]any{
		"email": "fixture@example.invalid", "chatgpt_account_id": "workspace-fixture",
	}
	delete(account.Extra, OpenAIOAuthLoginExitIPExtraKey)
	result, err := svc.GenerateReauthorizationAuthURL(t.Context(), account.ID,
		account.UpdatedAt.Format(time.RFC3339Nano), account.ProxyID, "", PlatformOpenAI,
		OpenAIOAuthAccountRevision(account), true)
	require.NoError(t, err)
	session, err := svc.sessionStore.Get(t.Context(), result.SessionID)
	require.NoError(t, err)
	require.True(t, session.ReauthorizationFreshLogin)
	require.Empty(t, session.ReauthorizationExitIP)
	return svc, account, session
}

func TestExplicitReloginAllowsMissingHistoryAndKeepsRouteBinding(t *testing.T) {
	svc, account, session := explicitReloginFixture(t)
	require.NoError(t, svc.validateReauthorizationSession(t.Context(), session, true))
	account.ProxyID = nil
	require.ErrorIs(t, svc.validateReauthorizationSession(t.Context(), session, false), ErrOpenAIOAuthProxyMismatch)
}

func TestExplicitReloginProofBindsIdentityAndIsOneUse(t *testing.T) {
	svc, account, session := explicitReloginFixture(t)
	result := &OpenAITokenInfo{
		AccessToken: "fixture-access", Email: account.GetCredential("email"),
		ChatGPTAccountID: account.GetCredential("chatgpt_account_id"),
	}
	require.NoError(t, svc.issueReauthorizationProof(t.Context(), session, result))
	credentials := svc.BuildAccountCredentials(result)
	ctx, err := svc.ConsumeReauthorizationProof(t.Context(), result.ReauthorizationProof,
		account.ID, account.UpdatedAt, credentials)
	require.NoError(t, err)
	extra := maps.Clone(account.Extra)
	require.NoError(t, RecordOpenAIOAuthReloginEvidence(ctx, account, credentials, extra))
	require.NotEmpty(t, extra["openai_oauth_relogin_verified_at"])
	require.NotContains(t, extra, OpenAIOAuthLoginExitIPExtraKey)
	require.NotContains(t, account.Extra, "openai_oauth_relogin_verified_at")
	_, err = svc.ConsumeReauthorizationProof(t.Context(), result.ReauthorizationProof,
		account.ID, account.UpdatedAt, credentials)
	require.Error(t, err)
}

func TestExplicitReloginRejectsDifferentEmailOrWorkspace(t *testing.T) {
	for _, field := range []string{"email", "chatgpt_account_id"} {
		t.Run(field, func(t *testing.T) {
			svc, account, session := explicitReloginFixture(t)
			result := &OpenAITokenInfo{
				AccessToken: "fixture-access", Email: account.GetCredential("email"),
				ChatGPTAccountID: account.GetCredential("chatgpt_account_id"),
			}
			require.NoError(t, svc.issueReauthorizationProof(t.Context(), session, result))
			proof, err := svc.sessionStore.Get(t.Context(), result.ReauthorizationProof)
			require.NoError(t, err)
			if field == "email" {
				result.Email = "another@example.invalid"
			} else {
				result.ChatGPTAccountID = "another-workspace"
			}
			credentials := svc.BuildAccountCredentials(result)
			require.ErrorIs(t, svc.issueReauthorizationProof(t.Context(), session, result), ErrOpenAIOAuthIdentityChanged)
			proof.ReauthorizationCredentialsHash, err = openAIReauthorizationCredentialsHash(credentials)
			require.NoError(t, err)
			ctx := context.WithValue(t.Context(), openAIReauthorizationProofKey{}, *proof)
			require.ErrorIs(t, ValidateOpenAIOAuthReauthorizationCommit(ctx, account, credentials), ErrOpenAIOAuthIdentityChanged)
		})
	}
}

func TestExplicitReloginBrowserUsesAccountBoundWorkspace(t *testing.T) {
	svc, account, session := explicitReloginFixture(t)
	launcher := &OpenAIAuthBrowserLauncher{}
	launcher.SetOAuthService(svc)
	login := &OpenAIAuthBrowserLogin{
		Email: "  FIXTURE@example.invalid  ", Password: "one-time", WorkspaceID: "caller-substitution",
	}
	require.NoError(t, launcher.prepareLogin(t.Context(), session, login))
	require.Equal(t, account.GetCredential("chatgpt_account_id"), login.WorkspaceID)
	login.Email = "different@example.invalid"
	require.ErrorIs(t, launcher.prepareLogin(t.Context(), session, login), ErrOpenAIOAuthIdentityChanged)
}

func TestExplicitReloginSessionModeIsDurableAndStrict(t *testing.T) {
	_, _, session := explicitReloginFixture(t)
	raw := map[string]any{
		"proxy_id": "1", "created_at": session.CreatedAt.Format(time.RFC3339Nano),
		"reauthorization_account_id": "42", "reauthorization_revision": session.ReauthorizationRevision,
		"reauthorization_account_revision": session.ReauthorizationAccountRevision,
		"reauthorization_fresh_login":      true, "reauthorization_exit_ip": "",
	}
	row := &dbent.PendingAuthSession{LocalFlowState: map[string]any{"openai_oauth": raw}}
	decoded, err := decodeOpenAIOAuthSession(row)
	require.NoError(t, err)
	require.True(t, decoded.ReauthorizationFreshLogin)
	for _, invalid := range []any{"true", 1, nil} {
		raw["reauthorization_fresh_login"] = invalid
		_, err := decodeOpenAIOAuthSession(row)
		require.Error(t, err)
	}
}
