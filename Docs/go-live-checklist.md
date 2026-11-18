# GO-LIVE CHECKLIST

The build is sealed ([`architecture.md`](./architecture.md#build-status-and-the-two-ceilings); verdict **GO**
for a pilot). Everything below is a **going-live confirmation or operator action**, not a build blocker —
the module is implemented and proven in the real-infra harness (`scripts/dev.sh test`, **62 passed, 0 failed**
on GitHub Actions, [run 42633655325](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42633655325),
commit `96a189f`) without any of these. They become mandatory the moment this module points at a real
platform database and starts sending real mail. Nothing here is code to write; each item is a setup step,
a coordination point, or an operational decision an operator or ops team makes before or at launch.

Cross-reference: [`history/pre-live-gates.md`](./history/pre-live-gates.md) (G1–G4, the
platform-coordination half) and [`history/open-decisions.md`](./history/open-decisions.md) (G1–G5, P9-A, S8,
S15) are the authoritative source for the reasoning behind each item; the living ledger is
[`open-decisions.md`](./open-decisions.md) (zero open). This document is the single ordered checklist a
launch operator actually works through.

## How to read the status marks

Every item carries one real status — nothing is "unchecked by default".

| Mark | Meaning |
|---|---|
| ✅ | **Done**, with the evidence cited (run URL, commit, test, ADR). |
| ⏳ | **Owner-only, real-world step.** It needs a real platform database, a real KMS key, a real provider or a real decision that only the owner (or the platform/ops team) can make. The repository cannot do it, and it has **not** been done. Where the *mechanism* behind the step is already proven in this repo, the evidence is cited next to it so you know exactly what is and is not left to do. |
| ➖ | **Not applicable** to this module or this deployment shape, with the reason. *(None at present.)* |

The ⏳ items are also collected, with why only the owner can do them, in the owner's action list (`OWNER-ACTIONS.md` in the parent repository).

## Status at a glance

| # | Item | Status | Evidence — or what only you can do |
|---|---|---|---|
| 1 | Set `default_shared_pool_id` | ⏳ | Needs the real pool and real accounts on the live deployment. Mechanism proven: `tests/harness/phase3_send.sh` sets it and delivers end to end through a real worker (SUITE 9, run 42633655325). |
| 2 | Confirm the real `public.users` / `public.job_items` shapes | ⏳ | Needs the real platform schema. The pre-flight (`db/preflight/check_platform_tables.sql`) checks existence only, by design; the assumed shape is recorded in [ADR-0007](./adr/ADR-0007-assumed-platform-table-shapes.md). |
| 3 | Advisory-lock band and migration admin credential | ⏳ | Needs platform-side confirmation. The band is registered in [architecture](./architecture.md#advisory-lock-key-registry) (OD-10). |
| 4 | KMS/IAM separation — platform roles hold no Decrypt | ⏳ | Needs the real cloud IAM policy; **no automated re-check exists**. The code-level half is done: [ADR-0006](./adr/ADR-0006-brain-sole-decryptor.md), and SUITE 5 proves the worker container cannot read key material (run 42633655325). |
| 5 | Provision workers out-of-band | ⏳ | Needs the real worker hosts. Procedure proven on the dev stack: add and remove a worker (Stage 11 steps 3–4, `b103980`); a revoked worker is refused on its next connect. |
| 6 | Promote auto-quarantine / auto-pause to `enforce` | ⏳ | **Deliberately post-launch**, after an observation window of real traffic — it cannot be done before there is traffic. Mechanism: [ADR-0014](./adr/ADR-0014-every-autonomous-mutator-gets-a-shadow-ladder.md); both ladders driven live in Stage 10 steps 3–4 (`6d773ba`). |
| 7 | Accounts are pre-warmed | ⏳ | Needs real mailboxes on real domains with real reputation. The module has no ramp logic by design ([future-work §1](./future-work.md)). |
| 8 | Back up the master key and run the recovery drill | ⏳ | Needs the real key and real off-host storage — the one unrecoverable loss. The restore drill is proven on the dev stack **by a decrypt, not a clean boot** (Stage 11 step 1, `d158119`), and rotation both ways (`ff83bac`). |
| 9 | Module vulnerabilities | ✅ cleared 2026-11-07 · ⏳ re-run on pilot day | `GO-2026-5004` / `-5970` cleared by moving the builder to `golang:1.26-alpine` (`4640dfb`): `govulncheck` 0 live call paths in all four Go modules. The advisory database moves, so only a run on the day can say it is still clean. |
| 10 | Wire monitoring; the webhook needs a poller | ⏳ | Your uptime monitor, cron entry, webhook receiver and log shipping. Mechanism proven: all three alert conditions fire and are cross-checked (Stage 10 steps 6–7, `bfb3910`). |
| 11 | Real-provider smoke test | ⏳ | Real provider, real sending reputation. The procedure is written (below) and **deliberately not run**. |
| 12 | Promote `retention_mode` to `enforce` | ⏳ | A required config flip on the real deployment, after confirming `debug_log_retention_days` against your own privacy commitments. Mechanism: ADR-0014 / `c300fe6` (`send_managed_0026`). |
| PF | Final pre-flight | see [its own table](#final-pre-flight-run-immediately-before-flipping-live-traffic-on) | |

## Evidence already recorded in this repository

- **Harness and gates (✅):** `HARNESS RESULT: 62 passed, 0 failed`; **15 gate scripts** green — 11 gates and
  4 paired self-tests (run 42633655325). The authoritative count is derived by `test_enumeration`, not
  written here. After any later push, read the run, not this line.
- **Database outage (✅):** the real `docker stop postgres` drill passes 22/22 and the whole harness passes
  in the default (non-RAM) mode, on a runner
  ([run 42499590634](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42499590634), on-demand
  `outage-drill` workflow): the Control halts cleanly, fails closed, invents nothing, and delivers the
  in-flight job exactly once on recovery. This is the repository's own evidence for the outage item; re-run it
  on the deployed shape before a pilot.
- **Dev key ownership (✅):** dev keys stay mode 600 but are owned by the uid of the container that reads them
  (`tools/own-secrets.sh`, run by `dev.sh certs`); the blocking `secret_ownership` step fails if they are
  loosened. Dev/CI only — **production key handling is unchanged** and governed by
  [`install-and-deploy.md`](./install-and-deploy.md) (`chmod 400`, root-owned key directory).
- **Supply chain (✅):** every image is pinned by digest and `image_pins` rejects any unpinned reference;
  worker `node:24-alpine`, Go final stages `alpine:3.24` (ledger rows M2-CI-OD-4/5).
- **Decision ledger (✅):** zero open — [`open-decisions.md`](./open-decisions.md).

---

## 1. Set `default_shared_pool_id` (REQUIRED — base sending does not work without it)

> **Status: ⏳ owner-only.** A real pool, real accounts and one config write on the live deployment. The mechanism is already proven: `tests/harness/phase3_send.sh` sets the key and delivers end to end through a real worker (SUITE 9, CI run 42633655325).

**Why it blocks:** Phase 4 §4.2/§4.3 — base (non-dedicated) campaigns resolve their sending pool
through `default_shared_pool_id`. It is seeded `NULL` (Phase 1 `send_managed_0010`) and stays `NULL`
until an operator sets it. Every base-campaign job created before this is set stays cleanly
`queued` (`shared_pool_not_configured`, logged once per interval, never an error the platform
sees) — it does not fail loudly, which means it is easy to forget.

**Action:**
1. Create at least one enabled shared pool: `POST /v1/admin/pools {"kind":"shared","name":"..."}`.
2. Add at least one eligible account to it: `POST /v1/admin/accounts {...}`.
3. `PUT /v1/admin/config/default_shared_pool_id {"value":"<pool-uuid>"}`.
4. Confirm: `GET /v1/admin/config/default_shared_pool_id` returns the pool id, and
   `GET /v1/admin/health` shows the pool with `headroom` other than `exhausted`.

**Verify before declaring done:** run one real `quote`→`run`→`report` cycle against a throwaway
`job_item` for the reserved system user (or any base tenant) and confirm it reaches `sent`.

---

## 2. Confirm the real `public.users` / `public.job_items` column shapes (G3 / OD-8)

> **Status: ⏳ owner-only.** Needs the real platform schema, which this repository cannot see. Pre-flight checks existence only, by design; the assumed shape is [ADR-0007](./adr/ADR-0007-assumed-platform-table-shapes.md).

**Why it blocks:** the module's pre-flight (`db/preflight/check_platform_tables.sql`) checks table
**existence only**, deliberately (a column-level check would false-fail every time the platform
legitimately adds an unrelated column). It does NOT verify the actual column set the module's
FKs/reads assume.

**Assumed shape** (ADR-0007; verify against the real platform schema before the first live
migration run):
- `public.users(id uuid PRIMARY KEY, ...)`
- `public.job_items(id uuid PRIMARY KEY, campaign_id uuid, user_id uuid, recipient_email text, rendered_subject text, rendered_body text, rendered_text text NULL (Phase 3 close-out B3: the optional plain-text part; the pre-flight REQUIRES this column), reply_to text, status text, created_at timestamptz, ...)`

**Action:** diff the real platform schema against the assumed shape above. If the platform's real
columns differ (renamed, different type, additional NOT NULL constraints the module doesn't
populate), that is a genuine spec-vs-platform mismatch — flag it as a new Open Decision, do not
silently adapt the module's assumptions.

---

## 3. Settle the advisory-lock key namespace + the migration admin credential source (G1, G2 / OD-1, OD-10)

> **Status: ⏳ owner-only.** Needs explicit platform-side confirmation of both points. The band is registered in [`architecture.md`](./architecture.md#advisory-lock-key-registry); the migration runner takes its admin credential from the `MIGRATE_DATABASE_URL` environment variable.

**Why it blocks:** two distinct collision risks, both operational rather than code:

- **Advisory-lock band (G2 / OD-10).** The module reserves the
  `9_000_000_000–9_999_999_999` advisory-lock key band (`Docs/architecture.md#advisory-lock-key-registry`) for its own
  singleton guard + per-user campaign-start locks. If the platform's own migrator or application
  code uses advisory locks in the SAME Postgres instance, confirm it does not use this band.
- **Migration admin credential (G1 / OD-1).** The migration runner (`tools/send-managed-migrate`)
  connects via `MIGRATE_DATABASE_URL` as an admin/owner-capable role, DISTINCT from the
  login-only, non-superuser `send_managed_app` app role — it must be able to `CREATE ROLE`/
  `CREATE SCHEMA`/assign ownership to `send_managed_owner`. Confirm which real role that is in the live
  environment and that its credential is sourced from the platform's secret store, never
  committed to a repo or migration file.

**Action:** get explicit platform-side confirmation of both before the first live migration run.
Neither is fixable retroactively without risk once the module is live against shared
infrastructure.

---

## 4. Verify KMS/IAM separation — platform roles must NOT hold Decrypt (G4 / S15)

> **Status: ⏳ owner-only.** The cloud IAM grant is not code-checkable and has no automated re-check. The code-level half is done: [ADR-0006](./adr/ADR-0006-brain-sole-decryptor.md) (the `Decrypt` capability is wired only into `internal/dispatch`), and SUITE 5 proves the worker container cannot read key material (CI run 42633655325).

**Why it blocks:** this is CLAUDE.md Rule 6's load-bearing security property — "Control is the SOLE
decryptor. KMS Decrypt is Control-only. The platform and operators hold NO Decrypt capability." It
is enforced structurally in code (the `internal/credential` Decryptor is wired only into
`internal/dispatch`, never into `internal/adminapi` — OD-7) but the actual cloud IAM grant is
**not code-checkable**; it is a deployment/infrastructure configuration that must be verified
against the real KMS key policy.

**Action (a deployment-checklist sign-off, per S15's ruling):**
1. Confirm the prod KMS key's IAM policy grants `Decrypt` to the Control's runtime role ONLY.
2. Confirm the admin/operator-facing service's IAM role has `Encrypt`/DEK-wrap grants but
   explicitly NOT `Decrypt` (mirrors the code-level separation: `internal/adminapi`'s `Encryptor`
   has no `Decrypt` method at all).
3. Confirm no human operator or platform service identity holds `Decrypt` on this key.
4. Record the sign-off (who verified, against which key policy, when) — this is the one gate in
   this checklist with no automated re-check; treat it as an incident if it's ever found to have
   drifted.

---

## 5. Provision workers out-of-band (token = authority, cert = transport, revocation)

> **Status: ⏳ owner-only** for the real worker hosts. The procedure is proven on the dev stack: a third worker added and a worker removed with no Control reconfiguration (Stage 11 steps 3–4, `b103980`); a worker with a cleared token is refused on its next connect regardless of its cert.

**Why it's a checklist item, not a build gap:** V1 deliberately has no `/v1/admin/workers` REST
surface (P9-A, RULED — Phase 9 §5.4a). Worker lifecycle is an out-of-band operator procedure using
`tools/provision-worker`.

**Action — provisioning a new worker:**
1. `provision-worker new [--id <id>]` — mints a `box_id` (client-supplied UUID; the operator's
   choice) + a high-entropy token + its bcrypt hash. **Record the plaintext token somewhere it
   will reach the worker's own deploy config — it is never stored anywhere else and cannot be
   recovered after this step.**
2. `provision-worker seed --id <id> --token-hash <hash>` — emits the SQL to insert/update the
   `send_managed.boxes` row (`token_hash`, `status='active'`). Run it against the live DB via the
   migration-admin role (never `send_managed_app` directly, matching the rest of the module's DDL
   discipline).
3. Deploy the worker process with the plaintext token in its own environment/secret store. On first
   connect it `register`s over mTLS; the Control hashes the presented token and compares to
   `token_hash` — a match starts routing jobs to it.
4. Confirm: `docker compose logs control` (or the equivalent prod log sink) shows
   `worker registered id="<id>"`.

**Action — decommissioning/rotating a worker:**
1. `provision-worker revoke --id <id>` — emits SQL clearing/rotating `token_hash` and setting
   `status='disabled'`. Run it against the live DB.
2. **Stop the worker process.** The token check runs only in the register handshake, so the SQL above
   refuses the *next* connect but does not disconnect a worker that is already streaming — and until it
   disconnects the Control keeps pushing it credentialed jobs. For a planned decommission you are
   stopping it anyway; for a *suspected compromise* isolate the host **first** and revoke second
   ([`operator-runbook.md`](./operator-runbook.md#disaster-recovery-key-backup-and-rotation) §4a).
3. On its next connect attempt the worker is refused regardless of its still-valid mTLS client
   cert (**token is the authority; cert is transport only** — a decommissioned worker cannot talk
   its way back in with cert alone).
4. Any job that worker held in flight falls to the Phase 6 reclaim loop and re-dispatches to a live
   worker automatically — no manual job requeue needed.
5. **Rotation** is the same procedure as decommission + re-provisioning: revoke the old token,
   mint + seed a new one, roll the worker's deploy secret.

---

## 6. Decide when to promote auto-quarantine — and auto-pause — from `shadow` to `enforce`

> **Status: ⏳ owner-only, and deliberately post-launch** — it needs an observation window of real traffic, so it cannot be done before there is any. The mechanism is done: [ADR-0014](./adr/ADR-0014-every-autonomous-mutator-gets-a-shadow-ladder.md), with both ladders driven live in Stage 10 steps 3–4 (`6d773ba`).

> **Since `send_managed_0026` (fix-plan item 13) this section covers TWO dials.** `autopause_mode`
> joined `autoquarantine_mode` on the same `off`/`shadow`/`enforce` ladder, seeded `shadow` for the same
> reason, and everything below applies to it with `throttle` substituted for the quarantine trigger:
> in `shadow` a throttle outcome writes a `shadow_would_pause` observation row to `account_events` and
> the account keeps sending, and `PUT /v1/admin/config/autopause_mode {"value":"enforce"}` restores the
> pre-`0026` behaviour with no redeploy. What to watch is whether `throttle_pause_seconds` and your
> provider's actual rate-limit responses agree — a pause that fires on every routine `450` is a
> throttle threshold mis-calibrated for that provider, and a real tenant stops sending for its
> duration each time.
>
> `autopause_enabled` still works and is still honoured, composed as a CEILING: `false` caps the dial at
> `off` no matter what `autopause_mode` says. Setting the boolean to `true` does **not** promote a mode
> you left in `shadow` — promotion is always an explicit write to `autopause_mode`.
>
> **The third dial, `retention_mode`, is NOT this item.** It is REQUIRED and blocking — see item 12.

**Why it's a checklist item:** `autoquarantine_mode` seeds `shadow` (Phase 9.5 §2.1) — a
deliberate launch-safety default. In `shadow`, the Control computes the SAME quarantine decision it
would in `enforce` and logs it (`account_events`, a `shadow_would_quarantine` observation), but
does not act — the account keeps sending. This gives an operator a real trial period to observe
what the scoring model WOULD do before it starts actually quarantining accounts.

**What to watch before promoting to `enforce`:**
1. Pull `account_events` filtered to shadow observations over a representative window (at least
   several days of real send volume, ideally spanning a full `daily_reset` cycle).
2. For each shadow-would-quarantine account, manually sanity-check: was the account genuinely
   unhealthy (recurring `auth_failure`/`bounce_permanent`/sustained `throttle`), or is the score
   formula (`status_score_weights`) producing a false positive on a healthy account under normal
   transient noise?
3. If the shadow decisions look correct (they consistently flag genuinely bad accounts and leave
   healthy ones alone), promote: `PUT /v1/admin/config/autoquarantine_mode {"value":"enforce"}`.
   No redeploy, no restart — the very next outcome-handling report picks up the new mode.
4. If the shadow decisions look wrong (over- or under-sensitive), tune `quarantine_threshold`
   and/or `status_score_weights` FIRST, re-observe another window, THEN promote — never promote
   to `enforce` on a scoring model you haven't watched operate for real.
5. `autoquarantine_mode="off"` remains available if the decision itself needs to be disabled
   entirely (not just deferred) — distinct from `shadow`, which still computes and logs.

**Do not promote silently.** This is the one kill-switch flip in the whole system that changes
what happens to a REAL account (it starts getting quarantined), not just what the Control observes
— treat the promotion itself as a deliberate, reviewed operational change, not a config tweak.

---

## 7. Confirm accounts are pre-warmed (no ramp logic in V1)

> **Status: ⏳ owner-only.** Needs real mailboxes on real sending domains. The module has no ramp logic by design ([`future-work.md`](./future-work.md) §1).

**Why it blocks:** the module has NO built-in warm-up/ramp mechanism for a newly-added SMTP
account — `daily_limit` applies at its full configured value from the account's very first day.
A brand-new account added to a shared pool inherits `daily_limit_default` and will be routed up to
that full figure on day one if the pool needs the volume, with no automatic gradual ramp. (What the
default actually is: [`config-reference.md`](./config-reference.md).)

**Also check while you are looking at the accounts:** no account's `smtp_host` is a private-range or
link-local address. The API accepts one, but the worker **skips TLS certificate verification** for those
(a harness affordance for the local sink), so the credential would cross an unverified connection —
[`security-posture.md`](./security-posture.md) §3.13. `SELECT id, smtp_host FROM send_managed.accounts;`

**Action:** if the SMTP accounts being provisioned are genuinely new (freshly created mailboxes,
new sending domains, accounts that haven't built sending reputation with receiving providers),
the OPERATOR must pre-warm them manually before go-live — e.g. by setting a conservative
`daily_limit` at account-creation time and raising it manually over subsequent days/weeks as
deliverability holds, rather than relying on the module to do this automatically. This is a
reputation/deliverability concern the module does not manage; a too-aggressive ramp on a
brand-new account risks provider throttling/blocking, which the module WILL correctly detect and
react to (throttle class → pause, or repeated auth/reputation failures → quarantine) — but that
reactive handling is not a substitute for a sensible warm-up plan on the operator's side.

---

## 8. Back up the master key, and run the recovery drill (REQUIRED — the one unrecoverable loss)

> **Status: ⏳ owner-only.** Needs the real key and real off-host storage. The restore drill itself is proven on the dev stack **by a decrypt, not a clean boot** (Stage 11 step 1, `d158119`), and rotation in both directions (`ff83bac`).

**Why it blocks:** SMTP credentials are encrypted with a key held **outside** the database. A DB
backup restores ciphertext and nothing else. Lose the key and **every SMTP credential is permanently
unrecoverable** — recovery is a manual password reset at each provider, for every account. There is
no escrow and no recovery share, and Decrypt is Control-only by design (CLAUDE.md Rule 6), so nothing
can rescue the plaintext.

**Action:** work through [`operator-runbook.md`](./operator-runbook.md#disaster-recovery-key-backup-and-rotation) §1 before the **first real
credential is entered** — not before launch, before the first `smtp_secret`. Specifically:

- ⏳ The key is backed up **off-host**, encrypted, in at least two independent locations, with the
      passphrase held separately.
- ⏳ The `CREDENTIAL_KEY_ID` **pairing** is recorded with the backup (a restore under the wrong
      label fails AAD authentication).
- ⏳ The "where the backup lives / who can reach it" table is **filled in**. An unfilled table is
      the same as no backup.
- ⏳ The **recovery drill has been run** on staging and reached a completed send. A clean Control
      startup proves only that the key is 32 bytes — it does **not** prove the key is right. Only a
      decrypt proves a restore.

---

## 9. Re-check the module vulnerabilities (was blocking — cleared 2026-11-07; re-run before a pilot)

> **Status: ✅ cleared 2026-11-07 (`4640dfb`) · ⏳ re-run on pilot day.** Only a run on the day can say the advisory database is still clean.

**Status:** `GO-2026-5004` (`pgx/v5`) and `GO-2026-5970` (`golang.org/x/text`) were cleared on 2026-11-07
by moving the builder to `golang:1.26-alpine` (go1.26.10) and bumping `pgx` to v5.10.0 and `x/text` to
v0.43.0 — see [`future-work.md`](./future-work.md) §5. `govulncheck` reported **no vulnerabilities** in
all four Go modules, and the full harness plus all gates were green on the new toolchain.

**Why it is still a checklist item:** the advisory database keeps moving, so "clean on 2026-11-07" does
not mean clean on the day a pilot carries real data.

**Action:** immediately before the pilot, run `govulncheck ./...` in `services/control`,
`tools/send-managed-migrate`, `tools/provision-worker` and `tests/harness`; any live call path blocks.

---

## 10. Wire monitoring, and know that the webhook needs a poller

> **Status: ⏳ owner-only.** Your uptime monitor, scheduled poll, webhook receiver and log shipping. The mechanism is proven: all three alert conditions fire and are cross-checked against the health snapshot (Stage 10 steps 6–7, `bfb3910`).

**Why it blocks:** alert conditions are evaluated **when `GET /v1/admin/health` is called**, not by a
background scanner. **With nothing polling it, no webhook ever fires** — alerting is silently inert.
This is the single most missable operational fact in the system.

**Action:** [`operator-runbook.md`](./operator-runbook.md#monitoring-and-alerting).

- ⏳ Uptime monitor points at **`/readyz`** (not `/healthz`, which stays green while nothing is
      dispatching), alerting after two consecutive failures.
- ⏳ A scheduled health poll exists (the cron entry in `operator-runbook.md#monitoring-and-alerting` §3). This is what makes the
      webhook work.
- ⏳ `alert_webhook_url` is set, and a test alert has actually been **received** — not just
      configured. Note `SEND_MANAGED_ALERT_WEBHOOK_URL` in `.env.example` is **inert**; the config row is
      the only source.
- ⏳ Logs are shipped somewhere with retention. The internal `reason=` behind a client error
      envelope exists **only** in the Control's log; without retention, a reported error is a code and
      a request id with nothing behind them. **This is also your only record of config changes** —
      there is no `config_events` table, so a `PUT /v1/admin/config` leaves a process-log line and
      nothing else. Retention here is an audit control, not just an ops convenience
      ([`security-posture.md`](./security-posture.md) §3.6).
- ⏳ `alert_webhook_url` points where you intend. It is validated for scheme prefix only — a private
      or link-local address is accepted and will be POSTed to from inside the trust boundary
      ([`security-posture.md`](./security-posture.md) §3.14).

---

## 11. Run the real-provider smoke test (the first live send)

> **Status: ⏳ owner-only.** Real provider, real sending reputation. The procedure is written (below) and deliberately **not run**.

**Why it blocks:** every test up to this point sends into a local sink. The first contact with a real
provider touches **real sending reputation**, and a bad first impression is not a test failure you
re-run.

**Action:** the [real-provider smoke test](#real-provider-smoke-test-the-first-live-send) below, start to finish, in that
order — one message to yourself, then a second provider, then the deliberate failure-handling checks,
then a small real batch. It is written to be small, to addresses you control, and reversible. Item 8
(key backup **and** drill) is a prerequisite of this item, not a follow-up.

---

## 12. Promote `retention_mode` to `enforce` (REQUIRED — `send_logs` grows without bound until you do)

> **Status: ⏳ owner-only.** A required config flip on the real deployment. The mechanism is done: `send_managed_0026` / [ADR-0014](./adr/ADR-0014-every-autonomous-mutator-gets-a-shadow-ladder.md) (`c300fe6`); the sweep is asserted never to touch anything but `send_logs`.

**Why it blocks:** `send_managed_0026` (fix-plan item 13) seeds `retention_mode` at `shadow`, which
means **the retention sweep deletes nothing**. It counts what it would drop and logs it; the rows stay.
So on a freshly migrated deployment, `send_managed.send_logs` — which holds **recipient address,
subject, body, headers and SMTP response** for every send — accumulates forever, and:

- **The retention promise is not being kept.** `debug_log_retention_days` reads like a bound and is not
  one while this dial is at `shadow`. `Docs/security-posture.md` is explicit that
  `debug_log_retention_days` is a *privacy* decision rather than a tuning one, and that a database dump
  inside the retention window exposes message content and recipient lists. At `shadow` there is no
  window — every dump is inside it.
- **The disk bill is unbounded**, and the growth is in the widest rows in the schema.

This is the deliberate cost of seeding the dial safe (ADR-0014 decision 2: a safety default that has to
be opted into is not a default). It is a blocking checklist item precisely because the safe seed makes
it silent otherwise.

**Do this:**

1. Confirm `debug_log_retention_days` is the value you actually intend — this promotion makes it real
   for the first time. Check it against your own privacy commitments, not just against a plausible
   number.
2. Read the shadow line the sweep logs on every tick and note the count:
   ```
   retention: retention_shadow_would_delete — retention_mode="shadow", so NOTHING was deleted this
   tick; 41823 expired send_logs row(s) would be dropped at retention_mode=enforce. send_logs GROWS
   WITHOUT BOUND until this dial is promoted
   ```
   If that number is large because the deployment has been running at `shadow` for a while, the FIRST
   enforcing sweep will delete all of it. `maxSweepBatchesPerTick` bounds one tick's work (default
   64 × 1000 = 64k rows), so a big backlog drains over several ticks rather than in one statement —
   but plan the promotion for a window when you can watch it, not at peak.
3. Promote: `PUT /v1/admin/config/retention_mode {"value":"enforce"}`. No redeploy, no restart — the
   very next sweep tick acts.
4. Verify on the next tick: the shadow line stops and
   `retention: dropped N expired send_logs row(s) (raw content only; job_events + final job states
   untouched)` appears instead.
5. **`shadow` and `off` both remain available afterwards**, and one of them is the right answer during
   an incident: if a sweep is dropping more than you expect, demote to `shadow` to stop the deletions
   while keeping the count visible. The dial is re-read every tick in both directions.

**What promoting does NOT put at risk:** the sweep touches `send_managed.send_logs` and nothing else at
any value of the dial. Job outcomes — `job_events` and final `jobs` states — are permanent by
construction (Phase 7 §3.2), which is asserted against the sweep's own source, so per-day reporting
still works for a swept day. What is lost at the drop is signal that lived only in the raw rows:
recipient-domain distribution and `smtp_response` text (the accepted V1 cost, Phase 7 §3.2).

---

## Order of operations (recommended)

1. **Before the first live migration run:** items 2 (column shapes) and 3 (advisory-lock band +
   migration credential) — these are platform-coordination items that must be settled before the
   module's schema exists in the live DB.
2. **Before the first real credential is entered:** item 8 (key backup **and** the recovery drill).
   Deliberately earlier than "before launch" — the moment a real `smtp_secret` exists, an unbacked-up
   key is an unrecoverable-loss exposure. Item 9 (the two module vulns) belongs here too: fix the
   build before it carries real data.
3. **Before the first live send:** item 4 (KMS/IAM sign-off) — the sole-decryptor security
   property must be verified against real infrastructure before any real credential is decrypted.
   Item 10 (monitoring) as well: going live unmonitored means finding out from a customer.
4. **Before the first live campaign:** items 1 (`default_shared_pool_id`) and 5 (worker
   provisioning) and 7 (account pre-warming) — the system has no capacity to route a job until
   these are done. Then item 11 (the real-provider smoke test) — the first live send is a
   deliberate, small, watched one, never a campaign.
5. **Before the first live send, alongside item 4:** item 12 (`retention_mode=enforce`). Placed here
   and not later because from the first real send onward `send_logs` is accumulating recipient
   addresses and message bodies with no bound — the exposure starts with the data, so the bound has to
   as well. It is the one item on the "promote a shadow dial" list that is REQUIRED rather than
   deferred, because leaving it is not a cautious choice but an unbounded one.
6. **After a launch-safety observation window:** item 6 (auto-quarantine **and auto-pause**
   promotion) — deliberately NOT day-one; give both shadow dials real time to prove themselves first.
   These two are genuinely deferrable: leaving them at `shadow` costs observation rows and a control
   that is not yet acting, not unbounded growth.

## Final pre-flight (run immediately before flipping live traffic on)

Status per row: ✅ done with evidence · ⏳ owner-only, at the moment of go-live. The only row that can be ✅ now is PF-2;
the rest describe the deployed system, which does not exist yet.

- ⏳ **PF-1** `scripts/dev.sh test` (or the prod-equivalent harness invocation) is green — full behavioral
      suite against the ACTUAL deployed images, not a stale build. *Repository side done:* 62 passed, 0 failed
      on a runner (run 42633655325). *Left for you:* running it against the images you actually deploy.
- ✅ **PF-2** All 17 CI gate scripts green (run 43594048091 for the two newest, `consume_contention` and its self-test, with the other 15 passing in the same run; `test_enumeration` prints the live
      count on each run — re-read the run after any later push): `config_drift`, `contract_drift`, `migration_lint`,
      `config_consumption`, `config_consumption_selftest`, `no_secret_leak`, `drain_grace`,
      `test_enumeration`, `test_enumeration_selftest`, `dsn_scrub_wiring`, `postgres_durability`,
      `secret_ownership`, `go_suite_selftest`, `image_pins`, `image_pins_selftest`, `consume_contention`, `consume_contention_selftest` (re-derived from
      `tests/harness/checks/` on 2026-11-14; this line said 9 while 15 were wired, and 15 before the last two were added). (The `_selftest` gates are paired controls:
      each deliberately breaks the gate it names and fails if that gate stays green, so a gate cannot
      quietly stop checking anything.)
- ⏳ **PF-3** The deployment's SIGTERM grace clears `shutdown_drain_timeout_sec` — `stop_grace_period` if
      you run under Compose, `TimeoutStopSec` if you run under systemd, and **both** if the unit
      wraps a compose file (the shorter of the two wins). `drain_grace` checks the committed dev
      file and the documented unit; it cannot see YOUR prod file, so confirm this one by hand.
      Getting it wrong is not unsafe — leave-in-place invents nothing — but it turns every clean
      shutdown into reclaim work and exits 137. *Repository side done:* `drain_grace OK: drain 30s < docker
      stop_grace_period 90s and systemd TimeoutStopSec=120` (run 42633655325); the open part is your prod file.
- ⏳ **PF-4** `GET /readyz` reports `db:true, singleton_lock:true`. *(Proven on the dev stack, incl. the
      lock-loss path — Stage 11 step 6, `e7f92a6`; on the deployed Control it is yours to read.)*
- ⏳ **PF-5** `GET /v1/admin/health` shows the expected pool(s) with non-`exhausted` headroom and
      `dispatch_paused:false`.
- ⏳ **PF-6** Items 1–5, 7–11 and **12** above are done; item 6 is deliberately deferred to a post-launch
      observation window, per its own section. Item 12 (`retention_mode=enforce`) is NOT deferrable
      the way item 6 is: until it is promoted, `send_logs` accumulates recipient addresses and message
      bodies with no bound and the retention promise in `security-posture.md` is not being kept. If you
      are flipping live traffic on with `retention_mode` still at `shadow`, that is a decision someone
      has to have made on purpose, not a box left unchecked.
- ⏳ **PF-7** The master key backup **and drill** (item 8) are done. This is the one item on this page whose
      omission cannot be corrected after the fact.
- ⏳ **PF-8** You have read [`future-work.md`](./future-work.md) and accept the V1 limits it records —
      particularly that there is **no bounce or complaint data**, so provider-side monitoring is your
      only deliverability feedback loop.

---

## Real-provider smoke test (the first live send)

> **STATUS: ⏳ WRITTEN, NOT RUN — owner-only.** Deliberately. Every test in this repo up to now sends into
> [Mailpit](http://localhost:8025), a local sink — nothing has ever left a machine. This procedure is
> the system's first contact with a real SMTP provider and a real inbox, and it touches **real
> sending reputation**. Running it is **step one of the pilot**, not part of the build. Do not run it
> to "check the docs are right."

The reason for the care: a bad first impression with a provider is not a test failure you re-run. An
account that authenticates wrong repeatedly, sends to an address that hard-bounces, or trips a
volume heuristic on day one carries that history forward. This procedure is shaped to make the first
send **small, to yourself, and reversible**.

Config keys and request shapes are not restated here — [`config-reference.md`](./config-reference.md)
and [`api-contract.md`](./api-contract.md).

---

### 1. Prerequisites

Do not start until all of these are true.

- ⏳ The install guide has been completed end to end and a **Mailpit** send has succeeded.
      [`install-and-deploy.md`](./install-and-deploy.md). If it has not sent to a sink, it is not
      ready to send to a provider.
- ⏳ The master key is **backed up off-host and the recovery drill has been run**.
      [`operator-runbook.md`](./operator-runbook.md#disaster-recovery-key-backup-and-rotation) §1. You are about to enter a real credential;
      do not enter one into a system whose key you cannot restore.
- ⏳ `jq` is installed, and you can read `GET /v1/admin/health`.
      [`operator-runbook.md`](./operator-runbook.md).
- ⏳ You have a **dedicated throwaway or low-stakes sending account** at the provider — not the
      company's primary mailbox.
- ⏳ You have **two recipient addresses you control**, at *different* providers (e.g. one Gmail, one
      Outlook). Different providers is the point: it is how you learn whether you landed in inbox or
      spam.
- ⏳ You know the provider's real per-mailbox daily and per-hour limits, and the account's SPF/DKIM
      state for the sending domain.
- ⏳ Someone other than you knows you are doing this, and when.

**Never** point a first smoke test at a purchased list, a customer list, or any address you do not
personally control. One hard bounce from a guessed address is a worse outcome than any information
this test could give you.

---

### 2. Pre-flight configuration

Set these before the first send. They are the difference between a controlled test and an incident.

| Setting | Why, for this test |
|---|---|
| `daily_limit` on the test account: **set it low** (e.g. 10) | A bug that loops cannot become a hundred messages. This is the single most valuable guard here. |
| `dispatch_enabled` | Leave `true`, but know exactly how to set it `false`. That is your stop button; keep the command in the terminal ready to paste. |
| `autoquarantine_mode` | Leave at its **shadow** default. You want to see what it *would* do without it acting on a first-contact anomaly. |
| Pacing (`send_delay_min_sec`/`send_delay_max_sec`) | Do not zero them. The default delay is the reputation-friendly behavior; the load test zeroes them only because it is measuring the Control. |
| `alert_webhook_url` + a health poll | Wire [`operator-runbook.md`](./operator-runbook.md#monitoring-and-alerting) *first*. Alerts that only fire when the endpoint is polled are worth having during a first run. |
| SMTP port | 587 (STARTTLS) or 465 (implicit TLS). AUTH never precedes TLS; on 587/25 the session **aborts before AUTH** if the server does not advertise STARTTLS, and the credential is never sent. |

Record the starting state so you can tell what the test changed:

```bash
export ADMIN=http://127.0.0.1:8080
read -rs -p 'operator admin key: ' KEY; echo; export KEY   # not an assignment: keeps it out of ~/.bash_history
ahealth() { curl -fsS "$ADMIN/v1/admin/health" -H "Authorization: Bearer $KEY"; }

# Two captures on purpose: a wide one to read now, and a narrow SORTED one to diff against in §5.
# The diff baseline must use the SAME filter as the diff itself or it will never come back clean.
ahealth | jq '{dispatch, accounts, pools, workers, errors_last_window}' | tee /tmp/smoke-before-full.json
ahealth | jq -S '{accounts, pools}' > /tmp/smoke-before.json
```

---

### 3. The procedure

#### Step 1 — Create the account with the real credential

`POST /v1/admin/accounts` with the provider's host, port, username, and `smtp_secret`, a low
`daily_limit`, and the pool you will send from. Exact body:
[`api-contract.md`](./api-contract.md).

Verify **only** that the response reports `has_credential: true`. That is by design the most you can
ever read back — the admin surface encrypts on write and holds no Decrypt capability, so there is no
route that will show you the secret again ([`security-posture.md`](./security-posture.md)).

**Expected:** `201`, `has_credential: true`. Nothing has authenticated to the provider yet — a
credential is only exercised at send time, so a wrong password looks fine at this step.

#### Step 2 — Confirm eligibility before queueing anything

```bash
ahealth | jq '{pools, accounts, workers}'
```

**Expected:** the pool present with `headroom` **not** `exhausted`, the account counted as eligible,
at least one worker connected, `dispatch_paused: false`. If any of that is wrong, stop and use the
runbook — do not queue a job hoping it resolves.

#### Step 3 — ONE message, to yourself

Queue exactly one job (`POST /v1/run` with `{job_item_id, user_id}`) whose single recipient is the
first address you control.

Then watch it, and give it a **hard time bound**: if it has not reached a terminal state within a few
minutes, set `dispatch_enabled=false` and diagnose before trying again. An unattended retry loop
against a real provider is the thing to avoid.

```bash
# Domain A platform token here — NOT the operator key. The two never share a credential.
curl -fsS "$PLATFORM/v1/report?user_id=$USER_ID&job_id=$JOB_ID" \
  -H "Authorization: Bearer $PLATFORM_TOKEN" | jq
```

**Expected:** `queued` → (briefly `assigned`/`sending`) → `sent`, with a `provider_message_id`
present. Note that `provider_message_id` and `completed_at` are **omitted entirely** until set — check
for key presence, not for `null`.

**Then check the actual inbox.** This is the step the whole procedure exists for, and no field in any
API can substitute for it:

- Did it arrive at all?
- **Inbox or spam?**
- Are SPF/DKIM/DMARC passing in the received headers?
- Is the From/display name what you intended?

A `sent` state means the provider returned `250` and nothing more. **V1 has no bounce, complaint,
open, or click data** — if this message later bounces, is filtered, or is silently dropped, this
system will never know. Your eyes on the inbox are the only feedback loop that exists.

#### Step 4 — The second provider

Repeat step 3 with the address at the *other* provider. Different receivers apply very different
filtering, and one inbox placement tells you much less than two.

**Expected:** delivered at both, ideally both to inbox. Inbox at one and spam at the other is normal
for a new sending domain and is a *domain reputation* signal, not a defect in this module — the levers
that exist are in the runbook's deliverability section.

#### Step 5 — Prove failure handling on purpose

Reputation-safe, and worth far more than another successful send, because it exercises the paths you
will actually need at 3am.

1. **Wrong credential → quarantine.** Temporarily `PATCH` the account with an incorrect
   `smtp_secret`, queue one job to your own address, and watch. **Expected:** the job does not send;
   the account is **quarantined immediately** on `auth_failure` (regardless of score), with an
   `account_events` row recording reason `auth_failure`. This costs one failed AUTH at the provider —
   acceptable once, deliberately. Then restore the correct secret and unquarantine.
2. **Daily limit.** Set `daily_limit` to 1, send one message, then queue a second. **Expected:** the
   second sits in `waiting_for_capacity` and the pool reads `exhausted`. Confirm via `/v1/report` that
   it reads as clean pending `queued` — **never** `failed` — and that it resumes at the next daily
   reset. This is the single most misread state in the system; seeing it once here is worth the five
   minutes.
3. **Worker death.** Stop a worker process mid-run. **Expected:** the job is reclaimed after
   `job_max_processing_time_sec`, `attempt_count` increments (saturating), the job returns to clean
   `queued`, and it goes out on another worker. Nothing is marked `sent` that the worker did not confirm.

Do **not** test a bounce by sending to an invalid address. That is exactly the reputation damage this
procedure is designed to avoid, and V1 would not record the bounce anyway.

#### Step 6 — A small real batch

Only after steps 1–5 pass. Ten to twenty messages to addresses you control, watching the rate and the
error counts:

```bash
ahealth | jq '{rate: .dispatch, errors: .errors_last_window, accounts, pools}'
```

**Expected:** every job terminal as `sent`, `errors_last_window` empty or benign, no unexpected pause
or quarantine, observed rate consistent with the configured pacing (not faster). A rate *higher* than
pacing should allow means something is misconfigured — investigate before scaling.

---

### 4. Stop conditions

Set `dispatch_enabled=false` immediately, and do not resume until you understand why, if:

- More than one account quarantines.
- Any message reaches an address you did not intend.
- The observed send rate exceeds what pacing should allow.
- The provider returns throttling responses (visible as `throttle`-class errors and self-clearing
  pauses).
- A job reaches `sent` without a `provider_message_id`, or a state transition you cannot explain.
- Anything at all surprises you. This is the one run where "I'll look at it after" is the wrong call.

`dispatch_enabled=false` is a clean halt: zero job loss, fully resumable, queued jobs simply wait.
Using it costs nothing.

---

### 5. After the test

- ⏳ Restore `daily_limit` to its intended production value.
- ⏳ Confirm the account is not left paused or quarantined from step 5:
      `ahealth | jq '{accounts, pools}'`.
- ⏳ Confirm the correct credential is in place (step 5.1) — re-`PATCH` if there is any doubt, and
      prove it with one more successful send.
- ⏳ Diff against the recorded baseline — same filter as the capture in §2, or the diff is noise:
      `diff /tmp/smoke-before.json <(ahealth | jq -S '{accounts, pools}')` (expect only the intended
      new account and its pool).
- ⏳ Review `send_managed.account_events` for the account — every lifecycle change during the test is
      recorded there, and it is the honest log of what happened.
- ⏳ Record the result: date, provider, ports used, inbox-vs-spam per receiver, anything surprising.
      Append it here or alongside [`operator-runbook.md`](./operator-runbook.md#measured-throughput-ceiling-e11-load-test); a first-contact
      result that nobody wrote down has to be re-earned.
- ⏳ Work through the checklist above before widening beyond addresses
      you control.
