package service

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func reauthorizationIPFixture(t *testing.T) (*OpenAIOAuthService, *Account, *OpenAIOAuthSession, *Proxy, *openaiOAuthClientStateStub) {
	t.Helper()
	svc, _, proxy, client := oauthRouteFixture(t)
	account := &Account{
		ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		ProxyID: &proxy.ID, UpdatedAt: time.Now().UTC(),
		Extra: map[string]any{OpenAIOAuthLoginExitIPExtraKey: "198.51.100.25"},
	}
	svc.SetReauthorizationAccountLookup(func(context.Context, int64) (*Account, error) {
		copy := *account
		return &copy, nil
	})
	result, err := svc.GenerateReauthorizationAuthURL(t.Context(), account.ID,
		account.UpdatedAt.Format(time.RFC3339Nano), account.ProxyID, "", PlatformOpenAI, "")
	require.NoError(t, err)
	session, err := svc.sessionStore.Get(t.Context(), result.SessionID)
	require.NoError(t, err)
	return svc, account, session, proxy, client
}

func reauthorizationProofFixture(t *testing.T) (*OpenAIOAuthService, *Account, *OpenAITokenInfo, map[string]any) {
	t.Helper()
	svc, account, session, _, _ := reauthorizationIPFixture(t)
	result, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
		SessionID: session.ID, State: session.State, Code: "fixture",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.ReauthorizationProof)
	return svc, account, result, svc.BuildAccountCredentials(result)
}

func TestLocalDirectReauthorizationProof(t *testing.T) {
	svc, account, _, _, _ := reauthorizationIPFixture(t)
	account.ProxyID = nil
	account.Extra = nil
	result, err := svc.GenerateReauthorizationAuthURL(t.Context(), account.ID, account.UpdatedAt.Format(time.RFC3339Nano), nil, "", PlatformOpenAI, "")
	require.NoError(t, err)
	session, err := svc.sessionStore.Get(t.Context(), result.SessionID)
	require.NoError(t, err)
	require.Zero(t, session.ProxyID)
	tokens, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{SessionID: session.ID, State: session.State, Code: "direct"})
	require.NoError(t, err)
	credentials := svc.BuildAccountCredentials(tokens)
	ctx, err := svc.ConsumeReauthorizationProof(t.Context(), tokens.ReauthorizationProof, account.ID, account.UpdatedAt, credentials)
	require.NoError(t, err)
	require.NoError(t, ValidateOAuthReauthorizationUpdate(ctx, account, account.UpdatedAt, credentials))
	_, err = svc.ConsumeReauthorizationProof(t.Context(), tokens.ReauthorizationProof, account.ID, account.UpdatedAt, credentials)
	require.Error(t, err)
	account.Credentials = map[string]any{"access_token": "changed"}
	require.Error(t, ValidateOAuthReauthorizationUpdate(ctx, account, account.UpdatedAt, credentials))
}
