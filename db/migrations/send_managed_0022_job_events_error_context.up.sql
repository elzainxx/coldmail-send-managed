-- send_managed_0022_job_events_error_context.up.sql — restore send_managed_0008's OWN declared contract:
-- job_events.error_class / error_message are "Error context (nullable, only for error transitions)".
--
-- THE DEFECT (found walking Stage 9 of Docs/coldmail-send-managed-local-test-guide.md, on real infra).
-- send_managed_0008 declares that contract verbatim over the two columns, then BOTH of its triggers
-- violate it: they copy `NEW.last_error_class, NEW.last_error_message` into EVERY row they write,
-- unconditionally. The job row's `last_error_*` columns are STICKY by design — the name says "last",
-- and no success path clears them (outcome.go's applyAccepted sets state/provider_message_id/
-- completed_at, never last_error_class). So once a job has failed ONCE, every subsequent event it
-- ever emits carries that stale class — including its eventual SUCCESS.
--
-- Observed live on job 10773800-0a1a-4e32-a53a-159d584caac1 — ONE requeue, four tagged rows:
--     seq  actor       transition            error_class  error_message
--     755  system      (new)    -> queued    -            -
--     759  dispatcher  queued   -> assigned  -            -
--     763  dispatcher  assigned -> queued    unknown      no_eligible_account   <- the ONE real error
--     768  dispatcher  queued   -> assigned  unknown      no_eligible_account
--     769  dispatcher  assigned -> sending   unknown      no_eligible_account
--     770  worker         sending  -> sent      unknown      no_eligible_account   <- a SUCCESS, tagged
--
-- WHY IT MATTERS BEYOND TIDINESS. adminapi/health_store.go's queryErrorsInWindow is
-- `COUNT(*) ... WHERE error_class IS NOT NULL GROUP BY error_class`, so GET /v1/admin/health
-- reported `errors_last_window: {"unknown": 4}` for that single requeue — a 4x inflation of an
-- operator-facing metric, three of the four rows being non-error transitions and one of them the
-- send that SUCCEEDED. An operator reading the runbook's "Sends are failing" triage
-- (Docs/operator-runbook.md) would size the incident 4x too large. It is also plainly false as an
-- audit record: `sending -> sent` did not fail with error_class=unknown.
--
-- ROW-LEVEL vs EVENT-LEVEL is the whole distinction, and it is why send_managed.jobs is NOT touched here.
-- `send_managed.jobs.last_error_*` is a RUNNING fact about a mutable row ("the last error this job saw"),
-- and retaining it across a later success is correct and useful. `send_managed.job_events` is APPEND-ONLY
-- and every row is a POINT-IN-TIME fact about ONE transition. Stamping a past error onto an
-- unrelated later transition is not retention, it is fabrication — CLAUDE.md Rule 5 ("never invent
-- an outcome"). So the fix is at the trigger, where the point-in-time row is written.
--
-- ADDITIVE-ONLY (Rule 5). No column, constraint, enum, index, or grant changes. Two trigger
-- FUNCTION BODIES are replaced (CREATE OR REPLACE, same signatures, same triggers, same owner) and
-- one helper predicate is added. Nothing that was recorded stops being recordable: a genuine error
-- transition still carries the identical class and message it carried before. The change is strictly
-- a NARROWING of which rows carry error context, in the direction the schema already documented.
--
-- EXISTING ROWS ARE NOT REWRITTEN. job_events is append-only at the database level (send_managed_0008's
-- trg_job_events_reject_update + `REVOKE UPDATE, DELETE ... FROM send_managed_app`), and this migration
-- does not weaken that to launder history — an audit log that can be retro-edited is worth less than
-- one with a known, dated defect. Already-written rows keep their spurious tags. The two consequences
-- are handled deliberately:
--   (1) the health metric is corrected AT THE READER too (queryErrorsInWindow now also filters on the
--       transition shape), so `errors_last_window` is right IMMEDIATELY on deploy rather than after
--       health_metrics_window_sec rolls the bad rows out of the window; and
--   (2) that reader filter is defense in depth, not a duplicate: it makes the metric structurally
--       correct no matter what any writer stamps.

-- The single definition of "is this transition an error transition?", shared by both triggers so the
-- two can never drift apart. Derived from the job_state machine (send_managed_0003), not from a list of
-- reasons:
--   * to_state 'failed'  — terminal failure (exhaustion.go's failExhaustedJobs).
--   * to_state 'bounced' — terminal bounce (outcome.go's applyBounced).
--   * to_state 'queued' WITH a non-NULL from_state — a REQUEUE, which only ever happens because an
--     attempt did not succeed: reclaim.go's lease timeout, dispatch.go's no_eligible_account /
--     push_failed / header_injection, and outcome.go's applyClassifiedFailure.
-- Everything else is forward progress or success and carries NO error context:
--   * from_state IS NULL -> 'queued' — GENESIS. A brand-new job has no error history. (send_managed_0008's
--     job_events_from_state_check already pins genesis to exactly this shape, so genesis is provably
--     non-error under this predicate rather than special-cased.)
--   * 'queued' -> 'assigned', 'assigned' -> 'sending' — forward progress.
--   * 'sending' -> 'sent' — SUCCESS. This is the row from the live trace above.
-- A FUTURE additive job_state (Rule 5 keeps the enum additive-only forever) defaults to NOT an error
-- here. That is the deliberate safe direction: the failure mode of guessing wrong becomes "an error
-- was not labelled", never "a success was labelled as an error". Adding a state is a reviewed change
-- that revisits this function.
CREATE OR REPLACE FUNCTION send_managed.fn_job_event_is_error_transition(
    p_from send_managed.job_state,
    p_to   send_managed.job_state
) RETURNS BOOLEAN AS $$
    SELECT p_to IN ('failed', 'bounced')
        OR (p_to = 'queued' AND p_from IS NOT NULL);
$$ LANGUAGE sql IMMUTABLE;
ALTER FUNCTION send_managed.fn_job_event_is_error_transition(send_managed.job_state, send_managed.job_state)
    OWNER TO send_managed_owner;

-- Genesis trigger: unchanged in every respect except that error context is now gated. Under the
-- predicate above a genesis row is never an error, so in practice this always writes NULL/NULL —
-- which is also what it SHOULD have written all along (a job's first event cannot describe a failure
-- that has not happened). Routing it through the shared predicate rather than hardcoding NULL keeps
-- ONE definition of "error transition" in the schema.
CREATE OR REPLACE FUNCTION send_managed.fn_job_events_on_insert()
RETURNS TRIGGER AS $$
DECLARE
    v_actor TEXT;
    v_is_error BOOLEAN;
BEGIN
    v_actor := current_setting('send_managed.actor', true);
    IF v_actor IS NULL OR v_actor = '' THEN
        v_actor := 'system'; -- lenient on genesis: job-creation paths are diverse
    END IF;

    v_is_error := send_managed.fn_job_event_is_error_transition(NULL, NEW.state);

    INSERT INTO send_managed.job_events (
        job_id, user_id, from_state, to_state, actor,
        account_id, box_id, error_class, error_message, metadata
    ) VALUES (
        NEW.id, NEW.user_id, NULL, NEW.state, v_actor::send_managed.event_actor,
        NEW.assigned_account_id, NEW.assigned_box_id,
        CASE WHEN v_is_error THEN NEW.last_error_class   ELSE NULL END,
        CASE WHEN v_is_error THEN NEW.last_error_message ELSE NULL END,
        NULL
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
ALTER FUNCTION send_managed.fn_job_events_on_insert() OWNER TO send_managed_owner;

-- State-change trigger: the one that produced the live 4x inflation. Same early-return on an
-- unchanged state (the semantics CLAUDE.md Rule 9(a) depends on — `SET state = state` is still not
-- an event), same strict actor GUC, same one-row-per-transition guarantee. ONLY the error-context
-- columns are gated.
CREATE OR REPLACE FUNCTION send_managed.fn_job_events_on_state_change()
RETURNS TRIGGER AS $$
DECLARE
    v_actor TEXT;
    v_is_error BOOLEAN;
BEGIN
    IF OLD.state = NEW.state THEN
        RETURN NEW; -- no state change, no event
    END IF;

    v_actor := current_setting('send_managed.actor', true);
    IF v_actor IS NULL OR v_actor = '' THEN
        RAISE EXCEPTION 'send_managed.actor GUC not set: cannot record state transition'
            USING HINT = 'Call set_config(''send_managed.actor'', ''dispatcher'', true) before UPDATE';
    END IF;

    v_is_error := send_managed.fn_job_event_is_error_transition(OLD.state, NEW.state);

    INSERT INTO send_managed.job_events (
        job_id, user_id, from_state, to_state, actor,
        account_id, box_id, error_class, error_message, metadata
    ) VALUES (
        NEW.id, NEW.user_id, OLD.state, NEW.state, v_actor::send_managed.event_actor,
        NEW.assigned_account_id, NEW.assigned_box_id,
        CASE WHEN v_is_error THEN NEW.last_error_class   ELSE NULL END,
        CASE WHEN v_is_error THEN NEW.last_error_message ELSE NULL END,
        NULL
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
ALTER FUNCTION send_managed.fn_job_events_on_state_change() OWNER TO send_managed_owner;

INSERT INTO send_managed.schema_migrations (version) VALUES ('0022');
