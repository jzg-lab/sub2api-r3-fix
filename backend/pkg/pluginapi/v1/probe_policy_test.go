package pluginv1

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
)

func TestProbeRateLimitPolicyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", DefaultProbe429Fallback},
		{"-1", DefaultProbe429Fallback},
		{"0", DefaultProbe429Fallback},
		{"invalid", DefaultProbe429Fallback},
		{"9223372036854775808", DefaultProbe429Fallback},
		{"1", time.Second},
		{"7201", MaxProbe429Fallback},
		{"9223372036854775807", MaxProbe429Fallback},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(probeFallbackKey, tc.raw))
			if got := ReadProbeRateLimitPolicy(ctx).FallbackDuration(); got != tc.want {
				t.Fatalf("fallback = %s, want %s", got, tc.want)
			}
		})
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		probeFallbackKey, "1", probeFallbackKey, "2", probeHoldKey, "invalid"))
	p := ReadProbeRateLimitPolicy(ctx)
	if p.Fallback != DefaultProbe429Fallback || !p.NotBefore.IsZero() {
		t.Fatal("ambiguous or malformed metadata must use conservative defaults")
	}
}

func TestProbeRateLimitPolicyRoundTripDoesNotMutateMetadata(t *testing.T) {
	hold := time.Date(2026, 10, 7, 8, 0, 0, 123, time.FixedZone("CST", 8*3600))
	original := metadata.Pairs("trace-id", "fixture", probeHoldKey, "old")
	ctx := metadata.NewOutgoingContext(context.Background(), original)
	out := WithProbeRateLimitPolicy(ctx, ProbeRateLimitPolicy{Fallback: time.Nanosecond, NotBefore: hold})
	md, _ := metadata.FromOutgoingContext(out)
	got := ReadProbeRateLimitPolicy(metadata.NewIncomingContext(context.Background(), md))
	if got.Fallback != time.Second || !got.NotBefore.Equal(hold) || md.Get("trace-id")[0] != "fixture" {
		t.Fatal("policy or unrelated metadata lost in round trip")
	}
	if original.Get(probeHoldKey)[0] != "old" || len(original.Get(probeFallbackKey)) != 0 {
		t.Fatal("caller metadata was mutated")
	}
	cleared := WithProbeRateLimitPolicy(out, ProbeRateLimitPolicy{})
	md, _ = metadata.FromOutgoingContext(cleared)
	if len(md.Get(probeHoldKey)) != 0 || md.Get("trace-id")[0] != "fixture" {
		t.Fatal("stale hold retained or unrelated metadata lost")
	}
}

func TestProbeRateLimitPolicyPluginCopyMatches(t *testing.T) {
	host, err := os.ReadFile("probe_policy.go")
	if err != nil {
		t.Fatal(err)
	}
	plugin, err := os.ReadFile("../../../../deploy/codex-lb-cookie-pin/pkg/pluginapi/v1/probe_policy.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(host, plugin) {
		t.Fatal("host and independently built plugin policy contracts differ")
	}
}
