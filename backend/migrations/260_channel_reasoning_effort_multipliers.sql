-- New optional pricing maps; existing prices and historical bills are unchanged.
ALTER TABLE channel_model_pricing
    ADD COLUMN IF NOT EXISTS reasoning_effort_multipliers JSONB NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE channel_account_stats_model_pricing
    ADD COLUMN IF NOT EXISTS reasoning_effort_multipliers JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Group pricing stores the same entries as JSON. An existing generic map wins,
-- even when empty. Remove the consumed legacy key so replay cannot revive it.
UPDATE groups AS g
SET model_pricing = (
    SELECT jsonb_agg(
        CASE
            WHEN jsonb_typeof(entry) = 'object' AND entry ? 'max_reasoning_effort_multiplier' THEN
                (entry - 'max_reasoning_effort_multiplier') ||
                CASE
                    WHEN NOT (entry ? 'reasoning_effort_multipliers')
                         AND jsonb_typeof(entry->'max_reasoning_effort_multiplier') = 'number' THEN
                        jsonb_build_object('reasoning_effort_multipliers',
                            jsonb_build_object('max', entry->'max_reasoning_effort_multiplier'))
                    ELSE '{}'::jsonb
                END
            ELSE entry
        END ORDER BY ordinal
    )
    FROM jsonb_array_elements(g.model_pricing) WITH ORDINALITY AS pricing(entry, ordinal)
)
WHERE jsonb_typeof(g.model_pricing) = 'array'
  AND EXISTS (
      SELECT 1
      FROM jsonb_array_elements(
          CASE WHEN jsonb_typeof(g.model_pricing) = 'array' THEN g.model_pricing ELSE '[]'::jsonb END
      ) AS pricing(entry)
      WHERE jsonb_typeof(entry) = 'object' AND entry ? 'max_reasoning_effort_multiplier'
  );

COMMENT ON COLUMN channel_model_pricing.reasoning_effort_multipliers IS
    'Custom billing multipliers by reasoning effort; omitted efforts use 1x';
COMMENT ON COLUMN channel_account_stats_model_pricing.reasoning_effort_multipliers IS
    'Custom account statistics billing multipliers by reasoning effort; omitted efforts use 1x';
