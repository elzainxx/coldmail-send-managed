# Where documents went (path map for the frozen record)

The documents in this directory are **frozen**: they are left exactly as written, including their
citations of paths that have since moved. This page is the map from an old path to where that material
lives now. If a path cited in a frozen document does not resolve, look it up here (or, for anything
else, prepend `history/`).

Changes are listed once, on **2026-11-09** (documentation restructure; the same structure is used by
all three module repositories).

## Renamed or moved, content unchanged

| Old path (as cited in frozen documents) | Now |
|---|---|
| `Docs/architecture-overview.md` | [`../architecture.md`](../architecture.md) |
| `Docs/sender-module-local-test-guide.md` (the local test guide's name before it was `send-managed-local-test-guide.md`; cited by ADR-0011 and ADR-0012) | [`../install-and-deploy.md` Part 7](../install-and-deploy.md#part-7--developing-and-testing-locally) |
| `Docs/decisions/NNNN-*.md` and `Docs/history/decisions/NNNN-*.md` (the ADRs) | [`../adr/`](../adr/) — same file names, 0000–0014 |

## Merged into a canonical document; the source is kept here

Each merged source is preserved verbatim in [`merged-2026-11/`](./merged-2026-11/). The living text
is the section named in the right-hand column — edit that, never the preserved copy.

| Old path | Preserved copy | Living home |
|---|---|---|
| `Docs/advisory-locks.md` | [`merged-2026-11/advisory-locks.md`](./merged-2026-11/advisory-locks.md) | [`architecture.md` § Advisory-lock key registry](../architecture.md#advisory-lock-key-registry) |
| `Docs/capacity-planning.md` | [`merged-2026-11/capacity-planning.md`](./merged-2026-11/capacity-planning.md) | [`operator-runbook.md` § Capacity planning](../operator-runbook.md#capacity-planning) |
| `Docs/load-test-results.md` | [`merged-2026-11/load-test-results.md`](./merged-2026-11/load-test-results.md) | [`operator-runbook.md` § Measured throughput ceiling](../operator-runbook.md#measured-throughput-ceiling-e11-load-test) |
| `Docs/monitoring.md` | [`merged-2026-11/monitoring.md`](./merged-2026-11/monitoring.md) | [`operator-runbook.md` § Monitoring and alerting](../operator-runbook.md#monitoring-and-alerting) |
| `Docs/disaster-recovery.md` | [`merged-2026-11/disaster-recovery.md`](./merged-2026-11/disaster-recovery.md) | [`operator-runbook.md` § Disaster recovery, key backup, and rotation](../operator-runbook.md#disaster-recovery-key-backup-and-rotation) |
| `Docs/smoke-test-real-provider.md` | [`merged-2026-11/smoke-test-real-provider.md`](./merged-2026-11/smoke-test-real-provider.md) | [`go-live-checklist.md` § Real-provider smoke test](../go-live-checklist.md#real-provider-smoke-test-the-first-live-send) |
| `Docs/send-managed-local-test-guide.md` | [`merged-2026-11/send-managed-local-test-guide.md`](./merged-2026-11/send-managed-local-test-guide.md) | [`install-and-deploy.md` Part 7 — Developing and testing locally](../install-and-deploy.md#part-7--developing-and-testing-locally) |

Two things changed in the merge besides location, and only because a section under a new parent
document could not stay literally as written:

- Section numbers inside a merged document were kept (`§1`–`§6` under *Capacity planning*, `§1`–`§5`
  under *Monitoring* and *Disaster recovery*), so citations such as "disaster-recovery §4a" still point at
  the right paragraph in the new home.
- The local-test guide's Parts were renumbered under Part 7 (`7.1`–`7.5`); its `Stage 0`–`Stage 12`
  headings are unchanged. A cite of "guide Part 3" is now Part 7.3.

`Docs/coldmail-send-managed-local-test-guide.md`, cited by some code comments and two applied
migrations, **never existed on disk** — the file was always `send-managed-local-test-guide.md`; it maps to
Part 7 as above.

## Ledger

[`open-decisions.md`](./open-decisions.md) in this directory is the **frozen** phase-0–10 ledger. The
living ledger, which records every ruling in force since the fork, is
[`../open-decisions.md`](../open-decisions.md) and links back here as provenance.

## Stale entries in this directory's own index

[`README.md`](./README.md) (frozen) still points at `../disaster-recovery.md`, `../architecture-overview.md`
and `decisions/NNNN-*.md`; the rows above give the current locations.
