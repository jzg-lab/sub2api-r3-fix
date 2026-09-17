CREATE TABLE openai_downgrade_probe_controls (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    manual_paused BOOLEAN NOT NULL DEFAULT FALSE,
    owned_error TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- Historical pauses have no trustworthy owner. Preserve them rather than
-- interpreting an old qualification marker as permission to resume.
INSERT INTO openai_downgrade_probe_controls(account_id, manual_paused)
SELECT id, TRUE FROM accounts
WHERE platform = 'openai' AND type = 'oauth' AND parent_account_id IS NULL
    AND deleted_at IS NULL AND schedulable IS FALSE;

-- Even an explicit rewrite of the same error text revokes prior ownership.
-- Probe commits establish their ownership only after their account write.
CREATE OR REPLACE FUNCTION revoke_openai_probe_error_ownership()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    UPDATE openai_downgrade_probe_controls
    SET owned_error = NULL, updated_at = clock_timestamp()
    WHERE account_id = NEW.id AND owned_error IS NOT NULL;
    RETURN NEW;
END;
$$;

CREATE TRIGGER accounts_revoke_probe_error_ownership
AFTER UPDATE OF status, error_message ON accounts
FOR EACH ROW EXECUTE FUNCTION revoke_openai_probe_error_ownership();

-- A manual resume updates both rows in one transaction. Other writers cannot
-- accidentally schedule a paused account, including ClearError and raw SQL.
CREATE OR REPLACE FUNCTION enforce_openai_probe_manual_pause()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    target_id BIGINT;
BEGIN
    IF TG_TABLE_NAME = 'accounts' THEN
        target_id := NEW.id;
    ELSE
        target_id := NEW.account_id;
    END IF;
    IF EXISTS (
        SELECT 1 FROM accounts a
        JOIN openai_downgrade_probe_controls c ON c.account_id = a.id
        WHERE a.id = target_id AND a.deleted_at IS NULL
            AND a.schedulable AND c.manual_paused
    ) THEN
        RAISE EXCEPTION 'manually paused account cannot be scheduled'
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER accounts_enforce_probe_manual_pause
AFTER INSERT OR UPDATE ON accounts DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION enforce_openai_probe_manual_pause();

CREATE CONSTRAINT TRIGGER controls_enforce_probe_manual_pause
AFTER INSERT OR UPDATE ON openai_downgrade_probe_controls DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION enforce_openai_probe_manual_pause();
