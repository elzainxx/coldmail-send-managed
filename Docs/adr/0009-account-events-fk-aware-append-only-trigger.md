# ADR-0009: account_events append-only trigger must permit the FK ON DELETE SET NULL update

- **Status:** accepted
- **Date:** 2026-07-23

## Context
The locked Phase 2 spec (`Docs/phase-2-account-management.md` §12) gives the DDL for the new
`sender.account_events` audit sink. That DDL specifies **two** properties that, taken literally,
contradict each other:

1. `account_id UUID … REFERENCES sender.accounts(id) ON DELETE SET NULL` — so that a `deleted`
   audit row **survives** the account's hard delete (§2.5, §7.4, test T9: "the `action='deleted'`
   row exists with `account_id` nulled by the FK … and the account's earlier lifecycle events
   also survive with nulled `account_id`").
2. A `BEFORE UPDATE` reject-trigger (`fn_account_events_reject_update`) that raises on **every**
   update, "same discipline as `job_events`."

Postgres implements `ON DELETE SET NULL` by issuing an internal
`UPDATE ONLY sender.account_events SET account_id = NULL WHERE account_id = <deleted id>`. A
row-level `BEFORE UPDATE` trigger fires on that maintenance update. So with the trigger as
literally written, deleting an account that has any audit rows **raises**
`integrity_constraint_violation` and the whole delete fails — making the `DELETE /v1/admin/
accounts/{id}` → `204` + surviving-audit behavior that §2.5/§7.4/T9 mandate **impossible**.

This was caught by the behavioral suite before merge: T5 (delete a pool after deleting its
account) and T9 (delete the account through the API) both returned `500 internal_error`, with the
Postgres log showing the reject-trigger firing on the FK's `SET NULL` update.

The spec **already cites the correct principle** — the Phase 1 `#23` lesson, quoted in §7.4 and
in the `0014` DDL comment: *"No `BEFORE DELETE` reject-trigger — it would block the FK
`SET NULL`/cascade cleanup."* The authors correctly removed the DELETE trigger but did not notice
that for `account_events` the **inbound** FK maintenance is a `SET NULL` (an UPDATE), which the
UPDATE trigger blocks for exactly the same reason. The spec is internally inconsistent, not
merely under-specified.

Per CLAUDE.md Rule 2 this is a locked-fact conflict: it was **flagged** (OD-23) and resolved on
the spec's own terms rather than silently patched or resolved by weakening either property.

## Decision (RULED — the only reading consistent with §7.4 + Phase 1 #23)
Make the `BEFORE UPDATE` reject-trigger **FK-aware**: permit **only** the FK-maintenance
transition and reject every genuine content mutation.

Concretely, the trigger returns `NEW` (allows the update) iff **all** of:
- `OLD.account_id IS NOT NULL AND NEW.account_id IS NULL` (the `SET NULL` transition), **and**
- every other column is unchanged (`id`, `user_id`, `actor`, `action`, `created_at` equal;
  `from_flags`, `to_flags`, `reason`, `request_id` `IS NOT DISTINCT FROM` their old values).

Any other update — a content change, or a content change bundled with `account_id → NULL` —
raises `integrity_constraint_violation` exactly as before.

This preserves the two things the append-only guarantee is actually protecting:
- **Content immutability** — you can never change *what happened* (action, actor, flag
  snapshots, reason, timestamps). That is the audit property that matters.
- **Defense-in-depth** — the trigger still backstops the `REVOKE UPDATE` grant against a
  future misgrant, for every mutation except the one the database itself must perform.

And it makes the mandated behavior work: the account delete's `SET NULL` succeeds, so the
`deleted` audit row (and all prior events for that account) survive with `account_id` nulled.

## Alternatives rejected
- **Drop the reject-trigger, rely on `REVOKE UPDATE` alone.** Loses the defense-in-depth
  backstop the spec explicitly wanted ("same discipline as `job_events`"). The trigger is the
  belt to the grant's braces.
- **Change the FK to `ON DELETE CASCADE` or `NO ACTION`.** Would either destroy the audit trail
  on delete (contradicts §7.4 "the audit survives") or block the delete (contradicts §2.5
  "returns `204`"). Both violate a locked behavior; the FK shape is locked as `SET NULL`.
- **Forbid deleting an account that has audit rows.** Contradicts §2.5 (delete is unconditional;
  no FK to `accounts.id` uses `RESTRICT`) and defeats the GDPR-style hard-delete the spec
  provides.

## Consequences
- `db/migrations/sender_0014_create_account_events.up.sql` implements the FK-aware trigger; the
  `down` is unchanged (drops the function/trigger/table).
- Proven on real Postgres by the behavioral suite: **T5** (delete pool after account delete →
  `204`) and **T9** (delete account through the API → `deleted` audit row survives with
  `account_id` nulled; earlier events survive nulled) both pass. **T9 also proves the trigger
  still has teeth**: a superuser content `UPDATE` and an `account_id=NULL` bundled with a content
  change are both rejected.
- No Phase 1–9 spec file is edited; the resolution lives in the migration + this ADR +
  `Docs/open-decisions.md` OD-23. The `job_events` trigger is unaffected (its inbound FK cleanup
  is a cascade DELETE, not a `SET NULL`, so its unconditional UPDATE-reject is already correct).

## The pattern, generalized (recorded in the Phase 2 closeout)

This class of conflict — **a DB-level append-only/immutability trigger colliding with a foreign
key's own referential action** — has now occurred **twice**:

1. **Phase 1 (`sender_0008`, `job_events`):** a `BEFORE DELETE` reject-trigger would have blocked
   the `job_events → jobs` FK **`ON DELETE CASCADE`** cleanup (Postgres fires row-level
   `BEFORE DELETE` on cascade). Resolved by **not** installing a `BEFORE DELETE` trigger at all
   and relying on `REVOKE DELETE` to stop application deletes — the cascade is system-performed
   and bypasses the app role's table privileges. (ADR-0002 / Phase 1 `#23`.)
2. **Phase 2 (`sender_0014`, `account_events`):** a `BEFORE UPDATE` reject-trigger blocked the
   `account_events → accounts` FK **`ON DELETE SET NULL`** cleanup (the `SET NULL` is an UPDATE).
   Resolved by an **FK-aware `BEFORE UPDATE` trigger** that permits exactly the FK's own mutation
   (`account_id` non-null → NULL, every other column unchanged) and rejects everything else.

**The resolution pattern (mandatory for any future append-only table):** an immutability trigger
must permit **exactly the mutation its own inbound FK referential actions perform, and nothing
else** — whether that is achieved by omitting the trigger for the action the DB performs
(the CASCADE-DELETE case) or by an explicit narrow carve-out in the trigger body (the SET-NULL
case). The content the audit protects (what happened) stays frozen; only the FK-maintenance
column transition the schema already committed to is allowed. A blanket "reject all
UPDATE/DELETE" trigger is a latent bug whenever the table is the *target* of an FK whose parent
can be deleted.

**The pattern is CLOSED for the current schema.**
> **This claim was WRONG when written, and stayed wrong for eight migrations. Corrected
> 2026-07-29 by ADR-0012 (`sender_0024`).** The enumeration below missed two FKs, so "closed"
> described a schema survey that had not actually been carried out. The generalized rule above is
> the correct one and would have caught it; the ADR asserted closure on an incomplete census
> instead of applying its own rule. The pattern IS closed now — but by `sender_0024`, not by this
> ADR.

The only tables carrying an immutability trigger are:
- `sender.job_events` — append-only (`INSERT, SELECT`); inbound FK cleanup is `CASCADE DELETE`,
  handled by the no-`BEFORE-DELETE` approach (correct as-is).
  > **CORRECTION (2026-07-29) — this bullet is WRONG about the schema as built; FIXED by
  > `sender_0024` (ADR-0012, G12-OD-1 ruled option A).** `job_events`' inbound cleanup is *both*
  > kinds. `job_events_job_fk`/`job_events_user_fk` are `ON DELETE CASCADE` as stated, but
  > `job_events_account_fk` and `job_events_box_fk` are **`ON DELETE SET NULL`** — the same shape as
  > the `account_events` FK this ADR was written to fix — and were not enumerated here. The
  > unconditional UPDATE-reject therefore *did* collide with a referential action, wedging
  > `DELETE /v1/admin/accounts/{id}` (and box deletes) with `500` for any account/box with job
  > history, for the whole of Phases 3–10. `sender_0024` gives the trigger this ADR's own carve-out,
  > enumerated **per nullable FK column** because `job_events` has two where `account_events` has
  > one. Current state: `job_events`' inbound cleanup is CASCADE *and* SET NULL, and BOTH are now
  > handled — CASCADE by installing no `BEFORE DELETE` trigger, SET NULL by the carve-out.
- `sender.account_events` — append-only (`INSERT, SELECT`); inbound FK cleanup is `SET NULL`,
  handled by the FK-aware `BEFORE UPDATE` trigger (this ADR).
- `sender.send_logs` — write-once (`INSERT, SELECT, DELETE`; `REVOKE UPDATE` + `BEFORE UPDATE`
  reject-trigger). It has **no inbound FK** (nothing references `send_logs.id`), so its
  unconditional UPDATE-reject can never collide with a referential action; DELETE is granted
  (Phase 7 retention), so there is no DELETE trigger to conflict either. No change needed.

There are no other append-only/write-once tables in `sender.*` (verified against the migration
chain `0000..0014`). Any table added in a later phase that installs an immutability trigger MUST
apply this pattern in the same migration.

**Census RE-RUN 2026-07-29 against the live schema, not against the migration files** — because
reading the chain is exactly what produced the two-FK miss above. Queried on a database migrated
`0000..0024`:

- Immutability triggers (`pg_trigger … tgname ILIKE '%reject%'`, non-internal, schema `sender`):
  **exactly three** — `trg_account_events_reject_update`, `trg_job_events_reject_update`,
  `trg_send_logs_reject_update`. No fourth table crept in across Phases 3–10.
- Every `ON DELETE SET NULL` FK in the schema (`pg_constraint … confdeltype='n'`): **six** —
  `pools_fallback_fk`, `jobs_assigned_account_fk`, `jobs_assigned_box_fk`,
  `job_events_account_fk`, `job_events_box_fk`, `account_events_account_fk`. The first three sit on
  tables with **no** immutability trigger (`pools`, `jobs` are freely updatable), so they cannot
  collide. The last three are the ones on trigger-bearing tables, and all three are now carved out.
- Inbound FKs *referencing* `send_logs`, `job_events`, or `account_events`
  (`confrelid IN (…)`): **zero rows**. So `send_logs`' "no inbound FK" claim is confirmed by query
  rather than assumed, and nothing references the two event tables either.

That is the check this ADR should have run the first time. A future phase adding either an
immutability trigger **or** a `SET NULL` FK must re-run both queries, not re-read this list.

## Links
`Docs/phase-2-account-management.md` §2.5/§7.4/§12, ADR-0002 (Phase 1 #23 append-only ordering /
no-BEFORE-DELETE lesson), ADR-0008 (grants trap; account_events explicit grants),
`Docs/open-decisions.md` OD-23, CLAUDE.md Rules 2/5.
