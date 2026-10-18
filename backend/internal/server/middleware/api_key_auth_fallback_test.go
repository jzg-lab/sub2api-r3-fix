//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthBindsFallbackAccessAtBothIngresses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, google := range []bool{false, true} {
		for _, mode := range []string{config.RunModeSimple, config.RunModeStandard} {
			name := mode + map[bool]string{false: "_standard_ingress", true: "_google_ingress"}[google]
			t.Run(name, func(t *testing.T) {
				groupID := int64(1)
				key := &service.APIKey{
					ID: 10, UserID: 7, Key: "test-key", Status: service.StatusActive,
					User: &service.User{
						ID: 7, Status: service.StatusActive, Balance: 100,
						RestrictPublicGroups: true, AllowedGroups: []int64{1, 2},
					},
					GroupID: &groupID,
					Group: &service.Group{
						ID: groupID, Status: service.StatusActive, Hydrated: true,
						SubscriptionType: service.SubscriptionTypeStandard,
					},
				}
				repo := &stubApiKeyRepo{getByKey: func(context.Context, string) (*service.APIKey, error) {
					return key, nil
				}}
				cfg := &config.Config{RunMode: mode}
				svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
				router := gin.New()
				if google {
					router.Use(APIKeyAuthGoogle(svc, cfg))
				} else {
					router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, nil, cfg)))
				}
				called := false
				router.GET("/t", func(c *gin.Context) {
					called = true
					ctx := c.Request.Context()
					require.Equal(t, int64(7), ctx.Value(ctxkey.UserID))
					require.True(t, service.CanUseGatewayFallbackGroup(ctx, &service.Group{ID: 2, Status: service.StatusActive, IsExclusive: true}))
					require.False(t, service.CanUseGatewayFallbackGroup(ctx, &service.Group{ID: 3, Status: service.StatusActive}))
					c.Status(http.StatusOK)
				})
				req := httptest.NewRequest(http.MethodGet, "/t", nil)
				if google {
					req.Header.Set("x-goog-api-key", key.Key)
				} else {
					req.Header.Set("Authorization", "Bearer "+key.Key)
				}
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code)
				require.True(t, called)
			})
		}
	}
}
