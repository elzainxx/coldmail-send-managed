# Docs/ — living documentation

Everything in this directory tracks the **running system** and is kept honest by CI drift gates.
Provenance — the locked phase specs, the ADRs, the decision ledger, the build record — lives
in [`history/`](./history/README.md) and is not part of operating or extending the module.

Start at the [root README](../README.md) if you have not already.

## Operate

| Doc | What it answers |
|---|---|
| [`install-and-deploy.md`](./install-and-deploy.md) | How do I stand this up on a real server, from a bare VPS to a verified first send? How do I add a worker, redeploy, roll back? What is the safe migration/deploy ordering? |
| [`operator-runbook.md`](./operator-runbook.md) | Something is broken — what do I do? Diagnostic trees for the real failure modes. |
| [`go-live-checklist.md`](./go-live-checklist.md) | What must be confirmed before the first live deployment? |
| [`disaster-recovery.md`](./disaster-recovery.md) | KMS key backup and recovery, DB restore, credential/key rotation. **Read the key-backup section before going live** — losing the master key is unrecoverable. |
| [`monitoring.md`](./monitoring.md) | Where do I point uptime monitoring and the alert webhook? |
| [`capacity-planning.md`](./capacity-planning.md) | How many accounts do I need for N sends/day, and what binds first? |
| [`send-managed-local-test-guide.md`](./send-managed-local-test-guide.md) | How do I verify the whole system on a dev machine before deploying? The 13-stage local gate, and the honest list of what local testing cannot prove. |
| [`smoke-test-real-provider.md`](./smoke-test-real-provider.md) | The first-contact-with-a-real-provider procedure. Written, deliberately not yet run. |

## Reference (the integrator/extender tier)

| Doc | What it answers | Gate |
|---|---|---|
| [`config-reference.md`](./config-reference.md) | All 53 config keys: meaning, `value_type`, `default_value`, which subsystem consumes it. | `config_drift`, `config_consumption` |
| [`api-contract.md`](./api-contract.md) | Every route on both auth domains: request/response shape, error envelopes, status codes. | `contract_drift` |
| [`architecture-overview.md`](./architecture-overview.md) | The system's shape — services, boundaries, send path, where each guarantee is enforced. | — |
| [`security-posture.md`](./security-posture.md) | What is protected, how, and the honest residual risks. The page to hand a security reviewer. | — |
| [`future-work.md`](./future-work.md) | What is deliberately **not** built, and why it was deferred. | — |
| [`advisory-locks.md`](./advisory-locks.md) | Every advisory-lock key in the repo + the reserved band. | — |
| [`load-test-results.md`](./load-test-results.md) | The measured single-Control throughput ceiling (committed artifact, not a dashboard). | — |
| `config-manifest.txt`, `config-consumers.txt` | Machine-readable inputs to the config gates. Not prose — do not hand-edit without re-running the gates. | both config gates |
| `send_managed_backend_architecture.svg`, `send_managed_job_lifecycle_states.svg` | The canonical diagrams. | — |

**Operating rules for changing the code** are in [`../CLAUDE.md`](../CLAUDE.md) — canonical, and
deliberately not mirrored here (OD-11).

## The rule that keeps these docs useful

The reference tier (`config-reference.md`, `api-contract.md`) is the **single source** for config
keys and endpoint shapes. Operational docs **link** to it and never restate it — a config default
copied into a runbook is a default that will silently go stale. If you are about to document a key's
value or a request body outside the reference tier, don't.

**Build status: COMPLETE.** All eleven phases are built, tested, and closed; the open-decisions
ledger is empty and deferred work is registered in [`future-work.md`](./future-work.md). See
[`architecture-overview.md`](./architecture-overview.md)'s build-status section for the verdict and
the known operational limits, and [`go-live-checklist.md`](./go-live-checklist.md) before deploying
live. The build-by-build *evidence* behind the verdict is provenance:
[`history/phase-build-record.md`](./history/phase-build-record.md).
