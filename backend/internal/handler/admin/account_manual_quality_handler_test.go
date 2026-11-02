package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type qualityHandlerRepo struct {
	service.AccountRepository
	calls         int
	panicOnLookup bool
}

func (r *qualityHandlerRepo) GetByID(context.Context, int64) (*service.Account, error) {
	r.calls++
	if r.panicOnLookup {
		panic("test panic")
	}
	return nil, errors.New("account unavailable")
}

func TestManualQualityHandlerValidatesBeforeExecution(t *testing.T) {
	for _, body := range []string{`{`, `{"model":"test","kind":"invalid","prompt":"test"}`, `{"model":"","kind":"candy","prompt":"test"}`, `{"model":"test","kind":"candy","prompt":""}`} {
		repo := &qualityHandlerRepo{}
		h := &AccountHandler{accountTestService: service.NewAccountTestService(repo, nil, nil, nil, nil, nil, nil, nil)}
		router := gin.New()
		router.POST("/accounts/:id/manual-quality", h.RunManualQuality)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/accounts/1/manual-quality", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		require.Equal(t, 400, rec.Code)
		require.Zero(t, repo.calls)
	}
}

func TestManualQualityHandlerSSEReportsFailureAndContainsWorkerPanics(t *testing.T) {
	for _, panicOnLookup := range []bool{false, true} {
		repo := &qualityHandlerRepo{panicOnLookup: panicOnLookup}
		h := &AccountHandler{accountTestService: service.NewAccountTestService(repo, nil, nil, nil, nil, nil, nil, nil)}
		router := gin.New()
		router.POST("/accounts/:id/manual-quality", h.RunManualQuality)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/accounts/1/manual-quality", strings.NewReader(`{"model":"test","kind":"candy","prompt":"test"}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		require.Equal(t, 200, rec.Code)
		require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
		require.Contains(t, rec.Body.String(), `"saved":false`)
		require.Contains(t, rec.Body.String(), `"result":null`)
		require.NotContains(t, rec.Body.String(), "test panic")
	}
}
