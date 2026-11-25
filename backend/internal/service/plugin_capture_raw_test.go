package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRawAttemptRedactsRequestResponseAndModelBeforeQueue(t *testing.T) {
	manager := NewPluginManager(nil, nil, nil, PluginHostInfo{})
	runtime := &pluginRuntime{}
	manager.captureRuntime.Store(runtime)
	ctx, finish := manager.beginCapture(context.Background(), nil, &Account{ID: 3, Platform: PlatformOpenAI})
	defer finish(false)
	s := ctx.Value(captureContextKey{}).(*captureSession)
	s.groupID, s.requestedModel = 42, "CiYuan.Fast/demo"
	s.clientSecrets = captureHeaderSecrets(http.Header{"X-Api-Key": {"example-customer-needle"}})
	keys := captureHeaderSecrets(http.Header{"Authorization": {"Bearer example-provider-needle"}})
	s.emitRawAttempt([]byte(`{ "model":"词元", "n":9007199254740993, "input":"example-customer-needle" }`), []byte(`{"message":"example-provider-needle CIYUAN"}`), nil, "http", 429, true, keys)
	require.Len(t, manager.captureQueue, 1)
	item := <-manager.captureQueue
	defer item.release()
	var saved struct {
		Request        string `json:"request"`
		Response       string `json:"response"`
		Model          string `json:"model"`
		RequestedModel string `json:"requested_model"`
		HTTPStatus     int    `json:"http_status"`
	}
	require.NoError(t, json.Unmarshal(item.request.RecordJson, &saved))
	require.Equal(t, `{ "model":"中转", "n":9007199254740993, "input":"[REDACTED]" }`, saved.Request)
	require.Equal(t, `{"message":"[REDACTED] 中转"}`, saved.Response)
	require.Equal(t, "中转", saved.Model)
	require.Equal(t, "中转/demo", saved.RequestedModel)
	require.Equal(t, 429, saved.HTTPStatus)
}
