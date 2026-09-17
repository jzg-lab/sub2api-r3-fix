-- Keep rate-limit escalation across restarts and commit it with the probe state.
-- Historical in-memory counts are unknowable; do not infer them from unrelated
-- events or current proxy assignments.
ALTER TABLE openai_downgrade_probe_states
    ADD COLUMN IF NOT EXISTS consecutive_429s INT NOT NULL DEFAULT 0
        CHECK (consecutive_429s >= 0);
