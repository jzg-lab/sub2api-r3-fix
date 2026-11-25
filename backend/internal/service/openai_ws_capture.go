package service

import (
	"context"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"sync"
)

type openAIWSCapture struct {
	mu      sync.Mutex
	frames  []string
	size    int
	dropped bool
	ctx     context.Context
	release func()
}

func newOpenAIWSCapture(session *captureSession) *openAIWSCapture {
	ctx, release := pkghttputil.WithRequestBodyMemoryScope(session.ctx)
	return &openAIWSCapture{ctx: ctx, release: release}
}

func captureSessionFromContext(ctx context.Context) *captureSession {
	if ctx == nil {
		return nil
	}
	session, _ := ctx.Value(captureContextKey{}).(*captureSession)
	return session
}

func (c *openAIWSCapture) add(frame []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	charge := len(frame) + 48
	if c.dropped || charge > captureRecordMaxBytes-c.size || pkghttputil.ReserveRequestBodyMemory(c.ctx, int64(charge)) != nil {
		c.dropped = true
		c.frames = nil
		return
	}
	c.frames = append(c.frames, string(frame))
	c.size += charge
}

func (c *openAIWSCapture) snapshot() ([]string, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frames, !c.dropped
}

func emitOpenAIWSCapture(session *captureSession, request []byte, capture *openAIWSCapture, transport string, status int, complete bool, secrets []string, requestedModel string) {
	if session == nil || capture == nil {
		return
	}
	defer capture.release()
	frames, bounded := capture.snapshot()
	if !bounded {
		return
	}
	session.emitRawAttempt(request, nil, frames, transport, status, complete, secrets, requestedModel)
}
