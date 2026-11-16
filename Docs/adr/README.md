# Architecture Decision Records — module 2 (`coldmail-send-managed`)

Numbered, append-only. A decision that is superseded gets a **new** ADR and the old one's status flips; nothing is renumbered or rewritten. The flag-don't-fix
ledger that precedes an ADR is [`../open-decisions.md`](../open-decisions.md) (`CLAUDE.md` Rule 2). Module 3 (`coldmail-send-byo`) was forked from this module at
ADR-0012 and carries 0000–0012 as provenance; its own numbering starts at 0013.

(Until 2026-11-13 these files lived in `Docs/history/decisions/` under bare names `NNNN-….md`; they were moved here and prefixed `ADR-` so all three module
repositories use the same layout and file naming. The numbers, titles and text are unchanged. See [`../history/MOVED-DOCS.md`](../history/MOVED-DOCS.md).)

| ADR | Decision |
|---|---|
| [0000](ADR-0000-adr-template.md) | the template every ADR follows |
| [0001](ADR-0001-single-repo-multi-service.md) | single repo, multiple services |
| [0002](ADR-0002-phase0-bootstrap-migration.md) | Phase 0 owns a bootstrap migration (schema + roles + grants) |
| [0003](ADR-0003-thin-self-tracking-migration-runner.md) | a thin self-tracking migration runner |
| [0004](ADR-0004-sender-owner-role.md) | a dedicated non-superuser `sender_owner` role |
| [0005](ADR-0005-mailpit-sink-and-mtls.md) | Mailpit SMTP sink + the mTLS box channel |
| [0006](ADR-0006-brain-sole-decryptor.md) | the control plane is the sole decryptor; the worker has no DB or KMS |
| [0007](ADR-0007-assumed-platform-table-shapes.md) | assumed platform-table shapes + a pre-flight existence check |
| [0008](ADR-0008-phase1-migrations-on-bootstrap-ground.md) | Phase 1 feature migrations layer onto the `sender_0000` bootstrap ground |
| [0009](ADR-0009-account-events-fk-aware-append-only-trigger.md) | the `account_events` append-only trigger must permit the FK `ON DELETE SET NULL` update |
| [0010](ADR-0010-concurrent-account-state-mutation-race-testing.md) | account-state mutation under concurrent outcomes requires a two-connection race test |
| [0011](ADR-0011-singleton-lock-loss-does-not-stop-dispatch.md) | a control plane that loses the singleton lock mid-life keeps dispatching (two can dispatch at once) |
| [0012](ADR-0012-job-events-set-null-fks-collide-with-append-only-trigger.md) | `job_events`' two `ON DELETE SET NULL` FKs collide with its append-only trigger |
| [0013](ADR-0013-dispatch-kill-switch-becomes-a-graduated-rung-ladder.md) | the dispatch kill-switch becomes a graduated rung ladder |
| [0014](ADR-0014-every-autonomous-mutator-gets-a-shadow-ladder.md) | every autonomous mutator gets an off / shadow / enforce ladder seeded at `shadow` |
| [0015](ADR-0015-the-mvcc-recheck-path-is-forced-by-a-test-only-rls-gate.md) | the consume's MVCC re-check path is forced by a test-only row-level-security gate, not left to volume |
| [0016](ADR-0016-cancel-attempt-cap-item-reasons-and-the-keyset-cursor.md) | `POST /v1/cancel` (with a row-level cancel guard), the attempt cap, `reason`/`error_class` on every report item, and the keyset cursor fixes |

**Next number: 0017.** (Module 2's file names have no leading zeros beyond four digits; keep `ADR-NNNN-kebab-title.md`.)
