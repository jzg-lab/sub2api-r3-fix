//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type unauthorizedRecoveryRepo struct {
	mockAccountRepoForGemini
	mu       sync.Mutex
	account  *Account
	writes   int
	readErr  error
	writeErr error
}

func (r *unauthorizedRecoveryRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return snapshotOAuthRefreshAccount(r.account), r.readErr
}

func (r *unauthorizedRecoveryRepo) UpdateCredentials(_ context.Context, _ int64, credentials map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writeErr != nil {
		return r.writeErr
	}
	r.account.Credentials = shallowCopyMap(credentials)
	r.writes++
	return nil
}

func TestOpenAIUnauthorizedRecoveryBoundaries(t *testing.T) {
	type fixtureMutation func(*Account, *unauthorizedRecoveryRepo, *unauthorizedRecoveryExecutor, *OpenAIGatewayService, *http.Request)
	tests := []struct {
		name       string
		body       string
		mutate     fixtureMutation
		refreshes  int32
		writes     int
		stateError bool
	}{
		{name: "revoked", body: `{"error":{"code":"token_revoked"}}`},
		{name: "invalidated", body: `{"error":{"code":"token_invalidated"}}`},
		{name: "generic", body: `{"detail":"Unauthorized"}`},
		{name: "unknown", body: `{"error":{"code":"other"}}`},
		{name: "permanent_message", body: `{"error":{"code":"invalid_token","message":"account disabled"}}`},
		{name: "html", body: "<h1>Unauthorized</h1>"},
		{name: "oversized", body: strings.Repeat("x", openAIUnauthorizedBodyLimit+20)},
		{name: "api_key", mutate: func(a *Account, _ *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			a.Type = AccountTypeAPIKey
		}},
		{name: "missing_refresh", mutate: func(a *Account, _ *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			delete(a.Credentials, "refresh_token")
		}},
		{name: "unreplayable", mutate: func(_ *Account, _ *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, req *http.Request) {
			req.GetBody = nil
		}},
		{name: "unreplayable_error", mutate: func(_ *Account, _ *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, req *http.Request) {
			req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("local body unavailable") }
		}},
		{name: "missing_executor", mutate: func(_ *Account, _ *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, g *OpenAIGatewayService, _ *http.Request) {
			g.openAITokenProvider.executor = nil
		}},
		{name: "lock_held", mutate: func(_ *Account, r *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, g *OpenAIGatewayService, _ *http.Request) {
			g.openAITokenProvider.refreshAPI = NewOAuthRefreshAPI(r, &refreshAPICacheStub{})
		}},
		{name: "read_failure", mutate: func(_ *Account, r *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			r.readErr = errors.New("local read failure")
		}},
		{name: "refresh_failure", refreshes: 1, mutate: func(_ *Account, _ *unauthorizedRecoveryRepo, e *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			e.hook = func(context.Context, *Account) error { return errors.New("invalid_grant") }
		}},
		{name: "write_failure", refreshes: 1, mutate: func(_ *Account, r *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			r.writeErr = errors.New("local write failure")
		}},
		{name: "reauthorized_during_refresh", refreshes: 1, stateError: true, mutate: func(_ *Account, r *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			r.writeErr = ErrOAuthReauthorizationStale
		}},
		{name: "disabled_during_request", stateError: true, mutate: func(_ *Account, r *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			r.account.Status = StatusDisabled
		}},
		{name: "paused_during_request", stateError: true, mutate: func(_ *Account, r *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			r.account.Schedulable = false
		}},
		{name: "proxy_changed", stateError: true, mutate: func(_ *Account, r *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			id := int64(77)
			r.account.ProxyID = &id
		}},
		{name: "proxy_endpoint_changed", stateError: true, mutate: func(_ *Account, r *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			next := *r.account.Proxy
			next.Port++
			r.account.Proxy = &next
		}},
		{name: "identity_changed", stateError: true, mutate: func(_ *Account, r *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			r.account.Credentials["email"] = "another@example.invalid"
		}},
		{name: "login_ip_changed", stateError: true, mutate: func(_ *Account, r *unauthorizedRecoveryRepo, _ *unauthorizedRecoveryExecutor, _ *OpenAIGatewayService, _ *http.Request) {
			r.account.Extra = map[string]any{OpenAIOAuthLoginExitIPExtraKey: "192.0.2.5"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, repo, executor, gateway := newUnauthorizedRecoveryFixture()
			body := tt.body
			if body == "" {
				body = `{"error":{"code":"token_expired","message":"expired"}}`
			}
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("X-Local-Error", "preserved")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			gateway.httpUpstream = &unauthorizedRecoveryHTTP{client: server.Client()}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader(`{}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer local-before")
			if tt.mutate != nil {
				tt.mutate(a, repo, executor, gateway, req)
			}
			resp, err := gateway.doOpenAIUpstream(req.Context(), req, a.Proxy.URL(), a)
			if tt.stateError {
				require.ErrorIs(t, err, errOAuthRefreshAccountStateChanged)
				require.Nil(t, resp)
			} else {
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
				received, readErr := io.ReadAll(resp.Body)
				require.NoError(t, readErr)
				require.Equal(t, body, string(received))
				require.Equal(t, "preserved", resp.Header.Get("X-Local-Error"))
			}
			require.EqualValues(t, 1, attempts.Load())
			require.Equal(t, tt.refreshes, executor.calls.Load())
			require.Equal(t, tt.writes, repo.writes)
		})
	}
}

func TestOpenAIUnauthorizedRecoveryReusesAlreadyRotatedCredential(t *testing.T) {
	a, repo, executor, gateway := newUnauthorizedRecoveryFixture()
	repo.account.Credentials["access_token"] = "local-after"
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("Authorization") != "Bearer local-after" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_token"}}`)
		}
	}))
	defer server.Close()
	gateway.httpUpstream = &unauthorizedRecoveryHTTP{client: server.Client()}
	req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer local-before")
	resp, err := gateway.doOpenAIUpstream(req.Context(), req, a.Proxy.URL(), a)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.EqualValues(t, 2, attempts.Load())
	require.Zero(t, executor.calls.Load())
	require.Zero(t, repo.writes)
	require.Equal(t, "Bearer local-before", req.Header.Get("Authorization"))
}

func TestOpenAIUnauthorizedRecoveryConcurrentSingleRotation(t *testing.T) {
	a, repo, executor, gateway := newUnauthorizedRecoveryFixture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer local-before" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"token_expired"}}`)
		}
	}))
	defer server.Close()
	gateway.httpUpstream = &unauthorizedRecoveryHTTP{client: server.Client()}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer local-before")
			resp, err := gateway.doOpenAIUpstream(req.Context(), req, a.Proxy.URL(), a)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, executor.calls.Load())
	require.Equal(t, 1, repo.writes)
}

func TestOpenAIUnauthorizedRecoveryDoesNotLoop(t *testing.T) {
	a, repo, executor, gateway := newUnauthorizedRecoveryFixture()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"token_expired"}}`)
	}))
	defer server.Close()
	gateway.httpUpstream = &unauthorizedRecoveryHTTP{client: server.Client()}
	req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer local-before")
	resp, err := gateway.doOpenAIUpstream(req.Context(), req, a.Proxy.URL(), a)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.EqualValues(t, 2, attempts.Load())
	require.EqualValues(t, 1, executor.calls.Load())
	require.Equal(t, 1, repo.writes)
}

func TestOpenAIUnauthorizedRecoveryPreservesInitialTransportContext(t *testing.T) {
	for _, tc := range []struct {
		name     string
		detached bool
		status   int
	}{
		{name: "attached", status: http.StatusOK},
		{name: "detached_success", detached: true, status: http.StatusOK},
		{name: "detached_401_no_refresh", detached: true, status: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account, repo, _, gateway := newUnauthorizedRecoveryFixture()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			requestCtx := ctx
			if tc.detached {
				requestCtx = context.WithoutCancel(ctx)
			}
			req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "https://example.invalid/responses", strings.NewReader(`{}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+account.GetOpenAIAccessToken())
			attempts := 0
			response, err := doOpenAIUpstreamWithRecovery(ctx, req, account.Proxy.URL(), account,
				gateway.openAITokenProvider, false, func(request *http.Request, _ string, _ *Account) (*http.Response, error) {
					attempts++
					require.NoError(t, request.Context().Err())
					return &http.Response{
						StatusCode: tc.status,
						Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"token_expired"}}`)),
					}, nil
				})
			if tc.detached && tc.status == http.StatusOK {
				require.NoError(t, err)
				require.NotNil(t, response)
				response.Body.Close()
			} else {
				require.ErrorIs(t, err, context.Canceled)
				require.Nil(t, response)
			}
			require.Equal(t, map[bool]int{false: 0, true: 1}[tc.detached], attempts)
			require.Zero(t, repo.writes, "a cancelled caller must never refresh or replay")
		})
	}
}

func TestOpenAIUnauthorizedRecoveryCancellationWithDetachedTransport(t *testing.T) {
	for _, phase := range []string{"before_refresh", "during_refresh", "during_replay"} {
		t.Run(phase, func(t *testing.T) {
			a, repo, executor, gateway := newUnauthorizedRecoveryFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var attempts atomic.Int32
			executor.hook = func(context.Context, *Account) error {
				if phase == "during_refresh" {
					cancel()
				}
				return nil
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if attempts.Add(1) == 1 {
					if phase == "before_refresh" {
						cancel()
					}
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":{"code":"token_expired"}}`)
				} else {
					cancel()
					select {
					case <-r.Context().Done():
					case <-time.After(2 * time.Second):
						t.Error("replay transport ignored cancellation")
					}
				}
			}))
			defer server.Close()
			gateway.httpUpstream = &unauthorizedRecoveryHTTP{client: server.Client()}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader(`{}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer local-before")
			resp, err := gateway.doOpenAIUpstream(ctx, req, a.Proxy.URL(), a)
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, resp)
			if phase == "during_replay" {
				require.EqualValues(t, 2, attempts.Load())
				require.Equal(t, 1, repo.writes)
			} else {
				require.EqualValues(t, 1, attempts.Load())
				require.Zero(t, repo.writes)
			}
		})
	}
}

func TestOpenAIUnauthorizedRecoveryCancellationDuringErrorRead(t *testing.T) {
	a, repo, executor, gateway := newUnauthorizedRecoveryFixture()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
			t.Error("error body read ignored caller cancellation")
		}
	}))
	defer server.Close()
	gateway.httpUpstream = &unauthorizedRecoveryHTTP{client: server.Client()}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer local-before")
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	resp, err := gateway.doOpenAIUpstream(ctx, req, a.Proxy.URL(), a)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, resp)
	require.Zero(t, executor.calls.Load())
	require.Zero(t, repo.writes)
}

type unauthorizedRecoveryWinnerCache struct {
	refreshAPICacheStub
	onAcquire func()
}

func (c *unauthorizedRecoveryWinnerCache) AcquireRefreshLock(context.Context, string, time.Duration) (string, error) {
	c.onAcquire()
	return "", nil
}

func TestOpenAIUnauthorizedRecoveryDistributedWinner(t *testing.T) {
	for _, outcome := range []string{"rotated", "changed_identity", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			a, repo, executor, gateway := newUnauthorizedRecoveryFixture()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cache := &unauthorizedRecoveryWinnerCache{onAcquire: func() {
				repo.mu.Lock()
				defer repo.mu.Unlock()
				switch outcome {
				case "rotated":
					repo.account.Credentials["access_token"] = "local-winner"
				case "changed_identity":
					repo.account.Credentials["email"] = "another@example.invalid"
				case "cancelled":
					cancel()
				}
			}}
			gateway.openAITokenProvider.refreshAPI = NewOAuthRefreshAPI(repo, cache)
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if attempts.Add(1) == 1 {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":{"code":"token_expired"}}`)
					return
				}
				if r.Header.Get("Authorization") != "Bearer local-winner" {
					t.Error("did not reuse distributed winner")
				}
			}))
			defer server.Close()
			gateway.httpUpstream = &unauthorizedRecoveryHTTP{client: server.Client()}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(`{}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer local-before")
			resp, err := gateway.doOpenAIUpstream(ctx, req, a.Proxy.URL(), a)
			switch outcome {
			case "rotated":
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, http.StatusOK, resp.StatusCode)
				require.EqualValues(t, 2, attempts.Load())
			case "changed_identity":
				require.ErrorIs(t, err, errOAuthRefreshAccountStateChanged)
				require.Nil(t, resp)
			case "cancelled":
				require.ErrorIs(t, err, context.Canceled)
				require.Nil(t, resp)
			}
			require.Zero(t, executor.calls.Load())
			require.Zero(t, repo.writes)
		})
	}
}

func TestOpenAIUnauthorizedRecoveryAccountTestConsumers(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		for _, scheduled := range []bool{false, true} {
			t.Run(strconv.FormatBool(useTLS)+"/scheduled="+strconv.FormatBool(scheduled), func(t *testing.T) {
				a, repo, executor, gateway := newUnauthorizedRecoveryFixture()
				a.Schedulable = scheduled
				repo.account.Schedulable = scheduled
				var attempts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if attempts.Add(1) == 1 {
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = io.WriteString(w, `{"error":{"code":"token_expired"}}`)
						return
					}
					if r.Header.Get("Authorization") == "Bearer local-before" {
						t.Error("account test replayed the rejected credential")
					}
					_, _ = io.WriteString(w, "accepted")
				}))
				defer server.Close()
				upstream := &unauthorizedRecoveryHTTP{client: server.Client()}
				svc := ProvideAccountTestService(repo, nil, nil, nil, nil, upstream, nil, nil, gateway, nil, nil)
				require.Same(t, gateway.openAITokenProvider, svc.openAITokenProvider)
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(`{}`))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer local-before")
				resp, err := svc.doOpenAIAccountTestUpstream(req, a.Proxy.URL(), a, useTLS)
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, http.StatusOK, resp.StatusCode)
				require.EqualValues(t, 2, attempts.Load())
				require.EqualValues(t, 1, executor.calls.Load())
				require.Equal(t, 1, repo.writes)
				require.Equal(t, scheduled, repo.account.Schedulable, "refresh cannot graduate the account")
				require.Equal(t, []string{a.Proxy.URL(), a.Proxy.URL()}, upstream.routes)
			})
		}
	}
}

type unauthorizedRecoveryProbeHTTP struct {
	*unauthorizedRecoveryHTTP
	target string
}

func (u *unauthorizedRecoveryProbeHTTP) DoProbeWithTLS(req *http.Request, route string, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	local, err := http.NewRequestWithContext(req.Context(), req.Method, u.target, req.Body)
	if err != nil {
		return nil, err
	}
	local.Header = req.Header.Clone()
	return u.Do(local, route, 0, n)
}

func TestOpenAIUnauthorizedRecoveryQualificationConsumer(t *testing.T) {
	a, repo, executor, gateway := newUnauthorizedRecoveryFixture()
	a.Schedulable = false
	a.Extra = map[string]any{openAIDowngradeQualificationExtraKey: true}
	repo.account = snapshotOAuthRefreshAccount(a)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"token_expired"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	upstream := &unauthorizedRecoveryProbeHTTP{
		unauthorizedRecoveryHTTP: &unauthorizedRecoveryHTTP{client: server.Client()},
		target:                   server.URL,
	}
	runner := &OpenAIDowngradeProbeRunner{tokenProvider: gateway.openAITokenProvider, httpUpstream: upstream}
	result := runner.probe(t.Context(), a, "qualification")
	require.Equal(t, http.StatusOK, result.HTTPStatus)
	require.EqualValues(t, 2, attempts.Load())
	require.EqualValues(t, 1, executor.calls.Load())
	require.Equal(t, 1, repo.writes)
	require.False(t, repo.account.Schedulable, "HTTP recovery is not quality qualification")
	require.Equal(t, []string{a.Proxy.URL(), a.Proxy.URL()}, upstream.routes)
}

type unauthorizedRecoveryProxyRepo struct {
	ProxyRepository
	proxy *Proxy
	err   error
}

func (r *unauthorizedRecoveryProxyRepo) GetByID(context.Context, int64) (*Proxy, error) {
	return r.proxy, r.err
}

func TestOpenAIUnauthorizedRecoveryQualificationIDOnlyProxy(t *testing.T) {
	for _, mode := range []string{"available", "unavailable", "inactive", "mismatched"} {
		t.Run(mode, func(t *testing.T) {
			a, _, _, gateway := newUnauthorizedRecoveryFixture()
			proxy := *a.Proxy
			route := proxy.URL()
			a.Proxy = nil
			proxies := &unauthorizedRecoveryProxyRepo{proxy: &proxy}
			switch mode {
			case "unavailable":
				proxies.err = errors.New("fixture proxy unavailable")
			case "inactive":
				proxy.Status = StatusDisabled
			case "mismatched":
				proxy.ID++
			}
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			upstream := &unauthorizedRecoveryProbeHTTP{
				unauthorizedRecoveryHTTP: &unauthorizedRecoveryHTTP{client: server.Client()},
				target:                   server.URL,
			}
			runner := &OpenAIDowngradeProbeRunner{tokenProvider: gateway.openAITokenProvider, httpUpstream: upstream, proxyRepo: proxies}
			result := runner.probe(t.Context(), a, "qualification")
			require.Nil(t, a.Proxy, "probing must not mutate the scheduler snapshot")
			if mode == "available" {
				require.Equal(t, http.StatusOK, result.HTTPStatus)
				require.Equal(t, []string{route}, upstream.routes)
				require.EqualValues(t, 1, attempts.Load())
			} else {
				require.Zero(t, result.HTTPStatus)
				require.Empty(t, upstream.routes)
				require.Zero(t, attempts.Load())
			}
		})
	}
}

func TestOpenAIUnauthorizedRecoveryProbeCannotBypassStateChange(t *testing.T) {
	for _, mutation := range []string{"paused", "disabled", "cooldown"} {
		t.Run(mutation, func(t *testing.T) {
			a, repo, executor, gateway := newUnauthorizedRecoveryFixture()
			switch mutation {
			case "paused":
				repo.account.Schedulable = false
			case "disabled":
				repo.account.Status = StatusDisabled
			case "cooldown":
				until := time.Now().Add(time.Hour)
				repo.account.TempUnschedulableUntil = &until
			}
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"code":"token_expired"}}`)
			}))
			defer server.Close()
			svc := ProvideAccountTestService(repo, nil, nil, nil, nil,
				&unauthorizedRecoveryHTTP{client: server.Client()}, nil, nil, gateway, nil, nil)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(`{}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer local-before")
			resp, err := svc.doOpenAIAccountTestUpstream(req, a.Proxy.URL(), a, false)
			require.ErrorIs(t, err, errOAuthRefreshAccountStateChanged)
			require.Nil(t, resp)
			require.EqualValues(t, 1, attempts.Load())
			require.Zero(t, executor.calls.Load())
			require.Zero(t, repo.writes)
		})
	}
}

type unauthorizedRecoveryExecutor struct {
	calls atomic.Int32
	hook  func(context.Context, *Account) error
}

func (*unauthorizedRecoveryExecutor) CacheKey(a *Account) string { return OpenAITokenCacheKey(a) }
func (*unauthorizedRecoveryExecutor) CanRefresh(a *Account) bool {
	return IsOpenAIBrowserOAuthAccount(a)
}
func (*unauthorizedRecoveryExecutor) NeedsRefresh(a *Account, window time.Duration) bool {
	return (&OpenAITokenRefresher{}).NeedsRefresh(a, window)
}
func (e *unauthorizedRecoveryExecutor) Refresh(ctx context.Context, a *Account) (map[string]any, error) {
	e.calls.Add(1)
	if e.hook != nil {
		if err := e.hook(ctx, a); err != nil {
			return nil, err
		}
	}
	next := shallowCopyMap(a.Credentials)
	next["access_token"] = "local-after"
	next["refresh_token"] = "local-rotated"
	return next, nil
}

type unauthorizedRecoveryHTTP struct {
	HTTPUpstream
	client *http.Client
	mu     sync.Mutex
	routes []string
}

func (u *unauthorizedRecoveryHTTP) Do(req *http.Request, route string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.routes = append(u.routes, route)
	u.mu.Unlock()
	return u.client.Do(req)
}
func (u *unauthorizedRecoveryHTTP) DoWithTLS(req *http.Request, route string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, route, id, n)
}

func newUnauthorizedRecoveryFixture() (*Account, *unauthorizedRecoveryRepo, *unauthorizedRecoveryExecutor, *OpenAIGatewayService) {
	proxyID := int64(23)
	a := &Account{
		ID: 53, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Schedulable: true, ProxyID: &proxyID,
		Proxy: &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 19023, Status: StatusActive},
		Credentials: map[string]any{
			"access_token": "local-before", "refresh_token": "local-refresh",
			"expires_at": strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
			"email":      "fixture@example.invalid",
		},
	}
	repo := &unauthorizedRecoveryRepo{account: snapshotOAuthRefreshAccount(a)}
	executor := &unauthorizedRecoveryExecutor{}
	provider := NewOpenAITokenProvider(repo, nil, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), executor)
	return a, repo, executor, &OpenAIGatewayService{accountRepo: repo, openAITokenProvider: provider}
}

func TestOpenAIUnauthorizedRecoveryFutureExpiry(t *testing.T) {
	a, repo, executor, gateway := newUnauthorizedRecoveryFixture()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"model":"local-fixture","input":"hello"}`, string(body))
		if r.Header.Get("Authorization") == "Bearer local-before" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"token_expired","message":"access token expired"}}`)
			return
		}
		require.Equal(t, "Bearer local-after", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"id":"local-response"}`)
	}))
	defer server.Close()
	upstream := &unauthorizedRecoveryHTTP{client: server.Client()}
	gateway.httpUpstream = upstream
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader(`{"model":"local-fixture","input":"hello"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer local-before")
	resp, err := gateway.doOpenAIUpstream(req.Context(), req, a.Proxy.URL(), a)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.EqualValues(t, 2, attempts.Load())
	require.EqualValues(t, 1, executor.calls.Load())
	require.Equal(t, 1, repo.writes)
	require.Equal(t, []string{a.Proxy.URL(), a.Proxy.URL()}, upstream.routes)
}
