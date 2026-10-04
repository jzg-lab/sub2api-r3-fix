package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type reauthorizationAdminService struct {
	service.AdminService
	account *service.Account
	apply   func(context.Context) (*service.Account, error)
}

func (s *reauthorizationAdminService) GetAccount(context.Context, int64) (*service.Account, error) {
	return s.account, nil
}

func (s *reauthorizationAdminService) ApplyOAuthCredentials(
	ctx context.Context, _ int64, _ time.Time, _ *service.ApplyOAuthCredentialsInput,
) (*service.Account, error) {
	return s.apply(ctx)
}

type reauthorizationTokenCache struct {
	service.GeminiTokenCache
	delete func(context.Context, string) error
}

func (c *reauthorizationTokenCache) DeleteAccessToken(ctx context.Context, key string) error {
	return c.delete(ctx, key)
}

func TestApplyOAuthCredentialsInvalidationAfterClientDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, committed := range []bool{true, false} {
		name := "rolled_back"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			account := &service.Account{
				ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				UpdatedAt: time.Now().UTC(), Status: service.StatusActive,
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			adminSvc := &reauthorizationAdminService{
				account: account,
				apply: func(context.Context) (*service.Account, error) {
					cancel()
					if !committed {
						return nil, service.ErrOAuthReauthorizationStale
					}
					return account, nil
				},
			}
			deletes := 0
			var cleanupCtx context.Context
			cache := &reauthorizationTokenCache{delete: func(ctx context.Context, key string) error {
				deletes++
				cleanupCtx = ctx
				require.NoError(t, ctx.Err(), "client disconnect must not cancel committed cleanup")
				deadline, bounded := ctx.Deadline()
				require.True(t, bounded, "detached cleanup must have a deadline")
				require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
				require.Equal(t, service.OpenAITokenCacheKey(account), key)
				return ctx.Err()
			}}
			handler := &AccountHandler{
				adminService: adminSvc, tokenCacheInvalidator: service.NewCompositeTokenCacheInvalidator(cache),
			}
			router := gin.New()
			router.POST("/accounts/:id/apply-oauth-credentials", handler.ApplyOAuthCredentials)
			body, err := json.Marshal(ApplyOAuthCredentialsRequest{
				Type: service.AccountTypeOAuth, Credentials: map[string]any{"access_token": "test-access"},
				ExpectedUpdatedAt: account.UpdatedAt.Format(time.RFC3339Nano),
			})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/accounts/42/apply-oauth-credentials", bytes.NewReader(body)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if committed {
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Equal(t, 1, deletes)
				require.ErrorIs(t, cleanupCtx.Err(), context.Canceled, "cleanup must release its timer")
			} else {
				require.Equal(t, http.StatusConflict, recorder.Code)
				require.Zero(t, deletes, "failed transactions must not invalidate current credentials")
			}
		})
	}
}
