package admin

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// OpenAIProbeHealthHandler 账号健康标签 + 主动检测（相位A）。
// 依赖探针 runner（service.OpenAIDowngradeProbeRunner）——经 wire 注入的
// service 层方法承载业务，handler 只做参数解析与响应。
type OpenAIProbeHealthHandler struct {
	runner *service.OpenAIDowngradeProbeRunner
}

func NewOpenAIProbeHealthHandler(runner *service.OpenAIDowngradeProbeRunner) *OpenAIProbeHealthHandler {
	return &OpenAIProbeHealthHandler{runner: runner}
}

// GetAccountHealth GET /api/v1/admin/openai/accounts/:id/health
func (h *OpenAIProbeHealthHandler) GetAccountHealth(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid account id")
		return
	}
	health, err := h.runner.GetOpenAIAccountHealth(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, health)
}

// ListAccountHealth GET /api/v1/admin/openai/accounts/health?ids=1,2,3
// 批量健康快照（账号列表页一次查齐，避免逐号 N+1）。ids 上限 200 与
// 列表页 page_size 对齐；逗号分隔，非法段直接 400。
func (h *OpenAIProbeHealthHandler) ListAccountHealth(c *gin.Context) {
	raw := strings.TrimSpace(c.Query("ids"))
	if raw == "" {
		response.BadRequest(c, "ids is required")
		return
	}
	seen := make(map[int64]bool, 32)
	ids := make([]int64, 0, 16)
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "invalid account id in ids")
			return
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		if len(ids) > 200 {
			response.BadRequest(c, "too many ids")
			return
		}
	}
	health, err := h.runner.ListOpenAIAccountHealth(c.Request.Context(), ids)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, health)
}

// TriggerProbeNow POST /api/v1/admin/openai/accounts/:id/probe-now
// 主动检测：落 audit_logs（审计中间件自动记录 POST 变更类请求）。
func (h *OpenAIProbeHealthHandler) TriggerProbeNow(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid account id")
		return
	}
	result, err := h.runner.TriggerProbeNow(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
