-- PostgreSQL 16+. Run with psql -v rescue_group_id=<confirmed group ID> -f <this file>.
-- Use the recorded previous group ID if the current rescue configuration was cleared.
-- This report never reads credentials or modifies accounts.
\set ON_ERROR_STOP on
\if :{?rescue_group_id}
\else
\echo 'Provide the confirmed rescue_group_id before running this report.'
\quit
\endif

BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL statement_timeout = '30s';
SET LOCAL lock_timeout = '3s';

WITH bindings AS (
    SELECT account_id, array_agg(group_id ORDER BY group_id) AS group_ids
    FROM account_groups GROUP BY account_id
), snapshots AS (
    SELECT a.id, a.status, a.schedulable, a.expires_at, a.auto_pause_on_expired,
           a.extra -> 'openai_rescue_lane' AS marker,
           a.extra ->> 'openai_rescue_terminated_at' AS terminated_at,
           COALESCE(c.manual_paused, FALSE) AS manual_paused,
           COALESCE(b.group_ids, ARRAY[]::bigint[]) AS current_group_ids,
           s.state, s.probe_mode
    FROM accounts a
    LEFT JOIN bindings b ON b.account_id = a.id
    LEFT JOIN openai_downgrade_probe_states s ON s.account_id = a.id
    LEFT JOIN openai_downgrade_probe_controls c ON c.account_id = a.id
    WHERE a.deleted_at IS NULL AND a.platform = 'openai'
      AND a.type = 'oauth' AND a.parent_account_id IS NULL
), assessed AS (
    SELECT *, array_remove(ARRAY[
        CASE WHEN terminated_at IS NOT NULL AND (
                  schedulable OR NOT manual_paused
                  OR (marker IS NOT NULL AND marker <> 'null'::jsonb))
             THEN 'manual_termination_state_requires_review' END,
        CASE WHEN marker IS NOT NULL AND marker <> 'null'::jsonb
                  AND state = 'on_duty' AND probe_mode = 'normal'
             THEN 'qualified_marker_pending_graduation' END,
        CASE WHEN :'rescue_group_id'::bigint = ANY(current_group_ids)
                  AND (marker IS NULL OR marker = 'null'::jsonb)
             THEN 'rescue_group_without_marker' END,
        CASE WHEN marker IS NOT NULL AND marker <> 'null'::jsonb AND (
                  jsonb_typeof(marker) <> 'object'
                  OR jsonb_typeof(marker -> 'entered_at') IS DISTINCT FROM 'string'
                  OR NOT COALESCE(pg_input_is_valid(marker ->> 'entered_at', 'timestamp with time zone'), false))
             THEN 'invalid_marker_or_entered_at' END,
        CASE WHEN jsonb_typeof(marker) = 'object' AND (
                  NOT (marker ? 'orig_group_ids')
                  OR jsonb_typeof(marker -> 'orig_group_ids') NOT IN ('array', 'null')
                  OR EXISTS (
                      SELECT 1 FROM jsonb_array_elements(CASE
                          WHEN jsonb_typeof(marker -> 'orig_group_ids') = 'array'
                          THEN marker -> 'orig_group_ids' ELSE '[]'::jsonb END) AS e(value)
                      WHERE jsonb_typeof(value) <> 'number'
                         OR value #>> '{}' !~ '^[1-9][0-9]*$'
                         OR length(value #>> '{}') > 19
                         OR CASE WHEN value #>> '{}' ~ '^[1-9][0-9]*$'
                                 THEN (value #>> '{}')::numeric > 9223372036854775807
                                 ELSE false END))
             THEN 'invalid_original_group_snapshot' END,
        CASE WHEN EXISTS (
                  SELECT 1 FROM jsonb_array_elements(CASE
                      WHEN jsonb_typeof(marker -> 'orig_group_ids') = 'array'
                      THEN marker -> 'orig_group_ids' ELSE '[]'::jsonb END) AS e(value)
                  WHERE NOT EXISTS (SELECT 1 FROM groups g
                      WHERE g.id::text = value #>> '{}' AND g.deleted_at IS NULL))
             THEN 'original_group_missing_or_deleted' END,
        CASE WHEN jsonb_typeof(marker -> 'orig_group_ids') = 'array' AND (
                  SELECT count(*) <> count(DISTINCT value)
                  FROM jsonb_array_elements(marker -> 'orig_group_ids') AS e(value))
             THEN 'duplicate_original_group_ids' END,
        CASE WHEN marker IS NOT NULL AND marker <> 'null'::jsonb AND
                  (status <> 'active' OR (auto_pause_on_expired AND expires_at <= now()))
             THEN 'marked_account_requires_eligibility_review' END,
        CASE WHEN COALESCE(marker ->> 'exit_reason', '') <> ''
             THEN 'auth_rejected_exit_in_progress' END
    ]::text[], NULL) AS findings
    FROM snapshots
)
SELECT id AS account_id, status, schedulable, state, probe_mode,
       manual_paused, terminated_at,
       current_group_ids, marker -> 'orig_group_ids' AS original_group_ids,
       marker ->> 'entered_at' AS entered_at, findings
FROM assessed
WHERE cardinality(findings) > 0
ORDER BY id;

ROLLBACK;
