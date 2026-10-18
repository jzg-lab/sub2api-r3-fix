package antibypass

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Policy is the administrator's admission budget. Zero never means unlimited.
// Lease renewal and request inspection limits keep their existing ownership.
type Policy struct {
	WindowSeconds            int `json:"window_seconds"`
	ReplayWindowSeconds      int `json:"replay_window_seconds"`
	RPMLimit                 int `json:"rpm_limit"`
	MaxConcurrent            int `json:"max_concurrent"`
	MaxDistinctKeys          int `json:"max_distinct_keys"`
	MaxDistinctIPs           int `json:"max_distinct_ips"`
	MaxDistinctFingerprints  int `json:"max_distinct_fingerprints"`
	MaxClientRequestAttempts int `json:"max_client_request_attempts"`
}

func DefaultPolicy() Policy {
	c := DefaultConfig()
	return Policy{
		WindowSeconds: int(c.Window / time.Second), ReplayWindowSeconds: int(c.ReplayWindow / time.Second),
		RPMLimit: c.RPMLimit, MaxConcurrent: c.MaxConcurrent, MaxDistinctKeys: c.MaxDistinctKeys,
		MaxDistinctIPs: c.MaxDistinctIPs, MaxDistinctFingerprints: c.MaxDistinctFingerprints,
		MaxClientRequestAttempts: c.MaxClientRequestAttempts,
	}
}

func (p Policy) Validate() error {
	for _, field := range []struct {
		name       string
		value, max int
	}{
		{"window_seconds", p.WindowSeconds, 3600},
		{"replay_window_seconds", p.ReplayWindowSeconds, 300},
		{"rpm_limit", p.RPMLimit, 60000},
		{"max_concurrent", p.MaxConcurrent, 4096},
		{"max_distinct_keys", p.MaxDistinctKeys, 1024},
		{"max_distinct_ips", p.MaxDistinctIPs, 1024},
		{"max_distinct_fingerprints", p.MaxDistinctFingerprints, 1024},
		{"max_client_request_attempts", p.MaxClientRequestAttempts, 10},
	} {
		if field.value < 1 || field.value > field.max {
			return fmt.Errorf("anti_bypass_policy.%s must be between 1 and %d", field.name, field.max)
		}
	}
	return nil
}

func ParsePolicy(raw string) (Policy, error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultPolicy(), nil
	}
	var p Policy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("invalid anti_bypass_policy JSON")
	}
	return p, p.Validate()
}

func (p Policy) Config() Config {
	c := DefaultConfig()
	c.Window = time.Duration(p.WindowSeconds) * time.Second
	c.ReplayWindow = time.Duration(p.ReplayWindowSeconds) * time.Second
	c.RPMLimit, c.MaxConcurrent = p.RPMLimit, p.MaxConcurrent
	c.MaxDistinctKeys, c.MaxDistinctIPs = p.MaxDistinctKeys, p.MaxDistinctIPs
	c.MaxDistinctFingerprints = p.MaxDistinctFingerprints
	c.MaxClientRequestAttempts = p.MaxClientRequestAttempts
	return c
}
