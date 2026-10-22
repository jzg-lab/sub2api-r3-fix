package admin

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SystemHandler) SetManagedDeploymentService(s *service.ManagedDeploymentService) {
	h.deploymentSvc = s
}

func (h *SystemHandler) GetDeployment(c *gin.Context) {
	result, err := h.deploymentSvc.Snapshot(c.Request.Context(), c.Query("force") == "true")
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, service.ErrManagedDeploymentUnavailable.Error())
		return
	}
	response.Success(c, result)
}

func (h *SystemHandler) PrepareDeployment(c *gin.Context)  { h.deploymentCommand(c, "prepare") }
func (h *SystemHandler) ActivateDeployment(c *gin.Context) { h.deploymentCommand(c, "activate") }
func (h *SystemHandler) RollbackDeployment(c *gin.Context) { h.deploymentCommand(c, "rollback") }

func (h *SystemHandler) deploymentCommand(c *gin.Context, action string) {
	if c.Request.Body == nil {
		response.Error(c, http.StatusBadRequest, "invalid deployment request")
		return
	}
	var request struct {
		ID string `json:"id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid deployment request")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		response.Error(c, http.StatusBadRequest, "invalid deployment request")
		return
	}
	result, err := h.deploymentSvc.Command(c.Request.Context(), action, request.ID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *SystemHandler) rejectUnmanagedOperation(c *gin.Context) bool {
	if h.deploymentSvc.Enabled() {
		response.ErrorFrom(c, service.ErrManagedDeploymentRequired)
		return true
	}
	return false
}
