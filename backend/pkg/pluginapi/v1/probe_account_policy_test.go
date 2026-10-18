package pluginv1

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
)

func TestProbeAccountPolicyBoundariesAndRateLimitCompatibility(t *testing.T) {
	now := time.Now()
	valid := ProbePolicy{IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(ProbePolicyTTL).UnixMilli(),
		Accounts: []ProbeAccountPolicy{{AccountID: 42, Identity: ProbeIdentity("fixture", "subject", "route")}}}
	for _, name := range []string{"valid", "expired", "future", "long lease", "duplicate", "bad identity", "suspended", "mixed suspension"} {
		t.Run(name, func(t *testing.T) {
			p := valid
			p.Accounts = append([]ProbeAccountPolicy(nil), valid.Accounts...)
			switch name {
			case "expired":
				p.ExpiresAt = now.UnixMilli()
			case "future":
				p.IssuedAt = now.Add(2 * time.Second).UnixMilli()
			case "long lease":
				p.ExpiresAt++
			case "duplicate":
				p.Accounts = append(p.Accounts, p.Accounts[0])
			case "bad identity":
				p.Accounts[0].Identity = strings.Repeat("z", 64)
			case "suspended":
				p.Suspended, p.Accounts = true, nil
			case "mixed suspension":
				p.Suspended = true
			}
			ctx := WithProbeRateLimitPolicy(context.Background(), ProbeRateLimitPolicy{Fallback: time.Hour, NotBefore: now.Add(time.Hour)})
			ctx = WithProbePolicy(ctx, p)
			md, _ := metadata.FromOutgoingContext(ctx)
			in := metadata.NewIncomingContext(context.Background(), md)
			_, ok := ReadProbePolicy(in, now)
			if ok != (name == "valid" || name == "suspended") {
				t.Fatalf("unexpected policy acceptance: %v", ok)
			}
			rate := ReadProbeRateLimitPolicy(in)
			if rate.Fallback != time.Hour || !rate.NotBefore.Equal(now.Add(time.Hour)) {
				t.Fatal("account policy replaced the existing rate-limit protocol")
			}
		})
	}
	if ProbeIdentity("a", "bc", "") == ProbeIdentity("ab", "c", "") {
		t.Fatal("identity boundaries are ambiguous")
	}
}
