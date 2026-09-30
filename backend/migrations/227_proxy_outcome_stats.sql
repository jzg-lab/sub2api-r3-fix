-- Proxy outcome statistics: ranxi node-learning pattern adapted to bucket
-- proxies. Every downgrade-probe outcome is aggregated per proxy in a rolling
-- 24h tumbling window and synced into proxies.bucket_risk_score while
-- auto_score_enabled is TRUE, so bucket selection orders by live behavior
-- instead of hand-filled values. Set auto_score_enabled = FALSE on a row to
-- pin that proxy back to manual scoring.
CREATE TABLE IF NOT EXISTS proxy_outcome_stats (
    id BIGSERIAL PRIMARY KEY,
    proxy_id BIGINT NOT NULL UNIQUE REFERENCES proxies(id) ON DELETE CASCADE,
    probe_successes BIGINT NOT NULL DEFAULT 0,
    probe_degraded BIGINT NOT NULL DEFAULT 0,
    auth_errors BIGINT NOT NULL DEFAULT 0,
    network_errors BIGINT NOT NULL DEFAULT 0,
    inconclusive BIGINT NOT NULL DEFAULT 0,
    consecutive_failures INT NOT NULL DEFAULT 0,
    last_success_at TIMESTAMPTZ,
    last_failure_at TIMESTAMPTZ,
    latency_ms_sum BIGINT NOT NULL DEFAULT 0,
    latency_ms_count BIGINT NOT NULL DEFAULT 0,
    last_result VARCHAR(32) NOT NULL DEFAULT '',
    auto_score_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    window_start TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_proxy_outcome_stats_failures
    ON proxy_outcome_stats (consecutive_failures DESC)
    WHERE consecutive_failures > 0;

COMMENT ON TABLE proxy_outcome_stats IS
    'Rolling 24h outcome stats per bucket proxy; syncs proxies.bucket_risk_score when auto_score_enabled';
