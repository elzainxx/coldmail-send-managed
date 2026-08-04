-- send_managed_0024_job_events_fk_carveout.up.sql — G12-OD-1, ruled option (A) by the user 2026-07-29:
-- give send_managed.job_events' append-only reject-trigger the SAME narrow per-nullable-FK-column carve-out
-- that send_managed.account_events has had since send_managed_0014, so the documented hard delete works again.
--
-- THE DEFECT (ADR-0012; found walking Stage 12 step 7 of Docs/coldmail-send-managed-local-test-guide.md, on
-- real infra). send_managed.job_events declares TWO `ON DELETE SET NULL` foreign keys:
--
--     job_events_account_fk  FOREIGN KEY (account_id) REFERENCES send_managed.accounts(id) ON DELETE SET NULL
--     job_events_box_fk      FOREIGN KEY (box_id)     REFERENCES send_managed.boxes(id)    ON DELETE SET NULL
--
-- Postgres implements `ON DELETE SET NULL` as an internal `UPDATE ONLY … SET <col> = NULL`. send_managed_0008's
-- BEFORE UPDATE reject-trigger raises on EVERY update with no carve-out, so the FK's own maintenance
-- update aborts and takes the parent DELETE with it:
--
--     code=internal_error reason=account_write_error:
--       ERROR: send_managed.job_events is append-only: UPDATE forbidden (SQLSTATE 23000)
--     CONTEXT: SQL statement "UPDATE ONLY "send_managed"."job_events" SET "account_id" = NULL WHERE …"
--
-- Consequence: `DELETE /v1/admin/accounts/{id}` returned 500 for any account that had ever been
-- assigned a job, and deleting a worker with job history failed identically — while an account with NO
-- job_events rows returned 204, because SET NULL then matches no row and issues no UPDATE. That split
-- is exactly why the defect survived the build: Phase 2's T5/T9 delete accounts that were never
-- dispatched to. Docs/api-contract.md documents this as "hard delete (audit row survives)" → 204, and
-- Phase 2 §2.5 locks the delete as unconditional, so a GDPR-style erasure of a REAL, used account was
-- impossible through any interface — including direct owner-role SQL. Nothing was corrupted: the delete
-- aborted atomically. A wedged operation, not data loss.
--
-- WHY THIS NEEDED A RULING RATHER THAN A HOTFIX (Rule 2). ADR-0009 examined job_events and explicitly
-- CLEARED it ("inbound FK cleanup is CASCADE DELETE … correct as-is"; "the pattern is CLOSED"). That
-- premise is wrong about the schema as built — job_events' inbound cleanup is BOTH kinds, and the two
-- SET NULL FKs were never enumerated. ADR-0009 was itself a ruled Open Decision, so revising its
-- finding is a ruling, not a refactor. ADR-0009's own generalized rule is the correct one and would
-- have caught this: an immutability trigger must permit "exactly the mutation its own inbound FK
-- referential actions perform, and nothing else."
--
-- WHY THIS IS NOT A COPY-PASTE OF account_events'. That table has ONE nullable FK column; this one has
-- TWO, so the permitted-transition set is strictly larger: account_id→NULL with box_id UNCHANGED, and
-- box_id→NULL with account_id UNCHANGED. Both arise for the SAME ROW when an account and a worker are
-- deleted in one transaction — Postgres issues one `UPDATE ONLY … SET <col> = NULL` per FK CONSTRAINT,
-- not one combined statement, so the two fire sequentially and the second sees the first's NULL already
-- in place. The set is enumerated per-column rather than written as the looser "an FK column changed",
-- because "too wide" widens the audit hole and "too narrow" leaves the delete wedged.
--
-- WHY THE UNCHANGED SIDE USES `IS NOT DISTINCT FROM` AND NOT `=`. Three-valued logic: `NULL = NULL` is
-- NULL, not TRUE, so a plain `=` fails closed and RE-WEDGES the delete on any row whose OTHER FK column
-- is already NULL. Proven, not reasoned about — a negative control with `=` substituted was run against
-- the behavioral suite and failed in TWO places, one of them unanticipated:
--   (a) the second of two sequential per-constraint updates (account and worker deleted together), and
--   (b) a plain single-FK WORKER delete — because the GENESIS event row is born with account_id NULL
--       (fn_job_events_on_insert copies NEW.assigned_account_id from a job that is not yet assigned),
--       so `NEW.account_id = OLD.account_id` is NULL on the very first row of every job.
-- (b) means the `=` form would have wedged the ordinary worker delete too, not merely the exotic combined
-- case. Hence a genuine behavioral test rather than a careful reading.
--
-- ADDITIVE-ONLY (Rule 5). No column, constraint, enum, index, grant, or FK action changes. ONE trigger
-- FUNCTION BODY is replaced (CREATE OR REPLACE, same signature, same trigger, same owner). The
-- REVOKE UPDATE, DELETE from send_managed_app (send_managed_0008) is untouched, so the app role still cannot
-- issue ANY update — this carve-out is reachable only by the system-performed FK maintenance update and
-- by the schema owner. Nothing that was rejected before is now accepted except that one mutation.
--
-- WHAT THE AUDIT GUARANTEE ACTUALLY IS, AFTER THIS. Every CONTENT column stays byte-immutable: id,
-- job_id, user_id, seq, from_state, to_state, actor, error_class, error_message, metadata, created_at.
-- The two FK columns are provenance POINTERS, and a pointer to a deleted row can only be NULL or a lie.
-- Nulling one loses no recorded fact that the parent delete had not already destroyed; the transition,
-- its actor, its timing, and its error context all survive. That is the same trade ADR-0009 made for
-- account_events and the same one "audit row survives" in api-contract.md promises.
--
-- REJECTED ALTERNATIVE, recorded so it is not revisited: changing the two FKs to ON DELETE CASCADE
-- would let the trigger stay unconditional, but it would DELETE the entire event history of every job
-- that ever touched the account — including jobs unrelated to the deletion. job_events has no DELETE
-- grant precisely to prevent that. See ADR-0012 option (C).

CREATE OR REPLACE FUNCTION send_managed.fn_job_events_reject_update()
RETURNS TRIGGER AS $$
BEGIN
    -- Permit ONLY the FK `ON DELETE SET NULL` maintenance update, enumerated per column:
    --   * account_id: unchanged, OR non-null -> NULL
    --   * box_id:     unchanged, OR non-null -> NULL
    --   * at least ONE of them actually nulled (so a no-op update is not silently blessed — it is
    --     still an UPDATE on an append-only table and there is no referential action that performs it)
    --   * every CONTENT column byte-identical
    -- Writing it as two per-column clauses rather than "an FK column changed" is what lets the two
    -- sequential per-constraint updates (a worker and its account both deleted) each pass, WITHOUT also
    -- permitting a NULL FK column being set back to non-NULL: `NEW.x IS NULL` is required in every
    -- branch that allows a change, so re-pointing a nulled provenance column at a different row —
    -- rewriting history to blame an account that was never involved — still raises.
    IF (   (NEW.account_id IS NOT DISTINCT FROM OLD.account_id)
        OR (OLD.account_id IS NOT NULL AND NEW.account_id IS NULL))
       AND (   (NEW.box_id IS NOT DISTINCT FROM OLD.box_id)
            OR (OLD.box_id IS NOT NULL AND NEW.box_id IS NULL))
       AND (   (OLD.account_id IS NOT NULL AND NEW.account_id IS NULL)
            OR (OLD.box_id     IS NOT NULL AND NEW.box_id     IS NULL))
       AND NEW.id            =            OLD.id
       AND NEW.job_id        =            OLD.job_id
       AND NEW.user_id       =            OLD.user_id
       AND NEW.seq           =            OLD.seq
       AND NEW.from_state    IS NOT DISTINCT FROM OLD.from_state
       AND NEW.to_state      =            OLD.to_state
       AND NEW.actor         =            OLD.actor
       AND NEW.error_class   IS NOT DISTINCT FROM OLD.error_class
       AND NEW.error_message IS NOT DISTINCT FROM OLD.error_message
       AND NEW.metadata      IS NOT DISTINCT FROM OLD.metadata
       AND NEW.created_at    =            OLD.created_at
    THEN
        RETURN NEW; -- FK ON DELETE SET NULL cleanup (audit content survives the parent delete)
    END IF;

    -- Unchanged message, hint, and ERRCODE: the rejection is the same contract it always was, and
    -- callers/tests that match on this string keep working (Rule 5, additive-only).
    RAISE EXCEPTION 'send_managed.job_events is append-only: UPDATE forbidden'
        USING HINT = 'Events are immutable audit records',
              ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
ALTER FUNCTION send_managed.fn_job_events_reject_update() OWNER TO send_managed_owner;

-- The trigger itself (trg_job_events_reject_update, send_managed_0008) is NOT recreated — CREATE OR REPLACE
-- FUNCTION rebinds it in place. And there is still deliberately NO trg_job_events_reject_delete:
-- Postgres fires row-level BEFORE DELETE on FK CASCADE, so one would block legitimate cascade cleanup
-- (send_managed_0008's header; Phase 1 #23). Direct DELETE by send_managed_app stays denied by the REVOKE.

INSERT INTO send_managed.schema_migrations (version) VALUES ('0024');
