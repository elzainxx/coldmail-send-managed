-- send_managed_0009_create_send_logs.up.sql — Phase 1: send_managed.send_logs (full-content debug logs,
-- retention-bounded). Write-once: a BEFORE UPDATE reject-trigger + REVOKE UPDATE. Unlike
-- job_events, DELETE is RETAINED for send_managed_app (the Phase 7 retention sweep needs it).
-- Bodies/subject/headers are NOT encrypted — retention (ttl_expires_at) is the sole privacy
-- control (DECIDED). The REVOKE UPDATE lands in THIS migration (ADR-0002 ordering mandate).
-- Owned by send_managed_owner.

CREATE TABLE send_managed.send_logs (
    -- Identity
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id UUID NOT NULL,
    user_id UUID NOT NULL,

    -- Recipient and content (denormalized for log queries)
    recipient_email TEXT NOT NULL,
    subject TEXT NOT NULL,
    body TEXT NOT NULL,

    -- SMTP details
    headers JSONB NOT NULL,
    smtp_response TEXT,

    -- Retention (pre-calculated expiry)
    ttl_expires_at TIMESTAMPTZ NOT NULL,

    -- Timestamp
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Constraints
    CONSTRAINT send_logs_job_fk
        FOREIGN KEY (job_id)
        REFERENCES send_managed.jobs(id)
        ON DELETE CASCADE,

    CONSTRAINT send_logs_user_fk
        FOREIGN KEY (user_id)
        REFERENCES public.users(id)
        ON DELETE CASCADE
);

ALTER TABLE send_managed.send_logs OWNER TO send_managed_owner;

-- Indexes for querying and retention
CREATE INDEX send_logs_job_id_idx              ON send_managed.send_logs (job_id);
CREATE INDEX send_logs_user_id_created_at_idx  ON send_managed.send_logs (user_id, created_at);
CREATE INDEX send_logs_ttl_expires_at_idx      ON send_managed.send_logs (ttl_expires_at);
CREATE INDEX send_logs_recipient_email_idx     ON send_managed.send_logs (recipient_email);

-- Write-once: reject UPDATE (defense in depth alongside the REVOKE below).
CREATE OR REPLACE FUNCTION send_managed.fn_send_logs_reject_update()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'send_managed.send_logs is immutable: UPDATE forbidden'
        USING HINT = 'Logs are write-once records. Delete and re-insert if needed.',
              ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
ALTER FUNCTION send_managed.fn_send_logs_reject_update() OWNER TO send_managed_owner;

CREATE TRIGGER trg_send_logs_reject_update
    BEFORE UPDATE ON send_managed.send_logs
    FOR EACH ROW
    EXECUTE FUNCTION send_managed.fn_send_logs_reject_update();

-- Revoke UPDATE only (DELETE retained for the Phase 7 retention sweep + cascade cleanup).
REVOKE UPDATE ON send_managed.send_logs FROM send_managed_app;

INSERT INTO send_managed.schema_migrations (version) VALUES ('0009');
