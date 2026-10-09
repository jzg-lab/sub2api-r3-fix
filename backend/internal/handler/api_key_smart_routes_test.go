//go:build unit

package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type smartRouteGroups struct {
	service.GroupRepository
	groups map[int64]*service.Group
	err    error
}

func (r smartRouteGroups) GetByIDLite(_ context.Context, id int64) (*service.Group, error) {
	if r.err != nil {
		return nil, r.err
	}
	if group := r.groups[id]; group != nil {
		return group, nil
	}
	return nil, service.ErrGroupNotFound
}

type smartRouteCache struct {
	service.APIKeyCache
	failed   map[string]map[int64]bool
	sessions map[string]int64
}

func (r *smartRouteCache) APIKeyRouteSession(_ context.Context, key int64, scope, session string) (int64, error) {
	return r.sessions[fmt.Sprintf("%d/%s/%s", key, scope, session)], nil
}
func (r *smartRouteCache) RememberAPIKeyRouteSession(_ context.Context, key int64, scope, session string, id int64) error {
	if r.sessions == nil {
		r.sessions = make(map[string]int64)
	}
	r.sessions[fmt.Sprintf("%d/%s/%s", key, scope, session)] = id
	return nil
}

func TestSmartRouteSamePackageAccountRecoveryAndSessionPreference(t *testing.T) {
	h, key, cache := smartRoutingFixture(t)
	var groups []int64
	primaryAvailable := true
	run := h.WithSmartRoutes(func(c *gin.Context) {
		routed, _ := middleware2.GetAPIKeyFromContext(c)
		groups = append(groups, *routed.GroupID)
		if *routed.GroupID == 1 {
			excluded := service.APIKeyRouteExcludedAccounts(c.Request.Context(), nil)
			if _, failed := excluded[10]; !failed {
				require.True(t, nextSmartRoute(c, &service.UpstreamFailoverError{StatusCode: 502}, &service.Account{ID: 10}))
				return
			}
			if !primaryAvailable {
				require.True(t, skipUnavailableSmartRoute(c, service.ErrNoAvailableAccounts))
				return
			}
		}
		c.JSON(200, gin.H{"ok": true})
	}, nil, nil)
	c, rec := smartRoutingContext(key, `{"model":"gpt-test"}`)
	run(c)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, []int64{1, 1}, groups)
	require.Empty(t, cache.failed, "one failed account must not demote a healthy package")
	primaryAvailable = false
	groups = nil
	c, rec = smartRoutingContext(key, `{"model":"gpt-test"}`)
	c.Request.Header.Set("Session_id", "conversation-one")
	run(c)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, []int64{1, 1, 2}, groups)
	cache.failed = make(map[string]map[int64]bool) // Failure cooldown expired, but session preference remains.
	groups = nil
	c, rec = smartRoutingContext(key, `{"model":"gpt-test"}`)
	c.Request.Header.Set("Session_id", "conversation-one")
	run(c)
	require.Equal(t, []int64{2}, groups)
	groups = nil
	primaryAvailable = true
	c, rec = smartRoutingContext(key, `{"model":"gpt-test"}`)
	c.Request.Header.Set("Session_id", "conversation-two")
	run(c)
	require.Equal(t, []int64{1, 1}, groups)
}

func (r *smartRouteCache) FailedAPIKeyRouteGroups(_ context.Context, _ int64, scope string, _ []int64) (map[int64]bool, error) {
	return r.failed[scope], nil
}
func (r *smartRouteCache) MarkAPIKeyRouteFailed(_ context.Context, _ int64, scope string, id int64) error {
	if r.failed[scope] == nil {
		r.failed[scope] = map[int64]bool{}
	}
	r.failed[scope][id] = true
	return nil
}

func smartRoutingFixture(t *testing.T) (*GatewayHandler, *service.APIKey, *smartRouteCache) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groups := map[int64]*service.Group{
		1: {ID: 1, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, RateMultiplier: 1},
		2: {ID: 2, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, RateMultiplier: 3},
	}
	cache := &smartRouteCache{failed: make(map[string]map[int64]bool)}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	svc := service.NewAPIKeyService(nil, nil, smartRouteGroups{groups: groups}, nil, nil, cache, cfg)
	h := &GatewayHandler{apiKeyService: svc, gatewayService: &service.GatewayService{}, cfg: cfg, maxAccountSwitches: 3}
	id := int64(1)
	key := &service.APIKey{ID: 9, UserID: 7, User: &service.User{ID: 7}, GroupID: &id, Group: groups[1], RouteGroupIDs: []int64{1, 2}}
	return h, key, cache
}
func smartRoutingContext(key *service.APIKey, body string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	c.Set(string(middleware2.ContextKeyAPIKey), key)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: key.UserID})
	return c, rec
}

func TestSmartRoutesSwitchWithinRequestAndRememberNextReconnect(t *testing.T) {
	h, key, _ := smartRoutingFixture(t)
	calls := []int64{}
	const payload = `{"model":"gpt-test","input":"hello"}`
	run := h.WithSmartRoutes(func(c *gin.Context) {
		selected, _ := middleware2.GetAPIKeyFromContext(c)
		calls = append(calls, *selected.GroupID)
		body, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		require.JSONEq(t, payload, string(body))
		if *selected.GroupID == 1 {
			c.Request.Header.Set("X-Failed-Attempt", "must not leak")
			c.Set("attempt_policy", "must not leak")
			require.True(t, nextSmartRoute(c, &service.UpstreamFailoverError{StatusCode: 502}, nil))
			return
		}
		require.Empty(t, c.GetHeader("X-Failed-Attempt"))
		_, leaked := c.Get("attempt_policy")
		require.False(t, leaked)
		require.Equal(t, 3.0, selected.Group.RateMultiplier)
		c.JSON(200, gin.H{"group_id": *selected.GroupID})
	}, nil, nil)
	for i := 0; i < 5; i++ {
		c, rec := smartRoutingContext(key, payload)
		run(c)
		require.Equal(t, 200, rec.Code)
		require.JSONEq(t, `{"group_id":2}`, rec.Body.String())
	}
	require.Equal(t, []int64{1, 2, 2, 2, 2, 2}, calls)
	require.Equal(t, int64(1), *key.GroupID, "cached primary must not mutate")
	require.Equal(t, []int64{1, 2}, key.RouteGroupIDs)
}

func TestSmartRoutesSkipDisabledPrimary(t *testing.T) {
	h, key, _ := smartRoutingFixture(t)
	key.Group.Status = "inactive"
	key.User.AllowedGroups = []int64{}
	c, rec := smartRoutingContext(key, `{"model":"gpt-test"}`)
	run := h.WithSmartRoutes(func(c *gin.Context) {
		selected, _ := middleware2.GetAPIKeyFromContext(c)
		require.Equal(t, int64(2), *selected.GroupID)
		c.Status(204)
	}, nil, nil)
	run(c)
	require.Equal(t, 204, c.Writer.Status())
	require.Empty(t, rec.Body.String())
}

func TestSmartRoutesDoNotReplayCommittedOutputOrClientErrors(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "client error", true: "committed stream"}[stream], func(t *testing.T) {
			h, key, cache := smartRoutingFixture(t)
			calls := 0
			c, rec := smartRoutingContext(key, `{"model":"gpt-test"}`)
			h.WithSmartRoutes(func(c *gin.Context) {
				calls++
				if stream {
					service.MarkResponseCommitted(c)
					_, _ = c.Writer.WriteString("data: partial\n\n")
					require.False(t, nextSmartRoute(c, &service.UpstreamFailoverError{StatusCode: 502}, nil))
				} else {
					c.JSON(400, gin.H{"error": "invalid input"})
				}
			}, nil, nil)(c)
			require.Equal(t, 1, calls)
			require.NotEmpty(t, rec.Body.String())
			require.Empty(t, cache.failed)
		})
	}
}

func TestSmartRoutesContinuationStaysOnPrimary(t *testing.T) {
	h, key, _ := smartRoutingFixture(t)
	c, _ := smartRoutingContext(key, `{"model":"gpt-test","previous_response_id":"resp_123"}`)
	h.WithSmartRoutes(func(c *gin.Context) {
		selected, _ := middleware2.GetAPIKeyFromContext(c)
		require.Equal(t, int64(1), *selected.GroupID)
		require.Nil(t, smartRouteAttemptFrom(c))
		c.Status(204)
	}, nil, nil)(c)
}

type smartRouteAccounts struct {
	openAIImagesFailoverAccountRepo
}

func (r smartRouteAccounts) ListSchedulableByGroupIDAndPlatform(_ context.Context, groupID int64, platform string) ([]service.Account, error) {
	out := []service.Account{}
	for _, account := range r.accounts {
		if account.Platform == platform && account.ID/10 == groupID {
			out = append(out, account)
		}
	}
	return out, nil
}

type smartRouteHTTPUpstream struct {
	service.HTTPUpstream
	calls     []int64
	succeedID int64
}

func (u *smartRouteHTTPUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func (u *smartRouteHTTPUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.calls = append(u.calls, id)
	code, body := http.StatusOK, `{"id":"resp_ok","object":"response","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	if id/10 == 1 && id != u.succeedID {
		code = http.StatusBadGateway
		body = `{"error":{"type":"server_error","message":"upstream unavailable"}}`
	}
	contentType := "application/json"
	if code == 200 && gjson.GetBytes(mustReadRouteBody(req), "stream").Bool() {
		contentType = "text/event-stream"
		body = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_ok\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"gpt-5.1\",\"output\":[]}}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"OK\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
	}
	if code == 200 && strings.HasSuffix(req.URL.Path, "/chat/completions") {
		if contentType == "text/event-stream" {
			body = "data: {\"id\":\"chat_ok\",\"model\":\"gpt-5.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat_ok\",\"model\":\"gpt-5.1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"
		} else {
			body = `{"id":"chat_ok","object":"chat.completion","model":"gpt-5.1","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
		}
	}
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func TestSmartRoutesRealTextForwardAndBilling(t *testing.T) {
	for _, endpoint := range []string{"responses", "messages", "chat/completions"} {
		for _, stream := range []bool{false, true} {
			for _, subscription := range []bool{false, true} {
				for _, samePackage := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%v/subscription=%v/same_package=%v", endpoint, stream, subscription, samePackage), func(t *testing.T) {
						h, key, cache := smartRoutingFixture(t)
						core, observed := observer.New(zap.DebugLevel)
						t.Cleanup(func() {
							if !t.Failed() {
								return
							}
							for _, log := range observed.All() {
								t.Log(log.Message, log.ContextMap())
							}
						})
						accounts := smartRouteAccounts{openAIImagesFailoverAccountRepo{accounts: []service.Account{
							{ID: 10, Name: "failed-primary", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "primary", "base_url": "https://primary.invalid", "pool_mode": true, "pool_mode_retry_count": float64(5)}, Extra: map[string]any{"openai_passthrough": true}},
							{ID: 11, Name: "second-failed-account", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1, Credentials: map[string]any{"api_key": "primary-2", "base_url": "https://primary.invalid"}, Extra: map[string]any{"openai_passthrough": true}},
							{ID: 20, Name: "backup", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "backup", "base_url": "https://backup.invalid"}, Extra: map[string]any{"openai_passthrough": true}},
						}}}
						upstream := &smartRouteHTTPUpstream{}
						successID, groupID, multiplier := int64(20), int64(2), 3.0
						if samePackage {
							upstream.succeedID = 11
							successID = 11
							groupID = 1
							multiplier = 1
						}
						gatewayCfg := *h.cfg
						gatewayCfg.RunMode = config.RunModeStandard
						usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 10)}
						ledger := &smartRouteLedger{}
						billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, h.cfg, nil)
						gateway := service.NewOpenAIGatewayService(accounts, usage, ledger, nil, nil, nil, nil, &gatewayCfg, nil, nil, service.NewBillingService(&gatewayCfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
						t.Cleanup(billing.Stop)
						openAI := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, h.apiKeyService, nil, nil, nil, nil, h.cfg)
						key.Group.AllowMessagesDispatch = true
						backup := &service.Group{ID: 2, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, RateMultiplier: 3, AllowMessagesDispatch: true}
						h.apiKeyService = service.NewAPIKeyService(nil, nil, smartRouteGroups{groups: map[int64]*service.Group{1: key.Group, 2: backup}}, nil, nil, cache, h.cfg)
						var subs *service.SubscriptionService
						if subscription {
							if samePackage {
								key.Group.SubscriptionType = service.SubscriptionTypeSubscription
							} else {
								backup.SubscriptionType = service.SubscriptionTypeSubscription
							}
							h.cfg = &gatewayCfg
							now := time.Now()
							sub := &service.UserSubscription{ID: 50, UserID: key.UserID, GroupID: groupID, Status: service.SubscriptionStatusActive, StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}
							subs = service.NewSubscriptionService(nil, smartRouteSubscriptions{sub: sub}, nil, nil, h.cfg)
							t.Cleanup(subs.Stop)
						}
						next := openAI.Responses
						if endpoint == "messages" {
							next = openAI.Messages
						}
						if endpoint == "chat/completions" {
							next = openAI.ChatCompletions
						}
						run := h.WithSmartRoutes(next, subs, openAI)
						requests := 2
						if samePackage {
							requests = 1
						}
						for i := 0; i < requests; i++ {
							c, rec := smartRoutingContext(key, fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"hello","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`, stream))
							c.Request.URL.Path = "/v1/" + endpoint
							c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
							run(c)
							require.Equal(t, 200, rec.Code, "body=%s calls=%v", rec.Body.String(), upstream.calls)
							require.Contains(t, rec.Body.String(), "OK")
							select {
							case log := <-usage.created:
								require.Equal(t, groupID, *log.GroupID)
								require.Equal(t, multiplier, log.RateMultiplier)
								require.InDelta(t, log.TotalCost*multiplier, log.ActualCost, 0.00000001)
							case <-time.After(time.Second):
								t.Fatal("usage was not recorded")
							}
							require.Equal(t, i+1, len(ledger.commands), "only the successful group is charged")
							require.Equal(t, successID, ledger.commands[i].AccountID)
							if subscription {
								require.Zero(t, ledger.commands[i].BalanceCost)
								require.Greater(t, ledger.commands[i].SubscriptionCost, 0.0)
								require.Equal(t, int64(50), *ledger.commands[i].SubscriptionID)
							} else {
								require.Greater(t, ledger.commands[i].BalanceCost, 0.0)
								require.Nil(t, ledger.commands[i].SubscriptionID)
							}
						}
						if samePackage {
							require.Equal(t, []int64{10, 11}, upstream.calls)
							require.Empty(t, cache.failed)
						} else {
							require.Equal(t, []int64{10, 11, 20, 20}, upstream.calls, "try each account in the current package once before switching packages")
						}
						require.Zero(t, observed.FilterMessage("openai.responses_panic_recovered").Len())
						if samePackage || endpoint != "responses" || stream {
							return
						}
						// Continuation must find the backup that issued this response even after avoidance expires.
						c, rec := smartRoutingContext(key, `{"model":"gpt-5.1","previous_response_id":"resp_ok","input":"continue"}`)
						run(c)
						require.Equal(t, 200, rec.Code, rec.Body.String())
						require.Equal(t, int64(20), upstream.calls[len(upstream.calls)-1])

					})
				}
			}
		}
	}
}

func mustReadRouteBody(req *http.Request) []byte { body, _ := io.ReadAll(req.Body); return body }

type smartRouteSubscriptions struct {
	service.UserSubscriptionRepository
	sub *service.UserSubscription
	err error
}

func (r smartRouteSubscriptions) GetActiveByUserIDAndGroupID(context.Context, int64, int64) (*service.UserSubscription, error) {
	return r.sub, r.err
}

func TestSmartRouteSubscriptionQualification(t *testing.T) {
	h, key, _ := smartRoutingFixture(t)
	key.Group.SubscriptionType = service.SubscriptionTypeSubscription
	now := time.Now()
	sub := &service.UserSubscription{ID: 4, UserID: key.UserID, GroupID: 1, Status: service.SubscriptionStatusActive, StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}
	for _, tc := range []struct {
		name   string
		change func(*service.UserSubscription)
		err    error
	}{
		{"valid", func(*service.UserSubscription) {}, nil},
		{"future", func(s *service.UserSubscription) { s.StartsAt = now.Add(time.Hour) }, service.ErrSubscriptionInvalid},
		{"wrong user", func(s *service.UserSubscription) { s.UserID++ }, service.ErrSubscriptionInvalid},
		{"wrong group", func(s *service.UserSubscription) { s.GroupID++ }, service.ErrSubscriptionInvalid},
		{"expired", func(s *service.UserSubscription) { s.ExpiresAt = now.Add(-time.Hour) }, service.ErrSubscriptionExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := *sub
			tc.change(&copy)
			svc := service.NewSubscriptionService(nil, smartRouteSubscriptions{sub: &copy}, nil, nil, h.cfg)
			t.Cleanup(svc.Stop)
			got, err := routeSubscription(context.Background(), svc, key, false)
			if tc.err == nil {
				require.NoError(t, err)
				require.Equal(t, sub.ID, got.ID)
			} else {
				require.ErrorIs(t, err, tc.err)
			}
		})
	}
}

func TestSmartRouteGlobalFailuresStayTerminal(t *testing.T) {
	for _, err := range []error{service.ErrUserRPMExceeded, service.ErrAPIKeyRateLimit5hExceeded, service.ErrBillingServiceUnavailable, errors.New("database unavailable")} {
		h, key, cache := smartRoutingFixture(t)
		c, _ := smartRoutingContext(key, `{"model":"gpt-test"}`)
		calls := 0
		h.WithSmartRoutes(func(c *gin.Context) { calls++; require.False(t, skipIneligibleSmartRoute(c, err)); c.Status(429) }, nil, nil)(c)
		require.Equal(t, 1, calls)
		require.Empty(t, cache.failed)
	}
}

func TestSmartRouteLookupFailureReturnsRetryableServiceError(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		t.Run(fmt.Sprintf("subscription=%t", subscription), func(t *testing.T) {
			h, key, cache := smartRoutingFixture(t)
			h.cfg.RunMode = "standard"
			var subs *service.SubscriptionService
			lookupErr := errors.New("database unavailable: private diagnostic")
			if subscription {
				key.Group.SubscriptionType = service.SubscriptionTypeSubscription
				subs = service.NewSubscriptionService(nil, smartRouteSubscriptions{err: lookupErr}, nil, nil, h.cfg)
				t.Cleanup(subs.Stop)
			} else {
				h.apiKeyService = service.NewAPIKeyService(nil, nil, smartRouteGroups{err: lookupErr}, nil, nil, cache, h.cfg)
			}
			c, rec := smartRoutingContext(key, `{"model":"gpt-test"}`)
			h.WithSmartRoutes(func(*gin.Context) { t.Fatal("must not bypass failed qualification") }, subs, nil)(c)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			require.NotContains(t, rec.Body.String(), "private diagnostic")
			require.Empty(t, cache.failed)
		})
	}
}

func TestSmartRouteExhaustedGroupRPMPreservesRetryAfter(t *testing.T) {
	h, key, cache := smartRoutingFixture(t)
	c, rec := smartRoutingContext(key, `{"model":"gpt-test"}`)
	calls := 0
	h.WithSmartRoutes(func(c *gin.Context) {
		calls++
		require.True(t, skipIneligibleSmartRoute(c, service.ErrGroupRPMExceeded))
	}, nil, nil)(c)
	require.Equal(t, 2, calls)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
	require.Empty(t, cache.failed)
}

func TestSmartRouteRemembersFailureAfterOutputWithoutReplaying(t *testing.T) {
	h, key, cache := smartRoutingFixture(t)
	c, rec := smartRoutingContext(key, `{"model":"gpt-test"}`)
	calls := 0
	h.WithSmartRoutes(func(c *gin.Context) {
		calls++
		service.MarkResponseCommitted(c)
		c.Writer.WriteString("data: partial\n\n")
		err := &service.UpstreamFailoverError{StatusCode: 502}
		observeSmartRouteFailure(c, err)
		require.False(t, nextSmartRoute(c, err, nil))
	}, nil, nil)(c)
	require.Equal(t, 1, calls)
	require.Equal(t, "data: partial\n\n", rec.Body.String())
	require.True(t, cache.failed["responses\x00gpt-test"][1])
}

type smartRouteLedger struct {
	service.UsageBillingRepository
	commands []*service.UsageBillingCommand
}

func (r *smartRouteLedger) Apply(_ context.Context, cmd *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	r.commands = append(r.commands, cmd)
	return &service.UsageBillingApplyResult{Applied: true}, nil
}

func TestSmartRoutesWebSocketSwitchBeforeFirstTurn(t *testing.T) {
	h, key, _ := smartRoutingFixture(t)
	core, logs := observer.New(zap.DebugLevel)
	t.Cleanup(func() {
		if t.Failed() {
			for _, entry := range logs.All() {
				t.Log(entry.Message, entry.ContextMap())
			}
		}
	})
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(502)
		w.Write([]byte(`{"error":{"message":"unavailable"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_, _, err = conn.Read(ctx)
		if err != nil {
			return
		}
		conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_ws_backup","model":"gpt-5.1","status":"in_progress"}}`))
		conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_ws_backup","model":"gpt-5.1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
		// A second turn stays with this established supplier.
		_, _, err = conn.Read(ctx)
		if err != nil {
			return
		}
		conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.failed","response":{"id":"resp_ws_failure","status":"failed","error":{"code":"server_error","message":"upstream unavailable"}}}`))
	}))
	defer backup.Close()
	cfg := *h.cfg
	cfg.RunMode = config.RunModeStandard
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 2
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	accounts := smartRouteAccounts{openAIImagesFailoverAccountRepo{accounts: []service.Account{}}}
	for i, url := range []string{primary.URL, backup.URL} {
		accounts.accounts = append(accounts.accounts, service.Account{ID: int64((i + 1) * 10), Concurrency: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "test", "base_url": url}, Extra: map[string]any{"openai_apikey_responses_websockets_v2_enabled": true, "openai_apikey_responses_websockets_v2_mode": service.OpenAIWSIngressModePassthrough}})
	}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, h.cfg, nil)
	t.Cleanup(billing.Stop)
	gateway := service.NewOpenAIGatewayService(accounts, &openAIWSUsageHandlerUsageLogRepoStub{}, &smartRouteLedger{}, nil, nil, nil, nil, &cfg, nil, nil, service.NewBillingService(&cfg, nil), nil, billing, nil, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	openAI := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, h.apiKeyService, nil, nil, nil, nil, &cfg)
	done := make(chan struct{})
	r := gin.New()
	r.GET("/v1/responses", func(c *gin.Context) {
		defer close(done)
		c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
		c.Set(string(middleware2.ContextKeyAPIKey), key)
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: key.UserID})
		openAI.ResponsesWebSocket(c)
	})
	server := httptest.NewServer(r)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"hello"}`)))
	for {
		_, body, err := conn.Read(ctx)
		require.NoError(t, err, string(body))
		if gjson.GetBytes(body, "type").String() == "response.completed" {
			break
		}
	}
	require.EqualValues(t, 1, primaryCalls.Load())
	require.EqualValues(t, 1, backupCalls.Load())
	require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"again"}`)))
	for {
		_, body, err := conn.Read(ctx)
		if err != nil || gjson.GetBytes(body, "type").String() == "response.failed" {
			break
		}
	}
	conn.CloseNow()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("handler did not stop")
	}
	require.EqualValues(t, 1, primaryCalls.Load(), "established session must not replay on the primary")
	require.EqualValues(t, 1, backupCalls.Load())
}

func TestSmartRouteRevokedGroupAndExpiredSubscriptionUseBackup(t *testing.T) {
	for _, subscribed := range []bool{false, true} {
		h, key, _ := smartRoutingFixture(t)
		h.cfg = &config.Config{RunMode: config.RunModeStandard}
		key.Group.IsExclusive = true
		if subscribed {
			key.Group.SubscriptionType = service.SubscriptionTypeSubscription
		}
		subs := service.NewSubscriptionService(nil, smartRouteSubscriptions{err: service.ErrSubscriptionNotFound}, nil, nil, h.cfg)
		t.Cleanup(subs.Stop)
		c, rec := smartRoutingContext(key, `{"model":"gpt-test"}`)
		h.WithSmartRoutes(func(c *gin.Context) {
			routed, _ := middleware2.GetAPIKeyFromContext(c)
			require.Equal(t, int64(2), *routed.GroupID)
			sub, _ := middleware2.GetSubscriptionFromContext(c)
			require.Nil(t, sub)
			c.JSON(200, gin.H{"ok": true})
		}, subs, nil)(c)
		require.Equal(t, 200, rec.Code)
	}
}

func TestSmartRouteModelsUnionKeepsMediaAndCustomDisplayOnly(t *testing.T) {
	h, key, _ := smartRoutingFixture(t)
	h.gatewayService = newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{}).gatewayService
	key.Group.ModelsListConfig = service.GroupModelsListConfig{Enabled: true, Models: []string{"gpt-5.1"}}
	c, rec := smartRoutingContext(key, "")
	h.SmartRouteModels(c, nil, false)
	ids := gjson.GetBytes(rec.Body.Bytes(), "data.#.id").Array()
	seen := map[string]bool{}
	for _, id := range ids {
		require.False(t, seen[id.String()])
		seen[id.String()] = true
	}
	for _, id := range defaultModelIDsForPlatform(service.PlatformOpenAI) {
		require.True(t, seen[id], id)
	}
	require.True(t, h.gatewayService.SmartRouteModelCompatible(c.Request.Context(), key, "responses", "gpt-5.4"), "display-only list must not reject another supported model")
}

func TestSmartRouteGlobalBudgetAndCrossPackageExclusion(t *testing.T) {
	h, key, _ := smartRoutingFixture(t)
	h.maxAccountSwitches = 2
	var attempts []int64
	run := h.WithSmartRoutes(func(c *gin.Context) {
		key, _ := middleware2.GetAPIKeyFromContext(c)
		excluded := service.APIKeyRouteExcludedAccounts(c.Request.Context(), nil)
		ids := []int64{10}
		if *key.GroupID == 2 {
			ids = []int64{10, 20, 21, 22}
		}
		for _, id := range ids {
			if _, failed := excluded[id]; failed {
				continue
			}
			attempts = append(attempts, id)
			require.True(t, nextSmartRoute(c, &service.UpstreamFailoverError{StatusCode: 502}, &service.Account{ID: id}))
			return
		}
		require.True(t, skipUnavailableSmartRoute(c, service.ErrNoAvailableAccounts))
	}, nil, nil)
	c, rec := smartRoutingContext(key, `{"model":"gpt-test"}`)
	run(c)
	require.Equal(t, 502, rec.Code)
	require.Equal(t, []int64{10, 20, 21}, attempts)
}

func TestSmartRouteKeepsExistingCacheBillingProtection(t *testing.T) {
	h, key, _ := smartRoutingFixture(t)
	calls := 0
	run := h.WithSmartRoutes(func(c *gin.Context) {
		calls++
		if calls == 1 {
			require.True(t, nextSmartRoute(c, &service.UpstreamFailoverError{StatusCode: 502}, &service.Account{ID: 10}))
			smartRouteAttemptFrom(c).forceCacheBilling = true
			return
		}
		require.True(t, service.IsForceCacheBilling(c.Request.Context()))
		c.JSON(200, gin.H{"ok": true})
	}, nil, nil)
	c, rec := smartRoutingContext(key, `{"model":"gpt-test"}`)
	run(c)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, 2, calls)
}

func TestSmartRouteFailureBudgetsAcrossPackages(t *testing.T) {
	for _, endpoint := range []string{"responses", "messages", "chat/completions"} {
		for _, stream := range []bool{false, true} {
			for _, failure := range []string{"capacity", "oauth_429", "bad_gateway"} {
				t.Run(fmt.Sprintf("%s/stream=%v/failure=%s", endpoint, stream, failure), func(t *testing.T) {
					h, key, _ := smartRoutingFixture(t)
					h.maxAccountSwitches = 10
					for _, id := range key.CandidateGroupIDs() {
						candidate, err := h.apiKeyService.APIKeyForRoute(context.Background(), key, id)
						require.NoError(t, err)
						candidate.Group.AllowMessagesDispatch = true
					}
					ids := []int64{10, 11, 20, 21, 22, 23}
					status, failureBody := http.StatusBadGateway, `{"error":{"message":"upstream unavailable"}}`
					if failure == "capacity" {
						status = http.StatusServiceUnavailable
						failureBody = `{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded."}}`
					}
					if failure == "oauth_429" {
						status = http.StatusTooManyRequests
						failureBody = `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Too many requests"}}`
					}
					var accounts []service.Account
					failByID := map[int64]int{}
					for _, id := range ids {
						accounts = append(accounts, service.Account{ID: id, Name: "failed-account", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Priority: int(id), Credentials: map[string]any{"access_token": "test-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}})
						failByID[id] = status
					}
					repo := smartRouteAccounts{openAIImagesFailoverAccountRepo{accounts: accounts}}
					upstream := &machineFailoverUpstream{failByID: failByID, failBody: failureBody}
					billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, h.cfg, nil)
					t.Cleanup(billing.Stop)
					gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, h.cfg, nil, nil, service.NewBillingService(h.cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
					openAI := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, h.apiKeyService, nil, nil, nil, nil, h.cfg)
					next := openAI.Responses
					if endpoint == "messages" {
						next = openAI.Messages
					}
					if endpoint == "chat/completions" {
						next = openAI.ChatCompletions
					}
					c, rec := smartRoutingContext(key, fmt.Sprintf(`{"model":"gpt-5.2","stream":%t,"input":"local test","max_tokens":100,"messages":[{"role":"user","content":"local test"}]}`, stream))
					c.Request.URL.Path = "/v1/" + endpoint
					h.WithSmartRoutes(next, nil, openAI)(c)
					hits, _, _ := upstream.snapshot()
					require.Equal(t, status, rec.Code, rec.Body.String())
					if failure == "capacity" {
						require.Equal(t, ids[:3], hits, "capacity limit must not reset when switching packages")
						if endpoint != "messages" {
							require.Contains(t, rec.Body.String(), "server_is_overloaded")
						}
					} else if failure == "oauth_429" {
						require.Equal(t, ids[:3], hits, "OAuth 429 limit must not reset when switching packages")
					} else {
						require.Equal(t, ids, hits, "ordinary 502 failures must keep the configured attempt budget")
					}
				})
			}
		}
	}
}

func TestSmartRouteFirstOutputTimeoutBudgetAcrossReentry(t *testing.T) {
	h, key, _ := smartRoutingFixture(t)
	calls := 0
	c, rec := smartRoutingContext(key, `{"model":"gpt-test","stream":true}`)
	h.WithSmartRoutes(func(c *gin.Context) {
		calls++
		require.True(t, nextSmartRoute(c, &service.UpstreamFailoverError{StatusCode: 504, SafeToFailoverAfterWrite: true}, &service.Account{ID: int64(calls)}))
	}, nil, nil)(c)
	require.Equal(t, 2, calls, "first-output timeout permits only one account switch")
	require.GreaterOrEqual(t, rec.Code, 400)
}

func TestSmartRouteModelsLookupFailures(t *testing.T) {
	for _, codex := range []bool{false, true} {
		for _, lookup := range []string{"group", "subscription", "ineligible_subscription"} {
			t.Run(fmt.Sprintf("codex=%v/%s", codex, lookup), func(t *testing.T) {
				h, key, cache := smartRoutingFixture(t)
				h.gatewayService = newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{}).gatewayService
				var subs *service.SubscriptionService
				if lookup == "group" {
					h.apiKeyService = service.NewAPIKeyService(nil, nil, smartRouteGroups{err: errors.New("database unavailable")}, nil, nil, cache, h.cfg)
				} else {
					h.cfg = &config.Config{RunMode: config.RunModeStandard}
					backup, err := h.apiKeyService.APIKeyForRoute(context.Background(), key, 2)
					require.NoError(t, err)
					backup.Group.SubscriptionType = service.SubscriptionTypeSubscription
					subErr := errors.New("database unavailable")
					if lookup == "ineligible_subscription" {
						subErr = service.ErrSubscriptionNotFound
					}
					subs = service.NewSubscriptionService(nil, smartRouteSubscriptions{err: subErr}, nil, nil, h.cfg)
					t.Cleanup(subs.Stop)
				}
				c, rec := smartRoutingContext(key, "")
				c.Request.Method = "GET"
				c.Request.URL.Path = "/v1/models"
				h.SmartRouteModels(c, subs, codex)
				if lookup == "ineligible_subscription" {
					require.Equal(t, 200, rec.Code, "an ineligible backup must not hide the primary's models")
					require.Contains(t, rec.Body.String(), "gpt-")
				} else {
					require.Equal(t, 503, rec.Code, "infrastructure failure must not publish an empty or partial model manifest")
					require.Empty(t, rec.Header().Get("ETag"))
				}
			})
		}
	}
}
