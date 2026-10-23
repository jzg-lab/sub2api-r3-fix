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
	outcomes     map[int64]int
}

func TestOpenAIAmbiguous400WebSocketRecoveryBudget(t *testing.T) {
	for _, tc := range []struct {
		name        string
		outcomes    map[int64]int
		calls       []int64
		maxSwitches int
		completed   bool
	}{
		{"same_group", map[int64]int{11: 200}, []int64{10, 11}, 8, true},
		{"backup_group_retry", map[int64]int{21: 200}, []int64{10, 11, 20, 21}, 8, true},
		{"third_group", map[int64]int{30: 200}, []int64{10, 11, 20, 21, 30}, 8, true},
		{"second_account_502", map[int64]int{11: 502, 30: 200}, []int64{10, 11, 20, 21, 30}, 8, true},
		{"second_account_413", map[int64]int{11: 413, 30: 200}, []int64{10, 11, 20, 21, 30}, 8, true},
		{"all_groups_fail", map[int64]int{}, []int64{10, 11, 20, 21, 30, 31}, 8, false},
		{"shared_budget", map[int64]int{30: 200}, []int64{10, 11, 20}, 2, false},
		{"single_account_groups", map[int64]int{30: 200}, []int64{10, 20, 30}, 8, true},
		{"no_backups", map[int64]int{30: 200}, []int64{10, 11}, 8, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, key, cache, accounts := ambiguous400BackupGroupsFixture(t, false)
			if tc.name == "single_account_groups" {
				accounts.accounts = []service.Account{accounts.accounts[0], accounts.accounts[3], accounts.accounts[6]}
			}
			if tc.name == "no_backups" {
				key.RouteGroupIDs = nil
			}
			cfg := *h.cfg
			cfg.RunMode = config.RunModeStandard
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.IngressModeDefault = service.OpenAIWSIngressModeHTTPBridge
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
			for i := range accounts.accounts {
				accounts.accounts[i].Extra["openai_apikey_responses_websockets_v2_mode"] = service.OpenAIWSIngressModeHTTPBridge
			}
			upstream := &ambiguous400Upstream{outcomes: tc.outcomes}
			usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 10)}
			ledger := &smartRouteLedger{}
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, h.cfg, nil)
			t.Cleanup(billing.Stop)
			gateway := service.NewOpenAIGatewayService(accounts, usage, ledger, nil, nil, nil, nil, &cfg, nil, nil, service.NewBillingService(&cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			openAI := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(&concurrencyCacheMock{
				acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
				acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
			}), billing, h.apiKeyService, nil, nil, nil, nil, &cfg)
			openAI.maxAccountSwitches = tc.maxSwitches
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
			require.Equal(t, tc.completed, completed)
			require.Equal(t, tc.calls, upstream.calls)
			if tc.completed {
				accountID := tc.calls[len(tc.calls)-1]
				select {
				case log := <-usage.created:
					require.Equal(t, accountID/10, *log.GroupID)
					require.Equal(t, float64(accountID/10*2-1), log.RateMultiplier)
				case <-time.After(time.Second):
					t.Fatal("usage was not recorded")
				}
				require.Len(t, ledger.commands, 1)
				require.Equal(t, accountID, ledger.commands[0].AccountID)
			} else {
				require.Empty(t, ledger.commands)
			}
			if tc.name != "second_account_502" {
				require.Empty(t, cache.failed, "ambiguous request errors must not penalize packages")
			}
		})
	}
}

type ambiguous400SharedAccounts struct{ smartRouteAccounts }

func (r ambiguous400SharedAccounts) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]service.Account, error) {
	accounts, err := r.smartRouteAccounts.ListSchedulableByGroupIDAndPlatform(ctx, groupID, platform)
	if groupID == 2 {
		for _, account := range r.accounts {
			if account.ID == 10 || account.ID == 11 {
				accounts = append(accounts, account)
			}
		}
	}
	return accounts, err
}

func ambiguous400BackupGroupsFixture(t *testing.T, passthrough bool) (*GatewayHandler, *service.APIKey, *smartRouteCache, ambiguous400SharedAccounts) {
	h, key, cache := smartRoutingFixture(t)
	groups := map[int64]*service.Group{1: key.Group}
	for _, id := range []int64{2, 3} {
		groups[id] = &service.Group{ID: id, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, RateMultiplier: float64(id*2 - 1)}
	}
	key.RouteGroupIDs = []int64{1, 2, 3}
	h.apiKeyService = service.NewAPIKeyService(nil, nil, smartRouteGroups{groups: groups}, nil, nil, cache, h.cfg)
	var pool []service.Account
	for _, id := range []int64{10, 11, 12, 20, 21, 22, 30, 31, 32} {
		pool = append(pool, service.Account{ID: id, Name: fmt.Sprint(id), Concurrency: 1, Priority: int(id), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "test", "base_url": "https://provider.invalid", "pool_mode": true, "pool_mode_retry_count": float64(5)}, Extra: map[string]any{"openai_passthrough": passthrough}})
	}
	return h, key, cache, ambiguous400SharedAccounts{smartRouteAccounts{openAIImagesFailoverAccountRepo{accounts: pool}}}
}

func TestOpenAIAmbiguous400ContinuesThroughBackupGroups(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name        string
				outcomes    map[int64]int
				calls       []int64
				maxSwitches int
				success     bool
			}{
				{"third_group", map[int64]int{30: 200}, []int64{10, 11, 20, 21, 30}, 8, true},
				{"second_account_502", map[int64]int{11: 502, 30: 200}, []int64{10, 11, 20, 21, 30}, 8, true},
				{"second_account_413", map[int64]int{11: 413, 30: 200}, []int64{10, 11, 20, 21, 30}, 8, true},
				{"all_groups_fail", map[int64]int{}, []int64{10, 11, 20, 21, 30, 31}, 8, false},
				{"shared_budget", map[int64]int{30: 200}, []int64{10, 11, 20}, 2, false},
			} {
				t.Run(fmt.Sprintf("passthrough=%v/stream=%v/%s", passthrough, stream, tc.name), func(t *testing.T) {
					h, key, cache, accounts := ambiguous400BackupGroupsFixture(t, passthrough)
					h.maxAccountSwitches = tc.maxSwitches
					cfg := *h.cfg
					cfg.RunMode = config.RunModeStandard
					upstream := &ambiguous400Upstream{outcomes: tc.outcomes}
					usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 10)}
					ledger := &smartRouteLedger{}
					billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, h.cfg, nil)
					t.Cleanup(billing.Stop)
					gateway := service.NewOpenAIGatewayService(accounts, usage, ledger, nil, nil, nil, nil, &cfg, nil, nil, service.NewBillingService(&cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
					openAI := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, h.apiKeyService, nil, nil, nil, nil, &cfg)
					openAI.maxAccountSwitches = tc.maxSwitches
					c, rec := smartRoutingContext(key, fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"hello","prompt_cache_key":"backup-session"}`, stream))
					h.WithSmartRoutes(openAI.Responses, nil, openAI)(c)
					require.Equal(t, tc.calls, upstream.calls, rec.Body.String())
					if tc.success {
						require.Equal(t, 200, rec.Code, rec.Body.String())
						select {
						case log := <-usage.created:
							require.Equal(t, int64(3), *log.GroupID)
							require.Equal(t, 5.0, log.RateMultiplier)
						case <-time.After(time.Second):
							t.Fatal("usage was not recorded")
						}
						require.Len(t, ledger.commands, 1)
						require.Equal(t, int64(30), ledger.commands[0].AccountID)
						require.Greater(t, ledger.commands[0].BalanceCost, 0.0)
						require.Contains(t, cache.sessions, "9/responses\x00gpt-5.1/backup-session")
						require.Equal(t, int64(3), cache.sessions["9/responses\x00gpt-5.1/backup-session"])
					} else {
						require.Equal(t, 502, rec.Code, rec.Body.String())
						require.Empty(t, ledger.commands)
					}
					if tc.name != "second_account_502" {
						require.Empty(t, cache.failed, "ambiguous request errors must not penalize packages")
					}
				})
			}
		}
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
	if u.outcomes != nil {
		status, body = u.outcomes[id], ambiguous400Response
		if status == 0 {
			status = 400
		}
	}
	if status == 413 {
		body = `{"error":{"type":"invalid_request_error","message":"Request body too large"}}`
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
						if placement == "backup_package" && (outcome == "generic400" || outcome == "server502") {
							wantCalls = append(wantCalls, secondID+1)
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
