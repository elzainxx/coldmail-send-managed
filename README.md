# SendManaged Module

A backend-only email-sending service. A platform hands it a mail list and a template; it handles
everything else — routing across a pool of SMTP accounts, pacing, retries, error classification,
and pulling unhealthy accounts out of rotation. There is no UI and no user-facing surface: the
platform calls its API, and operators run it.

**Status: complete and sealed.** Eleven build phases are implemented, tested against real
infrastructure, and closed. Verdict is GO for a pilot. Nothing is marked open. Before a first live
deployment, work through [`Docs/go-live-checklist.md`](./Docs/go-live-checklist.md).

## How it works

Two services and one database. The **Control** (Go) is the only thing that talks to Postgres and the
only thing that can decrypt an SMTP credential; exactly one is ever active, guarded by a Postgres
advisory lock. It claims queued jobs, picks a healthy sending account, decrypts that account's
credential, and pushes the job over mutual TLS to a **send_managed worker** (Node.js) — a dumb SMTP relay
that ships no database driver and no KMS SDK, holds the credential in memory only, zeroes it on
every exit from the send path, and reports the outcome back. Workers scale horizontally and are never publicly
reachable. Everything tunable — 53 keys — is a seeded row in `send_managed.config`, changeable at
runtime with no redeploy; there are no hardcoded tuning constants. Full shape in
[`Docs/architecture.md`](./Docs/architecture.md).

## Run it locally

Needs Docker + Docker Compose. From a clone:

```bash
bash scripts/dev.sh certs   # dev CA + mTLS leaves + dev KMS key + operator key -> writes .env
bash scripts/dev.sh up      # postgres -> migrate -> control -> worker -> mailpit, blocks on healthchecks
bash scripts/dev.sh seed    # dev fixtures (system user + dev worker row)
bash scripts/dev.sh test    # the real-infra behavioral harness (should be all-green)
```

That gives you a working stack sending into [Mailpit](http://localhost:8025) — a local SMTP sink,
so nothing leaves your machine. `bash scripts/dev.sh help` lists the rest (`logs`, `ps`, `reset`,
`down`). `make` delegates to the same script where it is available.

Nothing actually sends until `default_shared_pool_id` is set — that is expected, not a bug. See
[`Docs/operator-runbook.md`](./Docs/operator-runbook.md).

## Where to go next

| You want to | Read |
|---|---|
| **Install this on a real server** | [`Docs/install-and-deploy.md`](./Docs/install-and-deploy.md) |
| **Fix something that is broken right now** | [`Docs/operator-runbook.md`](./Docs/operator-runbook.md) |
| Go live for the first time | [`Docs/go-live-checklist.md`](./Docs/go-live-checklist.md) |
| Recover from a disaster / rotate keys | [`Docs/operator-runbook.md`](./Docs/operator-runbook.md#disaster-recovery-key-backup-and-rotation) |
| Look up a config key or its default | [`Docs/config-reference.md`](./Docs/config-reference.md) |
| Call the API (platform or operator) | [`Docs/api-contract.md`](./Docs/api-contract.md) |
| Understand how the system is built | [`Docs/architecture.md`](./Docs/architecture.md) |
| Review the security posture and residual risks | [`Docs/security-posture.md`](./Docs/security-posture.md) |
| Size a deployment | [`Docs/operator-runbook.md`](./Docs/operator-runbook.md#capacity-planning) — and [`Docs/operator-runbook.md`](./Docs/operator-runbook.md#measured-throughput-ceiling-e11-load-test) for the measured run behind it |
| Wire up monitoring and alerting | [`Docs/operator-runbook.md`](./Docs/operator-runbook.md#monitoring-and-alerting) |
| Send the first real message through a real provider | [`Docs/go-live-checklist.md`](./Docs/go-live-checklist.md#real-provider-smoke-test-the-first-live-send) |
| Know what is deliberately NOT built | [`Docs/future-work.md`](./Docs/future-work.md) |
| **Change this code** | [`CLAUDE.md`](./CLAUDE.md) — non-negotiable build rules; read first |
| Know why something is the way it is | [`Docs/history/`](./Docs/history/README.md) — specs, ADRs, decision ledger |

The full index, including the rule that keeps these docs from drifting, is
[`Docs/README.md`](./Docs/README.md).

## Layout

```
CLAUDE.md                 operating rules — the build-discipline anchor
docker-compose.yml        the dev stack, identical to what CI runs
scripts/dev.sh            the one entrypoint for local dev
services/control/           Go orchestrator — the ONLY DB client, the SOLE decryptor
services/worker/             Node.js SMTP relay — no DB, no KMS, memory-only credentials
db/migrations/            centrally-owned, self-tracking SQL (send_managed_NNNN_*.up/.down.sql)
db/preflight/             platform-table existence checks, run before migrating
tools/                    send-managed-migrate, gen-certs, gen-secrets, provision-worker (worker + admin keys)
tests/harness/            real-infra behavioral harness (real Postgres, real worker, real SMTP)
tests/harness/checks/     the 5 blocking CI gates
Docs/                     living documentation (operate + extend)
Docs/history/             provenance — phase specs, ADRs, decision ledger, build record
deploy/                   production override material
```

## Tests and CI

Tests run against real infrastructure — real Postgres, a real Control↔worker mTLS channel, a real SMTP
server. There are no mock-the-database or mock-the-worker tests; that is a hard rule, not a
preference (CLAUDE.md Rule 3). `bash scripts/dev.sh test` runs the full suite.

Five blocking CI gates run on every push, each catching a specific class of drift:

| Gate | Fails when |
|---|---|
| `config_drift` | a seeded config key is missing from `config-reference.md`, or vice versa |
| `contract_drift` | a served route is undocumented in `api-contract.md`, or vice versa |
| `migration_lint` | the migration chain has a gap, a missing `.down.sql`, or a non-self-tracking file |
| `config_consumption` | a seeded key has no real consumer and is not marked RESERVED |
| `no_secret_leak` | the worker registration token or its hash appears in service logs (the SMTP-credential canary scan lives in the send-path harness) |

Run any of them directly: `bash tests/harness/checks/<name>.sh`.
