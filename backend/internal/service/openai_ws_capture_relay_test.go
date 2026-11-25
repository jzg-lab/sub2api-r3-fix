package service

import (
	"context"
	"encoding/json"
	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"testing"
)

type captureRelayFixture struct {
	openaiwsv2.FrameConn
	next []byte
}

func (f *captureRelayFixture) WriteFrame(_ context.Context, _ coderws.MessageType, p []byte) error {
	return nil
}
func (f *captureRelayFixture) ReadFrame(context.Context) (coderws.MessageType, []byte, error) {
	return coderws.MessageText, f.next, nil
}
func TestCaptureWSRelayTwoTurns(t *testing.T) {
	m := NewPluginManager(nil, nil, nil, PluginHostInfo{})
	m.captureRuntime.Store(&pluginRuntime{})
	ctx, closeSession := m.beginCapture(context.Background(), nil, &Account{Platform: PlatformOpenAI})
	defer closeSession(true)
	f := &captureRelayFixture{}
	c := &captureWSRelay{FrameConn: f, session: captureSessionFromContext(ctx), model: func() string { return "model" }, secrets: []string{"demo-needle"}}
	for i, resp := range []string{`{"type":"response.completed","text":"demo-needle"}`, `{"type":"response.output_text.delta","delta":"partial"}`} {
		require.NoError(t, c.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"model"}`)))
		f.next = []byte(resp)
		_, _, err := c.ReadFrame(ctx)
		require.NoError(t, err)
		if i == 1 {
			c.finish()
		}
	}
	require.Len(t, m.captureQueue, 2)
	ids := map[string]bool{}
	for i := range 2 {
		item := <-m.captureQueue
		defer item.release()
		id := item.request.RecordId
		require.False(t, ids[id])
		ids[id] = true
		var saved struct {
			Complete bool     `json:"response_complete"`
			Response []string `json:"response"`
		}
		require.NoError(t, json.Unmarshal(item.request.RecordJson, &saved))
		require.Equal(t, i == 0, saved.Complete)
		require.Len(t, saved.Response, 1)
		require.NotContains(t, saved.Response[0], "demo-needle")
		require.NotContains(t, saved.Response[0], "response.create")
	}
}
