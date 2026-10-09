package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func (h *GatewayHandler) SmartRouteModels(c *gin.Context, subscriptions *service.SubscriptionService, codex bool) {
	key, _ := middleware2.GetAPIKeyFromContext(c)
	ids := make([]string, 0)
	manifests := make([]json.RawMessage, 0)
	seen := make(map[string]bool)
	for _, groupID := range key.CandidateGroupIDs() {
		routed, err := h.apiKeyService.APIKeyForRoute(c.Request.Context(), key, groupID)
		if err != nil {
			if !errors.Is(err, service.ErrGroupNotFound) && !errors.Is(err, service.ErrGroupNotAllowed) {
				h.responsesErrorResponse(c, http.StatusServiceUnavailable, "api_error", "Failed to resolve route group")
				return
			}
			continue
		}
		if _, err := routeSubscription(c.Request.Context(), subscriptions, routed, h.cfg != nil && h.cfg.RunMode == config.RunModeSimple); err != nil {
			if !isSmartRouteEligibilityError(err) {
				h.responsesErrorResponse(c, http.StatusServiceUnavailable, "billing_service_error", "Failed to verify route subscription")
				return
			}
			continue
		}
		ctx := service.ContextWithAPIKeyRoute(c.Request.Context(), routed)
		modelIDs := h.modelIDsForGroup(ctx, routed.Group, "", codex)
		if codex {
			modelIDs = service.FilterCodexModelIDsForGroup(modelIDs, routed.Group)
			body, err := h.gatewayService.BuildCodexModelsManifestForGroup(ctx, routed.Group, "", modelIDs)
			if err != nil {
				h.responsesErrorResponse(c, 500, "api_error", "Failed to build models manifest")
				return
			}
			for _, model := range gjson.GetBytes(body, "models").Array() {
				id := model.Get("slug").String()
				if id != "" && !seen[id] {
					seen[id] = true
					manifests = append(manifests, json.RawMessage(model.Raw))
				}
			}
		} else {
			for _, id := range modelIDs {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	if !codex {
		writeModelsList(c, service.PlatformComposite, ids)
		return
	}
	body, err := json.Marshal(map[string]any{"models": manifests})
	if err != nil {
		h.responsesErrorResponse(c, 500, "api_error", "Failed to build models manifest")
		return
	}
	etag := service.CodexModelsManifestETag(body)
	c.Header("ETag", etag)
	if service.CodexModelsManifestETagMatches(c.GetHeader("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		c.Writer.WriteHeaderNow()
		return
	}
	c.Data(http.StatusOK, "application/json", body)
}
