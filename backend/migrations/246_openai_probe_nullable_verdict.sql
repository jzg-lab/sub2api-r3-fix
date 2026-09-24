-- A transport-interrupted probe has no judgeable answer. Preserve that state as
-- NULL instead of collapsing it into "wrong", and reject future mixed records.
ALTER TABLE openai_downgrade_probe_results
    ALTER COLUMN answer_correct DROP NOT NULL,
    ALTER COLUMN answer_correct DROP DEFAULT;

UPDATE openai_downgrade_probe_results
SET answer_correct = NULL
WHERE transport_ok IS FALSE
  AND answer_correct IS NOT NULL;

ALTER TABLE openai_downgrade_probe_results
    DROP CONSTRAINT IF EXISTS openai_downgrade_probe_results_verdict_check;

ALTER TABLE openai_downgrade_probe_results
    ADD CONSTRAINT openai_downgrade_probe_results_verdict_check
    CHECK (
        (transport_ok IS TRUE AND answer_correct IS NOT NULL)
        OR
        (transport_ok IS FALSE AND answer_correct IS NULL)
    );
