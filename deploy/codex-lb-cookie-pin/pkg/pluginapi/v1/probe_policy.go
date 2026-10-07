package pluginv1

import (
	"context"
	"strconv"
	"time"

	"google.golang.org/grpc/metadata"
)

const (
	probeFallbackKey = "sub2api-probe-429-fallback-seconds"
	probeHoldKey     = "sub2api-probe-not-before"

	DefaultProbe429Fallback = 5 * time.Minute
	MaxProbe429Fallback     = 2 * time.Hour
)

// ProbeRateLimitPolicy is host-only RPC metadata, never an upstream HTTP header.
// Older plugins ignore it; updated plugins use conservative defaults with old hosts.
type ProbeRateLimitPolicy struct {
	Fallback  time.Duration
	NotBefore time.Time
}

func (p ProbeRateLimitPolicy) FallbackDuration() time.Duration {
	if p.Fallback <= 0 {
		return DefaultProbe429Fallback
	}
	return max(time.Second, min(p.Fallback, MaxProbe429Fallback))
}

func WithProbeRateLimitPolicy(ctx context.Context, p ProbeRateLimitPolicy) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(probeFallbackKey, strconv.FormatInt(int64(p.FallbackDuration()/time.Second), 10))
	md.Delete(probeHoldKey)
	if !p.NotBefore.IsZero() {
		md.Set(probeHoldKey, p.NotBefore.UTC().Format(time.RFC3339Nano))
	}
	return metadata.NewOutgoingContext(ctx, md)
}

func ReadProbeRateLimitPolicy(ctx context.Context) ProbeRateLimitPolicy {
	p := ProbeRateLimitPolicy{Fallback: DefaultProbe429Fallback}
	md, _ := metadata.FromIncomingContext(ctx)
	if values := md.Get(probeFallbackKey); len(values) == 1 {
		if seconds, err := strconv.ParseInt(values[0], 10, 64); err == nil && seconds > 0 {
			p.Fallback = time.Duration(min(seconds, int64(MaxProbe429Fallback/time.Second))) * time.Second
		}
	}
	if values := md.Get(probeHoldKey); len(values) == 1 {
		p.NotBefore, _ = time.Parse(time.RFC3339Nano, values[0])
	}
	return p
}
