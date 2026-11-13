# SendManaged Module

A backend-only email-sending service. A platform hands it a mail list and a template; it handles
everything else — routing across a pool of SMTP accounts, pacing, retries, error classification,
and pulling unhealthy accounts out of rotation. There is no UI and no user-facing surface: the
platform calls its API, and operators run it.

**Status: complete and sealed.** Eleven build phases are implemented, tested against real
infrastructure, and closed. Verdict is GO for a pilot. The decision ledger is at **zero open**
([`Docs/open-decisions.md`](./Docs/open-decisions.md)). Before a first live deployment, work through
[`Docs/go-live-checklist.md`](./Docs/go-live-checklist.md) — every item there carries a real status
(done with evidence, or owner-only), not an empty box.

## How it works

Two services and one database. The **Control** (Go) is the only thing that talks to Postgres and the
only thing that can decrypt an SMTP credential; exactly one is ever active, guarded by a Postgres
advisory lock. It claims queued jobs, picks a healthy sending account, decrypts that account's
credential, and pushes the job over mutual TLS to a **send_managed worker** (Node.js) — a dumb SMTP relay
that ships no database driver and no KMS SDK, holds the credential in memory only, zeroes it on
every exit from the send path, and reports the outcome back. Workers scale horizontally and are never publicly
reachable. Everything tunable — 57 keys — is a seeded row in `send_managed.config`, changeable at
runtime with no redeploy; there are no hardcoded tuning constants. Full shape in
[`Docs/architecture.md`](./Docs/architecture.md).

## Run it locally

Needs Docker + Docker Compose. From a clone:

```bash
bash scripts/dev.sh certs   # dev CA + mTLS leaves + dev KMS key + operator key -> writes .env
bash scripts/dev.sh reset   # clean slate, in the seeded order: postgres -> migrate -> seed -> control -> worker -> mailpit
bash scripts/dev.sh test    # the real-infra behavioral harness (should be all-green)
```

That gives you a working stack sending into [Mailpit](http://localhost:8025) — a local SMTP sink,
so nothing leaves your machine. Use `reset` rather than a bare `up` on a fresh volume: a worker started
before its `send_managed.boxes` row is seeded is refused, and stays down. `bash scripts/dev.sh help` lists
the rest (`up`, `seed`, `logs`, `ps`, `down`); `make` delegates to the same script where it is available.

Nothing actually sends until `default_shared_pool_id` is set — that is expected, not a bug. See
[`Docs/operator-runbook.md`](./Docs/operator-runbook.md).

## Where to go next

The one-page map of every document is [`Docs/README.md`](./Docs/README.md). The short version:

| You want to | Read |
|---|---|
| **Install this on a real server** | [`Docs/install-and-deploy.md`](./Docs/install-and-deploy.md) |
| Verify the whole system on a dev machine first | [`Docs/install-and-deploy.md` Part 7](./Docs/install-and-deploy.md#part-7--developing-and-testing-locally) |
| **Fix something that is broken right now** | [`Docs/operator-runbook.md`](./Docs/operator-runbook.md) |
| Recover from a disaster / rotate keys | [`Docs/operator-runbook.md`](./Docs/operator-runbook.md#disaster-recovery-key-backup-and-rotation) |
| Size a deployment | [`Docs/operator-runbook.md`](./Docs/operator-runbook.md#capacity-planning) |
| Wire up monitoring and alerting | [`Docs/operator-runbook.md`](./Docs/operator-runbook.md#monitoring-and-alerting) |
| Go live for the first time | [`Docs/go-live-checklist.md`](./Docs/go-live-checklist.md) |
| Send the first real message through a real provider | [`Docs/go-live-checklist.md`](./Docs/go-live-checklist.md#real-provider-smoke-test-the-first-live-send) |
| Look up a config key or its default | [`Docs/config-reference.md`](./Docs/config-reference.md) |
| Call the API (platform or operator) | [`Docs/api-contract.md`](./Docs/api-contract.md) |
| Understand how the system is built | [`Docs/architecture.md`](./Docs/architecture.md) |
| Review the security posture and residual risks | [`Docs/security-posture.md`](./Docs/security-posture.md) |
| Know what is deliberately NOT built | [`Docs/future-work.md`](./Docs/future-work.md) |
| See every decision ruled since the fork | [`Docs/open-decisions.md`](./Docs/open-decisions.md) (zero open) and [`Docs/adr/`](./Docs/adr/) |
| **Change this code** | [`CLAUDE.md`](./CLAUDE.md) — non-negotiable build rules; read first |
| Know why something is the way it is | [`Docs/history/`](./Docs/history/README.md) — phase specs, the frozen ledger, the build record, merged-away documents |

## Layout

```
CLAUDE.md                 operating rules — the build-discipline anchor
docker-compose.yml        the dev stack, identical to what CI runs
scripts/dev.sh            the one entrypoint for local dev
services/control/         Go orchestrator — the ONLY DB client, the SOLE decryptor
services/worker/          Node.js SMTP relay — no DB, no KMS, memory-only credentials
db/migrations/            centrally-owned, self-tracking SQL (send_managed_NNNN_*.up/.down.sql)
db/preflight/             platform-table existence checks, run before migrating
tools/                    send-managed-migrate, provision-worker (worker + admin keys), gen-certs, gen-secrets, own-secrets
tests/harness/            real-infra behavioral harness (real Postgres, real worker, real SMTP)
tests/harness/checks/     the CI gate scripts and their paired self-tests
Docs/                     living documentation (operate + extend); Docs/README.md is the map
Docs/adr/                 architecture decision records (append-only)
Docs/history/             provenance — phase specs, the frozen ledger, build record, merged-away docs
deploy/                   production override material
```

## Tests and CI

Tests run against real infrastructure — real Postgres, a real Control↔worker mTLS channel, a real SMTP
server. There are no mock-the-database or mock-the-worker tests; that is a hard rule, not a
preference (CLAUDE.md Rule 3). `bash scripts/dev.sh test` runs the full suite, and CI
([`real-infra.yml`](./.github/workflows/real-infra.yml)) runs it on every push, followed by every gate in
`tests/harness/checks/`. Each gate catches a specific class of drift:

| Gate | Fails when |
|---|---|
| `config_drift` | a seeded config key is missing from `config-reference.md`, or vice versa |
| `contract_drift` | a served route is undocumented in `api-contract.md`, or vice versa |
| `migration_lint` | the migration chain has a gap, a missing `.down.sql`, or a non-self-tracking file |
| `config_consumption` | a seeded key has no real consumer and is not marked RESERVED |
| `no_secret_leak` | the worker registration token or its hash appears in service logs (the SMTP-credential canary scan lives in the send-path harness) |
| `dsn_scrub_wiring` | a database driver call site can put a connection string into a log |
| `drain_grace` | the shutdown grace in the compose file or the documented systemd unit is shorter than the drain timeout |
| `postgres_durability` | the default dev Postgres is ever RAM-backed (the RAM mode is an explicit opt-in) |
| `secret_ownership` | a dev key is loosened, or the worker's uid can read the KMS key |
| `image_pins` | any image reference — compose, Dockerfile, workflow, shell tooling — is not pinned by digest |
| `test_enumeration` | a test package is not run by `run.sh`, or a gate script is not wired into the workflow |

Four paired self-tests (`config_consumption_selftest`, `test_enumeration_selftest`, `image_pins_selftest`,
`go_suite_selftest`) break their gate on purpose and fail if it stays green. The live count is derived,
not written: `test_enumeration` prints it on every run. Run any gate directly:
`bash tests/harness/checks/<name>.sh`; the ones that read the live stack need `dev.sh reset` first.
