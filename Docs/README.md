# Docs/ — the map

Everything at this level tracks the **running system** and is kept honest by CI drift gates. Provenance —
the locked phase specs, the frozen decision ledger, the build record, and the documents that were merged
into the ones below — lives in [`history/`](./history/README.md) and is not part of operating or extending
the module. The same nine-document structure is used by all three module repositories.

Start at the [root README](../README.md) if you have not already.

## The documents

| Doc | What it answers | Gate |
|---|---|---|
| [`architecture.md`](./architecture.md) | The system's shape — services, boundaries, the send path, where each guarantee is enforced, and the **advisory-lock key registry**. Build status and the two capacity ceilings. | — |
| [`install-and-deploy.md`](./install-and-deploy.md) | How do I stand this up on a real server, from a bare VPS to a verified first send? Adding a worker, redeploying, rolling back, the safe migration ordering. **Part 7 — Developing and testing locally:** the 13-stage local gate and the honest list of what local testing cannot prove. | — |
| [`operator-runbook.md`](./operator-runbook.md) | Something is broken — what do I do? Diagnostic trees for the real failure modes, plus **capacity planning** and the measured throughput ceiling, **monitoring and alerting**, and **disaster recovery, key backup and rotation** (read the key-backup section before going live — losing the master key is unrecoverable). | — |
| [`config-reference.md`](./config-reference.md) | Every `send_managed.config` key: meaning, `value_type`, `default_value`, which subsystem consumes it. The single source for config. | `config_drift`, `config_consumption` |
| [`api-contract.md`](./api-contract.md) | Every route on both auth domains: request/response shape, error envelopes, status codes. The single source for endpoints. | `contract_drift` |
| [`security-posture.md`](./security-posture.md) | What is protected, how, and the honest residual risks. The page to hand a security reviewer. | — |
| [`go-live-checklist.md`](./go-live-checklist.md) | What must be confirmed before the first live deployment — each item with a real status (done with evidence, or owner-only) — and the **real-provider smoke test**, the first live send (written, deliberately not yet run). | — |
| [`future-work.md`](./future-work.md) | What is deliberately **not** built, why it was deferred, and the known defects that are flagged rather than fixed. | — |
| [`open-decisions.md`](./open-decisions.md) | Every ruling in force since the fork, with its evidence. **Zero open.** | — |
| [`adr/`](./adr/) | Architecture decision records, 0000 (template) and 0001–0014. Append-only; superseding writes a new ADR. | — |
| [`history/`](./history/README.md) | Provenance: locked phase specs, the frozen phase-0–10 ledger, the build record, merged-away documents. [`history/MOVED-DOCS.md`](./history/MOVED-DOCS.md) maps every old path to its new home. | — |

Machine-read inputs sit beside the documents they serve and are **not prose**:
`config-manifest.txt` and `config-consumers.txt` (inputs to the config gates — do not hand-edit without
re-running them) and the two canonical diagrams, `send_managed_backend_architecture.svg` and
`send_managed_job_lifecycle_states.svg`.

**Operating rules for changing the code** are in [`../CLAUDE.md`](../CLAUDE.md) — canonical, and
deliberately not mirrored here (OD-11).

## The rule that keeps these docs useful

`config-reference.md` and `api-contract.md` are the **single source** for config keys and endpoint shapes.
Operational docs **link** to them and never restate them — a config default copied into a runbook is a
default that will silently go stale. If you are about to document a key's value or a request body
outside those two files, don't.

Counts follow the same rule: do not write a number that a gate can derive. The config-key total is in the
header of `config-manifest.txt` (and printed by `config_drift`), the gate count is printed by
`test_enumeration`, and the harness total is the `HARNESS RESULT` line of a run. Where a doc quotes one, it
names the run and the commit it was read from.

## Build status

**COMPLETE.** All eleven phases are built, tested and closed (verdict GO for a pilot); the open-decisions
ledger is at zero open, and deferred work is registered in [`future-work.md`](./future-work.md). See
[`architecture.md`](./architecture.md#build-status-and-the-two-ceilings) for the verdict and the known
operational limits, and [`go-live-checklist.md`](./go-live-checklist.md) before deploying live. The
build-by-build *evidence* behind the verdict is provenance:
[`history/phase-build-record.md`](./history/phase-build-record.md).
