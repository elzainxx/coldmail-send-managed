# ADR-0008: Phase 1 feature migrations layer onto the sender_0000 bootstrap ground

- **Status:** accepted
- **Date:** 2026-07-23

## Context
The locked Phase 1 migration draft (`Docs/phase1-drafts/06-migrations.md`) places schema
creation at `sender_0001_create_schema` and role creation at `sender_0002_create_role`. But
Phase 0's bootstrap (`sender_0000`, ADR-0002) already creates the `sender` schema, the
non-login owner `sender_owner`, the login app role `sender_module`, `sender.schema_migrations`,
the `ALTER DEFAULT PRIVILEGES` blanket grant, and the two read-only platform-table SELECTs.
Both cannot literally create the same objects. The locked numbering must ALSO stay intact:
Phase 9 §4.1 pins config to `sender_0010`, and Phase 2/5 reference `sender_0013/0014/0016` —
so the feature chain must remain exactly `sender_0001..0012` with no shift.

A second, empirically-discovered wrinkle: the bootstrap's `ALTER DEFAULT PRIVILEGES FOR ROLE
sender_owner ... GRANT ... TO sender_module` keys on the *creator* of an object. Phase 1
feature tables are created by the admin migration role and then `ALTER ... OWNER TO
sender_owner` (required so `FORCE RLS` binds a non-superuser owner — ADR-0004). Default
privileges therefore do NOT fire for those tables — verified against real Postgres:
`sender_module` receives zero DML on the feature tables until an explicit grant.

## Decision (RULED — build-time reconciliation, no locked-spec edit)
1. **Keep the feature chain at its locked numbers `sender_0001..0012`.** `sender_0001` and
   `sender_0002` become **idempotent confirmations** of the bootstrap's schema + role +
   default privileges (`CREATE SCHEMA IF NOT EXISTS ... AUTHORIZATION sender_owner`,
   `IF NOT EXISTS` role guard, re-assert grants). Their DOWN un-records only the version row;
   they never drop the bootstrap-owned schema/roles (those reverse only via `sender_0000`'s
   down).
2. **Create feature objects as the admin runner, then `ALTER ... OWNER TO sender_owner`** —
   enums, all 7 tables, and trigger functions — so `FORCE RLS` on `sender.accounts` genuinely
   enforces against `sender_module`.
3. **Issue EXPLICIT table DML grants in `sender_0012`** (not relying on default privileges,
   which don't fire for reowned tables). This is *stronger* for the append-only invariant:
   `job_events` is born with zero `sender_module` DML and receives only `INSERT, SELECT`;
   `send_logs` receives `INSERT, SELECT, DELETE` (no UPDATE).
4. **Append-only REVOKEs land in the creating migration** (`0008` for `job_events`, `0009` for
   `send_logs`), per ADR-0002's mandatory ordering note — never deferred.
5. **The two platform-table SELECT grants stay owned solely by `sender_0000`.** `sender_0012`
   does not re-issue or revoke them, so rolling back `0012` never disturbs a grant the
   still-applied bootstrap owns.

## Grants invariant (enforced — every future phase)
The "explicit grants" requirement (point 3) is **manual discipline every future phase must
remember**: because default privileges do not fire for admin-created-then-reowned tables, a new
table with no explicit grant silently has **zero** rights for `sender_module` and fails at
runtime. This is now an **automated invariant**, not a convention:

> **New table ⇒ explicit grants, enforced by the grants invariant test.** Add the table's
> `GRANT` in the same migration that creates it. `TestGrantsInvariant`
> (`tests/harness/phase1_grants_invariant_test.go`) enumerates every `sender.*` table from the
> catalog and asserts `sender_module` holds exactly its class's privileges (normal = SIUD;
> append-only `job_events` = IS; write-once `send_logs` = ISD; bootstrap `schema_migrations` =
> S). A table added without grants FAILS the test rather than being skipped; a genuinely new
> privilege class must update the test's classifier in the same change.
> `TestGrantsInvariant_AntiVacuity` proves the check has teeth (an ungranted reowned table is
> rejected).

## Consequences
- Zero edits to any locked Phase 1–9 spec or to the Phase 9 §4 config reference; the feature
  chain is exactly `sender_0001..0012`, config at `sender_0010`.
- Proven on real Postgres by the single-connection `TestRunnerLifecycle` (race-immune): apply
  `0000→0012` in order, full post-state verified (roles, `sender_owner` ownership of all 7
  tables, 14 config keys, `FORCE RLS` on `accounts`), idempotent re-run, DOWN reverses
  `0012→0000` cleanly. Contiguity guard + `migration_lint` + `config_drift` green.
- The `config_drift` gate's doc-key regex was widened to accept digit-bearing keys
  (`retry_count_4xx`); this is gate maintenance, not a contract change.

## Links
ADR-0002 (Phase 0 bootstrap + append-only ordering note), ADR-0004 (`sender_owner`),
`Docs/phase-1-data-model.md`, `Docs/phase1-drafts/06-migrations.md`, CLAUDE.md Rules 1/5/6.
