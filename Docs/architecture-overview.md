# Architecture overview (living)

**What this is:** the system's shape — the services, the boundaries between them, the data flow,
and where each guarantee is enforced. It is the "how is this built" document. It deliberately does
**not** restate config keys (`config-reference.md`) or endpoint shapes (`api-contract.md`), and it
no longer carries the per-phase build diary — that moved to `history/phase-build-record.md`, which
is provenance rather than documentation.

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
| Exactly one active Control | session-level advisory lock, fixed key | `internal/singleton` (`Docs/advisory-locks.md`) |
| Control is sole decryptor | `internal/credential` wired only into `internal/dispatch`; KMS IAM grants Decrypt to the Control role only | code + cloud IAM (not code-checkable — see `security-posture.md`) |
| Worker holds no persistent secret | no DB driver, no KMS SDK shipped; in-memory creds zeroed on every **send-path** exit (not on shutdown — `security-posture.md` §3.12) | `services/worker` (structural) |
| Credential never crosses a client boundary | two-vocabulary error rule; `no_secret_leak` CI gate (worker token) + the send-path harness's SMTP-credential canary scan | `internal/adminapi`, CI |
| Tenant isolation | `user_id` on every table; RLS on all `send_managed.*`; `FORCE ROW LEVEL SECURITY` on `accounts` under the non-login `send_managed_owner` | DB |
| No deadlock on account/job writes | account → job lock ordering, codebase-wide | CLAUDE.md Rule 9 + ADR-0010 |
| Every tunable is config, not code | 53 seeded `send_managed.config` keys; `config_drift` + `config_consumption` CI gates | DB + CI |

## Control plane

Every autonomous subsystem has a kill-switch or mode dial, honored at runtime with no redeploy:
`dispatch_enabled` (panic switch — halts dispatch cleanly, zero job loss, resumable),
`autoquarantine_mode` (`off`/`shadow`/`enforce`, seeded **shadow**), `autopause_enabled`,
`variation_enabled`, and `alert_webhook_url` (empty = alerting off). Their exact semantics and
defaults are in `config-reference.md`; when and how to flip them is `operator-runbook.md`.

## Provenance

`history/phase-build-record.md` is the per-phase build diary (what each phase landed, which review
gates found what). `history/open-decisions.md` is the flag-don't-fix ledger, `history/decisions/`
the ADRs, and `history/` the locked phase specs (12 files for 11 phases — `history/README.md` says why). See it for when to consult
which.

## Build status and the two ceilings

**The build is complete and sealed.** All eleven phases are implemented, tested against real
infrastructure, and closed; the verdict is **GO** for a pilot. The full behavioral harness is green
and all **six** CI gates pass (`config_drift`, `contract_drift`, `migration_lint`,
`config_consumption`, `drain_grace`, `no_secret_leak` — this said "five" until 2026-07-29; the sixth
has been in `.github/workflows/real-infra.yml` since it was added). Deferred work is registered in
`future-work.md` rather than left implicit.

**Open Decisions.** The pre-deploy gate walkthrough (`coldmail-send-managed-local-test-guide.md`) surfaced two
new ones, **G11-OD-1** and **G12-OD-1**, and **both were ruled on 2026-07-29, implemented, tested on
real infra, and closed**: G11-OD-1 option B (a Control that loses the singleton lock **pauses** dispatch
and re-acquires on a bounded retry rather than exiting — ADR-0011), and G12-OD-1 option A (an FK-aware
per-column carve-out on `job_events`' append-only trigger, restoring the documented hard delete —
`send_managed_0024`, ADR-0012). The standing non-blocking exception (R2-5, a stale example inside a locked
spec) remains the only open item. Read `history/open-decisions.md`, not this paragraph, for the live
list. The evidence this verdict rests on — which
phase verified what, and what each review gate found — is in `history/phase-build-record.md`'s
"BUILD COMPLETE" section; it is provenance, not something you need in order to run the system.

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
what to measure before committing to a number are in `capacity-planning.md`; the measured run is
`load-test-results.md`.

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
