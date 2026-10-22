//go:build unit

package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSystemHandlerManagedModeRejectsLegacyMutations(t *testing.T) {
	h := NewSystemHandler(&systemHandlerUpdateServiceStub{}, nil)
	h.SetManagedDeploymentService(service.NewManagedDeploymentService("/missing/executor.sock"))
	for _, handler := range []gin.HandlerFunc{h.PerformUpdate, h.Rollback, h.RestartService, h.GetRollbackVersions} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		handler(c)
		require.Equal(t, http.StatusConflict, w.Code)
		require.Contains(t, w.Body.String(), "MANAGED_DEPLOYMENT_REQUIRED")
	}
}

func TestSystemHandlerDeploymentRejectsArbitraryFields(t *testing.T) {
	h := NewSystemHandler(nil, nil)
	for _, body := range []string{`{"id":"a","command":"sh"}`, `{"id":"a"}{}`, `not-json`, ``} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		h.PrepareDeployment(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	}
}
