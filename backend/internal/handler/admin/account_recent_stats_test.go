package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type recentAccountUsageRepo struct{ service.UsageLogRepository }

func (recentAccountUsageRepo) GetAccountWindowStatsBatch(context.Context, []int64, time.Time) (map[int64]*usagestats.AccountStats, error) {
	return map[int64]*usagestats.AccountStats{}, nil
}

type recentAccountAdminCache struct {
	service.GatewayCache
	fail bool
}

func (*recentAccountAdminCache) RecordAccountAttempt(context.Context, int64, service.AccountAttemptObservation, time.Time) error {
	return nil
}
func (c *recentAccountAdminCache) GetAccountRecentStats(_ context.Context, ids []int64, _ time.Time) (map[int64]service.AccountRecentStats, error) {
	if c.fail {
		return nil, errors.New("redis unavailable")
	}
	return map[int64]service.AccountRecentStats{ids[0]: {Successes: 3, Failures: 1}}, nil
}

func TestAccountBatchStatsIncludesRecentSamplesAndUnavailableState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage := service.NewAccountUsageService(nil, recentAccountUsageRepo{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	for i, fail := range []bool{false, true} {
		h := &AccountHandler{accountUsageService: usage, recentStatsCache: &recentAccountAdminCache{fail: fail}}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		id := 987100 + i*2
		c.Request = httptest.NewRequest("POST", "/api/v1/admin/accounts/today-stats/batch", strings.NewReader(fmt.Sprintf(`{"account_ids":[%d,%d]}`, id, id+1)))
		c.Request.Header.Set("Content-Type", "application/json")
		h.GetBatchTodayStats(c)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		data := gjson.Get(rec.Body.String(), "data")
		require.Equal(t, fail, data.Get("recent_stats_unavailable").Bool())
		if !fail {
			require.Equal(t, 0.75, data.Get(fmt.Sprintf("recent_stats.%d.success_rate", id)).Float())
			require.Equal(t, gjson.Null, data.Get(fmt.Sprintf("recent_stats.%d.success_rate", id+1)).Type)
		} else {
			require.Equal(t, "{}", data.Get("recent_stats").Raw)
		}
	}
}
