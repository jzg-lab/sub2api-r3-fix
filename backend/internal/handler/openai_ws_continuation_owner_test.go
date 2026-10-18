package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIResponsesWebSocketRejectsUnknownContinuationBeforeUpstream(t *testing.T) {
	var reached atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if _, _, err = conn.Read(ctx); err == nil {
			_ = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_unowned_result","model":"gpt-5.1"}}`))
		}
	}))
	defer upstream.Close()
	harness := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, harness.clientConn.Write(ctx, coderws.MessageText,
		[]byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_not_owned","input":"continue"}`)))
	_, _, err := harness.clientConn.Read(ctx)
	require.Error(t, err, "unowned continuation must be rejected before forwarding")
	require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(err))
	require.Zero(t, reached.Load(), "unowned continuation reached upstream")
}

func TestOpenAIResponsesWebSocketContinuationIsolation(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool, service.OpenAIWSIngressModeHTTPBridge} {
		for _, scenario := range []string{"same_user_other_key", "reconnect", "other_user", "other_group", "subsequent_other_user", "response_owner_conflict"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				var requests atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if mode == service.OpenAIWSIngressModeHTTPBridge {
						requests.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_result\",\"model\":\"gpt-5.1\"}}\n\n"))
						_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_result\",\"model\":\"gpt-5.1\",\"output\":[]}}\n\n"))
						return
					}
					conn, err := coderws.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer conn.CloseNow()
					ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
					defer cancel()
					for {
						if _, _, err := conn.Read(ctx); err != nil {
							return
						}
						requests.Add(1)
						if err := conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_result","model":"gpt-5.1"}}`)); err != nil {
							return
						}
						if err := conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_result","model":"gpt-5.1","output":[]}}`)); err != nil {
							return
						}
					}
				}))
				defer upstream.Close()
				h := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL, mode)
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				user, key, group := h.apiKey.UserID, h.apiKey.ID, *h.apiKey.GroupID
				switch scenario {
				case "same_user_other_key":
					require.NoError(t, h.gatewayService.BindOpenAIHTTPResponseOwner(ctx, group, "resp_previous", user, key+1))
				case "other_user", "subsequent_other_user":
					require.NoError(t, h.gatewayService.BindOpenAIHTTPResponseOwner(ctx, group, "resp_previous", user+1, key+1))
				case "other_group":
					require.NoError(t, h.gatewayService.BindOpenAIHTTPResponseOwner(ctx, group+1, "resp_previous", user, key))
				case "response_owner_conflict":
					require.NoError(t, h.gatewayService.BindOpenAIHTTPResponseOwner(ctx, group, "resp_result", user+1, key+1))
				}
				first := `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_previous","input":"continue"}`
				if scenario == "subsequent_other_user" || scenario == "response_owner_conflict" || scenario == "reconnect" {
					first = `{"type":"response.create","model":"gpt-5.1","input":"start"}`
				}
				require.NoError(t, h.clientConn.Write(ctx, coderws.MessageText, []byte(first)))
				switch scenario {
				case "other_user", "other_group", "response_owner_conflict":
					_, _, err := h.clientConn.Read(ctx)
					require.Error(t, err)
					expected := coderws.StatusPolicyViolation
					if scenario == "response_owner_conflict" {
						expected = coderws.StatusInternalError
						require.EqualValues(t, 1, requests.Load())
						allowed, ownerErr := h.gatewayService.ValidateOpenAIHTTPResponseOwner(ctx, group, "resp_result", user+1, key+1)
						require.NoError(t, ownerErr)
						require.True(t, allowed, "a conflicting response must not replace its original owner")
					} else {
						require.Zero(t, requests.Load(), "forbidden first turn reached upstream")
					}
					require.Equal(t, expected, coderws.CloseStatus(err))
				default:
					for {
						_, payload, err := h.clientConn.Read(ctx)
						require.NoError(t, err)
						allowed, err := h.gatewayService.ValidateOpenAIHTTPResponseOwner(ctx, group, "resp_result", user, key)
						require.NoError(t, err)
						require.True(t, allowed, "ownership must already be persisted when the client receives the ID")
						selected, err := h.gatewayService.SelectAccountByPreviousResponseID(ctx, h.apiKey.GroupID, "resp_result", "gpt-5.1", nil, false)
						require.NoError(t, err)
						require.NotNil(t, selected, "the visible response must already route back to its original account")
						require.EqualValues(t, 9951, selected.Account.ID)
						if selected.ReleaseFunc != nil {
							selected.ReleaseFunc()
						}
						if gjson.GetBytes(payload, "type").String() == "response.completed" {
							break
						}
					}
					if scenario == "subsequent_other_user" {
						require.NoError(t, h.clientConn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_previous","input":"continue"}`)))
						_, _, err := h.clientConn.Read(ctx)
						require.Error(t, err)
						require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(err))
						require.EqualValues(t, 1, requests.Load(), "forbidden subsequent turn reached upstream")
					} else {
						if scenario == "reconnect" {
							_ = h.clientConn.CloseNow()
							select {
							case <-h.handlerDone:
							case <-ctx.Done():
								t.Fatal("original handler did not release the connection")
							}
							h = newOpenAIWSHandlerHarnessWithCache(t, upstream.URL, h.gatewayCache, mode)
						}
						require.NoError(t, h.clientConn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_result","input":"continue"}`)))
						for {
							_, payload, err := h.clientConn.Read(ctx)
							require.NoError(t, err)
							if gjson.GetBytes(payload, "type").String() == "response.completed" {
								break
							}
						}
						require.EqualValues(t, 2, requests.Load(), "authorized continuation must still work")
					}
				}
				_ = h.clientConn.CloseNow()
				select {
				case <-h.handlerDone:
				case <-ctx.Done():
					t.Fatal("handler did not release the connection")
				}
			})
		}
	}
}
