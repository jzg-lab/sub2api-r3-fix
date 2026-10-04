package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthAuditOmitsLiveSessionHandles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repository := &auditCaptureRepository{}
	auditService := service.NewAuditLogService(repository, nil)
	auditService.Start()
	t.Cleanup(auditService.Stop)

	router := gin.New()
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	routes := []string{
		"/api/v1/admin/openai/launch-auth-browser",
		"/api/v1/admin/openai/exchange-code",
		"/api/v1/admin/openai/create-from-oauth",
	}
	for _, route := range routes {
		router.POST(route, func(c *gin.Context) {
			var body map[string]any
			require.NoError(t, c.ShouldBindJSON(&body))
			require.Equal(t, "session-canary", body["session_id"],
				"audit must not alter the actual handler input")
			c.Status(http.StatusOK)
		})
		request := httptest.NewRequest(http.MethodPost, route,
			bytes.NewBufferString(`{"session_id":"session-canary","state":"state-canary"}`))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code)
	}
	auditService.Stop()
	repository.mu.Lock()
	defer repository.mu.Unlock()
	require.Len(t, repository.logs, len(routes))
	for _, entry := range repository.logs {
		require.Equal(t, "<credential-bearing body omitted>", entry.RequestBody)
		require.Equal(t, http.StatusOK, entry.StatusCode)
		require.NotEmpty(t, entry.Action)
	}
}
