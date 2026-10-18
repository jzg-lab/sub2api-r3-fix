package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/antibypass"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type antiBypassPolicyStub struct {
	antiBypassSettingsStub
	config antibypass.Config
}

func (s *antiBypassPolicyStub) GetAntiBypassSettings(context.Context) (bool, antibypass.Config, error) {
	return s.enabled, s.config, s.err
}

func TestAntiBypassHTTPUsesConfiguredBudget(t *testing.T) {
	store := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: store.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cfg := antibypass.DefaultConfig()
	cfg.RPMLimit = 2
	var called bool
	router := antiBypassTestRouter(&antiBypassPolicyStub{antiBypassSettingsStub{enabled: true}, cfg}, client, &called)
	for _, status := range []int{200, 200, 429} {
		called = false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		require.Equal(t, status, rec.Code)
		require.Equal(t, status == 200, called)
	}
}

func TestAntiBypassPolicyRefreshNeverUsesStaleAfterError(t *testing.T) {
	settings := &antiBypassPolicyStub{antiBypassSettingsStub{enabled: true}, antibypass.DefaultConfig()}
	cache := &antiBypassSwitchCache{settings: settings}
	enabled, cfg, err := cache.Snapshot(context.Background())
	require.NoError(t, err)
	require.True(t, enabled)
	require.Equal(t, 32, cfg.MaxConcurrent)
	settings.config.MaxConcurrent = 64
	cache.expires = time.Time{}
	_, cfg, err = cache.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, 64, cfg.MaxConcurrent)
	settings.err = errors.New("settings unavailable")
	cache.expires = time.Time{}
	enabled, _, err = cache.Snapshot(context.Background())
	require.Error(t, err)
	require.False(t, enabled)
}

func TestAntiBypassLiveFrameBudgetChangeKeepsExistingLeases(t *testing.T) {
	session, _ := newBusinessFrameSession(t)
	cfg := antibypass.DefaultConfig()
	cfg.MaxConcurrent = 1
	first, err := session.InspectWithConfig(context.Background(), []byte(`{"type":"response.create","input":"one"}`), true, cfg)
	require.NoError(t, err)
	require.Empty(t, first.Reason)
	second, err := session.InspectWithConfig(context.Background(), []byte(`{"type":"response.create","input":"two"}`), true, cfg)
	require.NoError(t, err)
	require.Equal(t, antibypass.ReasonConcurrency, second.Reason)
	cfg.MaxConcurrent = 2
	third, err := session.InspectWithConfig(context.Background(), []byte(`{"type":"response.create","input":"three"}`), true, cfg)
	require.NoError(t, err)
	require.Empty(t, third.Reason)
	require.Len(t, session.leases, 2)
	session.Finish(1)
	session.Finish(3)
	require.Empty(t, session.leases)
}
