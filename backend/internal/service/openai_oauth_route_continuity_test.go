package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type changingOAuthExchangeClient struct {
	OpenAIOAuthClient
	afterExchange func()
	emptyResponse bool
}

func (c *changingOAuthExchangeClient) ExchangeCode(ctx context.Context, code, verifier, redirect, route, clientID string) (*openai.TokenResponse, error) {
	result, err := c.OpenAIOAuthClient.ExchangeCode(ctx, code, verifier, redirect, route, clientID)
	if c.afterExchange != nil {
		c.afterExchange()
	}
	if c.emptyResponse {
		return nil, err
	}
	return result, err
}

type changingOAuthSessionStore struct {
	OpenAIOAuthSessionStore
	onConsume func(*OpenAIOAuthSession) *OpenAIOAuthSession
}

func (s *changingOAuthSessionStore) Consume(ctx context.Context, id string) (*OpenAIOAuthSession, error) {
	session, err := s.OpenAIOAuthSessionStore.Consume(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.onConsume(session), nil
}

func oauthRouteFixture(t *testing.T) (*OpenAIOAuthService, *OpenAIOAuthSession, *Proxy, *openaiOAuthClientStateStub) {
	t.Helper()
	proxy := &Proxy{ID: 7, Status: StatusActive, Protocol: "http", Host: "127.0.0.1", Port: 8080}
	client := &openaiOAuthClientStateStub{}
	repo := &mockProxyRepoForOAuth{getByIDFunc: func(context.Context, int64) (*Proxy, error) {
		copy := *proxy
		return &copy, nil
	}}
	svc := NewOpenAIOAuthService(repo, client)
	svc.SetSessionStore(newTestOpenAIOAuthSessionStore())
	t.Cleanup(svc.Stop)
	result, err := svc.GenerateAuthURL(t.Context(), &proxy.ID, "", PlatformOpenAI)
	require.NoError(t, err)
	session, err := svc.sessionStore.Get(t.Context(), result.SessionID)
	require.NoError(t, err)
	return svc, session, proxy, client
}

func TestOpenAIOAuthRouteChangesFailBeforeExchange(t *testing.T) {
	for _, mutation := range []string{"host", "port", "protocol", "missing binding", "different assignment", "during consume"} {
		t.Run(mutation, func(t *testing.T) {
			svc, session, proxy, client := oauthRouteFixture(t)
			input := &OpenAIExchangeCodeInput{SessionID: session.ID, State: session.State, Code: "fixture"}
			switch mutation {
			case "host":
				proxy.Host = "localhost"
			case "port":
				proxy.Port++
			case "protocol":
				proxy.Protocol = "socks5"
			case "missing binding":
				session.ProxyRouteHash = ""
			case "different assignment":
				id := int64(9)
				input.ProxyID = &id
			case "during consume":
				svc.sessionStore = &changingOAuthSessionStore{svc.sessionStore, func(s *OpenAIOAuthSession) *OpenAIOAuthSession {
					proxy.Port++
					return s
				}}
			}
			_, err := svc.ExchangeCode(t.Context(), input)
			require.Error(t, err)
			require.Zero(t, atomic.LoadInt32(&client.exchangeCalled))
		})
	}
}

func TestOpenAIOAuthRouteChangesDuringExchangeDoNotPublish(t *testing.T) {
	for _, mutation := range []string{"host", "port", "protocol", "disabled", "deleted", "lookup failure"} {
		t.Run(mutation, func(t *testing.T) {
			svc, session, proxy, client := oauthRouteFixture(t)
			svc.oauthClient = &changingOAuthExchangeClient{
				OpenAIOAuthClient: client,
				afterExchange: func() {
					switch mutation {
					case "host":
						proxy.Host = "localhost"
					case "port":
						proxy.Port++
					case "protocol":
						proxy.Protocol = "socks5"
					case "disabled":
						proxy.Status = "disabled"
					case "deleted":
						svc.proxyRepo = &mockProxyRepoForOAuth{getByIDFunc: func(context.Context, int64) (*Proxy, error) {
							return nil, nil
						}}
					case "lookup failure":
						svc.proxyRepo = &mockProxyRepoForOAuth{getByIDFunc: func(context.Context, int64) (*Proxy, error) {
							return nil, errors.New("fixture lookup failure")
						}}
					}
				},
			}
			input := &OpenAIExchangeCodeInput{SessionID: session.ID, State: session.State, Code: "fixture"}
			info, err := svc.ExchangeCode(t.Context(), input)
			require.True(t, info == nil, "failed exchange must not return credentials")
			require.Equal(t, "OPENAI_OAUTH_PROXY_CHANGED", infraerrors.Reason(err))
			require.Equal(t, int32(1), atomic.LoadInt32(&client.exchangeCalled))

			_, err = svc.sessionStore.Get(t.Context(), session.ID)
			require.Error(t, err)
			_, err = svc.ExchangeCode(t.Context(), input)
			require.Error(t, err)
			require.Equal(t, int32(1), atomic.LoadInt32(&client.exchangeCalled))
		})
	}
}

func TestOpenAIOAuthExchangeDoesNotPublishAfterCancellation(t *testing.T) {
	svc, session, _, client := oauthRouteFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	svc.oauthClient = &changingOAuthExchangeClient{OpenAIOAuthClient: client, afterExchange: cancel}
	info, err := svc.ExchangeCode(ctx, &OpenAIExchangeCodeInput{
		SessionID: session.ID, State: session.State, Code: "fixture",
	})
	require.True(t, info == nil, "canceled exchange must not return credentials")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int32(1), atomic.LoadInt32(&client.exchangeCalled))
}

func TestOpenAIOAuthExchangeRejectsEmptyProviderResponse(t *testing.T) {
	svc, session, _, client := oauthRouteFixture(t)
	svc.oauthClient = &changingOAuthExchangeClient{OpenAIOAuthClient: client, emptyResponse: true}
	var info *OpenAITokenInfo
	var err error
	require.NotPanics(t, func() {
		info, err = svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
			SessionID: session.ID, State: session.State, Code: "fixture",
		})
	})
	require.True(t, info == nil, "empty exchange must not return credentials")
	require.Equal(t, "OPENAI_OAUTH_EMPTY_RESPONSE", infraerrors.Reason(err))
}

func TestOpenAIOAuthConsumedSessionMustMatchValidatedSession(t *testing.T) {
	for _, mutation := range []string{"nil", "state", "verifier", "client", "redirect", "proxy", "fingerprint", "platform", "id", "created"} {
		t.Run(mutation, func(t *testing.T) {
			svc, session, _, client := oauthRouteFixture(t)
			svc.sessionStore = &changingOAuthSessionStore{svc.sessionStore, func(s *OpenAIOAuthSession) *OpenAIOAuthSession {
				copy := *s
				switch mutation {
				case "nil":
					return nil
				case "state":
					copy.State += "-changed"
				case "verifier":
					copy.CodeVerifier += "-changed"
				case "client":
					copy.ClientID += "-changed"
				case "redirect":
					copy.RedirectURI += "/changed"
				case "proxy":
					copy.ProxyID++
				case "fingerprint":
					copy.ProxyRouteHash = ""
				case "platform":
					copy.Platform = PlatformAnthropic
				case "id":
					copy.ID += "-changed"
				case "created":
					copy.CreatedAt = copy.CreatedAt.Add(1)
				}
				return &copy
			}}
			_, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
				SessionID: session.ID, State: session.State, Code: "fixture",
			})
			require.ErrorContains(t, err, "session changed")
			require.Zero(t, atomic.LoadInt32(&client.exchangeCalled))
		})
	}
}

func TestOpenAIOAuthRouteReplayHasOneWinner(t *testing.T) {
	svc, session, proxy, client := oauthRouteFixture(t)
	var wg sync.WaitGroup
	var successes atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := svc.ExchangeCode(t.Context(), &OpenAIExchangeCodeInput{
				SessionID: session.ID, State: session.State, Code: "fixture",
			})
			if err == nil && info.ProxyID == proxy.ID {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), successes.Load())
	require.Equal(t, int32(1), atomic.LoadInt32(&client.exchangeCalled))
}

func TestOpenAIOAuthCreationTemplateKeepsAuthorizationProxy(t *testing.T) {
	for _, templateProxy := range []int64{0, 12} {
		for _, assigned := range []int64{0, 7} {
			input := &CreateAccountInput{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			if assigned != 0 {
				input.ProxyID = &assigned
			}
			concurrency := 6
			out := applyOpenAINewAccountDefaults(input, &OpenAINewAccountDefaults{
				ProxyID: &templateProxy, Concurrency: &concurrency,
			})
			require.Equal(t, input.ProxyID, out.ProxyID)
			require.Equal(t, concurrency, out.Concurrency)
		}
	}
}

func TestOpenAIOAuthCreationTemplateRetainsOtherAuthModes(t *testing.T) {
	for _, mode := range []string{OpenAIAuthModePersonalAccessToken, OpenAIAuthModeAgentIdentity, "api-key"} {
		for _, target := range []int64{0, 12} {
			input := &CreateAccountInput{
				Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Credentials: map[string]any{"auth_mode": mode},
			}
			if mode == "api-key" {
				input.Type = AccountTypeAPIKey
			}
			result := applyOpenAINewAccountDefaults(input, &OpenAINewAccountDefaults{ProxyID: &target})
			if target == 0 {
				require.Nil(t, result.ProxyID)
			} else {
				require.NotNil(t, result.ProxyID)
				require.Equal(t, target, *result.ProxyID)
			}
		}
	}
}

func TestOpenAIOAuthRouteBindingSurvivesSessionDecode(t *testing.T) {
	hash := openAIOAuthProxyRouteHash("http://127.0.0.1:8080")
	session, err := decodeOpenAIOAuthSession(&dbent.PendingAuthSession{
		SessionToken: "fixture",
		LocalFlowState: map[string]any{"openai_oauth": map[string]any{
			"proxy_id": "7", "proxy_route_hash": hash, "created_at": "2026-09-16T00:00:00Z",
		}},
	})
	require.NoError(t, err)
	require.Equal(t, int64(7), session.ProxyID)
	require.Equal(t, hash, session.ProxyRouteHash)
}

func TestOpenAIOAuthSessionDecodeClassifiesDamagedPayload(t *testing.T) {
	for _, session := range []*dbent.PendingAuthSession{
		nil,
		{SessionToken: "missing-payload"},
		{
			SessionToken: "invalid-proxy",
			LocalFlowState: map[string]any{"openai_oauth": map[string]any{
				"proxy_id": "invalid", "created_at": "2026-09-16T00:00:00Z",
			}},
		},
		{
			SessionToken: "invalid-created-at",
			LocalFlowState: map[string]any{"openai_oauth": map[string]any{
				"proxy_id": "7", "created_at": "invalid",
			}},
		},
	} {
		_, err := decodeOpenAIOAuthSession(session)
		require.ErrorIs(t, err, ErrOpenAIOAuthSessionInvalid)
	}
}
