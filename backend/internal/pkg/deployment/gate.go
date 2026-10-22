// Package deployment coordinates local application handoff, not business policy.
package deployment

import (
	"context"
	"os"
	"sync"
	"time"
)

// Managed processes always boot paused, including after an unexpected restart.
func Managed() bool { return os.Getenv("DEPLOYMENT_SOCKET") != "" }

type Gate struct {
	mu      sync.Mutex
	open    bool
	pending int
}

var Background Gate

func (g *Gate) Enter() (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		return nil, false
	}
	g.pending++
	return sync.OnceFunc(func() { g.mu.Lock(); g.pending--; g.mu.Unlock() }), true
}

func (g *Gate) SetOpen(open bool) { g.mu.Lock(); g.open = open; g.mu.Unlock() }
func (g *Gate) Pending() int      { g.mu.Lock(); defer g.mu.Unlock(); return g.pending }

func (g *Gate) Wait(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for g.Pending() != 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return ctx.Err()
}

// BeginBackground admits one autonomous job. Request settlement does not use it.
func BeginBackground() (func(), bool) {
	if !Managed() {
		return func() {}, true
	}
	return Background.Enter()
}

func RunBackground(fn func()) {
	done, ok := BeginBackground()
	if !ok {
		return
	}
	defer done()
	fn()
}
