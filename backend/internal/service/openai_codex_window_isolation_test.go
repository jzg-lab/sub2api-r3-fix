package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexMachineChain_WindowEnvironmentIsolation(t *testing.T) {
	routes := []struct {
		name        string
		endpoint    string
		passthrough bool
		websocket   bool
	}{
		{"http_map", "/v1/responses", false, false},
		{"http_raw", "/v1/responses", true, false},
		{"compact_map", "/v1/responses/compact", false, false},
		{"compact_raw", "/v1/responses/compact", true, false},
		{"websocket", "/v1/responses", false, true},
	}
	for _, route := range routes {
		for _, window := range []string{"customer-host-window", testMachineChainRoot + ":customer-host"} {
			t.Run(route.name+"/"+window, func(t *testing.T) {
				c, _, body := machineChainRequest(t, route.endpoint,
					testMachineChainRoot, testMachineChainRoot, window, "")
				var payload map[string]any
				require.NoError(t, json.Unmarshal(body, &payload))
				cm := payload["client_metadata"].(map[string]any)
				cm["installation_id"] = "real-install"
				cm["session-id"] = testMachineChainRoot
				cm["thread-id"] = testMachineChainRoot
				cm["window_id"] = window
				cm["x-client-request-id"] = testMachineChainRoot
				payload["stream"] = route.websocket
				body, err := json.Marshal(payload)
				require.NoError(t, err)

				var headers http.Header
				var upstreamBody []byte
				if route.websocket {
					svc, dialer, conn := newMachineWSChainService(t)
					_, err = svc.Forward(context.Background(), c,
						newMachineWSChainAccount(t, 6201, testCodexFingerprintSeed), body)
					require.NoError(t, err)
					require.NotNil(t, conn.lastWrite)
					headers = dialer.lastHeaders
					upstreamBody = []byte(requestToJSONString(conn.lastWrite))
				} else {
					svc, upstream := newMachineChainService()
					_, _ = svc.Forward(context.Background(), c,
						newMachineChainAccount(t, 6201, testCodexFingerprintSeed, route.passthrough, ""), body)
					require.NotNil(t, upstream.lastReq)
					headers = upstream.lastReq.Header
					upstreamBody = upstream.lastBody
				}

				mappedWindow := headers.Get("x-codex-window-id")
				_, err = uuid.Parse(mappedWindow)
				require.NoError(t, err)
				assert.NotEqual(t, window, mappedWindow)
				for _, values := range headers {
					for _, value := range values {
						assert.NotContains(t, value, "customer-host")
					}
				}
				assert.NotContains(t, string(upstreamBody), "customer-host")
				metadata := gjson.GetBytes(upstreamBody, "client_metadata")
				assert.Equal(t, mappedWindow, metadata.Get("window_id").String())
				assert.Equal(t, mappedWindow, metadata.Get("x-codex-window-id").String())
				assert.Equal(t, mappedWindow, gjson.Get(headers.Get("x-codex-turn-metadata"), "window_id").String())
				assert.Equal(t, mappedWindow, gjson.Get(metadata.Get("x-codex-turn-metadata").String(), "window_id").String())
				assert.Equal(t, headers.Get("x-codex-installation-id"), metadata.Get("installation_id").String())
				assert.Equal(t, metadata.Get("installation_id").String(), metadata.Get("x-codex-installation-id").String())
				assert.Equal(t, headers.Get("session-id"), metadata.Get("session-id").String())
				assert.Equal(t, metadata.Get("session_id").String(), metadata.Get("session-id").String())
				assert.Equal(t, headers.Get("thread-id"), metadata.Get("thread-id").String())
				assert.Equal(t, metadata.Get("thread_id").String(), metadata.Get("thread-id").String())
				assert.Equal(t, headers.Get("x-client-request-id"), metadata.Get("x-client-request-id").String())
			})
		}
	}
}
