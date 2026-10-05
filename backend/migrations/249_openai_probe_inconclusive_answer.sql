-- Completed transport can still have an ambiguous final answer. Such a result
-- must remain NULL, not a wrong answer; failed transport still cannot be graded.
ALTER TABLE openai_downgrade_probe_results
    DROP CONSTRAINT IF EXISTS openai_downgrade_probe_results_verdict_check;

ALTER TABLE openai_downgrade_probe_results
    ADD CONSTRAINT openai_downgrade_probe_results_verdict_check
    CHECK (transport_ok IS TRUE OR answer_correct IS NULL);
