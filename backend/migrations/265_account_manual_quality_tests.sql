-- Manual diagnostics are deliberately separate from probe/scheduling state.
CREATE TABLE IF NOT EXISTS account_manual_quality_tests (
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    model VARCHAR(200) NOT NULL,
    kind VARCHAR(16) NOT NULL CHECK (kind IN ('candy', 'pelican')),
    run_id UUID NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    running_until TIMESTAMPTZ,
    result JSONB NOT NULL,
    PRIMARY KEY (account_id, model, kind)
);
CREATE INDEX IF NOT EXISTS idx_account_manual_quality_latest
    ON account_manual_quality_tests (account_id, started_at DESC);
