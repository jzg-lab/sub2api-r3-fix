package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestClassifyOpsPluginUnavailableUsesFinalTypedEvent(t *testing.T) {
	local := &service.OpsUpstreamErrorEvent{Kind: service.OpsUpstreamErrorKindPluginUnavailable}
	upstream := &service.OpsUpstreamErrorEvent{Kind: "http_error", UpstreamStatusCode: http.StatusBadGateway}
	for _, tc := range []struct {
		name   string
		events []*service.OpsUpstreamErrorEvent
		local  bool
	}{
		{"local after upstream", []*service.OpsUpstreamErrorEvent{upstream, local, nil}, true},
		{"upstream after local", []*service.OpsUpstreamErrorEvent{local, upstream}, false},
		{"message alone", []*service.OpsUpstreamErrorEvent{{Kind: "request_error", Message: "plugin unavailable"}}, false},
		{"contradictory HTTP status", []*service.OpsUpstreamErrorEvent{{Kind: service.OpsUpstreamErrorKindPluginUnavailable, UpstreamStatusCode: 502}}, false},
		{"nil event", []*service.OpsUpstreamErrorEvent{nil}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set(service.OpsUpstreamErrorsKey, tc.events)
			phase, limited, owner, source := classifyOpsErrorLog(c, "upstream_error", "Upstream request failed", "", http.StatusBadGateway)
			require.False(t, limited, "plugin failures must remain in the failure SLA")
			if tc.local {
				require.Equal(t, "internal", phase)
				require.Equal(t, "platform", owner)
				require.Equal(t, "gateway", source)
			} else {
				require.Equal(t, "upstream", phase)
				require.Equal(t, "provider", owner)
				require.Equal(t, "upstream_http", source)
			}
		})
	}
}

func TestOpsErrorLoggerPluginUnavailableHTTPAndSSE(t *testing.T) {
	for _, mode := range []string{"http", "sse", "recovered"} {
		t.Run(mode, func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 1)
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.POST("/v1/responses", func(c *gin.Context) {
				c.Set(service.OpsUpstreamStatusCodeKey, http.StatusBadGateway)
				c.Set(service.OpsUpstreamErrorDetailKey, "stale prior response")
				c.Set(service.OpsUpstreamErrorsKey, []*service.OpsUpstreamErrorEvent{
					{Kind: "http_error", UpstreamStatusCode: http.StatusBadGateway, Message: "prior upstream failure"},
					{Kind: service.OpsUpstreamErrorKindPluginUnavailable, Message: "local plugin health timeout"},
				})
				switch mode {
				case "sse":
					service.MarkOpsStreamFailure(c, "upstream_error", "", "Upstream request failed", http.StatusBadGateway)
					c.Data(http.StatusOK, "text/event-stream", []byte("event: error\ndata: {}\n\n"))
				case "recovered":
					c.JSON(http.StatusOK, gin.H{"status": "completed"})
				default:
					c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error", "message": "Upstream request failed"}})
				}
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
			require.Equal(t, int64(1), OpsErrorLogQueueLength())
			job := <-opsErrorLogQueue
			require.Equal(t, "internal", job.entry.ErrorPhase)
			require.Equal(t, "platform", job.entry.ErrorOwner)
			require.Equal(t, "gateway", job.entry.ErrorSource)
			require.False(t, job.entry.IsBusinessLimited)
			require.Nil(t, job.entry.UpstreamStatusCode, "local failure must not borrow a prior HTTP status")
			require.Nil(t, job.entry.UpstreamErrorDetail)
			require.Equal(t, "local plugin health timeout", *job.entry.UpstreamErrorMessage)
			require.NotNil(t, job.entry.UpstreamErrorsJSON)
			events, err := service.ParseOpsUpstreamErrors(*job.entry.UpstreamErrorsJSON)
			require.NoError(t, err)
			require.Len(t, events, 2, "retain the original upstream attempt")
			require.Equal(t, http.StatusBadGateway, events[0].UpstreamStatusCode)
			if mode == "recovered" {
				require.Equal(t, http.StatusOK, job.entry.StatusCode)
				require.Contains(t, job.entry.ErrorMessage, "Recovered local plugin failure")
			} else {
				require.Equal(t, http.StatusBadGateway, job.entry.StatusCode)
			}
		})
	}
}

func TestOpsErrorLoggerPluginUnavailableWebSocketTurnIsolation(t *testing.T) {
	for _, localFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "local first", false: "local last"}[localFirst], func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 2)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
			for turn := 1; turn <= 2; turn++ {
				service.BeginOpsStreamTurn(c, turn)
				event := &service.OpsUpstreamErrorEvent{Kind: "http_error", UpstreamStatusCode: http.StatusBadGateway}
				if (turn == 1) == localFirst {
					event = &service.OpsUpstreamErrorEvent{
						Kind: service.OpsUpstreamErrorKindPluginUnavailable, Message: "local plugin health timeout",
					}
					c.Set(service.OpsUpstreamStatusCodeKey, http.StatusBadGateway)
					c.Set(service.OpsUpstreamErrorDetailKey, "stale prior response")
				}
				c.Set(service.OpsUpstreamErrorsKey, []*service.OpsUpstreamErrorEvent{event})
				service.MarkOpsStreamFailure(c, "upstream_error", "", "request failed", http.StatusBadGateway)
			}
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			logOpsStreamError(c, ops, http.StatusSwitchingProtocols)
			require.Equal(t, int64(2), OpsErrorLogQueueLength())
			for turn := 1; turn <= 2; turn++ {
				job := <-opsErrorLogQueue
				require.False(t, job.entry.IsBusinessLimited)
				if (turn == 1) == localFirst {
					require.Equal(t, "platform", job.entry.ErrorOwner)
					require.Equal(t, "internal", job.entry.ErrorPhase)
					require.Equal(t, "gateway", job.entry.ErrorSource)
					require.Nil(t, job.entry.UpstreamStatusCode)
					require.Nil(t, job.entry.UpstreamErrorDetail)
					require.Equal(t, "local plugin health timeout", *job.entry.UpstreamErrorMessage)
				} else {
					require.Equal(t, "provider", job.entry.ErrorOwner)
					require.Equal(t, "upstream", job.entry.ErrorPhase)
					require.Equal(t, "upstream_http", job.entry.ErrorSource)
				}
			}
		})
	}
}
