# ADR-0003: Thin self-tracking migration runner (OD-4)

- **Status:** accepted
- **Date:** 2026-07-22

## Context
The locked convention: every `sender_NNNN_*.up.sql` self-records into
`sender.schema_migrations` (`INSERT ... VALUES('NNNN')`); the `.down.sql` `DELETE`s it. This
one-row-per-migration model does not match golang-migrate/dbmate built-in trackers; adopting
one would require editing the locked migration bodies to strip the self-recording lines.

## Decision (RULED)
Build a thin purpose-built runner (`tools/sender-migrate`, Go). It reads
`sender.schema_migrations` to find applied versions, diffs against `sender_NNNN_*.up.sql` on
disk, and applies pending ones in `NNNN` order, each in its own transaction. It does NOT
write the tracking row — the file's own `INSERT`/`DELETE` does. Do NOT adopt golang-migrate.

## Consequences
Zero changes to any locked migration body. Single-writer bookkeeping (no dual-tracking).
`CREATE INDEX CONCURRENTLY` migrations are auto-detected and run outside a transaction. The
runner holds advisory lock `9_100_000_001` (registered in `advisory-locks.md`), distinct
from the Brain singleton key. Forward-only in prod (`down` gated behind an explicit flag).

## Links
CLAUDE.md Rule 1; `Docs/phase-0-groundwork.md` §5; open-decisions.md OD-4.
