-- Read-only rollback prerequisite. Keep the additive columns and all accounting data.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM batch_image_jobs
        WHERE billing_snapshot IS NOT NULL
          AND (billing_hold_state NOT IN ('captured', 'released')
               OR status NOT IN ('completed', 'failed', 'cancelled', 'output_deleted'))
    ) THEN
        RAISE EXCEPTION 'subscription batch accounting is still pending; an older worker must not take ownership';
    END IF;
END $$;
