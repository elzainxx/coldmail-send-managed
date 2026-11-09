# ADR-0002: Phase 0 owns a bootstrap migration (schema + roles + grants)

- **Status:** accepted
- **Date:** 2026-07-22 (amended 2026-07-23 — bootstrap renumbered to `sender_0000`)
- **Supersedes:** —

> **Amendment 2026-07-23 (Phase 0 closeout, OD-17 ruled).** The bootstrap is renumbered
> from `sender_0001_bootstrap` to **`sender_0000_bootstrap`** (pre-feature groundwork). This
> keeps the LOCKED feature chain exactly `sender_0001..sender_0020` as every Phase 1–9 spec
> and the Phase 9 §4 config reference assume — with ZERO edits to any locked spec and no
> shift of the feature chain. The original text below (which shifted Phase 1 to start at
> `sender_0002`) is **superseded** by this amendment; it is retained for history. Net rule
> now: **Phase 0 = `sender_0000` (pre-feature); Phase 1 feature chain begins at
> `sender_0001_create_schema`, unchanged from the locked specs.**

## Context
The Phase 0 execution DONE criteria require the migration runner to **apply the empty
`sender` schema + roles/grants and record it, prove idempotency, and prove down**. The
Phase 0 plan and the Phase 1 drafts, however, placed schema creation (`sender_0001`) and
role creation (`sender_0002`) in Phase 1. These cannot both hold: to *prove* the runner
applies schema+roles, a real schema+roles migration must exist and be owned by Phase 0.

OD-13 (ruled) also requires a non-superuser `sender_owner` that OWNS the schema so
`FORCE ROW LEVEL SECURITY` on `sender.accounts` actually enforces — ownership must be
established at schema-creation time, i.e. in the bootstrap migration.

## Decision (as amended 2026-07-23)
Phase 0 ships **`sender_0000_bootstrap`** (pre-feature groundwork): create the `sender`
schema owned by a new non-login, non-superuser `sender_owner`; create the login app role
`sender_module`; create `sender.schema_migrations`; grant `sender_module` the enumerated
privileges (USAGE + default privileges) and the two read-only platform-table SELECTs;
`REVOKE ALL ON SCHEMA sender FROM PUBLIC`. It creates **no feature tables, enums, or
config** — those remain Phase 1+.

**Phase 1's feature migration chain begins at `sender_0001_create_schema`, exactly as the
locked Phase 1–9 specs and the Phase 9 §4 config reference assume — the feature chain is
`sender_0001..sender_0020`, unchanged.** Numbering the bootstrap `0000` (rather than shifting
the feature chain to `0002+`, the original approach below) preserves every locked
migration-number reference (Phase 2 `0013/0014`, Phase 5 `0016`, config `0010`/`0017–0020`)
with ZERO edits to any locked spec.

## Consequences
- "Phase 1 lands on solid ground" is literally true and PROVEN in Phase 0 (real apply +
  idempotent re-run + down against real Postgres, at version `0000`).
- `sender_owner` ownership makes `FORCE RLS` meaningful from the first feature table.
- Phase 1 authors their feature migrations at the locked numbers `sender_0001..`; the
  self-tracking convention (OD-4) is unchanged. The contiguity guard accepts `0000` as the
  chain start (`0000 → 0001` is contiguous, not a gap).
- ORIGINAL (2026-07-22, now SUPERSEDED): the bootstrap was `sender_0001_bootstrap`, which
  shifted Phase 1's drafted `sender_0001..0012` to start at `sender_0002`. Replaced by the
  `sender_0000` renumber above so the locked numbering is untouched.

## Note to Phase 1 — append-only REVOKE ordering (mandatory)

The bootstrap sets `ALTER DEFAULT PRIVILEGES FOR ROLE sender_owner IN SCHEMA sender GRANT
SELECT, INSERT, UPDATE, DELETE ON TABLES TO sender_module` — a blanket grant on every table
`sender_owner` will later create. This is deliberate (it keeps `sender_module` usable without
a per-table grant in each migration), but it means the **append-only invariant for
`job_events` and `send_logs` (CLAUDE.md Rule 5/6, DoD-7) is not automatic**: those tables are
born with full DML until a REVOKE narrows them.

**Requirement for Phase 1:** the `REVOKE UPDATE, DELETE ON sender.job_events` and `REVOKE
UPDATE ON sender.send_logs` statements MUST land in the **same migration** that creates those
tables (or an immediately-following migration applied atomically in the same `up` run) — never
deferred to a later, separately-deployable migration. Otherwise there is a window inside the
Phase 1 chain where `sender_module` can UPDATE/DELETE append-only rows. The append-only
BEFORE-UPDATE reject triggers are defense in depth, but the privilege REVOKE is the primary
control and must not lag the table creation. `migration_lint` cannot detect this ordering, so
it is a spec/review requirement recorded here.

## Links
CLAUDE.md Rules 1, 6; `Docs/phase-0-groundwork.md` §5; open-decisions.md OD-4, OD-13, OD-17.
