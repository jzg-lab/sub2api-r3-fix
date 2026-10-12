//go:build unit

package handler

import (
	"net/http"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSmartRoutesCheckAllowlistForEachCandidate(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-test"}`,
		`{"model":"gpt-test","model":"blocked"}`,
		`{"model":"gpt-test","MODEL":"blocked"}`,
	} {
		t.Run(body, func(t *testing.T) {
			h, key, cache := smartRoutingFixture(t)
			key.Group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"primary-only"}}
			backup, err := h.apiKeyService.APIKeyForRoute(t.Context(), key, 2)
			require.NoError(t, err)
			backup.Group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-test"}}
			c, rec := smartRoutingContext(key, body)
			c.Request.Header.Set("Content-Type", "application/json")
			calls := 0
			h.WithSmartRoutes(func(c *gin.Context) {
				calls++
				routed, _ := middleware2.GetAPIKeyFromContext(c)
				require.EqualValues(t, 2, *routed.GroupID)
				c.Status(http.StatusOK)
			}, nil, nil)(c)
			if body == `{"model":"gpt-test"}` {
				require.Equal(t, 1, calls)
				require.Equal(t, http.StatusOK, rec.Code)
			} else {
				require.Zero(t, calls)
				require.Equal(t, http.StatusNotFound, rec.Code)
			}
			require.Empty(t, cache.failed, "policy rejection is not an upstream failure")
		})
	}
}
