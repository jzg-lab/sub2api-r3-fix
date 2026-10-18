package repository

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

const codexUploadIdleTimeout = 30 * time.Second

var errCodexUploadIdle = &net.OpError{
	Op: "write", Net: "http", Err: codexUploadTimeout{},
}

type codexUploadTimeout struct{}

func (codexUploadTimeout) Error() string   { return "upstream request upload made no progress" }
func (codexUploadTimeout) Timeout() bool   { return true }
func (codexUploadTimeout) Temporary() bool { return false }

// This guard owns only a request context, never a shared socket deadline.
// ResponseHeaderTimeout starts after upload and cannot bound a stalled write.
type codexUploadGuard struct {
	mu        sync.Mutex
	idle      time.Duration
	timer     *time.Timer
	last      time.Time
	ended     time.Time
	readBytes int64
	finished  bool
	ctx       context.Context
	cancel    context.CancelCauseFunc
}

func guardCodexUpload(req *http.Request, idle time.Duration) (*http.Request, *codexUploadGuard) {
	ctx, cancel := context.WithCancelCause(req.Context())
	g := &codexUploadGuard{idle: idle, ctx: ctx, cancel: cancel}
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn:      func(httptrace.GotConnInfo) { g.progress(true, 0) },
		WroteRequest: func(httptrace.WroteRequestInfo) { g.finish() },
	})
	req = req.Clone(ctx)
	req.GetBody = nil
	req.Body = &codexUploadBody{ReadCloser: req.Body, guard: g}
	return req, g
}

func (g *codexUploadGuard) progress(start bool, n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.readBytes += int64(n)
	if g.finished || (!start && g.timer == nil) {
		return
	}
	g.last = time.Now()
	if g.timer == nil {
		g.timer = time.AfterFunc(g.idle, g.expire)
	} else {
		g.timer.Reset(g.idle)
	}
}

func (g *codexUploadGuard) expire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.finished || g.ctx.Err() != nil {
		return
	}
	// A callback already queued before Reset must respect newer progress.
	if remaining := g.idle - time.Since(g.last); remaining > 0 {
		g.timer.Reset(remaining)
		return
	}
	g.finished = true
	g.ended = time.Now()
	g.cancel(errCodexUploadIdle)
}

func (g *codexUploadGuard) finish() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.finished {
		g.ended = time.Now()
	}
	g.finished = true
	if g.timer != nil {
		g.timer.Stop()
	}
}

func (g *codexUploadGuard) observation() (readBytes, idleMS int64, expired bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	idleMS = -1
	if !g.last.IsZero() && !g.ended.IsZero() {
		idleMS = g.ended.Sub(g.last).Milliseconds()
	}
	return g.readBytes, idleMS, context.Cause(g.ctx) == errCodexUploadIdle
}

func (g *codexUploadGuard) result(err error) error {
	g.finish()
	if context.Cause(g.ctx) == errCodexUploadIdle {
		return errCodexUploadIdle
	}
	return err
}

func (g *codexUploadGuard) close() {
	g.finish()
	g.cancel(context.Canceled)
}

type codexUploadBody struct {
	io.ReadCloser
	guard *codexUploadGuard
}

func (b *codexUploadBody) Read(p []byte) (int, error) {
	// Bound reads and do not expose WriterTo: observe progress between writes.
	if len(p) > 32*1024 {
		p = p[:32*1024]
	}
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.guard.progress(false, n)
	}
	return n, err
}
