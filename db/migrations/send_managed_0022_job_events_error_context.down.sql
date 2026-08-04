-- send_managed_0022_job_events_error_context.down.sql — restore send_managed_0008's trigger bodies VERBATIM
-- (the unconditional `NEW.last_error_class, NEW.last_error_message` copy) and drop the helper.
--
-- Reversing this migration deliberately REINSTATES the defect it fixed: every job_events row a job
-- emits after its first failure carries that stale class again, including its `sending -> sent`
-- success, and GET /v1/admin/health's errors_last_window re-inflates. That is what "reversible" means
-- here — the down path returns the schema EXACTLY to its pre-0022 shape rather than to a
-- half-corrected one, because a down migration that keeps part of an up migration's behavior is not a
-- reverse and cannot be reasoned about.
--
-- NOTE on the reader: adminapi/health_store.go's queryErrorsInWindow also filters on the transition
-- shape as of this change. That filter is pure Go and is NOT reversed by this file; running a
-- post-0022 Control against a downgraded schema therefore still reports a CORRECT errors_last_window
-- (the reader independently excludes non-error transitions). The reverse pairing — a pre-0022 Control
-- on a post-0022 schema — is also correct, since the trigger simply writes fewer tagged rows and the
-- old unfiltered COUNT then counts only genuine error transitions. Both mixed orderings are safe;
-- neither over-reports.
--
-- The two function bodies are restored with CREATE OR REPLACE (same signatures), so the triggers
-- created by send_managed_0008 keep pointing at them and are not dropped/recreated.

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

DROP FUNCTION IF EXISTS send_managed.fn_job_event_is_error_transition(send_managed.job_state, send_managed.job_state);

DELETE FROM send_managed.schema_migrations WHERE version = '0022';
