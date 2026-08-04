-- send_managed_0007_create_jobs.up.sql — Phase 1: send_managed.jobs (core state-machine table).
-- job_item_id is UNIQUE (exactly one send_managed job per platform job_item). provider_message_id is
-- UNIQUE (idempotency). CHECK constraints tie lease/completed_at validity to state. Owned by
-- send_managed_owner. FKs to workers/accounts SET NULL (a deleted worker/account requeues, never orphans).

CREATE TABLE send_managed.jobs (
    -- Identity
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_item_id UUID NOT NULL,
    campaign_id UUID NOT NULL,
    user_id UUID NOT NULL,

    -- Recipient and content (denormalized, frozen at creation)
    recipient_email TEXT NOT NULL,
    rendered_subject TEXT NOT NULL,
    rendered_body TEXT NOT NULL,
    reply_to TEXT,

    -- State machine
    state send_managed.job_state NOT NULL DEFAULT 'queued',
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Retry and attempt tracking
    attempt_count SMALLINT NOT NULL DEFAULT 0,
    max_attempts SMALLINT NOT NULL DEFAULT 3,

    -- Assignment and lease
    assigned_box_id UUID,
    assigned_account_id UUID,
    lease_expires_at TIMESTAMPTZ,

    -- Outcome tracking
    provider_message_id TEXT UNIQUE,
    last_error_class send_managed.error_class,
    last_error_message TEXT,

    -- Timestamps
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,

    -- Constraints
    CONSTRAINT jobs_job_item_fk
        FOREIGN KEY (job_item_id)
        REFERENCES public.job_items(id)
        ON DELETE CASCADE,

    -- Exactly one send_managed job per platform job_item; a 2nd insert for the same job_item_id fails.
    CONSTRAINT jobs_job_item_id_unique
        UNIQUE (job_item_id),

    CONSTRAINT jobs_assigned_box_fk
        FOREIGN KEY (assigned_box_id)
        REFERENCES send_managed.boxes(id)
        ON DELETE SET NULL,

    CONSTRAINT jobs_assigned_account_fk
        FOREIGN KEY (assigned_account_id)
        REFERENCES send_managed.accounts(id)
        ON DELETE SET NULL,

    CONSTRAINT jobs_attempt_count_check
        CHECK (attempt_count >= 0 AND attempt_count <= max_attempts),

    CONSTRAINT jobs_lease_valid_check
        CHECK (
            (state IN ('assigned', 'sending') AND lease_expires_at IS NOT NULL)
            OR (state NOT IN ('assigned', 'sending') AND lease_expires_at IS NULL)
        ),

    CONSTRAINT jobs_completed_at_valid_check
        CHECK (
            (state IN ('sent', 'failed', 'bounced') AND completed_at IS NOT NULL)
            OR (state NOT IN ('sent', 'failed', 'bounced') AND completed_at IS NULL)
        ),

    -- Cap error message length so pathological SMTP responses cannot bloat rows.
    CONSTRAINT jobs_last_error_message_len_check
        CHECK (last_error_message IS NULL OR char_length(last_error_message) <= 2000)
);

ALTER TABLE send_managed.jobs OWNER TO send_managed_owner;

INSERT INTO send_managed.schema_migrations (version) VALUES ('0007');
