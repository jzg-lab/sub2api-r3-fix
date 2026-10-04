package service

import (
	"context"
	"errors"
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

func reauthorizationBrowserLauncher(t *testing.T, svc *OpenAIOAuthService, succeed bool) *OpenAIAuthBrowserLauncher {
	t.Helper()
	launcher := NewOpenAIAuthBrowserLauncher(&config.Config{Gateway: config.GatewayConfig{
		AuthBrowserLauncher: "/usr/bin/true",
	}}, svc.sessionStore, svc.proxyRepo)
	launcher.fixedEgressRoutes = svc.fixedEgressRoutes
	launcher.SetReauthorizationService(svc)
	launcher.newCommand = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		require.Len(t, args, 4)
		require.Equal(t, "198.51.100.25", args[3])
		command := "/usr/bin/true"
		if !succeed {
			command = "/usr/bin/false"
		}
		return exec.CommandContext(ctx, command)
	}
	return launcher
}

func launchReauthorizationFixture(t *testing.T, svc *OpenAIOAuthService, session *OpenAIOAuthSession) {
	t.Helper()
	result, err := reauthorizationBrowserLauncher(t, svc, true).Launch(t.Context(), session.ID)
	require.NoError(t, err)
	require.True(t, result.Launched)
}

func TestReauthorizationBrowserRequiredBeforeExchange(t *testing.T) {
	for _, scenario := range []string{"not launched", "launch failed", "different session"} {
		t.Run(scenario, func(t *testing.T) {
			svc, account, session, _, client := reauthorizationIPFixture(t)
			switch scenario {
			case "launch failed":
				_, err := reauthorizationBrowserLauncher(t, svc, false).Launch(t.Context(), session.ID)
				require.Error(t, err)
			case "different session":
				other, err := svc.GenerateReauthorizationAuthURL(t.Context(), account.ID,
					session.ReauthorizationRevision, account.ProxyID, "", PlatformOpenAI)
				require.NoError(t, err)
				result, err := reauthorizationBrowserLauncher(t, svc, true).Launch(t.Context(), other.SessionID)
				require.NoError(t, err)
				require.True(t, result.Launched)
			}
			result, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
				SessionID: session.ID, State: session.State, Code: "fixture",
			})
			require.Error(t, err, "an unlaunched reauthorization must not reach the upstream")
			require.ErrorIs(t, err, ErrOpenAIOAuthReauthorizationProofRequired)
			require.Nil(t, result)
			require.Zero(t, atomic.LoadInt32(&client.exchangeCalled))
		})
	}
}

func TestReauthorizationBrowserEvidenceRejectsChangedBinding(t *testing.T) {
	for _, mutation := range []string{"account", "revision", "proxy", "route", "IP", "session", "expiry", "purpose"} {
		t.Run(mutation, func(t *testing.T) {
			svc, _, session, _, client := reauthorizationIPFixture(t)
			launchReauthorizationFixture(t, svc, session)
			evidenceID := authorizationBrowserEvidence(session).ID
			evidence, err := svc.sessionStore.Get(t.Context(), evidenceID)
			require.NoError(t, err)
			switch mutation {
			case "account":
				evidence.ReauthorizationAccountID++
			case "revision":
				evidence.ReauthorizationRevision = time.Now().Add(time.Second).Format(time.RFC3339Nano)
			case "proxy":
				evidence.ProxyID++
			case "route":
				evidence.ProxyRouteHash = strings.Repeat("f", 64)
			case "IP":
				evidence.ReauthorizationExitIP = "198.51.100.99"
			case "session":
				evidence.ReauthorizationBrowserSessionID = strings.Repeat("f", 32)
			case "expiry":
				evidence.CreatedAt = evidence.CreatedAt.Add(-openAIOAuthSessionTTL)
			case "purpose":
				evidence.ReauthorizationCredentialsHash = strings.Repeat("f", 64)
			}
			result, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
				SessionID: session.ID, State: session.State, Code: "fixture",
			})
			require.ErrorIs(t, err, ErrOpenAIOAuthReauthorizationProofRequired)
			require.Nil(t, result)
			require.Zero(t, atomic.LoadInt32(&client.exchangeCalled))
		})
	}
}

func TestReauthorizationBrowserRechecksAccountAfterLauncher(t *testing.T) {
	svc, account, session, _, _ := reauthorizationIPFixture(t)
	launcher := reauthorizationBrowserLauncher(t, svc, true)
	launcher.newCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		account.UpdatedAt = account.UpdatedAt.Add(time.Second)
		return exec.CommandContext(ctx, "/usr/bin/true")
	}
	result, err := launcher.Launch(t.Context(), session.ID)
	require.ErrorIs(t, err, ErrOAuthReauthorizationStale)
	require.False(t, result.Launched)
	_, err = svc.sessionStore.Get(t.Context(), authorizationBrowserEvidence(session).ID)
	require.ErrorIs(t, err, ErrPendingAuthSessionNotFound)
}

func TestReauthorizationBrowserConcurrentExchangeHasOneWinner(t *testing.T) {
	svc, _, session, _, client := reauthorizationIPFixture(t)
	launchReauthorizationFixture(t, svc, session)
	launchReauthorizationFixture(t, svc, session)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			result, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
				SessionID: session.ID, State: session.State, Code: "fixture",
			})
			if err == nil && result != nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, successes.Load())
	require.EqualValues(t, 1, atomic.LoadInt32(&client.exchangeCalled))
	require.ErrorIs(t, svc.recordAuthorizationBrowser(t.Context(), session), ErrPendingAuthSessionConsumed)
}

type unavailableBrowserEvidenceStore struct {
	OpenAIOAuthSessionStore
	cause   error
	creates int
}

func (s *unavailableBrowserEvidenceStore) Get(context.Context, string) (*OpenAIOAuthSession, error) {
	return nil, s.cause
}

func (s *unavailableBrowserEvidenceStore) Create(context.Context, *OpenAIOAuthSession) error {
	s.creates++
	return nil
}

func TestAuthorizationBrowserStorageFailureCannotRecreateEvidence(t *testing.T) {
	for _, cause := range []error{errors.New("storage unavailable"), ErrPendingAuthSessionConsumed, ErrPendingAuthSessionExpired} {
		svc, _, session, _, _ := reauthorizationIPFixture(t)
		store := &unavailableBrowserEvidenceStore{OpenAIOAuthSessionStore: svc.sessionStore, cause: cause}
		svc.sessionStore = store
		require.ErrorIs(t, svc.recordAuthorizationBrowser(t.Context(), session), cause)
		require.Zero(t, store.creates)
	}
}

func TestReauthorizationBrowserEvidenceDecodingAndPurpose(t *testing.T) {
	_, _, session, _, _ := reauthorizationIPFixture(t)
	evidence := authorizationBrowserEvidence(session)
	raw := map[string]any{
		"proxy_id": "7", "created_at": evidence.CreatedAt.Format(time.RFC3339Nano),
		"state": "", "code_verifier": "",
		"proxy_route_hash": evidence.ProxyRouteHash, "platform": PlatformOpenAI,
		"reauthorization_account_id": "42", "reauthorization_revision": evidence.ReauthorizationRevision,
		"reauthorization_exit_ip":            evidence.ReauthorizationExitIP,
		"reauthorization_browser_session_id": evidence.ReauthorizationBrowserSessionID,
	}
	decode := func() (*OpenAIOAuthSession, error) {
		return decodeOpenAIOAuthSession(&dbent.PendingAuthSession{
			SessionToken: evidence.ID, LocalFlowState: map[string]any{"openai_oauth": raw},
		})
	}
	decoded, err := decode()
	require.NoError(t, err)
	require.Equal(t, session.ID, decoded.ReauthorizationBrowserSessionID)
	for _, invalid := range []any{nil, 1, true, "bad", strings.Repeat("F", 32)} {
		raw["reauthorization_browser_session_id"] = invalid
		_, err = decode()
		require.ErrorIs(t, err, ErrOpenAIOAuthSessionInvalid)
	}
	raw["reauthorization_browser_session_id"] = session.ID
	raw["state"] = session.State
	_, err = decode()
	require.ErrorIs(t, err, ErrOpenAIOAuthSessionInvalid)
	raw["state"] = ""
	raw["reauthorization_account_id"] = "0"
	_, err = decode()
	require.ErrorIs(t, err, ErrOpenAIOAuthSessionInvalid)
}
