package service

import (
	"context"
	"sort"
	"time"

	pluginv1 "github.com/Wei-Shaw/sub2api/pkg/pluginapi/v1"
)

// Batch the existing manual-pause/owned-error control check. A rescue circuit
// is intentionally different from an operator disabling an account.
type OpenAIProbeBatchControlStore interface {
	AllowedOpenAIProbeAccounts(context.Context, []int64) (map[int64]bool, error)
}

const pluginProbePolicyTimeout = 2 * time.Second

func (r *pluginRuntime) trackProbeAccount(id int64, now time.Time) {
	r.probeAccountsMu.Lock()
	defer r.probeAccountsMu.Unlock()
	if r.probeAccounts == nil {
		r.probeAccounts = make(map[int64]time.Time)
	}
	if id <= 0 {
		return
	}
	if _, exists := r.probeAccounts[id]; !exists && len(r.probeAccounts) >= pluginv1.MaxProbePolicyAccounts {
		var oldestID int64
		oldest := now
		for accountID, seen := range r.probeAccounts {
			if !seen.After(oldest) {
				oldestID, oldest = accountID, seen
			}
		}
		delete(r.probeAccounts, oldestID)
	}
	r.probeAccounts[id] = now
}

func (m *PluginManager) probePolicyContext(ctx context.Context, r *pluginRuntime) context.Context {
	if r.installation == nil || r.installation.PluginKey != "lyunlong.codex.lb-cookie-pin" {
		return ctx
	}
	now := time.Now()
	policy := pluginv1.ProbePolicy{IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(pluginv1.ProbePolicyTTL).UnixMilli()}
	r.probeAccountsMu.Lock()
	ids := make([]int64, 0, len(r.probeAccounts))
	for id, seen := range r.probeAccounts {
		if now.Sub(seen) > 24*time.Hour {
			delete(r.probeAccounts, id)
		} else {
			ids = append(ids, id)
		}
	}
	r.probeAccountsMu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) == 0 {
		return pluginv1.WithProbePolicy(ctx, policy)
	}
	// Missing or failing control consumers issue a deny-all lease, never
	// extend an old permission. This is separate from process liveness.
	if m.rateLimitService == nil || m.rateLimitService.accountRepo == nil || m.probeControls == nil {
		policy.Suspended = true
		return pluginv1.WithProbePolicy(ctx, policy)
	}
	// A slow control store denies probing without consuming the separate
	// Health RPC budget or turning DB unavailability into a dead plugin.
	controlCtx, cancel := context.WithTimeout(ctx, pluginProbePolicyTimeout)
	defer cancel()
	accounts, err := m.rateLimitService.accountRepo.GetByIDs(controlCtx, ids)
	if err != nil {
		policy.Suspended = true
		return pluginv1.WithProbePolicy(ctx, policy)
	}
	allowed, err := m.probeControls.AllowedOpenAIProbeAccounts(controlCtx, ids)
	if err != nil {
		policy.Suspended = true
		return pluginv1.WithProbePolicy(ctx, policy)
	}
	for _, account := range accounts {
		if account == nil || !allowed[account.ID] || !isOpenAIDowngradeProbeAccountEligible(account, now) ||
			account.GetOpenAIAccessToken() == "" {
			continue
		}
		proxyURL := ""
		if account.ProxyID != nil {
			if account.Proxy == nil || account.Proxy.Status != StatusActive {
				continue
			}
			proxyURL = account.Proxy.URL()
		}
		entry := pluginv1.ProbeAccountPolicy{AccountID: account.ID,
			Identity: pluginv1.ProbeIdentity("Bearer "+account.GetOpenAIAccessToken(),
				account.GetChatGPTAccountID(), proxyURL)}
		for _, deadline := range []*time.Time{account.RateLimitResetAt, account.OverloadUntil, account.TempUnschedulableUntil} {
			if deadline != nil && deadline.UnixMilli() > entry.NotBefore {
				entry.NotBefore = deadline.UnixMilli()
			}
		}
		policy.Accounts = append(policy.Accounts, entry)
	}
	return pluginv1.WithProbePolicy(ctx, policy)
}
