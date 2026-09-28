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

// ReenableAccount POST /api/v1/admin/openai/accounts/:id/reenable?unpause=true
// 手动启用判死号（r17x 选项A）：清标签 → qualification 1 针结业 → 上岗。
// unpause=true（r17an）：manual_paused 刹车随本请求显式解除（专用解暂停，
// 不动 schedulable），解除动作独立落 manual_unpause 审计事件。
// 落 audit_logs（审计中间件自动记录 POST 变更类请求）。
func (h *OpenAIProbeHealthHandler) ReenableAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid account id")
		return
	}
	unpause := c.Query("unpause") == "true" || c.Query("unpause") == "1"
	result, err := h.runner.ReenableOpenAIAccount(c.Request.Context(), id, unpause)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// StartHarvest POST /api/v1/admin/openai/accounts/:id/harvest
// 问题号转打票线（相位B 自动打票）：迁动态桶采票 → 采到回静态复检 →
// 通过恢复上岗。落 audit_logs（审计中间件自动记录 POST 变更类请求）。
func (h *OpenAIProbeHealthHandler) StartHarvest(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "invalid account id")
		return
	}
	state, err := h.runner.StartHarvestOpenAIAccount(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"account_id":    state.AccountID,
		"probe_mode":    state.ProbeMode,
		"next_probe_at": state.NextProbeAt,
		"harvest_attempts": state.HarvestAttempts,
	})
}
