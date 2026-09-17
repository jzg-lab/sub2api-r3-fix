-- Every cached account generation must advance after both direct and related
-- writes. NOW() is a transaction start time and can move backwards at commit.
CREATE OR REPLACE FUNCTION advance_scheduler_account_revision()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := GREATEST(clock_timestamp(), OLD.updated_at + INTERVAL '1 microsecond',
                               NEW.updated_at);
    RETURN NEW;
END;
$$;

CREATE TRIGGER accounts_scheduler_revision
BEFORE UPDATE ON accounts
FOR EACH ROW EXECUTE FUNCTION advance_scheduler_account_revision();

-- Keep the parent revision and its notification in the writer's transaction.
-- This also covers raw SQL, bulk changes and cascades outside repository helpers.
CREATE OR REPLACE FUNCTION touch_scheduler_related_accounts(account_ids BIGINT[], group_ids BIGINT[])
RETURNS VOID LANGUAGE plpgsql AS $$
DECLARE
    affected_id BIGINT;
BEGIN
    FOR affected_id IN
        SELECT id FROM accounts
        WHERE id = ANY(account_ids) AND deleted_at IS NULL
        ORDER BY id FOR NO KEY UPDATE
    LOOP
        UPDATE accounts SET updated_at = clock_timestamp() WHERE id = affected_id;
        IF group_ids IS NULL THEN
            INSERT INTO scheduler_outbox(event_type, account_id)
            VALUES ('account_changed', affected_id);
        ELSE
            INSERT INTO scheduler_outbox(event_type, account_id, payload)
            VALUES ('account_groups_changed', affected_id, jsonb_build_object('group_ids', group_ids));
        END IF;
    END LOOP;
END;
$$;

CREATE OR REPLACE FUNCTION invalidate_scheduler_account_groups()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    affected_accounts BIGINT[];
    affected_groups BIGINT[];
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT array_agg(DISTINCT account_id), array_agg(DISTINCT group_id)
        INTO affected_accounts, affected_groups FROM new_bindings;
    ELSIF TG_OP = 'DELETE' THEN
        SELECT array_agg(DISTINCT account_id), array_agg(DISTINCT group_id)
        INTO affected_accounts, affected_groups FROM old_bindings;
    ELSE
        SELECT array_agg(DISTINCT account_id), array_agg(DISTINCT group_id)
        INTO affected_accounts, affected_groups
        FROM (SELECT account_id, group_id FROM old_bindings
              UNION SELECT account_id, group_id FROM new_bindings) AS bindings;
    END IF;
    PERFORM touch_scheduler_related_accounts(affected_accounts, affected_groups);
    RETURN NULL;
END;
$$;

CREATE TRIGGER account_groups_scheduler_insert
AFTER INSERT ON account_groups REFERENCING NEW TABLE AS new_bindings
FOR EACH STATEMENT EXECUTE FUNCTION invalidate_scheduler_account_groups();
CREATE TRIGGER account_groups_scheduler_update
AFTER UPDATE ON account_groups REFERENCING OLD TABLE AS old_bindings NEW TABLE AS new_bindings
FOR EACH STATEMENT EXECUTE FUNCTION invalidate_scheduler_account_groups();
CREATE TRIGGER account_groups_scheduler_delete
AFTER DELETE ON account_groups REFERENCING OLD TABLE AS old_bindings
FOR EACH STATEMENT EXECUTE FUNCTION invalidate_scheduler_account_groups();

CREATE OR REPLACE FUNCTION invalidate_scheduler_proxy_accounts()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    affected_accounts BIGINT[];
BEGIN
    IF NEW IS NOT DISTINCT FROM OLD THEN
        RETURN NEW;
    END IF;
    SELECT array_agg(id) INTO affected_accounts FROM accounts
    WHERE proxy_id = NEW.id AND deleted_at IS NULL;
    PERFORM touch_scheduler_related_accounts(affected_accounts, NULL);
    RETURN NEW;
END;
$$;

CREATE TRIGGER proxies_scheduler_update
AFTER UPDATE ON proxies
FOR EACH ROW EXECUTE FUNCTION invalidate_scheduler_proxy_accounts();

CREATE OR REPLACE FUNCTION invalidate_scheduler_group_accounts()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
    affected_accounts BIGINT[];
BEGIN
    IF NEW IS NOT DISTINCT FROM OLD THEN
        RETURN NEW;
    END IF;
    SELECT array_agg(account_id) INTO affected_accounts FROM account_groups WHERE group_id = NEW.id;
    PERFORM touch_scheduler_related_accounts(affected_accounts, ARRAY[NEW.id]);
    INSERT INTO scheduler_outbox(event_type, group_id) VALUES ('group_changed', NEW.id);
    RETURN NEW;
END;
$$;

CREATE TRIGGER groups_scheduler_update
AFTER UPDATE ON groups
FOR EACH ROW EXECUTE FUNCTION invalidate_scheduler_group_accounts();
