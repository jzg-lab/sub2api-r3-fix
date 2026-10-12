-- Preserve the legacy column for rollback; copy it only when introducing the new column.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema() AND table_name = 'groups' AND column_name = 'model_allowlist'
    ) THEN
        ALTER TABLE groups ADD COLUMN model_allowlist JSONB NOT NULL DEFAULT '{}'::jsonb;
        UPDATE groups SET model_allowlist = models_list_config;
    END IF;
END
$$;

ALTER TABLE groups ADD COLUMN IF NOT EXISTS codex_models_manifest_config JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Synchronize writes from either binary so a cleared config cannot revive after rollback/re-upgrade.
CREATE OR REPLACE FUNCTION sync_group_model_allowlist_legacy() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.model_allowlist = '{}'::jsonb THEN
            NEW.model_allowlist := NEW.models_list_config;
        ELSE
            NEW.models_list_config := NEW.model_allowlist;
        END IF;
    ELSIF NEW.model_allowlist IS DISTINCT FROM OLD.model_allowlist THEN
        NEW.models_list_config := NEW.model_allowlist;
    ELSIF NEW.models_list_config IS DISTINCT FROM OLD.models_list_config THEN
        NEW.model_allowlist := NEW.models_list_config;
    END IF;
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS groups_sync_model_allowlist_legacy ON groups;
CREATE TRIGGER groups_sync_model_allowlist_legacy
    BEFORE INSERT OR UPDATE OF model_allowlist, models_list_config ON groups
    FOR EACH ROW EXECUTE FUNCTION sync_group_model_allowlist_legacy();

COMMENT ON COLUMN groups.model_allowlist IS 'Model listing and request admission allowlist; legacy column retained for binary rollback';
COMMENT ON COLUMN groups.codex_models_manifest_config IS 'Pinned account IDs for OpenAI model discovery, with optional scheduler fallback';
