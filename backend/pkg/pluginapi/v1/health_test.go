package pluginv1

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestLivenessHealthMetadata(t *testing.T) {
	base := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(healthModeKey, "status", "other", "preserved"))
	ctx := WithLivenessHealth(WithLivenessHealth(base))
	md, _ := metadata.FromOutgoingContext(ctx)
	if !IsLivenessHealth(metadata.NewIncomingContext(context.Background(), md)) || md.Get("other")[0] != "preserved" {
		t.Fatal("liveness metadata must round trip without losing other metadata")
	}
	original, _ := metadata.FromOutgoingContext(base)
	if original.Get(healthModeKey)[0] != "status" {
		t.Fatal("input context mutated")
	}
	for _, values := range [][]string{nil, {"status"}, {"unknown"}, {"liveness", "liveness"}} {
		if IsLivenessHealth(metadata.NewIncomingContext(context.Background(), metadata.MD{healthModeKey: values})) {
			t.Fatalf("unexpected liveness for %v", values)
		}
	}
	if IsLivenessHealth(context.Background()) || IsLivenessHealth(ctx) {
		t.Fatal("missing or outgoing-only metadata must retain legacy status behavior")
	}
}
