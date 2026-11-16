-- send_managed_0028_job_cancel_and_attempt_cap.up.sql -- the schema half of two Domain-A close-out
-- items (M2-G2 and M2-G3 of the owner's research record), plus the reason column the report needs
-- (M2-G1):
--
--   * B3, an ATTEMPT CAP. Before this, ONE writer produced `failed` (pool exhaustion) and every other
--     SMTP failure returned the job to `queued` with unbounded retries: a persistent 554 or a dead
--     credential never became terminal until the whole pool was exhausted. Two new dials bound it
--     (job_max_failed_attempts, job_failed_max_age_sec, seeded below). The counters they act on are the
--     two new columns failed_attempt_count / first_failed_at: they count ONLY real failed SMTP attempts.
--     attempt_count cannot be used for this -- it SATURATES at max_attempts (3) and is also bumped by
--     dispatcher-side requeues that never reached SMTP (no_eligible_account, no_worker_available,
--     push_failed), which mean "waiting", not "failed".
--   * B4, CANCEL. The terminal state `cancelled` (added by send_managed_0027, which had to commit
--     first), the widened completed_at CHECK, and the cancel_requested_at flag + the BEFORE-UPDATE guard
--     that makes a cancel stick for an item that was ALREADY IN FLIGHT when it was issued (see below).
--   * B2, the REASON an item ended. terminal_reason records which of the two writers of `failed` (or the
--     cancel) produced a terminal row, so GET /v1/report can say pool_exhausted vs attempts_exhausted
--     without parsing last_error_message.
--
-- ALL OF THIS IS ADDITIVE: four nullable/defaulted columns, one widened CHECK, one trigger, two config
-- rows. Nothing is renamed or removed (CLAUDE.md Rule 5). An older Control binary running against this
-- schema keeps working: it never touches the new columns, the defaults satisfy every constraint, and the
-- guard trigger only acts on a row that has been flagged for cancellation.

-- ---- columns -------------------------------------------------------------------------------------
ALTER TABLE send_managed.jobs
    ADD COLUMN failed_attempt_count INTEGER     NOT NULL DEFAULT 0,  -- real failed SMTP attempts (never dispatcher-side requeues)
    ADD COLUMN first_failed_at      TIMESTAMPTZ,                     -- when the FIRST failed SMTP attempt was recorded; the age cap's clock
    ADD COLUMN cancel_requested_at  TIMESTAMPTZ,                     -- set by POST /v1/cancel on every non-terminal item of the campaign
    ADD COLUMN terminal_reason      TEXT;                            -- 'pool_exhausted' | 'attempts_exhausted' | 'cancelled' on failed/cancelled rows

COMMENT ON COLUMN send_managed.jobs.failed_attempt_count IS
    'Real failed SMTP attempts: a worker-reported classified failure (transport_4xx, auth_failure, throttle, timeout, unknown) or a reclaim of a job that was SENDING. NEVER incremented by a dispatcher-side requeue that did not reach SMTP (no_eligible_account, no_worker_available, push_failed, header_injection) and NEVER by the reclaim of an item that was only ASSIGNED. Not saturating, unlike attempt_count. The job is moved to failed/attempts_exhausted when it reaches job_max_failed_attempts.';
COMMENT ON COLUMN send_managed.jobs.first_failed_at IS
    'Timestamp of the first failed SMTP attempt (NULL until there is one). The age cap job_failed_max_age_sec is counted from here and evaluated each time a further failed attempt is recorded.';
COMMENT ON COLUMN send_managed.jobs.cancel_requested_at IS
    'Set by POST /v1/cancel on every non-terminal item of the cancelled campaign. For a QUEUED item the same call also moves it to cancelled. For an item already assigned/sending it only sets this flag: the in-flight attempt finishes and reports normally, and the guard trigger (fn_jobs_cancel_guard) turns any later return to queued into cancelled so the item can never be claimed or sent again. The claim queries skip a flagged row.';
COMMENT ON COLUMN send_managed.jobs.terminal_reason IS
    'Why a failed or cancelled row ended: pool_exhausted (exhaustion.go), attempts_exhausted (outcome.go / reclaim.go, the B3 cap) or cancelled. NULL on every non-terminal row and on sent/bounced (their state IS the reason). A failed row with a NULL terminal_reason pre-dates this column and is read as pool_exhausted.';

-- Rule 13 (a CHECK that evaluates to NULL passes): every predicate over a nullable column says IS NULL /
-- IS NOT NULL explicitly.
ALTER TABLE send_managed.jobs
    ADD CONSTRAINT jobs_failed_attempt_count_check CHECK (failed_attempt_count >= 0),
    ADD CONSTRAINT jobs_first_failed_at_check CHECK (
        (failed_attempt_count = 0 AND first_failed_at IS NULL)
        OR (failed_attempt_count > 0 AND first_failed_at IS NOT NULL)
    ),
    ADD CONSTRAINT jobs_terminal_reason_state_check CHECK (
        terminal_reason IS NULL OR state IN ('failed', 'cancelled')
    );

-- Every `failed` row that exists today was written by pool exhaustion (the only writer before this
-- migration, M2-G2), so the reason is known and is recorded. The AFTER UPDATE OF state event trigger
-- does not fire (state is not in this SET list) and the cancel guard is created below, after this runs.
UPDATE send_managed.jobs SET terminal_reason = 'pool_exhausted'
 WHERE state = 'failed' AND terminal_reason IS NULL;

-- ---- the terminal-state CHECK learns `cancelled` ------------------------------------------------
-- completed_at must be set exactly on terminal rows. `cancelled` is terminal, so it moves from the
-- "must be NULL" branch to the "must be set" branch. The 0007 text is reproduced with that one word
-- added -- not paraphrased -- so the two files read as the same constraint.
ALTER TABLE send_managed.jobs DROP CONSTRAINT jobs_completed_at_valid_check;
ALTER TABLE send_managed.jobs ADD CONSTRAINT jobs_completed_at_valid_check
    CHECK (
        (state IN ('sent', 'failed', 'bounced', 'cancelled') AND completed_at IS NOT NULL)
        OR (state NOT IN ('sent', 'failed', 'bounced', 'cancelled') AND completed_at IS NULL)
    );

-- ---- the cancel guard ---------------------------------------------------------------------------
-- THE PROBLEM IT CLOSES. POST /v1/cancel moves every QUEUED item to `cancelled`, but an item that is
-- ASSIGNED or SENDING at that moment is already on its way out: it must finish and report truthfully
-- (sent / bounced / its failure path) exactly once, and cancel does not change its state. Its failure
-- path, however, is to return to `queued` for a retry -- and a retry after a cancel is a NEW send, which
-- the platform asked us not to make. Five different code paths can return an in-flight job to queued
-- (the outcome handler's classified failure, the reclaim loop, requeue() for no_eligible_account /
-- push_failed / no_worker_available / header_injection), and a guard written into each of them is a
-- guard the next one forgets. So the rule lives HERE, at the one place every writer passes:
--
--   a row flagged cancel_requested_at that is moved assigned|sending -> queued is moved to `cancelled`
--   instead, completed_at stamped, lease cleared.
--
-- Everything else about that UPDATE is unchanged (last_error_class/message, attempt counters,
-- next_attempt_at are still written, so the item reports why its last attempt failed). Because this is a
-- BEFORE ROW trigger that edits NEW, the AFTER UPDATE OF state event trigger (send_managed_0008/0022) then
-- records the true transition (sending -> cancelled) under the actor of whichever path caused it: the
-- event row per transition is not special-cased.
--
-- RACE-FREEDOM. The guard reads cancel_requested_at from the row version it is about to replace, and
-- Postgres re-evaluates a BEFORE ROW trigger against the latest committed version after waiting on a
-- concurrent writer's row lock. So "cancel flags the row" and "an in-flight outcome requeues the row"
-- serialize on that row in either order: flag first -> the requeue becomes cancelled; requeue first ->
-- the row is queued when cancel's second step moves queued items to cancelled. There is no interleaving
-- that leaves the item queued and claimable.
--
-- It deliberately does NOT intercept transitions to `failed`: an in-flight item that exhausts its attempt
-- cap is still `failed` (it did fail, and that is the truthful report); and it does not intercept queued ->
-- assigned (the claim queries skip a flagged row; a trigger that RAISEd there would abort the claim of
-- every OTHER job in the same batch).
CREATE OR REPLACE FUNCTION send_managed.fn_jobs_cancel_guard()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.state = 'queued'
       AND OLD.state IN ('assigned', 'sending')
       AND NEW.cancel_requested_at IS NOT NULL THEN
        NEW.state            := 'cancelled';
        NEW.completed_at     := NOW();
        NEW.lease_expires_at := NULL;
        NEW.terminal_reason  := 'cancelled';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
ALTER FUNCTION send_managed.fn_jobs_cancel_guard() OWNER TO send_managed_owner;

CREATE TRIGGER trg_jobs_cancel_guard
    BEFORE UPDATE OF state ON send_managed.jobs
    FOR EACH ROW
    WHEN (NEW.state = 'queued' AND NEW.cancel_requested_at IS NOT NULL)
    EXECUTE FUNCTION send_managed.fn_jobs_cancel_guard();

-- ---- the two attempt-cap dials (CLAUDE.md Rule 1: config, not code) -------------------------------
INSERT INTO send_managed.config (key, value, default_value, description, value_type) VALUES
('job_max_failed_attempts', '{"v":6}', '{"v":6}',
 'Attempt cap (M2-G2): the number of FAILED SMTP attempts a job may accumulate before it is moved to the terminal state failed with reason attempts_exhausted. 6 by default, so the 6th failed attempt is the one that ends the job and a job with 5 failures is still queued. Counts ONLY real failed SMTP attempts: a worker-reported classified failure (transport_4xx, auth_failure, throttle, timeout, unknown) and the reclaim of a job that was sending (timeout). It NEVER counts a dispatcher-side requeue that did not reach SMTP (no_eligible_account, no_worker_available, push_failed, header_injection) or the reclaim of a job that was only assigned: those mean waiting, not failed. A job that hits the cap is not charged by the platform (the message never reached the world). Integer 1-1000. Read fresh on every outcome and reclaim, so a change applies on the very next failed attempt.', 'integer'),
('job_failed_max_age_sec', '{"v":86400}', '{"v":86400}',
 'Age cap (M2-G2): the maximum age in seconds of a job that has had at least one failed SMTP attempt, counted from its FIRST failed attempt. When a further failed attempt is recorded and the job is at least this old, it is moved to failed with reason attempts_exhausted even if job_max_failed_attempts has not been reached. 86400 (24 hours) by default. It is evaluated when a failed attempt is recorded, not by a sweep: a job that is only WAITING (no eligible account, dispatch paused) is never failed by age. Integer 60-2592000 (1 minute to 30 days).', 'integer');

INSERT INTO send_managed.schema_migrations (version) VALUES ('0028');
