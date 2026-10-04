package service

import (
	"context"
	"maps"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func initialLoginFixture(t *testing.T) (*OpenAIOAuthService, *OpenAIOAuthSession, *Proxy, *openaiOAuthClientStateStub) {
	t.Helper()
	svc, _, _, proxy, client := reauthorizationIPFixture(t)
	result, err := svc.GenerateAuthURL(t.Context(), &proxy.ID, "", PlatformOpenAI)
	require.NoError(t, err)
	session, err := svc.sessionStore.Get(t.Context(), result.SessionID)
	require.NoError(t, err)
	require.Equal(t, "198.51.100.25", session.LoginExitIP)
	return svc, session, proxy, client
}

func launchInitialLoginFixture(t *testing.T, svc *OpenAIOAuthService, session *OpenAIOAuthSession) {
	t.Helper()
	launcher := NewOpenAIAuthBrowserLauncher(&config.Config{Gateway: config.GatewayConfig{
		AuthBrowserLauncher: "/usr/bin/true",
	}}, svc.sessionStore, svc.proxyRepo)
	launcher.fixedEgressRoutes = svc.fixedEgressRoutes
	launcher.SetReauthorizationService(svc)
	launcher.newCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		require.Len(t, args, 4)
		require.Equal(t, session.LoginExitIP, args[3])
		return exec.CommandContext(ctx, "/usr/bin/true")
	}
	result, err := launcher.Launch(t.Context(), session.ID)
	require.NoError(t, err)
	require.True(t, result.Launched)
}

func initialLoginResult(t *testing.T) (*OpenAIOAuthService, *OpenAITokenInfo, *Account, *Proxy) {
	t.Helper()
	svc, session, proxy, _ := initialLoginFixture(t)
	launchInitialLoginFixture(t, svc, session)
	result, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
		SessionID: session.ID, State: session.State, Code: "fixture",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.InitialAuthorizationProof)
	require.Empty(t, result.ReauthorizationProof)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ProxyID: &proxy.ID,
		Credentials: svc.BuildAccountCredentials(result)}
	return svc, result, account, proxy
}

func TestOpenAIInitialLoginEndToEndProof(t *testing.T) {
	svc, result, account, proxy := initialLoginResult(t)
	ctx, err := svc.ConsumeInitialAuthorizationProof(t.Context(), result.InitialAuthorizationProof, account)
	require.NoError(t, err)
	ip, err := OpenAIOAuthInitialLoginIPForCreate(ctx, account, false, "")
	require.NoError(t, err)
	require.Equal(t, "198.51.100.25", ip)
	require.NoError(t, ValidateOpenAIOAuthInitialLoginProxy(ctx, proxy))
	_, err = svc.ConsumeInitialAuthorizationProof(t.Context(), result.InitialAuthorizationProof, account)
	require.ErrorIs(t, err, ErrOpenAIOAuthInitialLoginProofRequired)
	_, err = OpenAIOAuthInitialLoginIPForCreate(ctx, account, true, "")
	require.ErrorIs(t, err, ErrOpenAIOAuthLoginIPChanged, "current observation cannot restore missing history")
	_, err = OpenAIOAuthInitialLoginIPForCreate(ctx, account, true, "198.51.100.99")
	require.ErrorIs(t, err, ErrOpenAIOAuthLoginIPChanged)
	ip, err = OpenAIOAuthInitialLoginIPForCreate(ctx, account, true, "198.51.100.25")
	require.NoError(t, err)
	require.Equal(t, "198.51.100.25", ip)

	account.Credentials = maps.Clone(account.Credentials)
	account.Credentials["access_token"] = "other-fixture"
	_, err = OpenAIOAuthInitialLoginIPForCreate(ctx, account, false, "")
	require.ErrorIs(t, err, ErrOpenAIOAuthInitialLoginProofRequired)
	proxy.Port++
	require.ErrorIs(t, ValidateOpenAIOAuthInitialLoginProxy(ctx, proxy), ErrOpenAIOAuthFixedEgressRequired)
}

func TestOpenAIInitialLoginRejectsUnverifiedBrowserBeforeExchange(t *testing.T) {
	for _, mutation := range []string{"not launched", "launch failed", "different session", "different route", "wrong IP"} {
		t.Run(mutation, func(t *testing.T) {
			svc, session, proxy, client := initialLoginFixture(t)
			switch mutation {
			case "launch failed":
				launcher := NewOpenAIAuthBrowserLauncher(&config.Config{Gateway: config.GatewayConfig{
					AuthBrowserLauncher: "/usr/bin/false",
				}}, svc.sessionStore, svc.proxyRepo)
				launcher.fixedEgressRoutes = svc.fixedEgressRoutes
				launcher.SetReauthorizationService(svc)
				_, err := launcher.Launch(t.Context(), session.ID)
				require.Error(t, err)
			case "different session":
				other := *session
				other.ID = strings.Repeat("a", 32)
				require.NoError(t, svc.recordAuthorizationBrowser(t.Context(), &other))
			case "different route":
				launchInitialLoginFixture(t, svc, session)
				proxy.Port++
			case "wrong IP":
				launchInitialLoginFixture(t, svc, session)
				svc.observeReauthorizationExitIP = func(context.Context, string) (string, error) {
					return "198.51.100.99", nil
				}
			}
			result, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
				SessionID: session.ID, State: session.State, Code: "fixture",
			})
			require.Error(t, err)
			require.Nil(t, result)
			require.Zero(t, atomic.LoadInt32(&client.exchangeCalled))
		})
	}
}

func TestOpenAIInitialLoginProofRejectsStaleOrSubstitutedCreate(t *testing.T) {
	for _, mutation := range []string{"credentials", "identity", "proxy", "platform", "type", "expired",
		"future", "route", "pin", "IP", "purpose", "unlaunched proof"} {
		t.Run(mutation, func(t *testing.T) {
			svc, result, account, proxy := initialLoginResult(t)
			proof, err := svc.sessionStore.Get(t.Context(), result.InitialAuthorizationProof)
			require.NoError(t, err)
			switch mutation {
			case "credentials":
				account.Credentials["refresh_token"] = "other-fixture"
			case "identity":
				account.Credentials["email"] = "other@example.test"
			case "proxy":
				id := int64(999)
				account.ProxyID = &id
			case "platform":
				account.Platform = PlatformAnthropic
			case "type":
				account.Type = AccountTypeAPIKey
			case "expired":
				proof.CreatedAt = time.Now().Add(-openAIReauthorizationProofTTL)
			case "future":
				proof.CreatedAt = time.Now().Add(time.Minute)
			case "route":
				proxy.Port++
			case "pin":
				delete(svc.fixedEgressRoutes, proxy.ID)
			case "IP":
				svc.observeReauthorizationExitIP = func(context.Context, string) (string, error) {
					return "198.51.100.99", nil
				}
			case "purpose":
				proof.ReauthorizationAccountID = 42
			case "unlaunched proof":
				proof.LoginBrowserSessionID = ""
			}
			ctx, err := svc.ConsumeInitialAuthorizationProof(t.Context(), result.InitialAuthorizationProof, account)
			require.Error(t, err)
			require.Nil(t, ctx)
		})
	}
}

func TestOpenAIInitialLoginLegacyFlowDoesNotInventHistory(t *testing.T) {
	svc, session, proxy, _ := oauthRouteFixture(t)
	result, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
		SessionID: session.ID, State: session.State, Code: "fixture",
	})
	require.NoError(t, err)
	require.Empty(t, result.InitialAuthorizationProof)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ProxyID: &proxy.ID,
		Credentials: svc.BuildAccountCredentials(result),
		Extra:       map[string]any{OpenAIOAuthLoginExitIPExtraKey: "198.51.100.99"}}
	ctx, err := (*OpenAIOAuthService)(nil).ConsumeInitialAuthorizationProof(t.Context(), "", account)
	require.NoError(t, err)
	ip, err := OpenAIOAuthInitialLoginIPForCreate(ctx, account, false, "")
	require.NoError(t, err)
	require.Empty(t, ip)
}

func TestOpenAIInitialLoginSessionPersistenceAndMalformedBinding(t *testing.T) {
	svc, result, _, _ := initialLoginResult(t)
	proof, err := svc.sessionStore.Get(t.Context(), result.InitialAuthorizationProof)
	require.NoError(t, err)
	valid := map[string]any{
		"proxy_id": "7", "created_at": proof.CreatedAt.Format(time.RFC3339Nano), "platform": PlatformOpenAI,
		"state": "", "code_verifier": "", "proxy_route_hash": proof.ProxyRouteHash,
		"login_exit_ip": proof.LoginExitIP, "login_browser_session_id": proof.LoginBrowserSessionID,
		"login_credentials_hash": proof.LoginCredentialsHash,
	}
	decode := func(raw map[string]any) (*OpenAIOAuthSession, error) {
		return decodeOpenAIOAuthSession(&dbent.PendingAuthSession{LocalFlowState: map[string]any{"openai_oauth": raw}})
	}
	decoded, err := decode(valid)
	require.NoError(t, err)
	require.Equal(t, proof.LoginExitIP, decoded.LoginExitIP)
	require.Equal(t, proof.LoginBrowserSessionID, decoded.LoginBrowserSessionID)
	require.Equal(t, proof.LoginCredentialsHash, decoded.LoginCredentialsHash)
	for _, key := range []string{"login_exit_ip", "login_browser_session_id", "login_credentials_hash"} {
		for _, value := range []any{42, nil, "invalid"} {
			raw := maps.Clone(valid)
			raw[key] = value
			_, err := decode(raw)
			require.ErrorIs(t, err, ErrOpenAIOAuthSessionInvalid, key)
		}
	}
}

func TestOpenAIInitialLoginProofConcurrentConsumeHasOneWinner(t *testing.T) {
	svc, result, account, _ := initialLoginResult(t)
	var wg sync.WaitGroup
	var successes atomic.Int32
	for range 8 {
		wg.Go(func() {
			_, err := svc.ConsumeInitialAuthorizationProof(t.Context(), result.InitialAuthorizationProof, account)
			if err == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, successes.Load())
}
