package pluginv1

import (
	"context"

	"google.golang.org/grpc/metadata"
)

const probeTrafficKey = "x-sub2api-probe-traffic"

// WithProbeTraffic tags host-generated qualification requests, never user input.
func WithProbeTraffic(ctx context.Context) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(probeTrafficKey, "true")
	return metadata.NewOutgoingContext(ctx, md)
}

func IsProbeTraffic(ctx context.Context) bool {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(probeTrafficKey)
	return len(values) == 1 && values[0] == "true"
}
