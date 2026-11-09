-- Empty configuration preserves every group's existing pricing behavior.
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS long_context_pricing JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMENT ON COLUMN groups.long_context_pricing IS
    'Optional default and exact-model long-context thresholds in tokens; rates and exemptions are unchanged';
