package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIResponsesWebSocketRechecksCyberStoreBetweenTurns(t *testing.T) {
	for _, tc := range []struct {
		session string
		user    bool
	}{
		{session: "original-session"},
		{session: "different-session"},
		{session: "original-session", user: true},
		{session: "different-session", user: true},
	} {
		t.Run(fmt.Sprintf("%s/user=%t", tc.session, tc.user), func(t *testing.T) {
			upstreamDone := make(chan error, 1)
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					upstreamDone <- err
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				if _, _, err = conn.Read(ctx); err != nil {
					upstreamDone <- err
					return
				}
				completed := []byte(`{"type":"response.completed","response":{"id":"resp_before_external_block","model":"gpt-5.1","usage":{"input_tokens":2,"output_tokens":1}}}`)
				if err = conn.Write(ctx, coderws.MessageText, completed); err != nil {
					upstreamDone <- err
					return
				}
				if _, _, err = conn.Read(ctx); err == nil {
					upstreamDone <- fmt.Errorf("blocked follow-up reached upstream")
					return
				}
				if ctx.Err() != nil {
					upstreamDone <- ctx.Err()
					return
				}
				upstreamDone <- nil
			}))
			defer upstreamServer.Close()
			harness := newOpenAIWSPassthroughHandlerHarness(t, upstreamServer.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			first := []byte(`{"type":"response.create","model":"gpt-5.1","prompt_cache_key":"original-session","input":"first"}`)
			require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, first))
			_, event, err := harness.clientConn.Read(ctx)
			require.NoError(t, err)
			require.Contains(t, string(event), "resp_before_external_block")

			second, err := json.Marshal(map[string]any{
				"type": "response.create", "model": "gpt-5.1",
				"prompt_cache_key": tc.session, "input": "follow-up",
			})
			require.NoError(t, err)
			blockContext, _ := gin.CreateTestContext(httptest.NewRecorder())
			blockContext.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			key := service.CyberSessionExplicitBlockKey(harness.apiKey.ID, blockContext, second)
			if tc.user {
				require.Positive(t, harness.apiKey.UserID)
				key = service.CyberSessionUserExplicitBlockKey(harness.apiKey.UserID, blockContext, second)
			}
			require.NotEmpty(t, key)
			store, ok := harness.gatewayCache.(service.CyberSessionBlockStore)
			require.True(t, ok)
			// Simulate another connection recording a block after this one opened.
			require.NoError(t, store.SetCyberSessionBlocked(ctx, "", []string{key}, time.Minute))
			require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText, second))
			_, event, err = harness.clientConn.Read(ctx)
			require.NoError(t, err)
			require.Contains(t, string(event), "session_blocked_by_cyber_policy")
			_, _, err = harness.clientConn.Read(ctx)
			require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(err))

			select {
			case <-harness.handlerDone:
			case <-ctx.Done():
				t.Fatal("blocked websocket handler did not finish")
			}
			select {
			case err = <-upstreamDone:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("blocked websocket upstream did not close")
			}
		})
	}
}
