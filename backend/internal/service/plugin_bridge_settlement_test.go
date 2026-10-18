package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPluginHTTPBridgeWaitsForSettlement(t *testing.T) {
	reader, writer := io.Pipe()
	settled := make(chan struct{})
	body := &pluginResponseBody{reader: reader, cancel: func() {}, done: func() {}}
	go func() {
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		// Sending End/settlement may need another Read after the terminal.
		_, _ = io.WriteString(writer, ": trailer\n\n")
		close(settled)
		_ = writer.Close()
	}()
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body,
		}},
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	payload := []byte(`{"type":"response.create","model":"fixture-model","stream":true,"input":"hi"}`)
	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c,
		&Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1},
		"fixture-token", payload, len(payload), "fixture-model", "", "", "", "", 1,
		func([]byte) error { return nil })
	require.NoError(t, err)
	require.NotNil(t, result)
	select {
	case <-settled:
	default:
		t.Fatal("bridge returned before the plugin settled")
	}
}

func TestPluginSettlementDrainBounds(t *testing.T) {
	require.NoError(t, drainPluginResponseSettlement(io.NopCloser(strings.NewReader(": tail\n\n")), time.Second))
	require.ErrorContains(t, drainPluginResponseSettlement(io.NopCloser(strings.NewReader(strings.Repeat("x", 1<<20))), time.Second), "limit")
	reader, writer := io.Pipe()
	defer writer.Close()
	require.ErrorIs(t, drainPluginResponseSettlement(reader, 10*time.Millisecond), io.ErrClosedPipe)
	reader, writer = io.Pipe()
	failure := errors.New("fixture settlement failure")
	_ = writer.CloseWithError(failure)
	require.ErrorIs(t, drainPluginResponseSettlement(reader, time.Second), failure)
	_ = reader.Close()
}
