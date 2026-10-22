//go:build unit

package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestManagedDeploymentRoutesRequireSensitiveOperationGate(t *testing.T) {
	router := gin.New()
	h := &handler.Handlers{Admin: &handler.AdminHandlers{System: admin.NewSystemHandler(nil, nil)}}
	gateCalls := 0
	gate := middleware.StepUpAuthMiddleware(func(c *gin.Context) {
		gateCalls++
		c.AbortWithStatus(http.StatusForbidden)
	})
	registerSystemRoutes(router.Group("/admin"), h, gate)
	for _, action := range []string{"prepare", "activate", "rollback"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/admin/system/deployment/"+action, strings.NewReader(`{"id":"job-1"}`)))
		require.Equal(t, http.StatusForbidden, w.Code)
	}
	require.Equal(t, 3, gateCalls)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/system/deployment", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 3, gateCalls, "read-only status does not consume a sensitive-operation grant")
}
