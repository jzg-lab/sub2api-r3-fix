//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const ambiguous400Response = `{"error":{"type":"upstream_error","message":"Upstream request failed"}}`

type ambiguous400Upstream struct {
	service.HTTPUpstream
	calls        []int64
	firstBody    string
	secondStatus int
	secondBody   string
}

func TestOpenAIAmbiguous400WebSocketRecoveryBudget(t *testing.T) {
	for _, secondStatus := range []int{200, 400, 502} {
		t.Run(fmt.Sprint(secondStatus), func(t *testing.T) {
			h, key, _ := smartRoutingFixture(t)
			cfg := *h.cfg
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.IngressModeDefault = service.OpenAIWSIngressModeHTTPBridge
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
			var pool []service.Account
			for _, id := range []int64{10, 20, 21} {
				pool = append(pool, service.Account{ID: id, Concurrency: 1, Priority: int(id), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "test", "base_url": "https://provider.invalid"}, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": service.OpenAIWSIngressModeHTTPBridge}})
			}
			accounts := smartRouteAccounts{openAIImagesFailoverAccountRepo{accounts: pool}}
			upstream := &ambiguous400Upstream{firstBody: ambiguous400Response, secondStatus: secondStatus, secondBody: ambiguous400Response}
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, h.cfg, nil)
			t.Cleanup(billing.Stop)
			gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, &cfg, nil, nil, service.NewBillingService(&cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			openAI := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(&concurrencyCacheMock{
				acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
				acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
			}), billing, h.apiKeyService, nil, nil, nil, nil, &cfg)
			openAI.maxAccountSwitches = 8
			done := make(chan struct{})
			r := gin.New()
			r.GET("/v1/responses", func(c *gin.Context) {
				defer close(done)
				c.Set(string(middleware2.ContextKeyAPIKey), key)
				c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: key.UserID})
				openAI.ResponsesWebSocket(c)
			})
			server := httptest.NewServer(r)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			require.NoError(t, err)
			defer conn.CloseNow()
			require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"hello"}`)))
			completed := false
			for {
				_, body, readErr := conn.Read(ctx)
				if readErr != nil {
					require.NotErrorIs(t, readErr, context.DeadlineExceeded)
					break
				}
				if gjson.GetBytes(body, "type").String() == "response.completed" {
					completed = true
					break
				}
			}
			conn.CloseNow()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("websocket handler did not finish")
			}
			require.Equal(t, secondStatus == 200, completed)
			require.Equal(t, []int64{10, 20}, upstream.calls)
		})
	}
}

func (u *ambiguous400Upstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func (u *ambiguous400Upstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	u.calls = append(u.calls, id)
	status, body := 400, u.firstBody
	if len(u.calls) > 1 {
		status, body = u.secondStatus, u.secondBody
	}
	if status == 200 {
		success := &smartRouteHTTPUpstream{succeedID: id}
		return success.Do(req, "", id, 1)
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func TestOpenAIAmbiguous400RealRoutingAndBilling(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, placement := range []string{"single_group", "same_package", "backup_package"} {
				for _, outcome := range []string{"success", "subscription_success", "generic400", "server502", "rejected_field", "schema_error", "policy_error", "budget_exhausted"} {
					t.Run(fmt.Sprintf("passthrough=%v/stream=%v/%s/%s", passthrough, stream, placement, outcome), func(t *testing.T) {
						h, key, cache := smartRoutingFixture(t)
						h.maxAccountSwitches = 8
						secondID, groupID, multiplier := int64(11), int64(1), 1.0
						if placement == "single_group" {
							key.RouteGroupIDs = nil
						}
						if placement == "backup_package" {
							secondID, groupID, multiplier = 20, 2, 3
						}
						var pool []service.Account
						for i, id := range []int64{10, secondID, secondID + 1} {
							pool = append(pool, service.Account{ID: id, Name: fmt.Sprint(id), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: i, Credentials: map[string]any{"api_key": "test", "base_url": "https://provider.invalid", "pool_mode": true, "pool_mode_retry_count": float64(5)}, Extra: map[string]any{"openai_passthrough": passthrough}})
						}
						accounts := smartRouteAccounts{openAIImagesFailoverAccountRepo{accounts: pool}}
						upstream := &ambiguous400Upstream{firstBody: ambiguous400Response, secondStatus: 200}
						wantCalls := []int64{10, secondID}
						switch outcome {
						case "generic400":
							upstream.secondStatus, upstream.secondBody = 400, ambiguous400Response
						case "server502":
							upstream.secondStatus, upstream.secondBody = 502, `{"error":{"message":"unavailable"}}`
						case "rejected_field":
							upstream.secondStatus, upstream.secondBody = 400, `{"error":{"code":"unsupported_parameter","param":"truncation","message":"Unsupported parameter: truncation."}}`
						case "schema_error":
							upstream.firstBody = `{"error":{"code":"invalid_function_parameters","param":"tools[0].parameters","message":"Invalid schema for function request_user_input"}}`
							wantCalls = []int64{10}
						case "policy_error":
							upstream.firstBody = `{"error":{"code":"cyber_policy","message":"Request blocked by cyber policy"}}`
							wantCalls = []int64{10}
						case "budget_exhausted":
							h.maxAccountSwitches = 0
							wantCalls = []int64{10}
						}
						cfg := *h.cfg
						cfg.RunMode = config.RunModeStandard
						usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 10)}
						ledger := &smartRouteLedger{}
						billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, h.cfg, nil)
						t.Cleanup(billing.Stop)
						gateway := service.NewOpenAIGatewayService(accounts, usage, ledger, nil, nil, nil, nil, &cfg, nil, nil, service.NewBillingService(&cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
						openAI := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, h.apiKeyService, nil, nil, nil, nil, h.cfg)
						openAI.maxAccountSwitches = 8
						if outcome == "budget_exhausted" {
							openAI.maxAccountSwitches = 0
						}
						var subs *service.SubscriptionService
						if outcome == "subscription_success" {
							h.cfg = &cfg
							routed, err := h.apiKeyService.APIKeyForRoute(context.Background(), key, groupID)
							require.NoError(t, err)
							routed.Group.SubscriptionType = service.SubscriptionTypeSubscription
							now := time.Now()
							sub := &service.UserSubscription{ID: 50, UserID: key.UserID, GroupID: groupID, Status: service.SubscriptionStatusActive, StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}
							subs = service.NewSubscriptionService(nil, smartRouteSubscriptions{sub: sub}, nil, nil, &cfg)
							t.Cleanup(subs.Stop)
						}
						c, rec := smartRoutingContext(key, fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"hello","truncation":"auto"}`, stream))
						if outcome == "subscription_success" && placement == "single_group" {
							sub, err := subs.GetActiveSubscription(context.Background(), key.UserID, groupID)
							require.NoError(t, err)
							c.Set(string(middleware2.ContextKeySubscription), sub)
						}
						h.WithSmartRoutes(openAI.Responses, subs, openAI)(c)
						require.Equal(t, wantCalls, upstream.calls, "response=%s", rec.Body.String())
						if outcome == "success" || outcome == "subscription_success" {
							require.Equal(t, 200, rec.Code, rec.Body.String())
							require.Contains(t, rec.Body.String(), "OK")
							select {
							case log := <-usage.created:
								require.Equal(t, groupID, *log.GroupID)
								require.Equal(t, multiplier, log.RateMultiplier)
							case <-time.After(time.Second):
								t.Fatal("usage was not recorded")
							}
							require.Len(t, ledger.commands, 1)
							require.Equal(t, secondID, ledger.commands[0].AccountID)
							if outcome == "subscription_success" {
								require.Zero(t, ledger.commands[0].BalanceCost)
								require.Greater(t, ledger.commands[0].SubscriptionCost, 0.0)
							} else {
								require.Greater(t, ledger.commands[0].BalanceCost, 0.0)
							}
							require.Empty(t, cache.failed, "a generic 400 must not demote a package")
						} else if outcome == "policy_error" {
							// Policy refusals retain the existing asynchronous zero-token audit entry.
							select {
							case <-usage.created:
							case <-time.After(time.Second):
								t.Fatal("policy audit was not recorded")
							}
							require.Len(t, ledger.commands, 1)
							require.Zero(t, ledger.commands[0].BalanceCost)
						} else {
							require.NotContains(t, rec.Body.String(), `"text":"OK"`)
							require.Empty(t, ledger.commands)
						}
					})
				}
			}
		}
	}
}
