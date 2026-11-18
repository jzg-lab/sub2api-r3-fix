//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type routeWriteKeyRepo struct {
	service.APIKeyRepository
	key    *service.APIKey
	fields service.APIKeyUpdateFields
}

func (r *routeWriteKeyRepo) Create(_ context.Context, key *service.APIKey) error {
	key.ID = 7
	r.key = key
	return nil
}

func (r *routeWriteKeyRepo) GetByID(context.Context, int64) (*service.APIKey, error) {
	copy := *r.key
	return &copy, nil
}

func (r *routeWriteKeyRepo) Update(_ context.Context, key *service.APIKey, fields service.APIKeyUpdateFields) error {
	r.key, r.fields = key, fields
	return nil
}

type routeWriteUserRepo struct{ service.UserRepository }

func (routeWriteUserRepo) GetByID(context.Context, int64) (*service.User, error) {
	return &service.User{ID: 42, Status: service.StatusActive}, nil
}

type routeWriteGroupRepo struct{ service.GroupRepository }

func (routeWriteGroupRepo) GetByID(_ context.Context, id int64) (*service.Group, error) {
	return &service.Group{ID: id, Status: service.StatusActive, Platform: service.PlatformOpenAI}, nil
}

func TestAPIKeyHTTPWritesPreserveOrderedSmartRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &routeWriteKeyRepo{}
	h := NewAPIKeyHandler(service.NewAPIKeyService(repo, routeWriteUserRepo{}, routeWriteGroupRepo{}, nil, nil, nil, &config.Config{}))
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42}) })
	router.POST("/keys", h.Create)
	router.PUT("/keys/:id", h.Update)
	for _, test := range []struct {
		method, path, body string
		want               []int64
	}{
		{http.MethodPost, "/keys", `{"name":"routes","group_id":2,"group_ids":[2,1]}`, []int64{2, 1}},
		{http.MethodPut, "/keys/7", `{"name":"reordered","group_id":1,"group_ids":[1,2]}`, []int64{1, 2}},
	} {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Equal(t, test.want, repo.key.RouteGroupIDs)
		require.Equal(t, test.want[0], *repo.key.GroupID)
	}
	require.True(t, repo.fields.GroupRoutes)
}
