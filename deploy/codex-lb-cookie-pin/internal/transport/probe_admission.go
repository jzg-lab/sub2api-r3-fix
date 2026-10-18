package transport

import (
	"context"
	"time"

	pluginv1 "github.com/liyunlong/sub2api-cookie-plugin/pkg/pluginapi/v1"
)

const maxConcurrentProbes = 2

// Production requires the host's expiring, identity-bound account permission.
// Embedded test hosts may exercise transport without a database-backed host.
func (s *Server) RequireHostProbePolicy() {
	s.probeMu.Lock()
	s.hostPolicyRequired = true
	s.probeMu.Unlock()
}

func templateProbeIdentity(tmpl *probeTemplate) string {
	return pluginv1.ProbeIdentity(tmpl.Headers.Get("Authorization"),
		tmpl.Headers.Get("Chatgpt-Account-Id"), tmpl.ProxyURL)
}

func (s *Server) applyHostProbePolicy(ctx context.Context, now time.Time) {
	policy, ok := pluginv1.ReadProbePolicy(ctx, now)
	if !ok {
		return
	}
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if policy.IssuedAt <= s.hostPolicyIssued {
		return // Concurrent or delayed status replies cannot extend old authority.
	}
	s.hostPolicyIssued = policy.IssuedAt
	s.hostPolicyExpires = time.UnixMilli(policy.ExpiresAt)
	s.hostPolicies = make(map[int64]pluginv1.ProbeAccountPolicy, len(policy.Accounts))
	for _, entry := range policy.Accounts {
		s.hostPolicies[entry.AccountID] = entry
	}
	if policy.Suspended {
		// An unavailable DB is not proof of account deletion. Stop probes,
		// retaining templates so a fresh authoritative snapshot can recover.
		for id := range s.templates {
			s.invalidateProbeResultsLocked(id)
		}
		return
	}
	for id, tmpl := range s.templates {
		entry, allowed := s.hostPolicies[id]
		if (!allowed || entry.Identity != templateProbeIdentity(tmpl)) &&
			tmpl.CreatedAt.UnixMilli() <= policy.IssuedAt {
			s.invalidateProbeLocked(id)
			delete(s.templates, id)
			s.Store.Drop(id)
			s.resetProbeStateLocked(id, now)
			continue
		}
		if entry.NotBefore > now.UnixMilli() {
			s.invalidateProbeResultsLocked(id)
			s.extendProbeCooldownLocked(id, time.UnixMilli(entry.NotBefore))
		}
	}
	s.wakeProbeLoop()
}

// Caller holds probeMu. A policy mismatch never falls back to today's route or
// authorization. Failure protection is persisted separately from these leases.
func (s *Server) hostProbeAllowedLocked(id int64, tmpl *probeTemplate, now time.Time) bool {
	if !s.hostPolicyRequired {
		return true
	}
	entry, ok := s.hostPolicies[id]
	return ok && now.Before(s.hostPolicyExpires) && entry.NotBefore <= now.UnixMilli() &&
		entry.Identity == templateProbeIdentity(tmpl)
}

func probeBucket(proxy string) string {
	return cohort("", "", proxy).Bucket
}

// A fixed global budget and one probe per proxy endpoint cover both host and
// autonomous probes. Business requests do not queue behind probe admission.
func (s *Server) reserveProbeBudgetLocked(id int64, proxy string) bool {
	bucket := probeBucket(proxy)
	if _, busy := s.probeBudget[id]; busy || len(s.probeBudget) >= maxConcurrentProbes ||
		s.businessByBucket[bucket] > 0 {
		return false
	}
	for _, occupied := range s.probeBudget {
		if occupied == bucket {
			return false
		}
	}
	s.probeBudget[id] = bucket
	return true
}

func (s *Server) releaseProbeBudget(id int64) {
	s.probeMu.Lock()
	delete(s.probeBudget, id)
	s.probeMu.Unlock()
	s.wakeProbeLoop()
}

func (s *Server) finishBusinessBucket(bucket string) {
	s.probeMu.Lock()
	s.businessByBucket[bucket]--
	if s.businessByBucket[bucket] <= 0 {
		delete(s.businessByBucket, bucket)
	}
	s.probeMu.Unlock()
	s.wakeProbeLoop()
}
