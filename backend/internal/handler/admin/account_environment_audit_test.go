package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type environmentAuditAdmin struct {
	service.AdminService
	err error
}

func (a *environmentAuditAdmin) ListAccountsForSchedulerScoreFilter(
	_ context.Context, platform, accountType, status, search string, groupID int64, privacy string,
) ([]service.Account, error) {
	if platform != service.PlatformOpenAI || accountType != service.AccountTypeOAuth ||
		status != "" || search != "" || groupID != 0 || privacy != "" {
		return nil, errors.New("audit scope changed")
	}
	return []service.Account{{ID: 7, Platform: platform, Type: accountType}}, a.err
}

func TestAccountEnvironmentAuditHandler(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "storage_failure"}[failed], func(t *testing.T) {
			admin := &environmentAuditAdmin{}
			if failed {
				admin.err = errors.New("unavailable")
			}
			h := &AccountHandler{adminService: admin}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.GET("/environment-audit", h.EnvironmentAudit)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/environment-audit", nil))
			if failed {
				require.Equal(t, http.StatusInternalServerError, recorder.Code)
				require.NotContains(t, recorder.Body.String(), `"accounts"`)
			} else {
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
				require.Contains(t, recorder.Body.String(), `"account_id":7`)
				require.NotContains(t, recorder.Body.String(), "credentials")
			}
		})
	}
}
