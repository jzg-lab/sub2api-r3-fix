-- OpenAI downgrade probe, circuit-breaker, and proxy-bucket state.
-- Probe rows are deliberately separate from usage_logs: they are not billable
-- traffic and must never enter user-facing usage aggregation.

ALTER TABLE proxies
    ADD COLUMN IF NOT EXISTS bucket_role VARCHAR(20) NOT NULL DEFAULT 'main',
    ADD COLUMN IF NOT EXISTS exit_ip VARCHAR(64),
    ADD COLUMN IF NOT EXISTS bucket_capacity INT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS bucket_risk_score INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS bucket_enabled BOOLEAN NOT NULL DEFAULT TRUE;

-- The v1.5 policy caps every exit IP at three active accounts. Keep the
-- database default aligned with the scheduler's hard upper bound.
ALTER TABLE proxies
    ALTER COLUMN bucket_capacity SET DEFAULT 1;

UPDATE proxies
SET bucket_capacity = LEAST(GREATEST(bucket_capacity, 1), 3)
WHERE bucket_capacity IS NULL OR bucket_capacity < 1 OR bucket_capacity > 3;

ALTER TABLE proxies
    DROP CONSTRAINT IF EXISTS proxies_bucket_role_check;

ALTER TABLE proxies
    ADD CONSTRAINT proxies_bucket_role_check
    CHECK (bucket_role IN ('main', 'escape', 'fallback'));

CREATE INDEX IF NOT EXISTS idx_proxies_bucket_role_enabled
    ON proxies(bucket_role, bucket_enabled, status)
    WHERE deleted_at IS NULL;

-- Older deployments used a database trigger to force every OpenAI account to
-- one proxy.  The probe scheduler needs legal, explicit per-account rebinding.
DROP TRIGGER IF EXISTS accounts_bind_openai_proxy ON accounts;

CREATE TABLE IF NOT EXISTS openai_downgrade_probe_states (
    account_id              BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    state                   VARCHAR(32) NOT NULL DEFAULT 'on_duty',
    probe_mode              VARCHAR(24) NOT NULL DEFAULT 'normal',
    original_proxy_id       BIGINT REFERENCES proxies(id) ON DELETE SET NULL,
    current_proxy_id        BIGINT REFERENCES proxies(id) ON DELETE SET NULL,
    consecutive_failures    INT NOT NULL DEFAULT 0,
    consecutive_successes   INT NOT NULL DEFAULT 0,
    first_failure_at        TIMESTAMPTZ,
    circuit_opened_at       TIMESTAMPTZ,
    recovery_deadline       TIMESTAMPTZ,
    next_probe_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    swap_count_7d           INT NOT NULL DEFAULT 0,
    last_swap_at            TIMESTAMPTZ,
    last_probe_at           TIMESTAMPTZ,
    astra_consecutive_failures INT NOT NULL DEFAULT 0,
    astra_consecutive_successes INT NOT NULL DEFAULT 0,
    astra_next_probe_at     TIMESTAMPTZ,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT openai_downgrade_probe_states_state_check
        CHECK (state IN ('on_duty', 'circuit_open', 'reprobe', 'pending_replace')),
    CONSTRAINT openai_downgrade_probe_states_mode_check
        CHECK (probe_mode IN ('normal', 'qualification', 'half_open', 'accelerated'))
);

ALTER TABLE openai_downgrade_probe_states
    ADD COLUMN IF NOT EXISTS probe_mode VARCHAR(24) NOT NULL DEFAULT 'normal';

ALTER TABLE openai_downgrade_probe_states
    DROP CONSTRAINT IF EXISTS openai_downgrade_probe_states_mode_check;

ALTER TABLE openai_downgrade_probe_states
    ADD CONSTRAINT openai_downgrade_probe_states_mode_check
    CHECK (probe_mode IN ('normal', 'qualification', 'half_open', 'accelerated', 'sol_fallback'));

ALTER TABLE openai_downgrade_probe_states
    ADD COLUMN IF NOT EXISTS astra_consecutive_failures INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS astra_consecutive_successes INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS astra_next_probe_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_openai_downgrade_probe_states_due
    ON openai_downgrade_probe_states(next_probe_at, state);

CREATE TABLE IF NOT EXISTS openai_downgrade_probe_results (
    id                  BIGSERIAL PRIMARY KEY,
    account_id          BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    proxy_id            BIGINT REFERENCES proxies(id) ON DELETE SET NULL,
    mode                VARCHAR(20) NOT NULL DEFAULT 'normal',
    probe                BOOLEAN NOT NULL DEFAULT TRUE,
    transport_ok        BOOLEAN NOT NULL DEFAULT FALSE,
    answer_correct      BOOLEAN NOT NULL DEFAULT FALSE,
    reasoning_tokens    INT,
    juice               INT,
    latency_ms          INT,
    http_status         INT,
    error_message       TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_openai_downgrade_probe_results_account_time
    ON openai_downgrade_probe_results(account_id, created_at DESC);

CREATE TABLE IF NOT EXISTS openai_downgrade_probe_events (
    id              BIGSERIAL PRIMARY KEY,
    account_id      BIGINT REFERENCES accounts(id) ON DELETE CASCADE,
    proxy_id        BIGINT REFERENCES proxies(id) ON DELETE SET NULL,
    event_type      VARCHAR(40) NOT NULL,
    details         JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_openai_downgrade_probe_events_time
    ON openai_downgrade_probe_events(created_at DESC);
