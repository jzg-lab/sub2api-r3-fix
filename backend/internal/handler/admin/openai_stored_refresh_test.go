//go:build unit

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type storedOpenAIAdminRepo struct {
	service.AccountRepository
	current *service.Account
}

func (r *storedOpenAIAdminRepo) GetByID(context.Context, int64) (*service.Account, error) {
	copy := *r.current
	return &copy, nil
}

type storedOpenAIAdminService struct {
	service.AdminService
	selected     *service.Account
	privacyCalls int
}

func (s *storedOpenAIAdminService) GetAccount(context.Context, int64) (*service.Account, error) {
	return s.selected, nil
}

func (s *storedOpenAIAdminService) EnsureOpenAIPrivacy(context.Context, *service.Account) string {
	s.privacyCalls++
	return ""
}

func TestStoredOpenAIAdminRefreshConsumersReuseDurableWinner(t *testing.T) {
	for _, endpoint := range []string{"single_and_batch", "oauth_endpoint"} {
		t.Run(endpoint, func(t *testing.T) {
			selected := &service.Account{
				ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Status: service.StatusActive, Credentials: make(map[string]any),
			}
			selected.Credentials["access_token"] = "fixture-before"
			selected.Credentials["refresh_token"] = "fixture-before"
			current := *selected
			current.Credentials = make(map[string]any)
			current.Credentials["access_token"] = "fixture-after"
			current.Credentials["refresh_token"] = "fixture-after"
			current.Credentials["expires_at"] = time.Now().Add(time.Hour).Unix()
			repo := &storedOpenAIAdminRepo{current: &current}
			adminService := &storedOpenAIAdminService{selected: selected}
			oauth := service.ProvideOpenAIOAuthService(nil, nil, nil, nil, nil,
				service.NewOAuthRefreshAPI(repo, nil))
			// Nil upstream and embedded write methods fail if either caller
			// exchanges again or persists outside the coordinator.
			if endpoint == "single_and_batch" {
				handler := &AccountHandler{openaiOAuthService: oauth, adminService: adminService}
				got, warning, err := handler.refreshSingleAccount(t.Context(), selected)
				require.NoError(t, err)
				require.Empty(t, warning)
				require.Equal(t, current.Credentials, got.Credentials)
				require.Equal(t, 1, adminService.privacyCalls)
			} else {
				handler := &OpenAIOAuthHandler{openaiOAuthService: oauth, adminService: adminService}
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/openai/accounts/42/refresh", nil)
				ctx.Params = gin.Params{{Key: "id", Value: "42"}}
				handler.RefreshAccountToken(ctx)
				require.Equal(t, http.StatusOK, recorder.Code)
			}
		})
	}
}
