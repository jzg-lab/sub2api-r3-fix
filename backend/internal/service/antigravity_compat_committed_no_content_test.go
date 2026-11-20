package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// runCompatStreamToEnd drives the compat stream handler with an upstream that
// sends nothing and closes after closeAfter.
func runCompatStreamToEnd(t *testing.T, keepaliveInterval, maxWait, closeAfter time.Duration) error {
	t.Helper()
	svc := newAntigravityCompatService(config.GatewayConfig{
		MaxLineSize: defaultMaxLineSize, StreamDataIntervalTimeout: 30, StreamKeepaliveInterval: 0,
	}, nil)
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/responses", nil)
	reader, pipeWriter := io.Pipe()
	defer func() { _ = reader.Close() }()
	resp := &http.Response{StatusCode: http.StatusOK, Body: reader}
	done := make(chan error, 1)
	go func() {
		_, err := svc.handleAntigravityCompatStreamWithKeepaliveInterval(
			c, resp, time.Now(), "gemini-4-hydrogen-high",
			newAntigravityResponsesStreamAdapter("gemini-4-hydrogen-high"), "test", keepaliveInterval, maxWait,
		)
		done <- err
	}()
	time.Sleep(closeAfter)
	_ = pipeWriter.Close()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("compat stream handler did not return")
		return nil
	}
}

func TestAntigravityCompatEmptyStreamAfterKeepaliveIsCommittedNoContent(t *testing.T) {
	err := runCompatStreamToEnd(t, 10*time.Millisecond, time.Second, 50*time.Millisecond)
	require.Error(t, err)
	require.True(t, IsAntigravityCompatCommittedNoContent(err), "got %v", err)
}

func TestAntigravityCompatPreContentTimeoutAfterKeepaliveIsCommittedNoContent(t *testing.T) {
	err := runCompatStreamToEnd(t, 10*time.Millisecond, 40*time.Millisecond, 200*time.Millisecond)
	require.Error(t, err)
	require.ErrorContains(t, err, "pre-content stream timeout")
	require.True(t, IsAntigravityCompatCommittedNoContent(err), "got %v", err)
}

func TestAntigravityCompatEmptyStreamBeforeKeepaliveStaysFailover(t *testing.T) {
	err := runCompatStreamToEnd(t, time.Second, 2*time.Second, 10*time.Millisecond)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "got %v", err)
	require.False(t, IsAntigravityCompatCommittedNoContent(err))
}

func TestAntigravityCompatNoContentAfterQueueHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adapter func() antigravityCompatStreamAdapter
		frame   string
	}{
		{"responses", func() antigravityCompatStreamAdapter {
			return newAntigravityResponsesStreamAdapter("gemini-4-argon-high")
		}, "event: error"},
		{"chat", func() antigravityCompatStreamAdapter {
			return newAntigravityChatStreamAdapter("gemini-4-argon-high", false)
		}, `"upstream_error"`},
	} {
		for _, reason := range []string{"empty_stream", "stream_timeout"} {
			t.Run(tc.name+"/"+reason, func(t *testing.T) {
				svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
				c, out := newAntigravityCompatContext(http.MethodPost, "/", nil)
				n, err := c.Writer.Write([]byte("data: {\"type\": \"ping\"}\n\n"))
				require.NoError(t, err)
				RecordGatewayStreamHeartbeat(c, n)
				c.Writer.Flush()
				now := time.Now()
				streamStart := now
				if reason == "stream_timeout" {
					streamStart = now.Add(-2 * time.Minute)
				}
				resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}
				_, err = svc.handleAntigravityCompatStreamWithTiming(c, resp, streamStart, streamStart,
					"gemini-4-argon-high", tc.adapter(), "test", time.Hour, time.Minute)
				require.True(t, IsAntigravityCompatCommittedNoContent(err), "%v", err)
				require.Contains(t, out.Body.String(), tc.frame)
				require.Contains(t, out.Body.String(), reason)
			})
		}
	}
}

func TestReleaseStickySessionAfterCommittedNoContent(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10)
	committed := &antigravityCompatCommittedNoContentError{msg: "empty Antigravity compatibility stream after keepalive"}

	t.Run("unbinds the session from the account that failed", func(t *testing.T) {
		cache := &stubGatewayCache{sessionBindings: map[string]int64{"sess": 5}}
		svc := &GatewayService{cache: cache}
		require.True(t, svc.ReleaseStickySessionAfterCommittedNoContent(ctx, &groupID, "sess", 5, committed))
		_, bound := cache.sessionBindings["sess"]
		require.False(t, bound)
	})

	t.Run("keeps a binding that already moved to another account", func(t *testing.T) {
		cache := &stubGatewayCache{sessionBindings: map[string]int64{"sess": 6}}
		svc := &GatewayService{cache: cache}
		require.False(t, svc.ReleaseStickySessionAfterCommittedNoContent(ctx, &groupID, "sess", 5, committed))
		require.Equal(t, int64(6), cache.sessionBindings["sess"])
	})

	t.Run("ignores other errors", func(t *testing.T) {
		cache := &stubGatewayCache{sessionBindings: map[string]int64{"sess": 5}}
		svc := &GatewayService{cache: cache}
		require.False(t, svc.ReleaseStickySessionAfterCommittedNoContent(ctx, &groupID, "sess", 5, errors.New("stream data interval timeout")))
		require.Equal(t, int64(5), cache.sessionBindings["sess"])
	})

	t.Run("no session hash is a no-op", func(t *testing.T) {
		cache := &stubGatewayCache{sessionBindings: map[string]int64{"sess": 5}}
		svc := &GatewayService{cache: cache}
		require.False(t, svc.ReleaseStickySessionAfterCommittedNoContent(ctx, &groupID, "", 5, committed))
		require.Equal(t, int64(5), cache.sessionBindings["sess"])
	})
}
