-- Retire the import-time proxy qualification gate. Resume only accounts still
-- waiting for their first successful qualification, with no manual pause or
-- confirmed qualification failure. Other account errors and pauses stay owned
-- by their existing recovery mechanisms.
WITH resumed AS (
    UPDATE accounts a
    SET schedulable = TRUE, updated_at = clock_timestamp()
    WHERE a.platform = 'openai' AND a.type = 'oauth'
        AND a.parent_account_id IS NULL AND a.deleted_at IS NULL
        AND a.status = 'active' AND NOT a.schedulable
        AND a.extra ->> 'openai_downgrade_qualification' = 'true'
        AND (a.auto_pause_on_expired IS NOT TRUE OR a.expires_at IS NULL OR a.expires_at > NOW())
        AND NOT EXISTS (
            SELECT 1 FROM openai_downgrade_probe_controls c
            WHERE c.account_id = a.id AND c.manual_paused
        )
        AND NOT EXISTS (
            SELECT 1 FROM openai_downgrade_probe_states s
            WHERE s.account_id = a.id AND
                (s.state <> 'on_duty' OR s.probe_mode <> 'qualification')
        )
        AND NOT EXISTS (
            SELECT 1 FROM openai_downgrade_probe_results p
            WHERE p.account_id = a.id AND p.mode = 'qualification'
                AND p.transport_ok AND (
                    p.http_status >= 400 OR p.answer_correct IS FALSE
                )
        )
    RETURNING a.id
)
UPDATE openai_downgrade_probe_states s
SET probe_mode = 'normal', consecutive_failures = 0, consecutive_successes = 0,
    first_failure_at = NULL, next_probe_at = NOW() + INTERVAL '30 minutes',
    updated_at = clock_timestamp()
FROM resumed r WHERE s.account_id = r.id;

WITH changed AS (
    UPDATE accounts
    SET extra = COALESCE(extra, '{}'::jsonb)
            - 'openai_downgrade_qualification' - 'openai_oauth_qualified_proxy_id',
        updated_at = clock_timestamp()
    WHERE platform = 'openai' AND type = 'oauth' AND parent_account_id IS NULL
        AND (extra ? 'openai_downgrade_qualification' OR extra ? 'openai_oauth_qualified_proxy_id')
    RETURNING id, deleted_at
)
INSERT INTO scheduler_outbox(event_type, account_id)
SELECT 'account_changed', id FROM changed WHERE deleted_at IS NULL;
