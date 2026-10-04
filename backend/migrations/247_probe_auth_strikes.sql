-- Probe auth-failure strikes: a probe 401/403 no longer insta-kills the
-- account. The first strike pauses scheduling and schedules a fast recheck;
-- SetError only lands after repeated consecutive strikes.
ALTER TABLE openai_downgrade_probe_states
    ADD COLUMN IF NOT EXISTS auth_consecutive_failures INT NOT NULL DEFAULT 0;

COMMENT ON COLUMN openai_downgrade_probe_states.auth_consecutive_failures IS
    'Consecutive probe 401/403 strikes; 2 strikes graduate to account error state';
