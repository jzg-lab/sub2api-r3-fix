package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// GetOpenAIDowngradeDashboard returns the read-only probe/bucket snapshot.
// GET /api/v1/admin/ops/openai/downgrade-probe
func (h *OpsHandler) GetOpenAIDowngradeDashboard(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	start, _, err := parseOpsTimeRange(c, "24h")
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	eventLimit := parsePositiveLimit(c.Query("event_limit"), 100, 200)
	accountLimit := parsePositiveLimit(c.Query("account_limit"), 200, 500)
	data, err := h.opsService.GetOpenAIDowngradeDashboard(c.Request.Context(), start, eventLimit, accountLimit)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, data)
}

func parsePositiveLimit(value string, fallback, maximum int) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fallback
	}
	if parsed > maximum {
		return maximum
	}
	return parsed
}
