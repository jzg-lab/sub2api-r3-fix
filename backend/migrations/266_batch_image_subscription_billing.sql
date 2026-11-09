-- Existing jobs retain wallet settlement; new subscription jobs pin their quota source.
ALTER TABLE batch_image_jobs
    ADD COLUMN IF NOT EXISTS billing_snapshot JSONB,
    ADD COLUMN IF NOT EXISTS billing_hold_state VARCHAR(16) NOT NULL DEFAULT ''
        CHECK (billing_hold_state IN ('', 'held', 'captured', 'released'));
