package service

import (
	"context"
	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
	"sync"
)

// The relay admits at most one outstanding response.create. Register it before
// writing: a fast server may answer while the write goroutine is returning.
type captureWSRelay struct {
	openaiwsv2.FrameConn
	mu        sync.Mutex
	session   *captureSession
	secrets   []string
	model     func() string
	turnModel string
	request   []byte
	capture   *openAIWSCapture
}

func (c *captureWSRelay) finishLocked(complete bool) {
	if c.capture != nil {
		emitOpenAIWSCapture(c.session, c.request, c.capture, "websocket", 0, complete, c.secrets, c.turnModel)
	}
	c.capture = nil
	c.request = nil
}
func (c *captureWSRelay) finish() { c.mu.Lock(); defer c.mu.Unlock(); c.finishLocked(false) }
func (c *captureWSRelay) WriteFrame(ctx context.Context, t coderws.MessageType, p []byte) error {
	if gjson.GetBytes(p, "type").String() != "response.create" {
		return c.FrameConn.WriteFrame(ctx, t, p)
	}
	c.mu.Lock()
	c.finishLocked(false)
	if len(p) <= captureRecordMaxBytes {
		c.turnModel = c.model()
		c.request = p
		c.capture = newOpenAIWSCapture(c.session)
	}
	c.mu.Unlock()
	err := c.FrameConn.WriteFrame(ctx, t, p)
	if err != nil {
		c.finish()
	}
	return err
}
func (c *captureWSRelay) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	t, p, err := c.FrameConn.ReadFrame(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && c.capture != nil {
		c.capture.add(p)
		event := gjson.GetBytes(p, "type").String()
		if isOpenAIWSTerminalEvent(event) || event == "error" {
			c.finishLocked(true)
		}
	}
	return t, p, err
}
