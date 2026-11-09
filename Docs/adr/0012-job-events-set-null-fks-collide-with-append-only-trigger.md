# ADR-0012: `job_events`' two `ON DELETE SET NULL` FKs collide with its unconditional append-only trigger

- **Status:** **accepted — option (A) ruled by the user 2026-07-29; implemented as `sender_0024` and
  tested on real infra the same day** (see "Ruling and what landed" below)
- **Date:** 2026-07-29
- **Supersedes in part:** ADR-0009's "The pattern is CLOSED for the current schema" section

## Context

Walking Stage 12 step 7 of `Docs/sender-module-local-test-guide.md` (re-run the Stage 5 security
checks with every variation toggle on) required creating a throwaway account, sending real mail
through it, and then cleaning it up. The cleanup — `DELETE /v1/admin/accounts/{id}`, which
`Docs/api-contract.md:112` documents as *"hard delete (audit row survives)"* → `204` — returned
**`500 internal_error`**:

```
admin request_id=req_55e42a88e05f1ae880e6da1f36c737f3 method=DELETE
  path=/v1/admin/accounts/20a0655b-3d8b-444e-965c-1dde5fe43f5f outcome=error
  code=internal_error reason=account_write_error:
  ERROR: sender.job_events is append-only: UPDATE forbidden (SQLSTATE 23000) level=error
```

This is **exactly the failure ADR-0009 diagnosed and fixed for `account_events`**, on a table
ADR-0009 examined and explicitly cleared.

### The mechanism (identical to ADR-0009's)

`sender.job_events` declares **two** `ON DELETE SET NULL` foreign keys:

```
job_events_account_fk  FOREIGN KEY (account_id) REFERENCES sender.accounts(id) ON DELETE SET NULL
job_events_box_fk      FOREIGN KEY (box_id)     REFERENCES sender.boxes(id)    ON DELETE SET NULL
```

Postgres implements `ON DELETE SET NULL` as an internal `UPDATE ONLY … SET <col> = NULL`. The
table's `BEFORE UPDATE` reject-trigger (`sender.fn_job_events_reject_update`, `sender_0008`) raises
on **every** update with no carve-out, so the FK's own maintenance update aborts and the parent
delete fails.

### Why ADR-0009 missed it

ADR-0009 §"The pattern is CLOSED for the current schema" states:

> - `sender.job_events` — append-only (`INSERT, SELECT`); inbound FK cleanup is `CASCADE DELETE`,
>   handled by the no-`BEFORE-DELETE` approach (correct as-is).

That premise is **factually wrong about the schema as built**. `job_events`' inbound cleanup is
*both* kinds: `job_events_job_fk` and `job_events_user_fk` are `ON DELETE CASCADE` (correctly
handled by installing no `BEFORE DELETE` trigger), **and** `job_events_account_fk` /
`job_events_box_fk` are `ON DELETE SET NULL` (not handled at all). ADR-0009 reasoned about
`job_events`' CASCADE FKs, concluded the unconditional UPDATE-reject could never collide with a
referential action, and closed the pattern — without enumerating the table's other two FKs, which
are the SET NULL ones and are the same shape as the `account_events` FK the ADR was written to fix.

The generalized rule ADR-0009 itself lays down is the correct one and would have caught this:

> an immutability trigger must permit **exactly the mutation its own inbound FK referential actions
> perform, and nothing else** … A blanket "reject all UPDATE/DELETE" trigger is a latent bug
> whenever the table is the *target* of an FK whose parent can be deleted.

`job_events` is such a table, twice over. The rule was right; its application to `job_events` was
not carried out.

## Observed evidence (real infra, owner role, transactional — no fixture data harmed)

Both SET NULL paths were driven at the SQL level as the schema **owner**, so this is not a grants
artifact and not an API-layer bug:

```
BEGIN;
DELETE FROM sender.accounts WHERE id='de7a1191-495e-4d51-8695-5eda1fa99c18';   -- 18 job_events rows
-- ERROR: sender.job_events is append-only: UPDATE forbidden
-- CONTEXT: SQL statement "UPDATE ONLY "sender"."job_events" SET "account_id" = NULL WHERE …"
ROLLBACK;

BEGIN;
DELETE FROM sender.boxes WHERE id='33333333-3333-3333-3333-333333333333';      -- 30 job_events rows
-- ERROR: sender.job_events is append-only: UPDATE forbidden
-- CONTEXT: SQL statement "UPDATE ONLY "sender"."job_events" SET "box_id" = NULL WHERE …"
ROLLBACK;
```

**Negative control (proves the trigger is the blocker, not the FK or the grant).** An account with
**zero** `job_events` rows deletes cleanly, because `SET NULL` then matches no row and no UPDATE is
issued:

```
BEGIN;
INSERT INTO sender.accounts (…, smtp_username='ctl', …);
DELETE FROM sender.accounts WHERE smtp_username='ctl';   -- DELETE 1
ROLLBACK;
```

Through the API, the same split: a freshly created, never-dispatched-to account returned **`204`**
(it has an `account_events` row — whose trigger *has* the carve-out — but no `job_events` rows),
while the account that had sent three messages returned **`500`**. So the defect is invisible on any
account that has never been used, which is why it survived the build and Phase 2's own T5/T9 tests:
those delete accounts that never had a job dispatched to them.

## Scope of the breakage (what is actually unavailable today)

| Operation | Status today |
|---|---|
| `DELETE /v1/admin/accounts/{id}` on an account that has ever been assigned a job | **fails `500`**, permanently — `job_events` rows are never pruned |
| `DELETE FROM sender.boxes` for a box that has ever processed a job | **fails**, same cause |
| `DELETE /v1/admin/pools/{id}` whose accounts have history | fails transitively (the account delete inside it fails) |
| Deleting a `public.users` row (tenant erasure) | fails, but for an **unrelated** reason — checked, not assumed: `pools_user_fk` on `sender.pools` declares no referential action, so the delete is refused with `violates foreign key constraint "pools_user_fk"` before any `job_events` trigger fires. Tenant erasure therefore requires deleting pools first, which requires deleting accounts, which lands back on the row above. Not a second defect; the same one reached by a longer path |
| Any account/box with no job history | works (`204`) |

Two consequences worth stating plainly:

1. **A GDPR-style hard delete of a real, used account is currently impossible** through any
   interface, including direct owner-role SQL. The `api-contract.md:112` "hard delete (audit row
   survives)" behavior is not merely undocumented-different, it is unavailable.
2. **The audit trail is not at risk either way.** Nothing is silently dropped; the delete aborts
   atomically. This is a wedged operation, not a data-loss or leak defect — which is why it is being
   FLAGGED for a ruling rather than hot-fixed under time pressure.

## Why this is Rule 2 (flagged, not fixed)

The obvious fix — extend `fn_job_events_reject_update` with the same narrow carve-out
`account_events` already has — is very likely the right answer, and it is what ADR-0009's own
generalized pattern prescribes. It is **not** being applied unilaterally because:

- It **contradicts an accepted ADR's explicit conclusion** ("correct as-is", "the pattern is
  CLOSED"). CLAUDE.md Rule 2: *"Never resolve a locked contradiction on your own authority."*
  ADR-0009 was a ruled Open Decision (OD-23); revising its finding is a ruling, not a refactor.
- The carve-out for `job_events` is **not a copy of `account_events`'** — this table has **two**
  nullable FK columns, so the permitted transition set is larger (`account_id`→NULL,
  `box_id`→NULL, and, when a box and its account are deleted in one statement, potentially both).
  Getting that set wrong either re-wedges the delete or widens the immutability hole. Which
  transitions are permitted is a decision about how much the audit guarantee bends.
- There is a genuine alternative (option C below) that keeps the trigger absolutely unconditional
  and changes the FK instead. That trades a locked DDL shape for a locked trigger shape; the two
  are not interchangeable and the choice is the user's.

## Options

**(A) FK-aware carve-out on `fn_job_events_reject_update` — the ADR-0009 pattern, applied.**
Permit an update iff it nulls one or both of `account_id`/`box_id`, sets no previously-NULL FK
column to non-NULL, and leaves **every** content column byte-identical (`id`, `job_id`, `user_id`,
`from_state`, `to_state`, `actor`, `error_class`, `error_message`, `metadata`, `created_at`).
Reject everything else, including a content change bundled with a null-out.
*Pro:* consistent with `account_events`, with ADR-0009's stated general rule, and with every locked
behavior (`204` + surviving audit). Content immutability — the property the audit actually protects
— is fully preserved. *Con:* the trigger stops being a one-line absolute; the permitted set must be
exhaustively enumerated and tested in both directions.

**(B) Do nothing; document that used accounts/boxes cannot be hard-deleted.**
*Pro:* zero risk to the audit trail. *Con:* contradicts `api-contract.md:112` and the Phase 2
locked §2.5 "delete is unconditional → `204`", so it does not remove the contradiction, it just
relocates it into the docs. Leaves no erasure path at all.

**(C) Change the two FKs from `ON DELETE SET NULL` to `ON DELETE CASCADE`.**
Then the no-`BEFORE-DELETE` approach already in place handles them and the trigger stays
unconditional. *Con:* **rejected on the merits, recorded for completeness** — cascading would
DELETE the job's entire event history when an account is deleted, destroying audit rows for jobs
that have nothing to do with the deletion, and `job_events` has no DELETE grant precisely to
prevent that. It also contradicts the same "audit survives" principle ADR-0009 protected.

**Recommendation: (A)**, with the permitted-transition set enumerated per-column and asserted in
both directions.

## Required test shape if (A) is ruled (Rule 3 + Rule 10)

Not a syntax test and not a single-direction test. Against real Postgres:

- **Positive:** delete an account that HAS `job_events` rows → succeeds; the affected rows survive
  with `account_id IS NULL` and every content column unchanged (compare a full-row fingerprint
  taken before the delete). Same for a box → `box_id IS NULL`. Same for an account and its box
  deleted in one statement.
- **Negative (the teeth):** as superuser, a content `UPDATE` on `job_events` still raises; an
  `account_id = NULL` **bundled with** a content change still raises; setting a NULL FK column back
  to non-NULL still raises. Without these, the carve-out is indistinguishable from dropping the
  trigger.
- **Regression guard:** an account with no history still deletes (the control above), so the fix is
  not credited for behavior that already worked.

## Ruling and what landed (2026-07-29)

**The user ruled option (A): the ADR-0009 pattern applied per-column.** Shipped as `sender_0024`,
which replaces `sender.fn_job_events_reject_update`'s body via `CREATE OR REPLACE` (same signature,
same trigger, same owner) and adds **no** object — no column, constraint, enum, index, grant, or FK
action changes (Rule 5, additive-only). `sender_0008`'s `REVOKE UPDATE, DELETE … FROM sender_module`
is untouched, so the app role still cannot issue *any* update: the carve-out is reachable only by the
system-performed FK maintenance statement and by the schema owner.

**The permitted set, enumerated.** An update passes iff *all* of:

1. `account_id` is unchanged **or** goes non-NULL → NULL;
2. `box_id` is unchanged **or** goes non-NULL → NULL;
3. **at least one** of them was actually nulled — a bare no-op `UPDATE` is still an update on an
   append-only table, and no referential action performs it, so it must still raise; and
4. every CONTENT column is byte-identical: `id`, `job_id`, `user_id`, `seq`, `from_state`,
   `to_state`, `actor`, `error_class`, `error_message`, `metadata`, `created_at`.

Written as two per-column clauses rather than the looser "an FK column changed". That is what lets a
box-and-account delete pass while still rejecting a NULL FK column being re-pointed at a *different*
row — rewriting provenance to blame an account that was never involved. `NEW.x IS NULL` is required
in every branch that permits a change, so the history-rewrite direction has no path through.

**What the audit guarantee now is, stated plainly.** Every content column stays byte-immutable. The
two FK columns are provenance *pointers*, and a pointer to a deleted row can only be NULL or a lie;
nulling one loses no recorded fact that the parent delete had not already destroyed. The transition,
its actor, its timing and its error context all survive — which is exactly what
`api-contract.md`'s "hard delete (audit row survives)" promises.

**A Postgres semantic that decided the implementation, learned by a failing control rather than by
reading.** The unchanged-side comparisons must be `IS NOT DISTINCT FROM`, not `=`. Three-valued
logic: `NULL = NULL` is NULL, not TRUE, so a plain `=` fails closed. A negative control with `=`
substituted was run against the behavioral suite and failed in **two** places — one anticipated, one
not:

- *anticipated* — the second of two sequential per-constraint updates when an account and a box are
  deleted together. Postgres issues one `UPDATE ONLY … SET <col> = NULL` **per FK constraint**, not
  one combined statement, so the second update sees the first's NULL already in place; and
- *not anticipated* — a plain single-FK **box** delete, because the **genesis** event row is born
  with `account_id` NULL (`fn_job_events_on_insert` copies `NEW.assigned_account_id` from a job that
  is not yet assigned). So `NEW.account_id = OLD.account_id` is NULL on the very first row of every
  job, and the `=` form would have wedged the *ordinary* box delete too — not merely the exotic
  combined case.

The second finding is the whole argument for a behavioral test over a careful reading: a reviewer
reasoning about "the combined case" would have shipped a version that still failed the common one.

**Tests** (`internal/dispatch/job_events_fk_carveout_test.go`, 5 tests, real Postgres; plus two
probes in the migration lifecycle test). Jobs are driven to a terminal state through the REAL
dispatch and outcome paths, so the rows under test carry trigger-written `account_id`/`box_id` rather
than values a test INSERTed. Content immutability is checked against a **pre-delete fingerprint** of
every content column, taken per row id, with the two FK columns fingerprinted separately — a
whole-row hash could not tell "the FK was nulled" (permitted) from "the actor was rewritten"
(forbidden). Positives: account delete, box delete, and both-in-one-transaction; in each case the FK
under test goes NULL, the *other* FK is asserted UNCHANGED (the per-column half), row count is
unchanged (nothing cascaded away), and content is byte-identical. Negatives are all driven as the
**superuser** connection, deliberately: `sender_module` has UPDATE revoked, so a rejection observed
as that role would prove the REVOKE and would pass against a tree with no trigger at all.

**Anti-vacuity, proven by three controls rather than asserted** (Rule 10):

| Control | Expected | Observed |
|---|---|---|
| revert to `sender_0008`'s unconditional body (`sender_0024` down) | the three positives fail | all three failed with the exact ADR-0012 wedge message |
| `DROP TRIGGER trg_job_events_reject_update` (simulates an over-wide carve-out) | the negative-controls test fails | failed on the first content UPDATE — "UPDATE succeeded, want rejection" |
| substitute `=` for `IS NOT DISTINCT FROM` | the combined-delete test fails | failed there **and** in the plain box-delete test (the genesis-row NULL above) |

No single regression satisfies both halves: reverting the carve-out fails the positives, dropping the
trigger fails the negatives.

**The migration lifecycle probe.** `sender_0024` replaces one function body and adds no object, so
every existence check in `runner_integration_test.go` is structurally blind to it, and a
`pg_get_functiondef` text match would assert the migration's own SQL against itself. Instead the test
**drives** the trigger in a rolled-back transaction: after `Up` an `account_id → NULL` update must be
permitted **and** a content update must still raise (paired — the permitted half alone would report
"landed" for a tree with the trigger dropped), and after `Down 0024` the same probe must be rejected
again, proving the down is real rather than a version-row deletion. Both directions were observed in
a single passing run.

**Re-driven end-to-end on the live dev stack, through the API that produced the original `500`** — the
Go suite proves the trigger; this proves the operator-visible outcome. `sender_0024` was applied to the
dev `platform` database **by the real migration runner** (`docker compose up migrate` →
`applied 0024_job_events_fk_carveout`), then Stage 12 step 7's exact sequence was repeated: create a
throwaway account via `POST /v1/admin/accounts`, send **3 real SMTP-authenticated messages** through it
(all reached `sent`, 6 `job_events` rows pointing at it), then `DELETE /v1/admin/accounts/{id}` →
**`204`** where it previously returned `500`. Post-delete: the account row is gone, all **6** event rows
are still present with `account_id` NULL, and each one's content fingerprint — `seq`, `from_state` →
`to_state`, `actor`, **and `box_id`** — matches its pre-delete value verbatim, so the *other* FK was not
collaterally nulled on the live stack either.

Two incidental facts that cost time and are worth recording for anyone repeating this by hand:
routing selected a *different* pool's account than the throwaway's, so the sends had to be steered by
disabling the alternatives (a `pool_exhausted: all eligible accounts terminal_blocked (§5.1)` failure is
the signal that you disabled too many); and `psql -tAc "INSERT … RETURNING id"` emits the `INSERT 0 1`
status line *after* the returned id, so an unfiltered capture sends a two-line string as the
`job_item_id` and earns a `400 validation_failed` (`malformed_json: invalid character '\n' in string
literal`) that looks like a contract problem and is not. Pipe through `head -1`.

## Links

ADR-0009 (the pattern, and the closed finding this ADR corrects), ADR-0002 / Phase 1 `#23`
(no-`BEFORE-DELETE` lesson), `db/migrations/sender_0008_create_job_events.up.sql` (the
unconditional trigger), `db/migrations/sender_0014_create_account_events.up.sql` (the carve-out to
mirror), `Docs/api-contract.md:112`, `Docs/history/phase-2-account-management.md` §2.5/§7.4,
`Docs/history/open-decisions.md` G12-OD-1, CLAUDE.md Rules 2/3/5/10.
