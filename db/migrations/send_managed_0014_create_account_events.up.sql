-- send_managed_0014_create_account_events.up.sql — Phase 2 (ruling 4): the account audit sink.
-- New additive append-only table, SAME discipline as send_managed.job_events (0008): REVOKE
-- UPDATE, DELETE + a BEFORE UPDATE reject-trigger; NO BEFORE DELETE trigger (it would block
-- the FK ON DELETE SET NULL path — the Phase 1 #23 lesson), DELETE denied by the revoked
-- privilege. Every §7 lifecycle transition + account create/update/delete writes EXACTLY ONE
-- row, performed EXPLICITLY by the admin service inside the operation's transaction (NOT via a
-- trigger on send_managed.accounts — Phase 2 must not attach triggers that fire on the dispatcher's
-- future writes; the audit stays scoped to admin actions only).
--
-- Reconciliation (ground = Phase 0 send_managed_0000, ADR-0008): the table is created by the admin
-- migration role then ALTER ... OWNER TO send_managed_owner (so FORCE-RLS-class ownership matches the
-- other send_managed.* tables). Default privileges do NOT fire for a reowned table, so send_managed_app
-- is born with ZERO DML here; the EXPLICIT GRANT INSERT, SELECT below is what gives it rights,
-- and the REVOKE UPDATE, DELETE is defense-in-depth/intent (a no-op against never-granted DML).
-- The grants invariant test (phase1_grants_invariant_test.go) classifies account_events as an
-- append-only table (INSERT, SELECT) — a new table WITHOUT this grant would FAIL that test.
--
-- actor reuses the EXISTING Phase 1 send_managed.event_actor enum (which already includes 'admin') —
-- NO enum change (additive-only invariant honored). Phase 2 only ever writes 'admin' here.
-- No Phase 1 table column is added, dropped, or altered by this migration.

CREATE TABLE send_managed.account_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  UUID,                        -- ON DELETE SET NULL: audit survives account delete
    user_id     UUID NOT NULL,               -- tenant at time of action (real or SEND_MANAGED_SYSTEM_USER_ID)
    actor       send_managed.event_actor NOT NULL, -- reuses EXISTING Phase 1 enum; always 'admin' in Phase 2
    action      TEXT NOT NULL,
    from_flags  JSONB,                        -- {paused, pause_reason, quarantined, disabled} before
    to_flags    JSONB,                        -- ... after
    reason      TEXT,                         -- internal operator note (never a credential)
    request_id  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT account_events_account_fk
        FOREIGN KEY (account_id) REFERENCES send_managed.accounts(id) ON DELETE SET NULL,
    CONSTRAINT account_events_user_fk
        FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE,
    CONSTRAINT account_events_action_check
        CHECK (action IN ('quarantine','unquarantine','disable','enable',
                          'pause','unpause','created','updated','deleted')),
    CONSTRAINT account_events_reason_len_check
        CHECK (reason IS NULL OR char_length(reason) <= 2000)
);

ALTER TABLE send_managed.account_events OWNER TO send_managed_owner;

CREATE INDEX account_events_account_id_created_at_idx
    ON send_managed.account_events (account_id, created_at);
CREATE INDEX account_events_user_id_created_at_idx
    ON send_managed.account_events (user_id, created_at);

-- Append-only: BEFORE UPDATE reject-trigger (defense in depth alongside the REVOKE below).
--
-- FLAGGED SPEC RESOLUTION (OD-23, ADR-0009): §12's DDL specifies BOTH a BEFORE UPDATE trigger
-- that rejects *all* updates AND an account_id FK that is `ON DELETE SET NULL` (so §7.4's "the
-- deleted audit row survives" holds). Those two are in direct conflict: the FK SET NULL performs
-- an internal `UPDATE account_events SET account_id = NULL`, which an unconditional reject-trigger
-- blocks — making the account delete that §7.4 mandates impossible. This is exactly the Phase 1
-- #23 lesson the spec cites, but for the SET NULL (an UPDATE) rather than a cascade DELETE. The
-- only self-consistent reading of §7.4 + #23 is a trigger that permits ONLY the FK-maintenance
-- transition (account_id non-null -> NULL with every other column unchanged) and rejects every
-- genuine content mutation. This preserves the defense-in-depth backstop AND lets the audit
-- survive the account delete. Content immutability (you cannot alter WHAT happened) is fully
-- kept: action/reason/flags/timestamps can never change. (Flagged, not silently fixed — see
-- Docs/history/open-decisions.md OD-23 and Docs/history/decisions/0009-*.md.)
CREATE OR REPLACE FUNCTION send_managed.fn_account_events_reject_update()
RETURNS TRIGGER AS $$
BEGIN
    -- Permit ONLY the FK `ON DELETE SET NULL` maintenance update: account_id goes non-null -> NULL
    -- and NOTHING else changes. Everything else is a forbidden content mutation.
    IF OLD.account_id IS NOT NULL AND NEW.account_id IS NULL
       AND NEW.id         =            OLD.id
       AND NEW.user_id    =            OLD.user_id
       AND NEW.actor      =            OLD.actor
       AND NEW.action     =            OLD.action
       AND NEW.from_flags IS NOT DISTINCT FROM OLD.from_flags
       AND NEW.to_flags   IS NOT DISTINCT FROM OLD.to_flags
       AND NEW.reason     IS NOT DISTINCT FROM OLD.reason
       AND NEW.request_id IS NOT DISTINCT FROM OLD.request_id
       AND NEW.created_at =            OLD.created_at
    THEN
        RETURN NEW; -- FK ON DELETE SET NULL cleanup (audit survives the account delete, §7.4)
    END IF;

    RAISE EXCEPTION 'send_managed.account_events is append-only: UPDATE forbidden'
        USING HINT = 'Account audit events are immutable records',
              ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
ALTER FUNCTION send_managed.fn_account_events_reject_update() OWNER TO send_managed_owner;

CREATE TRIGGER trg_account_events_reject_update
    BEFORE UPDATE ON send_managed.account_events
    FOR EACH ROW
    EXECUTE FUNCTION send_managed.fn_account_events_reject_update();
-- NOTE: there is deliberately NO trg_account_events_reject_delete: Postgres fires row-level
-- BEFORE DELETE on FK SET NULL/cascade cleanup, which such a trigger would block. Direct DELETE
-- by send_managed_app is denied by the REVOKE below instead.

-- Explicit append-only grants (ADR-0008: default privileges don't fire for a reowned table).
REVOKE UPDATE, DELETE ON send_managed.account_events FROM send_managed_app;
GRANT  INSERT, SELECT  ON send_managed.account_events TO   send_managed_app;

-- RLS parity with other send_managed.* tables: permissive for the module. FORCE is not needed here —
-- this table holds NO credential material (the crown-jewel FORCE RLS is on send_managed.accounts only).
ALTER TABLE send_managed.account_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY account_events_sender_module_rw ON send_managed.account_events
    FOR ALL TO send_managed_app USING (true) WITH CHECK (true);

INSERT INTO send_managed.schema_migrations (version) VALUES ('0014');
