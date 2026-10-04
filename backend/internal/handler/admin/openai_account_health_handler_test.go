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

type reenableHandlerStore struct {
	service.OpenAIDowngradeProbeStore
	state     *service.OpenAIDowngradeProbeState
	commitErr error
	commits   int
	observed  *service.OpenAIAccountReenableMutation
}

func (s *reenableHandlerStore) GetOpenAIDowngradeState(context.Context, int64) (*service.OpenAIDowngradeProbeState, error) {
	return s.state, nil
}

func (s *reenableHandlerStore) CanRunOpenAIDowngradeProbe(context.Context, int64) (bool, error) {
	return true, nil
}

func (s *reenableHandlerStore) CommitOpenAIAccountReenable(_ context.Context, mutation *service.OpenAIAccountReenableMutation) (bool, error) {
	s.commits++
	s.observed = mutation
	return mutation.Unpause, s.commitErr
}

type reenableHandlerAccountRepo struct {
	service.AccountRepository
	account *service.Account
}

func (r *reenableHandlerAccountRepo) GetByID(context.Context, int64) (*service.Account, error) {
	return r.account, nil
}

func TestOpenAIReenableHandlerErrorContract(t *testing.T) {
	for _, tc := range []struct {
		name       string
		commitErr  error
		wantStatus int
		wantReason string
	}{
		{"stale", service.ErrOpenAIProbeStale, http.StatusConflict, "OPENAI_PROBE_GENERATION_CHANGED"},
		{"paused", service.ErrOpenAIReenablePaused, http.StatusConflict, "OPENAI_REENABLE_PAUSED"},
		{"blocked", service.ErrOpenAIReenableBlocked, http.StatusConflict, "OPENAI_REENABLE_BLOCKED"},
		{"not_dead", service.ErrOpenAIReenableNotDead, http.StatusBadRequest, "OPENAI_REENABLE_NOT_DEAD"},
		{"persistence_failure", errors.New("injected storage failure"), http.StatusInternalServerError, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &service.OpenAIDowngradeProbeState{
				AccountID: 7, State: service.OpenAIDowngradeStatePendingReplace,
				UpdatedAt: time.Now().Add(-time.Hour),
			}
			before := *state
			store := &reenableHandlerStore{state: state, commitErr: tc.commitErr}
			account := &service.Account{
				ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Status: service.StatusActive, UpdatedAt: state.UpdatedAt,
			}
			runner := service.NewOpenAIDowngradeProbeRunner(store,
				&reenableHandlerAccountRepo{account: account}, nil, nil, nil, nil)
			handler := NewOpenAIProbeHealthHandler(runner)
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.POST("/api/v1/admin/openai/accounts/:id/reenable", handler.ReenableAccount)
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost,
				"/api/v1/admin/openai/accounts/7/reenable?unpause=true", nil))

			require.Equal(t, tc.wantStatus, recorder.Code)
			if tc.wantReason != "" {
				require.Contains(t, recorder.Body.String(), tc.wantReason)
			}
			require.NotContains(t, recorder.Body.String(), `"probe_queued":true`)
			require.Equal(t, 1, store.commits)
			require.True(t, store.observed.Unpause)
			require.Equal(t, before, *state, "failed HTTP request must not publish the candidate state")
		})
	}
}

func TestOpenAIReenableHandlerSuccessAndInvalidID(t *testing.T) {
	for _, tc := range []struct {
		path       string
		wantStatus int
		unpause    bool
	}{
		{"/7/reenable", http.StatusOK, false},
		{"/7/reenable?unpause=true", http.StatusOK, true},
		{"/7/reenable?unpause=1", http.StatusOK, true},
		{"/7/reenable?unpause=false", http.StatusOK, false},
		{"/0/reenable", http.StatusBadRequest, false},
		{"/-1/reenable", http.StatusBadRequest, false},
		{"/bad/reenable", http.StatusBadRequest, false},
		{"/9223372036854775808/reenable", http.StatusBadRequest, false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			store := &reenableHandlerStore{state: &service.OpenAIDowngradeProbeState{
				AccountID: 7, State: service.OpenAIDowngradeStatePendingReplace,
			}}
			runner := service.NewOpenAIDowngradeProbeRunner(store,
				&reenableHandlerAccountRepo{account: &service.Account{
					ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
					Status: service.StatusActive,
				}}, nil, nil, nil, nil)
			handler := NewOpenAIProbeHealthHandler(runner)
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.POST("/api/v1/admin/openai/accounts/:id/reenable", handler.ReenableAccount)
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost,
				"/api/v1/admin/openai/accounts"+tc.path, nil))

			require.Equal(t, tc.wantStatus, recorder.Code)
			if tc.wantStatus == http.StatusOK {
				require.Equal(t, 1, store.commits)
				require.Equal(t, tc.unpause, store.observed.Unpause)
				require.Contains(t, recorder.Body.String(), `"probe_queued":true`)
			} else {
				require.Zero(t, store.commits)
			}
		})
	}
}
