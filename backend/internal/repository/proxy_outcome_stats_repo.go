package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// upsertProxyOutcomeStats folds one downgrade-probe outcome into the rolling
// per-proxy aggregate (proxy_outcome_stats) and re-syncs
// proxies.bucket_risk_score while auto_score_enabled is TRUE for the proxy.
//
// The window is a 24h tumbling window: the first outcome after expiry resets
// the counters instead of accumulating, so stale history cannot pin a risk
// score forever. Both statements are single-row and race-free on the
// proxy_id UNIQUE key; the score sync runs after the aggregate so it always
// reads the post-update counters.
func (r *openAIDowngradeProbeRepository) upsertProxyOutcomeStats(
	ctx context.Context,
	proxyID int64,
	result *service.OpenAIDowngradeProbeResult,
) error {
	outcome := service.ClassifyOpenAIDowngradeProxyOutcome(result)
	now := time.Now()
	var successDelta, degradedDelta, authDelta, networkDelta, inconclusiveDelta, failureDelta int64
	var latencySum, latencyCount int64
	var successAt, failureAt any
	switch outcome {
	case service.OpenAIProxyOutcomeSuccess:
		successDelta = 1
		successAt = now
		latencySum, latencyCount = result.Latency.Milliseconds(), 1
	case service.OpenAIProxyOutcomeDegraded:
		degradedDelta = 1
		failureDelta = 1
		failureAt = now
		latencySum, latencyCount = result.Latency.Milliseconds(), 1
	case service.OpenAIProxyOutcomeAuthError:
		authDelta = 1
		failureDelta = 1
		failureAt = now
	case service.OpenAIProxyOutcomeNetworkError:
		networkDelta = 1
		failureDelta = 1
		failureAt = now
		latencySum, latencyCount = result.Latency.Milliseconds(), 1
	default:
		inconclusiveDelta = 1
	}

	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO proxy_outcome_stats AS s
			(proxy_id, probe_successes, probe_degraded, auth_errors, network_errors,
			 inconclusive, consecutive_failures, last_success_at, last_failure_at,
			 latency_ms_sum, latency_ms_count, last_result, window_start, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)
		ON CONFLICT (proxy_id) DO UPDATE SET
			window_start = CASE WHEN s.window_start < $13 - INTERVAL '24 hours'
				THEN $13 ELSE s.window_start END,
			probe_successes = CASE WHEN s.window_start < $13 - INTERVAL '24 hours'
				THEN $2 ELSE s.probe_successes + $2 END,
			probe_degraded = CASE WHEN s.window_start < $13 - INTERVAL '24 hours'
				THEN $3 ELSE s.probe_degraded + $3 END,
			auth_errors = CASE WHEN s.window_start < $13 - INTERVAL '24 hours'
				THEN $4 ELSE s.auth_errors + $4 END,
			network_errors = CASE WHEN s.window_start < $13 - INTERVAL '24 hours'
				THEN $5 ELSE s.network_errors + $5 END,
			inconclusive = CASE WHEN s.window_start < $13 - INTERVAL '24 hours'
				THEN $6 ELSE s.inconclusive + $6 END,
			consecutive_failures = CASE
				WHEN s.window_start < $13 - INTERVAL '24 hours' THEN $7
				WHEN $12 = 'success' THEN 0
				ELSE s.consecutive_failures + $7 END,
			last_success_at = COALESCE($8, s.last_success_at),
			last_failure_at = COALESCE($9, s.last_failure_at),
			latency_ms_sum = CASE WHEN s.window_start < $13 - INTERVAL '24 hours'
				THEN $10 ELSE s.latency_ms_sum + $10 END,
			latency_ms_count = CASE WHEN s.window_start < $13 - INTERVAL '24 hours'
				THEN $11 ELSE s.latency_ms_count + $11 END,
			last_result = $12,
			updated_at = $13
	`,
		proxyID,
		successDelta, degradedDelta, authDelta, networkDelta, inconclusiveDelta,
		failureDelta,
		successAt, failureAt,
		latencySum, latencyCount,
		outcome,
		now,
	); err != nil {
		return err
	}

	// Risk score ladder (0 = pristine, 100 = avoid): each auth error costs 20
	// (capped at 3), each consecutive failure 10 (capped at 6), and the
	// failure rate contributes up to 30. A clean window scores 0.
	_, err := r.db.ExecContext(ctx, `
		UPDATE proxies p
		SET bucket_risk_score = sub.score, updated_at = NOW()
		FROM (
			SELECT s.proxy_id,
				LEAST(100,
					(LEAST(s.auth_errors, 3) * 20)
					+ (LEAST(s.consecutive_failures, 6) * 10)
					+ (CASE WHEN (s.probe_successes + s.probe_degraded + s.auth_errors + s.network_errors) = 0
						THEN 0
						ELSE CEIL(30.0 * (s.probe_degraded + s.auth_errors + s.network_errors)
							/ (s.probe_successes + s.probe_degraded + s.auth_errors + s.network_errors)) END)
				)::INT AS score
			FROM proxy_outcome_stats s
			WHERE s.proxy_id = $1 AND s.auto_score_enabled
		) sub
		WHERE p.id = sub.proxy_id AND p.deleted_at IS NULL
	`, proxyID)
	return err
}
