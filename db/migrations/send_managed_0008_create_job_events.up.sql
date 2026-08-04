-- send_managed_0008_create_job_events.up.sql — Phase 1: send_managed.job_events (append-only audit log)
-- plus the two guarantees enforced at the DATABASE level:
--   (1) Append-only: REVOKE UPDATE, DELETE FROM send_managed_app + a BEFORE UPDATE reject-trigger.
--       There is deliberately NO BEFORE DELETE reject-trigger: Postgres fires row-level BEFORE
--       DELETE triggers on FK CASCADE, so such a trigger would block legitimate cascade cleanup
--       when a send_managed.jobs row is deleted. DELETE is denied by the revoked privilege instead
--       (system-performed cascade bypasses the app role's table privileges).
--   (2) One-event-per-transition: an AFTER INSERT trigger writes the genesis event; an
--       AFTER UPDATE OF state trigger writes exactly one event per change and RAISES if the
--       actor GUC is unset — so state cannot move silently.
-- The append-only REVOKE lands in THIS migration (same change that creates the table), per
-- ADR-0002's mandatory ordering note: the blanket default-privilege grant from the bootstrap
-- makes job_events born with full DML, so it must be narrowed here, never in a later migration.
-- All objects owned by send_managed_owner (ADR-0004).

CREATE TABLE send_managed.job_events (
    -- Identity
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id UUID NOT NULL,
    user_id UUID NOT NULL,

    -- Monotonic ordering (table-wide IDENTITY; safe for per-job chronology filtered by job_id)
    seq BIGINT GENERATED ALWAYS AS IDENTITY,

    -- State transition
    from_state send_managed.job_state,
    to_state send_managed.job_state NOT NULL,

    -- Provenance
    actor send_managed.event_actor NOT NULL,
    account_id UUID,
    box_id UUID,

    -- Error context (nullable, only for error transitions)
    error_class send_managed.error_class,
    error_message TEXT,

    -- Metadata extensibility
    metadata JSONB,

    -- Timestamp
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Constraints
    CONSTRAINT job_events_job_fk
        FOREIGN KEY (job_id)
        REFERENCES send_managed.jobs(id)
        ON DELETE CASCADE,

    CONSTRAINT job_events_user_fk
        FOREIGN KEY (user_id)
        REFERENCES public.users(id)
        ON DELETE CASCADE,

    CONSTRAINT job_events_account_fk
        FOREIGN KEY (account_id)
        REFERENCES send_managed.accounts(id)
        ON DELETE SET NULL,

    CONSTRAINT job_events_box_fk
        FOREIGN KEY (box_id)
        REFERENCES send_managed.boxes(id)
        ON DELETE SET NULL,

    -- Genesis event has NULL from_state -> queued; all later events change state.
    CONSTRAINT job_events_from_state_check
        CHECK (
            (from_state IS NULL AND to_state = 'queued')
            OR (from_state IS NOT NULL AND from_state != to_state)
        ),

    CONSTRAINT job_events_error_message_len_check
        CHECK (error_message IS NULL OR char_length(error_message) <= 2000)
);

ALTER TABLE send_managed.job_events OWNER TO send_managed_owner;

-- Indexes for querying and retention
CREATE INDEX job_events_job_id_seq_idx        ON send_managed.job_events (job_id, seq);
CREATE INDEX job_events_job_id_created_at_idx  ON send_managed.job_events (job_id, created_at);
CREATE INDEX job_events_created_at_idx         ON send_managed.job_events (created_at);
CREATE INDEX job_events_user_id_created_at_idx ON send_managed.job_events (user_id, created_at);
CREATE INDEX job_events_error_class_idx        ON send_managed.job_events (error_class) WHERE error_class IS NOT NULL;

-- (1) Append-only: BEFORE UPDATE reject-trigger (defense in depth alongside the REVOKE below).
CREATE OR REPLACE FUNCTION send_managed.fn_job_events_reject_update()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'send_managed.job_events is append-only: UPDATE forbidden'
        USING HINT = 'Events are immutable audit records',
              ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
ALTER FUNCTION send_managed.fn_job_events_reject_update() OWNER TO send_managed_owner;

CREATE TRIGGER trg_job_events_reject_update
    BEFORE UPDATE ON send_managed.job_events
    FOR EACH ROW
    EXECUTE FUNCTION send_managed.fn_job_events_reject_update();

-- NOTE: there is deliberately NO trg_job_events_reject_delete (see header).

-- (2) One-event-per-transition: genesis event on job INSERT.
CREATE OR REPLACE FUNCTION send_managed.fn_job_events_on_insert()
RETURNS TRIGGER AS $$
DECLARE
    v_actor TEXT;
BEGIN
    v_actor := current_setting('send_managed.actor', true);
    IF v_actor IS NULL OR v_actor = '' THEN
        v_actor := 'system'; -- lenient on genesis: job-creation paths are diverse
    END IF;

    INSERT INTO send_managed.job_events (
        job_id, user_id, from_state, to_state, actor,
        account_id, box_id, error_class, error_message, metadata
    ) VALUES (
        NEW.id, NEW.user_id, NULL, NEW.state, v_actor::send_managed.event_actor,
        NEW.assigned_account_id, NEW.assigned_box_id, NEW.last_error_class, NEW.last_error_message, NULL
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
ALTER FUNCTION send_managed.fn_job_events_on_insert() OWNER TO send_managed_owner;

CREATE TRIGGER trg_job_events_on_insert
    AFTER INSERT ON send_managed.jobs
    FOR EACH ROW
    EXECUTE FUNCTION send_managed.fn_job_events_on_insert();

-- (2) One-event-per-transition: state-change event on job UPDATE OF state (strict actor).
CREATE OR REPLACE FUNCTION send_managed.fn_job_events_on_state_change()
RETURNS TRIGGER AS $$
DECLARE
    v_actor TEXT;
BEGIN
    IF OLD.state = NEW.state THEN
        RETURN NEW; -- no state change, no event
    END IF;

    v_actor := current_setting('send_managed.actor', true);
    IF v_actor IS NULL OR v_actor = '' THEN
        RAISE EXCEPTION 'send_managed.actor GUC not set: cannot record state transition'
            USING HINT = 'Call set_config(''send_managed.actor'', ''dispatcher'', true) before UPDATE';
    END IF;

    INSERT INTO send_managed.job_events (
        job_id, user_id, from_state, to_state, actor,
        account_id, box_id, error_class, error_message, metadata
    ) VALUES (
        NEW.id, NEW.user_id, OLD.state, NEW.state, v_actor::send_managed.event_actor,
        NEW.assigned_account_id, NEW.assigned_box_id, NEW.last_error_class, NEW.last_error_message, NULL
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
ALTER FUNCTION send_managed.fn_job_events_on_state_change() OWNER TO send_managed_owner;

CREATE TRIGGER trg_job_events_on_state_change
    AFTER UPDATE OF state ON send_managed.jobs
    FOR EACH ROW
    EXECUTE FUNCTION send_managed.fn_job_events_on_state_change();

-- Append-only privilege model: send_managed_app may INSERT/SELECT only, never UPDATE/DELETE.
-- (Cascade cleanup from send_managed.jobs deletion is system-performed and bypasses this.)
REVOKE UPDATE, DELETE ON send_managed.job_events FROM send_managed_app;

INSERT INTO send_managed.schema_migrations (version) VALUES ('0008');
