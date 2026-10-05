package service

import (
	"context"
	"errors"
	"maps"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
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
	svc.observeReauthorizationExitIP = func(_ context.Context, route string) (string, error) {
		require.Equal(t, proxy.URL(), route)
		return "198.51.100.25", nil
	}
	cfg := &config.Config{Gateway: config.GatewayConfig{
		AuthBrowserLauncher: "/usr/bin/true",
		AuthBrowserFixedEgressRoutes: []config.AuthBrowserFixedEgressRoute{{
			ProxyID: proxy.ID, ProxyRouteSHA256: openAIOAuthProxyRouteHash(proxy.URL()),
			BrowserIngress: openAIAuthBrowserLocalIngress(proxy), ExitIP: "198.51.100.25",
		}},
	}}
	NewOpenAIAuthBrowserLauncher(cfg, svc.sessionStore, svc.proxyRepo).SetReauthorizationService(svc)
	result, err := svc.GenerateReauthorizationAuthURL(t.Context(), account.ID,
		account.UpdatedAt.Format(time.RFC3339Nano), account.ProxyID, "", PlatformOpenAI, "")
	require.NoError(t, err)
	session, err := svc.sessionStore.Get(t.Context(), result.SessionID)
	require.NoError(t, err)
	return svc, account, session, proxy, client
}

func TestReauthorizationIPGenerationRejectsMissingHistoryAndChangedAccount(t *testing.T) {
	for _, mutation := range []string{"unknown IP", "invalid IP", "private IP", "missing account",
		"wrong account", "stale revision", "different proxy", "corrupt binding", "different qualified proxy",
		"shadow", "exit changed", "probe failed"} {
		t.Run(mutation, func(t *testing.T) {
			svc, account, _, _, _ := reauthorizationIPFixture(t)
			revision := account.UpdatedAt.Format(time.RFC3339Nano)
			proxyID := *account.ProxyID
			switch mutation {
			case "unknown IP":
				delete(account.Extra, OpenAIOAuthLoginExitIPExtraKey)
			case "invalid IP":
				account.Extra[OpenAIOAuthLoginExitIPExtraKey] = "not-an-ip"
			case "private IP":
				account.Extra[OpenAIOAuthLoginExitIPExtraKey] = "127.0.0.1"
			case "missing account":
				svc.SetReauthorizationAccountLookup(func(context.Context, int64) (*Account, error) { return nil, errors.New("unavailable") })
			case "wrong account":
				account.ID++
			case "stale revision":
				account.UpdatedAt = account.UpdatedAt.Add(time.Second)
			case "different proxy":
				proxyID++
			case "corrupt binding":
				account.Extra[OpenAIOAuthQualifiedProxyExtraKey] = "invalid"
			case "different qualified proxy":
				account.Extra[OpenAIOAuthQualifiedProxyExtraKey] = proxyID + 1
			case "shadow":
				parent := int64(1)
				account.ParentAccountID = &parent
			case "exit changed":
				svc.observeReauthorizationExitIP = func(context.Context, string) (string, error) { return "198.51.100.26", nil }
			case "probe failed":
				svc.observeReauthorizationExitIP = func(context.Context, string) (string, error) { return "", errors.New("probe failed") }
			}
			store := svc.sessionStore.(*testOpenAIOAuthSessionStore)
			before := len(store.sessions)
			result, err := svc.GenerateReauthorizationAuthURL(t.Context(), 42, revision, &proxyID, "", PlatformOpenAI, "")
			require.Error(t, err)
			require.Nil(t, result)
			require.Len(t, store.sessions, before)
		})
	}
}

func TestReauthorizationIPDriftStopsExchangeBeforeCredentialsLeave(t *testing.T) {
	for _, mutation := range []string{"exit", "unavailable", "route", "account", "credentials", "baseline", "missing baseline"} {
		t.Run(mutation, func(t *testing.T) {
			svc, account, session, proxy, client := reauthorizationIPFixture(t)
			switch mutation {
			case "exit":
				svc.observeReauthorizationExitIP = func(context.Context, string) (string, error) { return "198.51.100.99", nil }
			case "unavailable":
				svc.observeReauthorizationExitIP = func(context.Context, string) (string, error) { return "", errors.New("unavailable") }
			case "route":
				proxy.Port++
			case "account":
				account.ID++
			case "credentials":
				account.Credentials = map[string]any{"access_token": "newer-fixture"}
			case "baseline":
				account.Extra[OpenAIOAuthLoginExitIPExtraKey] = "198.51.100.99"
			case "missing baseline":
				delete(account.Extra, OpenAIOAuthLoginExitIPExtraKey)
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

func TestReauthorizationIPChangesDuringExchangeDoNotPublish(t *testing.T) {
	svc, _, session, _, client := reauthorizationIPFixture(t)
	launchReauthorizationFixture(t, svc, session)
	svc.oauthClient = &changingOAuthExchangeClient{
		OpenAIOAuthClient: client,
		afterExchange: func() {
			svc.observeReauthorizationExitIP = func(context.Context, string) (string, error) {
				return "198.51.100.99", nil
			}
		},
	}
	input := &OpenAIExchangeCodeInput{SessionID: session.ID, State: session.State, Code: "fixture"}
	result, err := svc.ExchangeCode(t.Context(), input)
	require.ErrorIs(t, err, ErrOpenAIOAuthLoginIPChanged)
	require.Nil(t, result)
	require.EqualValues(t, 1, atomic.LoadInt32(&client.exchangeCalled))
	_, err = svc.ExchangeCode(t.Context(), input)
	require.Error(t, err)
	require.EqualValues(t, 1, atomic.LoadInt32(&client.exchangeCalled))
}

func TestReauthorizationIPUnchangedExchangeAndReplay(t *testing.T) {
	svc, _, session, _, client := reauthorizationIPFixture(t)
	launchReauthorizationFixture(t, svc, session)
	input := &OpenAIExchangeCodeInput{SessionID: session.ID, State: session.State, Code: "fixture"}
	result, err := svc.ExchangeCode(t.Context(), input)
	require.NoError(t, err)
	require.NotNil(t, result)
	_, err = svc.ExchangeCode(t.Context(), input)
	require.Error(t, err)
	require.EqualValues(t, 1, atomic.LoadInt32(&client.exchangeCalled))
}

func TestReauthorizationRuntimeUpdatesDoNotInvalidateSession(t *testing.T) {
	svc, account, session, _, client := reauthorizationIPFixture(t)
	account.UpdatedAt = account.UpdatedAt.Add(time.Second)
	account.Status = StatusError
	account.ErrorMessage = "fixture: expired authorization"
	account.Extra["openai_rescue_probe_at"] = account.UpdatedAt.Format(time.RFC3339Nano)
	launchReauthorizationFixture(t, svc, session)
	result, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
		SessionID: session.ID, State: session.State, Code: "fixture",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.ReauthorizationProof)
	require.EqualValues(t, 1, atomic.LoadInt32(&client.exchangeCalled))
}

func TestReauthorizationIPLauncherPinsOriginalIPAndRejectsStaleAccount(t *testing.T) {
	svc, account, session, proxy, _ := reauthorizationIPFixture(t)
	session.CodeVerifier = strings.Repeat("a", openAIAuthBrowserCodeVerifierLength)
	launcher := &OpenAIAuthBrowserLauncher{
		launcherPath: "/usr/bin/true", sessionStore: svc.sessionStore,
		proxyRepo: svc.proxyRepo, now: time.Now, fixedEgressRoutes: svc.fixedEgressRoutes,
	}
	var args []string
	launcher.newCommand = func(ctx context.Context, _ string, argv ...string) *exec.Cmd {
		args = append([]string(nil), argv...)
		return exec.CommandContext(ctx, "/usr/bin/true")
	}
	_, err := launcher.Launch(t.Context(), session.ID)
	require.ErrorIs(t, err, ErrOpenAIAuthBrowserSessionInvalid)
	require.Empty(t, args)
	launcher.SetReauthorizationService(svc)
	result, err := launcher.Launch(t.Context(), session.ID)
	require.NoError(t, err)
	require.True(t, result.Launched)
	require.Len(t, args, 4)
	require.Equal(t, proxy.URL(), args[2])
	require.Equal(t, "198.51.100.25", args[3])
	args = nil
	account.Credentials = map[string]any{"access_token": "newer-fixture"}
	_, err = launcher.Launch(t.Context(), session.ID)
	require.ErrorIs(t, err, ErrOAuthReauthorizationStale)
	require.Empty(t, args)
}

func TestReauthorizationIPSessionDecodingPreservesBinding(t *testing.T) {
	_, _, session, _, _ := reauthorizationIPFixture(t)
	raw := map[string]any{
		"proxy_id": "7", "created_at": session.CreatedAt.Format(time.RFC3339Nano),
		"reauthorization_account_id": "42",
		"reauthorization_revision":   session.ReauthorizationRevision,
		"reauthorization_exit_ip":    session.ReauthorizationExitIP,
	}
	row := &dbent.PendingAuthSession{LocalFlowState: map[string]any{"openai_oauth": raw}}
	decoded, err := decodeOpenAIOAuthSession(row)
	require.NoError(t, err)
	require.Equal(t, session.ReauthorizationAccountID, decoded.ReauthorizationAccountID)
	require.Equal(t, session.ReauthorizationRevision, decoded.ReauthorizationRevision)
	require.Equal(t, session.ReauthorizationExitIP, decoded.ReauthorizationExitIP)
	for _, id := range []any{"0", "-1", "not-an-id", nil} {
		raw["reauthorization_account_id"] = id
		_, err = decodeOpenAIOAuthSession(row)
		require.ErrorIs(t, err, ErrOpenAIOAuthSessionInvalid)
	}
}

func TestReauthorizationIPSessionDecodingRejectsPartialOrMalformedBinding(t *testing.T) {
	valid := map[string]any{
		"proxy_id": "7", "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"reauthorization_account_id": "42",
		"reauthorization_revision":   "2026-10-03T01:02:03Z",
		"reauthorization_exit_ip":    "198.51.100.25",
	}
	for _, key := range []string{"reauthorization_account_id", "reauthorization_revision", "reauthorization_exit_ip"} {
		for _, replacement := range []any{nil, 42, map[string]any{}, ""} {
			raw := maps.Clone(valid)
			raw[key] = replacement
			row := &dbent.PendingAuthSession{LocalFlowState: map[string]any{"openai_oauth": raw}}
			_, err := decodeOpenAIOAuthSession(row)
			require.ErrorIs(t, err, ErrOpenAIOAuthSessionInvalid, "key %s", key)
		}
		raw := maps.Clone(valid)
		delete(raw, key)
		_, err := decodeOpenAIOAuthSession(&dbent.PendingAuthSession{LocalFlowState: map[string]any{"openai_oauth": raw}})
		require.ErrorIs(t, err, ErrOpenAIOAuthSessionInvalid, "missing %s", key)
	}
	for _, invalid := range []string{"not-an-ip", "127.0.0.1", "10.0.0.1", "::ffff:127.0.0.1", "fe80::1%en0", " 198.51.100.25"} {
		raw := maps.Clone(valid)
		raw["reauthorization_exit_ip"] = invalid
		_, err := decodeOpenAIOAuthSession(&dbent.PendingAuthSession{LocalFlowState: map[string]any{"openai_oauth": raw}})
		require.ErrorIs(t, err, ErrOpenAIOAuthSessionInvalid)
	}
	for _, legacy := range []bool{false, true} {
		raw := maps.Clone(valid)
		raw["reauthorization_account_id"] = "0"
		raw["reauthorization_revision"] = ""
		raw["reauthorization_exit_ip"] = ""
		if legacy {
			delete(raw, "reauthorization_account_id")
			delete(raw, "reauthorization_revision")
			delete(raw, "reauthorization_exit_ip")
		}
		decoded, err := decodeOpenAIOAuthSession(&dbent.PendingAuthSession{LocalFlowState: map[string]any{"openai_oauth": raw}})
		require.NoError(t, err)
		require.Zero(t, decoded.ReauthorizationAccountID)
	}
}

func TestReauthorizationIPChangesDuringEnrichmentDoNotPublish(t *testing.T) {
	for _, mutation := range []string{"account", "exit", "route", "canceled"} {
		t.Run(mutation, func(t *testing.T) {
			svc, account, session, proxy, client := reauthorizationIPFixture(t)
			launchReauthorizationFixture(t, svc, session)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			enrichmentReached := false
			svc.SetPrivacyClientFactory(func(string) (*req.Client, error) {
				enrichmentReached = true
				switch mutation {
				case "account":
					account.Credentials = map[string]any{"access_token": "newer-fixture"}
				case "exit":
					svc.observeReauthorizationExitIP = func(context.Context, string) (string, error) {
						return "198.51.100.99", nil
					}
				case "route":
					proxy.Port++
				case "canceled":
					cancel()
				}
				return nil, errors.New("fixture: no network")
			})
			result, err := svc.ExchangeCode(ctx, &OpenAIExchangeCodeInput{
				SessionID: session.ID, State: session.State, Code: "fixture",
			})
			require.True(t, enrichmentReached)
			require.Error(t, err)
			require.Nil(t, result)
			require.EqualValues(t, 1, atomic.LoadInt32(&client.exchangeCalled))
		})
	}
}

func TestReauthorizationIPConsumedBindingCannotChange(t *testing.T) {
	for _, mutation := range []string{"account", "revision", "exit", "downgrade"} {
		t.Run(mutation, func(t *testing.T) {
			svc, _, session, _, client := reauthorizationIPFixture(t)
			svc.sessionStore = &changingOAuthSessionStore{svc.sessionStore, func(s *OpenAIOAuthSession) *OpenAIOAuthSession {
				copy := *s
				switch mutation {
				case "account":
					copy.ReauthorizationAccountID++
				case "revision":
					copy.ReauthorizationRevision = "2026-10-02T00:00:00Z"
				case "exit":
					copy.ReauthorizationExitIP = "198.51.100.99"
				case "downgrade":
					copy.ReauthorizationAccountID = 0
					copy.ReauthorizationRevision = ""
					copy.ReauthorizationExitIP = ""
				}
				return &copy
			}}
			result, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
				SessionID: session.ID, State: session.State, Code: "fixture",
			})
			require.Error(t, err)
			require.Nil(t, result)
			require.Zero(t, atomic.LoadInt32(&client.exchangeCalled))
		})
	}
}

func TestReauthorizationIPLauncherRejectsCorruptBindingBeforeCommand(t *testing.T) {
	svc, _, session, _, _ := reauthorizationIPFixture(t)
	session.CodeVerifier = strings.Repeat("a", openAIAuthBrowserCodeVerifierLength)
	launcher := &OpenAIAuthBrowserLauncher{
		launcherPath: "/usr/bin/true", sessionStore: svc.sessionStore,
		proxyRepo: svc.proxyRepo, now: time.Now,
		newCommand: func(context.Context, string, ...string) *exec.Cmd {
			t.Fatal("corrupt binding reached launcher")
			return nil
		},
	}
	launcher.SetReauthorizationService(svc)
	for _, invalid := range []int64{0, -1} {
		session.ReauthorizationAccountID = invalid
		_, err := launcher.Launch(t.Context(), session.ID)
		require.ErrorIs(t, err, ErrOpenAIAuthBrowserSessionInvalid)
	}
}

func TestReauthorizationIPProbeDoesNotFallbackToEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, route := range []string{"", "direct", "http://127.0.0.1:1"} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := observeOpenAIOAuthExitIP(ctx, route)
		require.ErrorIs(t, err, ErrOpenAIOAuthLoginIPUnavailable)
	}
}
