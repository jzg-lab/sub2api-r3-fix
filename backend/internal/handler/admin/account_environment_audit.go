package admin

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// EnvironmentAudit is read-only and performs no account or network probes.
func (h *AccountHandler) EnvironmentAudit(c *gin.Context) {
	accounts, err := h.adminService.ListAccountsForSchedulerScoreFilter(
		c.Request.Context(), service.PlatformOpenAI, service.AccountTypeOAuth, "", "", 0, "",
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, service.BuildOpenAIEnvironmentAudit(accounts, time.Now()))
}
