package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type rescueTerminationHandlerRepo struct {
	service.AccountRepository
	service.OpenAIRescueLifecycleRepository
	stopped bool
	err     error
	calls   int
}

func (r *rescueTerminationHandlerRepo) TerminateOpenAIRescue(context.Context, int64, time.Time) (bool, error) {
	r.calls++
	return r.stopped, r.err
}

func TestOpenAIRescueTerminationHandler(t *testing.T) {
	for _, tc := range []struct {
		id      string
		stopped bool
		err     error
		status  int
	}{
		{"7", true, nil, http.StatusOK},
		{"7", false, nil, http.StatusOK},
		{"7", false, service.ErrAccountNotFound, http.StatusNotFound},
		{"7", false, errors.New("storage failed"), http.StatusInternalServerError},
		{"0", false, nil, http.StatusBadRequest},
		{"-1", false, nil, http.StatusBadRequest},
		{"bad", false, nil, http.StatusBadRequest},
	} {
		t.Run(tc.id+http.StatusText(tc.status), func(t *testing.T) {
			repo := &rescueTerminationHandlerRepo{stopped: tc.stopped, err: tc.err}
			runner := service.NewOpenAIDowngradeProbeRunner(nil, repo, nil, nil, nil, nil)
			runner.SetRescueLane(service.NewOpenAIRescueLane(repo, nil, nil, nil))
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.POST("/accounts/:id/rescue/terminate", NewOpenAIProbeHealthHandler(runner).TerminateRescueAccount)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/accounts/"+tc.id+"/rescue/terminate", nil))
			require.Equal(t, tc.status, recorder.Code)
			if tc.status == http.StatusBadRequest {
				require.Zero(t, repo.calls)
			} else {
				require.Equal(t, 1, repo.calls)
			}
			if tc.status == http.StatusOK {
				require.Contains(t, recorder.Body.String(), `"account_id":7`)
			}
		})
	}
}
