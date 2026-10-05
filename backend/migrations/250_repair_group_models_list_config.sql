-- Some legacy deployments recorded 143 but no longer have its physical column.
-- Repair forward without rewriting that ledger or replacing existing settings.
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS models_list_config JSONB NOT NULL DEFAULT '{}'::jsonb;
