package transport

import (
	"log/slog"
	"time"

	"github.com/liyunlong/sub2api-cookie-plugin/internal/prober"
)

// This allowlist must never include request headers, URLs, question text or
// answers. Generations correlate observations, not backend or model identity.
type probeObservation struct {
	accountID          int64
	templateGeneration uint64
	templateCreatedAt  time.Time
	cookieGeneration   uint64
	probe              int64
	qualityRerolls     int64
	verdict            prober.Verdict
	action             string
	reasoningTokens    int
	observedAt         time.Time
	nextProbeAt        time.Time
	retryNotBefore     time.Time
}

func newProbeObservation(tmpl *probeTemplate, state *prober.State, decision prober.Decision, now time.Time) probeObservation {
	action := "scheduled"
	switch {
	case decision.ShouldReroll:
		action = "reroll"
	case decision.EnterBackoff:
		action = "backoff"
	case state.RetryNotBefore.After(now):
		action = "retry_later"
	}
	return probeObservation{
		accountID:          state.AccountID,
		templateGeneration: tmpl.Generation,
		templateCreatedAt:  tmpl.CreatedAt,
		cookieGeneration:   tmpl.CookieVersion,
		probe:              state.Probes,
		qualityRerolls:     state.QualityRerolls,
		verdict:            state.LastVerdict,
		action:             action,
		reasoningTokens:    state.LastReasoningTokens,
		observedAt:         now,
		nextProbeAt:        maxTime(state.NextProbeAt, maxTime(state.BackoffUntil, state.RetryNotBefore)),
		retryNotBefore:     state.RetryNotBefore,
	}
}

func (observation probeObservation) log(logger *slog.Logger) {
	logger.Info("quality_probe_observation",
		"account_id", observation.accountID,
		"template_generation", observation.templateGeneration,
		"template_created_at", observation.templateCreatedAt,
		"cookie_generation", observation.cookieGeneration,
		"probe", observation.probe,
		"quality_rerolls", observation.qualityRerolls,
		"verdict", observation.verdict,
		"action", observation.action,
		"reasoning_tokens", observation.reasoningTokens,
		"observed_at", observation.observedAt,
		"next_probe_at", observation.nextProbeAt,
		"retry_not_before", observation.retryNotBefore)
}
