# Architecture

**What this is:** the system's shape — the services, the boundaries between them, the data flow,
and where each guarantee is enforced. It is the "how is this built" document. It deliberately does
**not** restate config keys (`config-reference.md`) or endpoint shapes (`api-contract.md`), and it
no longer carries the per-phase build diary — that moved to `history/phase-build-record.md`, which
is provenance rather than documentation. (Until 2026-11-09 this file was
`architecture-overview.md`, and the advisory-lock registry below was its own page,
`advisory-locks.md`.)

Canonical diagrams: `send_managed_backend_architecture.svg` and
`send_managed_job_lifecycle_states.svg` (both in this directory).

## Deployment (locked)

Single repository, multiple deployable services:

- **Control** (`services/control/`, Go) — the single active orchestrator. The ONLY DB client and
  the SOLE decryptor of account credentials. Guarded by a session-level Postgres advisory
  lock so exactly one is ever active. Serves the platform (Domain A) and operator (Domain B)
  surfaces and the mTLS worker-link.
- **SendManaged workers** (`services/worker/`, Node.js) — pure SMTP relays. NO DB, NO KMS; credentials
  held in memory only and zeroed on every exit from the send path. Scale by adding worker containers.
- **Shared Postgres/Supabase** — the module owns the `send_managed` schema (owned by the non-login
  `send_managed_owner` role); the platform owns `public`. The module reads exactly two platform
  tables (`public.users`, `public.job_items`) read-only.

For the physical deployment (which VPS runs what, the private network, systemd units,
version-skew ordering), see `install-and-deploy.md`.

## Channels & security boundaries

- worker ↔ Control: **mutual TLS** (both directions verified). Registration **token = authority**;
  client **cert = transport**. Decommission clears `token_hash` → refused regardless of cert.
  Alongside the register/heartbeat routes, the worker opens a **persistent** mTLS stream that
  carries job push + outcome report.
- **worker ↔ SMTP upstream**: the worker runs the SMTP session — STARTTLS-before-AUTH on
  587/25 (abort before AUTH if STARTTLS is absent); implicit TLS on 465.
- KMS Decrypt is **Control-only** (IAM separation; worker and operators hold none). Decrypt is scoped
  to the send path only (`internal/credential`), never reachable from the admin surface.

The full threat model, what each boundary actually protects, and the honest residual risks are in
`security-posture.md`.

## The send path, end to end

1. **Platform calls Domain A** (`quote` → `run` → `report` / `describe`, authenticated by the
   platform service token). `run` creates `send_managed.jobs` rows in state `queued`, under a per-user
   advisory lock enforcing one active campaign per user.
2. **The dispatcher tick** (Control, `internal/dispatch`) claims queued jobs with a
   `FOR UPDATE SKIP LOCKED` CTE against a partial index — exactly-once pickup under real
   concurrency. It resolves the sending pool (dedicated if the campaign has one, else
   `default_shared_pool_id`), picks an eligible account (not paused/quarantined/disabled, under
   its daily limit, respecting pacing), and reserves a concurrency lane.
3. **Control decrypts** that account's SMTP secret (AES-256-GCM; key held outside the DB, AAD binds
   ciphertext to `account.id ‖ credential_key_id`) and pushes the job + credential to a connected
   worker over the mTLS stream, with a lease and an attempt token.
4. **The worker runs the SMTP session** — TLS before AUTH, always — and reports the outcome back on
   the same stream. It holds the credential in memory only and zeroes it on every exit from the
   send path — `finally`, so accept, SMTP error and thrown exception all zero; process-level
   shutdown and uncaught-exception zeroing are registered residuals (`security-posture.md` §3.12). It
   never touches the DB or KMS.
5. **Control records the outcome** idempotently (`provider_message_id UNIQUE` + a pre-persist
   idempotency check), writes the `job_events` audit row via a DB trigger, applies the error
   taxonomy (which may pause or quarantine the account), and releases the lane.
6. **Failure recovery is automatic**: a dead worker or an expired lease is reclaimed by the reclaim
   loop and re-dispatched without penalizing the account; outcomes the worker could not deliver are
   buffered and idempotently replayed across Control restarts and DB outages.

The job state machine (`queued → assigned → sending → sent`, plus `sending → queued` requeue,
`sending → bounced`, `queued → failed`) is drawn in
`send_managed_job_lifecycle_states.svg`. Diagnosing where a job is stuck is
`operator-runbook.md`.

## Where each guarantee lives

| Guarantee | Enforced by | Where |
|---|---|---|
| Exactly-once job pickup | `FOR UPDATE SKIP LOCKED` CTE + partial claim index | DB + `internal/dispatch` |
| No double-send under retry/replay | `provider_message_id UNIQUE` + attempt token + pre-persist idempotency check | DB + `internal/dispatch` |
| Append-only audit | `job_events` / `account_events` triggers + `REVOKE UPDATE, DELETE` | DB (`send_managed_0008`, `send_managed_0014`) |
| Exactly one active Control | session-level advisory lock, fixed key | `internal/singleton` ([registry](#advisory-lock-key-registry)) |
| Control is sole decryptor | `internal/credential` wired only into `internal/dispatch`; KMS IAM grants Decrypt to the Control role only | code + cloud IAM (not code-checkable — see `security-posture.md`) |
| Worker holds no persistent secret | no DB driver, no KMS SDK shipped; in-memory creds zeroed on every **send-path** exit (not on shutdown — `security-posture.md` §3.12) | `services/worker` (structural) |
| Credential never crosses a client boundary | two-vocabulary error rule; `no_secret_leak` CI gate (worker token) + the send-path harness's SMTP-credential canary scan | `internal/adminapi`, CI |
| Tenant isolation | `user_id` on every table; RLS on all `send_managed.*`; `FORCE ROW LEVEL SECURITY` on `accounts` under the non-login `send_managed_owner` | DB |
| No deadlock on account/job writes | account → job lock ordering, codebase-wide | CLAUDE.md Rule 9 + ADR-0010 |
| Every tunable is config, not code | 57 seeded `send_managed.config` keys; `config_drift` + `config_consumption` CI gates | DB + CI |

## Control plane

Every autonomous subsystem has a kill-switch or mode dial, honored at runtime with no redeploy:
`dispatch_mode` (the panic switch, a graduated four-rung ladder — `serving` / `accept_and_defer` /
`refuse_new` / `stopped`, seeded `serving`; each rung stops strictly more than the one above, and
`refuse_new` and deeper also refuse `POST /v1/run` with `503 paused`. `accept_and_defer` is the
default halt: stop sending, keep accepting, lose nothing. The rung the Control is actually at is
reported as `dispatch.dispatch_rung` on `/v1/admin/health`),
`dispatch_enabled` (the original boolean panic switch — RETAINED and still honored, composed as a
severity FLOOR of `accept_and_defer` rather than as an independent lever, which is exactly what it
already did),
`autoquarantine_mode`, `autopause_mode` and `retention_mode` (all three `off`/`shadow`/`enforce`, all
three seeded **shadow** — every autonomous mutator is observable before it is allowed to act, which is
the property module 1 has had from the start and `send_managed_0026` finished here. **Consequence worth
knowing before an upgrade:** a freshly migrated deployment neither auto-pauses nor deletes expired
`send_logs` until an operator promotes each dial, so `retention_mode=enforce` is a REQUIRED go-live step —
left at `shadow`, `send_logs` grows without bound),
`autopause_enabled` (RETAINED, composed as a CEILING of `autopause_mode` — the same "a boolean can only
reduce what the system does" rule `dispatch_enabled` follows as a FLOOR, stated on the inverse scale),
`variation_enabled`, and `alert_webhook_url` (empty = alerting off). Their exact semantics and
defaults are in `config-reference.md`; when and how to flip them is `operator-runbook.md`.

## Advisory-lock key registry

Every Postgres advisory-lock key used in this repo is registered here so a future consumer
greps one file instead of discovering a collision in prod.

**Reserved key band (OD-10, ruled 2026-07-23):** the send_managed module reserves the
**`9_000_000_000 – 9_999_999_999`** single-key advisory-lock space (the "9-billions" band).
The platform owner (who owns the platform DB) treats this band as reserved for the send_managed
module; the platform's own advisory locks MUST NOT use keys in it. This is the documented,
reserved namespace that guarantees no collision on the shared instance.

| Key | Consumer | Scope | Notes |
|---|---|---|---|
| `9_100_000_001` | `tools/send-managed-migrate` migration runner | held only during `up`/`down`/`status` | prevents two migrators racing on the shared DB |
| `9_200_000_001` | Control singleton guard (`internal/singleton`) | session-level; held for the Control's lifetime **but not guaranteed to be** — see the note below | SIGKILL auto-releases (no stale lock) |
| `9_300_000_001` | Config-write serializer (`internal/adminapi` `handleConfigPut`) | transaction-scoped (`pg_advisory_xact_lock`); auto-released on commit/rollback | Serializes ALL `PUT /v1/admin/config/{key}` writes against each other, so the ordering-invariant check cannot be defeated by two concurrent PUTs to the two halves of one invariant pair (without it, each read a counterpart value that was valid when it read, both passed, and an inverted pair landed — wedging both dispatch and reclaim at config-LOAD). Config writes are rare operator actions, never on a hot path, so global serialization costs nothing. Added by Phase 10's R4 gate. |
| `hashtext('send_managed.campaign_start:' \|\| user_id)` | `internal/platformapi` `createJob` (Phase 4 §8, P4-7) | transaction-scoped (`pg_advisory_xact_lock`); auto-released on commit/rollback | Per-user lock serializing the one-active-campaign-per-user check-then-insert; a DYNAMIC key (hash of `send_managed.campaign_start:<user_id>`), not a fixed reserved value, so it is exempt from the fixed-key registry above by construction (the spec's own §8 pseudocode). `hashtext` returns a 32-bit hash spread across the full `int4` range (positive and negative), so it is not confined to the reserved `9_000_000_000–9_999_999_999` band, but the collision surface is a different user's SAME lock call, not the migration runner or the Control singleton (whose keys are fixed literals far outside `hashtext`'s typical output) — practically negligible, and the lock's blast radius on a false collision is a serialized (not incorrect) check, never data corruption. |

> **The singleton lock can be LOST mid-life, and that is designed for (G11-OD-1 / ADR-0011).**
> "Session-level" cuts both ways: the lock survives as long as its session does, so a Postgres restart,
> a failover, a connection reaper, or a `pg_terminate_backend` on the lock session drops it while the
> Control keeps running. This was observed live on 2026-07-29 — the lock went to **zero holders** and the
> Control kept dispatching for 56 minutes, letting a second Control acquire it. Since the fix, a Control that
> loses the lock **pauses** dispatch/reclaim/retention (it claims nothing and pushes nothing) and the
> keeper re-attempts acquisition every `brain_singleton_reacquire_interval_sec` (default 5s), so
> "exactly one dispatcher" holds across a loss rather than only from startup. Consequently a query for
> holders of this key returning **zero rows** does not mean no Control is running — it means no Control is
> currently entitled to dispatch. The operator triage for that state is in
> [`operator-runbook.md`](./operator-runbook.md#readyz-says-singleton_lockfalse-on-a-control-that-was-ready-mid-life-lock-loss).

> **OD-10 — pre-live setup step (not a build blocker):** before Phase 1 runs against the
> live platform DB, confirm with platform-db that the platform's own migrator/advisory locks
> avoid the reserved `9_000_000_000–9_999_999_999` band above. `pg_advisory_lock` scoping is
> per-database, so a collision only matters when both run against the same instance
> concurrently — which they do here. Recorded in the pre-live gate list
> ([`history/pre-live-gates.md`](./history/pre-live-gates.md)) and carried as item 3 of
> [`go-live-checklist.md`](./go-live-checklist.md).

---

## Provenance

`history/phase-build-record.md` is the per-phase build diary (what each phase landed, which review
gates found what). [`open-decisions.md`](./open-decisions.md) is the living flag-don't-fix ledger (every
ruling in force; zero open), [`history/open-decisions.md`](./history/open-decisions.md) is its frozen
phase-0–10 predecessor, [`adr/`](./adr/) holds the ADRs, and `history/` the locked phase specs (12 files
for 11 phases — `history/README.md` says why; documents merged into the living set on 2026-11-09 are under
`history/merged-2026-11/`, mapped in `history/MOVED-DOCS.md`).

## Build status and the two ceilings

**The build is complete and sealed.** All eleven phases are implemented, tested against real
infrastructure, and closed; the verdict is **GO** for a pilot. On the last verified GitHub Actions run
([42633655325](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42633655325), commit
`96a189f`) the behavioral harness reports `62 passed, 0 failed` and **15 gate scripts** pass — 11 gates
(`config_drift`, `contract_drift`, `migration_lint`, `config_consumption`, `drain_grace`,
`no_secret_leak`, `dsn_scrub_wiring`, `postgres_durability`, `secret_ownership`, `image_pins`,
`test_enumeration`) and 4 paired self-tests that break a gate on purpose and fail if it stays green.
The authoritative count is derived, not written here: `tests/harness/checks/test_enumeration.sh` fails
unless every `checks/*.sh` is named in `.github/workflows/real-infra.yml`, and prints the live number.
Deferred work is registered in [`future-work.md`](./future-work.md) rather than left implicit.

**Open Decisions: zero open.** Every decision raised across phases 0–10 and since the fork is ruled and
closed, and the rulings in force are listed in [`open-decisions.md`](./open-decisions.md). Two of them
shaped this document: **G11-OD-1** option B (a Control that loses the singleton lock **pauses** dispatch
and re-acquires on a bounded retry rather than exiting — [ADR-0011](./adr/ADR-0011-singleton-lock-loss-does-not-stop-dispatch.md))
and **G12-OD-1** option A (an FK-aware per-column carve-out on `job_events`' append-only trigger,
restoring the documented hard delete — `send_managed_0024`,
[ADR-0012](./adr/ADR-0012-job-events-set-null-fks-collide-with-append-only-trigger.md)). R2-5 (a stale example
inside a locked spec) was ruled 2026-08-07: annotate, do not rewrite. The evidence this verdict rests
on — which phase verified what, and what each review gate found — is in
`history/phase-build-record.md`'s "BUILD COMPLETE" section; it is provenance, not something you need in
order to run the system.

Two capacity ceilings exist and they are different kinds of number:

- **Per-account daily volume** is a hard, deliberate cap (`daily_limit` per account), reached in the
  normal course of business. Raising it is a reputation decision.
- **Single-Control send rate** is a soft, *measured* ceiling — ~370 sends/min on the specific dev
  hardware of the 2026-07-26 run, with that run's own caveat that the worker side was likely still a
  partial co-bottleneck.

**In essentially every V1-scale deployment, account daily limits bind first and the Control is idle.**
The Control's throughput only becomes the constraint once the account pool grows into the hundreds —
which is itself the signal that a scale-out conversation is due, and scale-out is a CLAUDE.md Rule 7
matter (one active Control, by invariant), not a knob. The arithmetic, a volume→accounts table, and
what to measure before committing to a number are in `operator-runbook.md#capacity-planning`; the measured run is
`operator-runbook.md#measured-throughput-ceiling-e11-load-test`.

## Known operational limits

Stated here because each one is a property of the architecture, not a bug:

- **The accepted-duplicate window** (a crash after SMTP `250` but before persist) is a rare, bounded,
  accepted residual. `provider_message_id UNIQUE` plus the pre-persist idempotency check bound it to
  at most one extra *delivery* — never a second billed job, never a corrupted state. Phase 6 §2.6 is
  the sole authoritative definition; it is referenced, never redefined.
- **No bounce or complaint ingestion, and no open/click tracking.** A provider `250` is recorded as
  `sent` and nothing downstream is observed. Synchronous SMTP-time bounces *are* classified; what is
  absent is everything a provider says after accepting the message. `describe` reports this honestly
  in its own capabilities block. See `future-work.md` §1 — it is the largest single gap.
- **One effective shared pool.** Dedicated pools exist and route correctly; campaign-to-pool binding
  beyond `default_shared_pool_id` is future work.
- **Auto-quarantine starts in `shadow`.** It decides and logs but does not act until an operator
  promotes it — see `go-live-checklist.md` for what to watch first.

`go-live-checklist.md` is the ordered set of platform-coordination and operator actions that must be
confirmed before the first live deployment.
