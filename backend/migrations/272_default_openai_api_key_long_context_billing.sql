-- Adopt the official API Key default. OAuth and SetupToken accounts remain exempt,
-- and explicitly enabled accounts are left unchanged.
UPDATE accounts
SET extra = jsonb_set(
    COALESCE(extra, '{}'::jsonb),
    '{openai_long_context_billing_enabled}',
    'true'::jsonb,
    true
)
WHERE platform = 'openai'
  AND type = 'apikey'
  AND extra->'openai_long_context_billing_enabled' = 'false'::jsonb;
