package pluginv1

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"google.golang.org/grpc/metadata"
)

const (
	probePolicyKey          = "sub2api-probe-policy"
	MaxProbePolicyAccounts  = 1024
	ProbePolicyTTL          = 60 * time.Second
	probeFallbackKey        = "sub2api-probe-429-fallback-seconds"
	probeHoldKey            = "sub2api-probe-not-before"
	DefaultProbe429Fallback = 5 * time.Minute
	MaxProbe429Fallback     = 2 * time.Hour
)

// ProbeRateLimitPolicy remains independent of the background account lease.
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

type ProbeAccountPolicy struct {
	AccountID int64  `json:"id"`
	Identity  string `json:"identity,omitempty"`
	NotBefore int64  `json:"not_before,omitempty"`
}

// An absent account is denied. This lease authorizes background probes only,
// not user traffic, credential refresh, account mutation or route changes.
type ProbePolicy struct {
	IssuedAt  int64                `json:"issued_at"`
	ExpiresAt int64                `json:"expires_at"`
	Suspended bool                 `json:"suspended,omitempty"`
	Accounts  []ProbeAccountPolicy `json:"accounts"`
}

func ProbeIdentity(authorization, subject, proxy string) string {
	raw, _ := json.Marshal([]string{authorization, subject, proxy})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func WithProbePolicy(ctx context.Context, policy ProbePolicy) context.Context {
	raw, err := json.Marshal(policy)
	if err != nil || len(raw) > 256<<10 || len(policy.Accounts) > MaxProbePolicyAccounts {
		return ctx
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(probePolicyKey, string(raw))
	return metadata.NewOutgoingContext(ctx, md)
}

func ReadProbePolicy(ctx context.Context, now time.Time) (ProbePolicy, bool) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(probePolicyKey)
	var policy ProbePolicy
	if len(values) != 1 || len(values[0]) > 256<<10 ||
		json.Unmarshal([]byte(values[0]), &policy) != nil {
		return policy, false
	}
	if len(policy.Accounts) > MaxProbePolicyAccounts || policy.IssuedAt <= 0 ||
		(policy.Suspended && len(policy.Accounts) != 0) ||
		policy.IssuedAt > now.Add(time.Second).UnixMilli() ||
		policy.ExpiresAt <= now.UnixMilli() || policy.ExpiresAt <= policy.IssuedAt ||
		policy.ExpiresAt-policy.IssuedAt > ProbePolicyTTL.Milliseconds() {
		return ProbePolicy{}, false
	}
	seen := make(map[int64]bool, len(policy.Accounts))
	for _, account := range policy.Accounts {
		if account.AccountID <= 0 || seen[account.AccountID] || len(account.Identity) != 64 {
			return ProbePolicy{}, false
		}
		if _, err := hex.DecodeString(account.Identity); err != nil {
			return ProbePolicy{}, false
		}
		seen[account.AccountID] = true
	}
	return policy, true
}
