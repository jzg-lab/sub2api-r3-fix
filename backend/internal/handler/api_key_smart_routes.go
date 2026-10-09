package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const smartRouteAttemptKey = "smart_route_attempt"

type smartRouteAttempt struct {
	retry             bool
	upstream          *service.UpstreamFailoverError
	err               error
	failedAccountID   int64
	failedAccount     *service.Account
	forceCacheBilling bool
}

func smartRouteAttemptFrom(c *gin.Context) *smartRouteAttempt {
	value, _ := c.Get(smartRouteAttemptKey)
	attempt, _ := value.(*smartRouteAttempt)
	return attempt
}

// Even when output prevents replay, the next client request should avoid this route.
func observeSmartRouteFailure(c *gin.Context, err *service.UpstreamFailoverError, accountIDs ...int64) {
	if attempt := smartRouteAttemptFrom(c); attempt != nil && rememberSmartRouteFailure(err) && c.Request.Context().Err() == nil {
		attempt.upstream = err
		if len(accountIDs) > 0 {
			attempt.failedAccountID = accountIDs[0]
		}
	}
}

func rememberSmartRouteFailure(err *service.UpstreamFailoverError) bool {
	return err != nil && err.ShouldRetryNextAccount() && !err.RequestScopedTransient && err.Scope != service.GatewayFailureScopeRequest &&
		(err.StatusCode >= 500 || err.StatusCode == 429 || err.StatusCode == 401 || err.StatusCode == 403 || err.IsCredentialFailure() || err.IsProxyChainFailure())
}

// Called only after the forwarder's output-commit and cancellation checks.
func nextSmartRoute(c *gin.Context, err *service.UpstreamFailoverError, account *service.Account) bool {
	attempt := smartRouteAttemptFrom(c)
	if attempt == nil || err == nil || service.IsResponseCommitted(c) || !err.ShouldRetryNextAccount() || c.Request.Context().Err() != nil {
		return false
	}
	attempt.retry, attempt.upstream = true, err
	if account != nil {
		attempt.failedAccount, attempt.failedAccountID = account, account.ID
		service.FailAPIKeyRouteAccount(c.Request.Context(), account.ID)
	}
	return true
}

func skipUnavailableSmartRoute(c *gin.Context, err error) bool {
	attempt := smartRouteAttemptFrom(c)
	if attempt == nil || c.Request.Context().Err() != nil {
		return false
	}
	if !errors.Is(err, service.ErrNoAvailableAccounts) && !errors.Is(err, service.ErrNoAvailableCompactAccounts) &&
		!errors.Is(err, service.ErrClaudeCodeOnly) && !errors.Is(err, service.ErrGroupNotFound) {
		return false
	}
	attempt.retry, attempt.err = true, err
	return true
}

func skipIneligibleSmartRoute(c *gin.Context, err error) bool {
	attempt := smartRouteAttemptFrom(c)
	if attempt == nil {
		return false
	}
	if isSmartRouteEligibilityError(err) {
		attempt.retry, attempt.err = true, err
		return true
	}
	return false
}

func isSmartRouteEligibilityError(err error) bool {
	switch {
	case errors.Is(err, service.ErrSubscriptionExpired), errors.Is(err, service.ErrSubscriptionSuspended), errors.Is(err, service.ErrInsufficientBalance), errors.Is(err, service.ErrSubscriptionInvalid),
		errors.Is(err, service.ErrDailyLimitExceeded), errors.Is(err, service.ErrWeeklyLimitExceeded),
		errors.Is(err, service.ErrMonthlyLimitExceeded), errors.Is(err, service.ErrGroupRPMExceeded),
		errors.Is(err, service.ErrUserPlatformDailyQuotaExhausted), errors.Is(err, service.ErrUserPlatformWeeklyQuotaExhausted), errors.Is(err, service.ErrUserPlatformMonthlyQuotaExhausted):
		return true
	}
	return false
}

func routeSubscription(ctx context.Context, subscriptions *service.SubscriptionService, key *service.APIKey, simple bool) (*service.UserSubscription, error) {
	if simple || !key.Group.IsSubscriptionType() {
		return nil, nil
	}
	if subscriptions == nil {
		return nil, service.ErrBillingServiceUnavailable
	}
	sub, err := subscriptions.GetActiveSubscription(ctx, key.UserID, key.Group.ID)
	if errors.Is(err, service.ErrSubscriptionNotFound) {
		return nil, service.ErrSubscriptionInvalid
	}
	if err != nil {
		return nil, err
	}
	if sub == nil || sub.UserID != key.UserID || sub.GroupID != key.Group.ID || sub.Status != service.SubscriptionStatusActive || sub.StartsAt.After(time.Now()) {
		return nil, service.ErrSubscriptionInvalid
	}
	needsMaintenance, err := subscriptions.ValidateAndCheckLimits(sub, key.Group)
	if needsMaintenance {
		sub, err = subscriptions.EnsureWindowMaintenance(ctx, sub)
		if err != nil {
			return nil, err
		}
		_, err = subscriptions.ValidateAndCheckLimits(sub, key.Group)
	}
	return sub, err
}

// WithSmartRoutes re-enters the existing protocol handler with a fresh request
// snapshot. Forwarders explicitly yield before writing an error; arbitrary HTTP
// errors are never replayed or buffered by this layer.
func (h *GatewayHandler) WithSmartRoutes(next gin.HandlerFunc, subscriptions *service.SubscriptionService, openAI *OpenAIGatewayHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		key, _ := middleware2.GetAPIKeyFromContext(c)
		if !key.HasSmartRoutes() || !service.APIKeySmartRouteEndpoint(c.Request.Method, c.Request.URL.Path) {
			next(c)
			return
		}
		body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
		if err != nil {
			status := http.StatusBadRequest
			if _, tooLarge := extractMaxBytesError(err); tooLarge {
				status = http.StatusRequestEntityTooLarge
			}
			h.smartRouteError(c, status, "invalid_request_error", "Failed to read request body")
			return
		}
		if !gjson.ValidBytes(body) || gjson.GetBytes(body, "model").Type != gjson.String || strings.TrimSpace(gjson.GetBytes(body, "model").String()) == "" {
			h.smartRouteError(c, http.StatusBadRequest, "invalid_request_error", "model is required in a valid JSON request")
			return
		}
		model := gjson.GetBytes(body, "model").String()
		scope := smartRouteScope(c.Request.URL.Path, model)
		session := smartRouteSession(c, body)
		ids := h.apiKeyService.OrderAPIKeyRoutes(c.Request.Context(), key, scope, session)
		// A server-owned continuation cannot be resumed on a different provider.
		// Image-generation tools also keep their existing single-group behavior.
		pinned := gjson.GetBytes(body, "previous_response_id").String() != "" || gjson.GetBytes(body, "conversation").Exists() || smartRouteImageRequest(body)
		if pinned && key.GroupID != nil {
			ids = []int64{*key.GroupID}
			if previous := gjson.GetBytes(body, "previous_response_id").String(); previous != "" && openAI != nil {
				id, lookupErr := openAI.gatewayService.SmartRouteResponseGroup(c.Request.Context(), key, previous, true)
				if lookupErr != nil {
					h.smartRouteError(c, 503, "api_error", "Failed to resolve response owner")
					return
				}
				if id > 0 {
					ids = []int64{id}
				}
			}
		}
		base := c.Copy()
		baseHeaders := c.Writer.Header().Clone()
		base.Request = c.Request.WithContext(service.WithAPIKeyRouteAdmission(c.Request.Context(), !pinned))
		var lastErr error
		var lastUpstream *service.UpstreamFailoverError
		forwards := 0
		var capacityRetryBudget openAICapacityRetryBudget
		var oauth429FailoverState service.OpenAIOAuth429FailoverState
		firstOutputTimeoutSwitchCount := 0
		failedGroups := make(map[int64]bool)
		forceCacheBilling := false
		for routeIndex := 0; routeIndex < len(ids); {
			id := ids[routeIndex]
			routeIndex++
			if c.Request.Context().Err() != nil {
				return
			}
			routed, err := h.apiKeyService.APIKeyForRoute(base.Request.Context(), key, id)
			if err != nil {
				lastErr = err
				if !errors.Is(err, service.ErrGroupNotFound) && !errors.Is(err, service.ErrGroupNotAllowed) {
					h.smartRouteError(c, http.StatusServiceUnavailable, "api_error", "Failed to resolve route group")
					return
				}
				continue
			}
			sub, err := routeSubscription(base.Request.Context(), subscriptions, routed, h.cfg != nil && h.cfg.RunMode == config.RunModeSimple)
			if err != nil {
				lastErr = err
				if !isSmartRouteEligibilityError(err) {
					h.smartRouteError(c, http.StatusServiceUnavailable, "billing_service_error", "Failed to verify route subscription")
					return
				}
				continue
			}

			routeCtx := service.ContextWithAPIKeyRoute(base.Request.Context(), routed)
			if forceCacheBilling {
				routeCtx = service.WithForceCacheBilling(routeCtx)
			}
			if !pinned && !h.gatewayService.SmartRouteModelCompatible(routeCtx, routed, strings.SplitN(scope, "\x00", 2)[0], model) {
				continue
			}
			if _, restricted := h.gatewayService.ResolveChannelMappingAndRestrict(routeCtx, routed.GroupID, model); restricted {
				continue
			}
			attempt := base.Copy()
			attempt.Writer = c.Writer
			service.CopyOpenAIKeepaliveState(attempt, c)
			attempt.Request = base.Request.Clone(routeCtx)
			attempt.Request.Body = io.NopCloser(bytes.NewReader(body))
			attempt.Request.ContentLength = int64(len(body))
			routed.RouteGroupIDs = nil
			attempt.Set(string(middleware2.ContextKeyAPIKey), routed)
			attempt.Set(string(middleware2.ContextKeySubscription), sub)
			state := &smartRouteAttempt{}
			if !pinned {
				attempt.Set(smartRouteAttemptKey, state)
			}
			if !c.Writer.Written() {
				for name := range c.Writer.Header() {
					c.Writer.Header().Del(name)
				}
				for name, values := range baseHeaders {
					c.Writer.Header()[name] = append([]string(nil), values...)
				}
			}
			if events, exists := c.Get(service.OpsUpstreamErrorsKey); exists {
				if prior, ok := events.([]*service.OpsUpstreamErrorEvent); ok {
					attempt.Set(service.OpsUpstreamErrorsKey, append([]*service.OpsUpstreamErrorEvent(nil), prior...))
				}
			}
			next(attempt)
			forceCacheBilling = forceCacheBilling || state.forceCacheBilling
			// Retain the selected billing identity and operational evidence for outer middleware.
			for name, value := range attempt.Keys {
				c.Set(name, value)
			}
			c.Request = c.Request.WithContext(attempt.Request.Context())
			if rememberSmartRouteFailure(state.upstream) {
				failedGroups[id] = true
			}
			if state.err != nil && failedGroups[id] {
				h.apiKeyService.MarkAPIKeyRouteFailed(base.Request.Context(), routed, scope, session)
			}
			if !state.retry {
				if state.upstream != nil && state.failedAccountID == 0 {
					h.apiKeyService.MarkAPIKeyRouteFailed(base.Request.Context(), routed, scope, session)
				}
				if state.upstream == nil && c.Writer.Status() < 400 && c.Request.Context().Err() == nil {
					if streamErr, found := service.GetOpsStreamError(attempt); !found || !streamErr.CountTowardsSLA {
						h.apiKeyService.RememberAPIKeyRouteSession(base.Request.Context(), routed, scope, session)
					}
				}
				return
			}
			if state.upstream != nil {
				lastUpstream = state.upstream
				forwards++
				// Handler re-entry must not reset the existing request-wide failure limits.
				if capacityRetryBudget.exhausted(state.upstream) ||
					openAIFirstOutputFailoverExhausted(state.upstream, &firstOutputTimeoutSwitchCount) ||
					forwards >= h.maxAccountSwitches+1 ||
					(openAI != nil && state.failedAccount != nil && openAI.gatewayService.ShouldStopOpenAIOAuth429Failover(state.failedAccount, state.upstream.StatusCode, forwards, &oauth429FailoverState)) {
					if failedGroups[id] {
						h.apiKeyService.MarkAPIKeyRouteFailed(base.Request.Context(), routed, scope, session)
					}
					break
				}
				if state.failedAccountID > 0 {
					routeIndex--
				} else if failedGroups[id] {
					h.apiKeyService.MarkAPIKeyRouteFailed(base.Request.Context(), routed, scope, session)
				}
			}
			if state.err != nil {
				lastErr = state.err
			}
		}
		if lastUpstream != nil {
			if lastUpstream.IsOpenAICapacityShed() && openAI != nil {
				if strings.HasSuffix(c.Request.URL.Path, "/messages") {
					openAI.handleAnthropicFailoverExhausted(c, lastUpstream, c.Writer.Written())
				} else {
					openAI.handleFailoverExhausted(c, lastUpstream, c.Writer.Written())
				}
				return
			}
			status, kind, message := h.mapUpstreamError(lastUpstream.StatusCode)
			copyFailoverRetryAfter(c, lastUpstream.ResponseHeaders)
			if lastUpstream.ClientStatusCode > 0 {
				status = lastUpstream.ClientStatusCode
			}
			if lastUpstream.ClientMessage != "" {
				message = lastUpstream.ClientMessage
			}
			if lastUpstream.IsOpenAIRequestBodyTooLarge() {
				kind = "invalid_request_error"
			}
			h.smartRouteError(c, status, kind, message)
		} else if isSmartRouteEligibilityError(lastErr) {
			status, kind, message, retryAfter := billingErrorDetails(lastErr)
			if retryAfter > 0 && !c.Writer.Written() {
				c.Header("Retry-After", strconv.Itoa(retryAfter))
			}
			h.smartRouteError(c, status, kind, message)
		} else {
			h.smartRouteError(c, http.StatusServiceUnavailable, "api_error", "No compatible, available group for this API key")
		}
	}
}

func smartRouteSession(c *gin.Context, body []byte) string {
	if session := service.ExtractClientSessionID(c); session != "" {
		return session
	}
	if metadata := service.ParseMetadataUserID(gjson.GetBytes(body, "metadata.user_id").String()); metadata != nil {
		return metadata.SessionID
	}
	return ""
}

func smartRouteScope(path, model string) string {
	endpoint := "responses"
	if strings.HasSuffix(path, "/messages") {
		endpoint = "messages"
	}
	if strings.HasSuffix(path, "/chat/completions") {
		endpoint = "chat/completions"
	}
	return endpoint + "\x00" + model
}

func smartRouteImageRequest(body []byte) bool {
	return service.IsExplicitImageGenerationIntent("/v1/responses", gjson.GetBytes(body, "model").String(), body)
}

func (h *GatewayHandler) smartRouteError(c *gin.Context, status int, kind, message string) {
	if strings.HasSuffix(c.Request.URL.Path, "/messages") {
		h.handleStreamingAwareError(c, status, kind, message, c.Writer.Written())
		return
	}
	h.smartRouteOpenAIError(c, status, kind, message)
}

func (h *GatewayHandler) smartRouteOpenAIError(c *gin.Context, status int, kind, message string) {
	if !c.Writer.Written() {
		h.responsesErrorResponse(c, status, kind, message)
		return
	}
	h.handleStreamingAwareError(c, status, kind, message, true)
}
