package pluginv1

import (
	"context"

	"google.golang.org/grpc/metadata"
)

const healthModeKey = "sub2api-health-mode"

// WithLivenessHealth requests a cheap RPC/process check, not a status snapshot.
// It is local RPC metadata only. Old plugins ignore it; old hosts still get status.
func WithLivenessHealth(ctx context.Context) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(healthModeKey, "liveness")
	return metadata.NewOutgoingContext(ctx, md)
}

func IsLivenessHealth(ctx context.Context) bool {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(healthModeKey)
	return len(values) == 1 && values[0] == "liveness"
}
