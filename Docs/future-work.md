# Future work — what is deliberately not built

This is the register of work that was **considered and consciously deferred** across eleven build
phases. It exists so that "we have zero open items" can be an honest statement rather than a
bookkeeping trick: deferred work does not disappear, it moves here, labelled, with the reason.

**Nothing on this page is a bug in V1.** Each item is either a capability the locked specs
deliberately scoped out, or a defect found during Phase 10's verification whose fix would be a
behavior change — and Phase 10 changes no behavior. Items of the second kind say so explicitly and
name the file and line.

§4a is worth reading first if you are picking up security work: those nine items came from checking
`security-posture.md`'s claims line-by-line against the code, and each one has a **residual risk that
is live today** and now stated at the linked section. Two of them (the worker's unzeroed shutdown path,
and revocation not disconnecting an already-connected worker) change what an operator should do during an
incident, which is why they are also written into the runbook and DR procedures rather than left here.

A note on how to read this: the *reason* column matters more than the item. Several of these were
deferred because building them requires a design decision nobody has made yet, not because they were
forgotten — and picking one up without reading its reason is how a scoped-out item becomes an
accidental redesign. Anything here that resolves an ambiguity needs an Open Decision and an ADR
(CLAUDE.md Rule 2), not just a commit.

---

## 1. Deliverability feedback — the largest single gap

| Item | What it is | Why deferred |
|---|---|---|
| **Bounce / complaint ingestion** | Consuming asynchronous provider feedback (DSN bounce mail, ARF complaint reports, or a provider webhook) and feeding it back into account health and suppression. | Out of scope for V1 by design. **This is the most consequential absence in the system** and is stated plainly wherever it matters: a provider `250` is recorded as `sent`, and nothing downstream is ever observed. There is no bounce data, so there is no suppression list, no complaint-rate signal, and no reputation feedback loop. The account scoring model was written *around* this — [`Docs/history/`](./history/) Phase 5 §3.3 carves bounce data out of scoring explicitly. Note the distinction: a **synchronous** SMTP-time bounce (a `550` during the session) *is* classified and lands the job in `bounced`; what is missing is everything the provider tells you **after** it accepted the message. |
| **Open / click tracking** | Pixel or redirect-based engagement tracking. | Deliberately excluded, and the exclusion is *enforced*: a Phase 8 behavioral test asserts there is no tracking or redirect surface in a delivered message. Adding it is a product decision with privacy consequences, not a technical gap — and it would breach the forward boundary that test protects. |
| **Warm-up / ramp logic** | Automatically increasing an account's daily limit over its first weeks. | Needs the feedback loop above to be safe. Ramping without bounce or complaint data is ramping blind. Today the equivalent is manual: set a low `daily_limit` and raise it yourself. |

Until the first item exists, **provider-side monitoring is the only deliverability feedback that
exists.** [`operator-runbook.md`](./operator-runbook.md) says so in its deliverability section, and
[`smoke-test-real-provider.md`](./smoke-test-real-provider.md) is built around the consequence: your
eyes on a real inworker are the only signal.

---

## 2. Routing and pool capabilities

| Item | What it is | Why deferred |
|---|---|---|
| **Multi-shared-pool campaign binding** | Binding a campaign to a *specific* shared pool rather than to the one `default_shared_pool_id` names. | V1 has exactly **one effective shared pool**. The data model already supports many (`send_managed.pools` with `kind`), and the routing code resolves a pool per campaign — what is missing is the binding surface that lets a campaign choose. Additive when built: a new column plus a routing branch, no migration to existing behavior. |
| **Dedicated → dedicated fallback** | Letting a dedicated pool fall back to *another dedicated* pool instead of only to the shared pool. | The fallback ladder in V1 is dedicated → shared. Dedicated-to-dedicated needs a policy answer nobody has given: whose accounts may carry whose mail, and what the billing consequence is. That is a product/pricing decision, which is why it is not a code task. |
| **Control horizontal scaling** | More than one active dispatching Control. | **Locked out on purpose, not deferred lightly.** The singleton (a session-level Postgres advisory lock) is a CLAUDE.md Rule 7 invariant: "Never add a second dispatch path or distributed dispatch." Exceeding one Control's throughput is a redesign requiring an ADR and a ruling, not a scaling knob. See [`capacity-planning.md`](./capacity-planning.md) — at realistic V1 volumes account daily limits bind long before the Control does. |
| **Sequences / multi-step campaigns** | Drip sequences, follow-ups conditioned on a previous send. | A campaign-orchestration feature that belongs above this module. This module sends what it is handed; sequencing decides *what* to hand it. |
| **Per-pool variation scope** | Scoping content-variation toggles per pool (or per account) rather than globally. | Phase 8's variation dials are process-global config keys. Per-pool scope means a resolution order (pool overrides global overrides default) and a place to store it — a design decision, deliberately not invented mid-phase. |
| **Consolidate the eligibility predicate — it is duplicated in five places** | The account-eligibility condition (`quarantined = FALSE AND disabled = FALSE`, plus the paused/daily-window branch) is written out separately in **five** sites: `dispatch/eligibility.go:113-114` (the consume), `dispatch/eligibility.go:300-301` (`poolHasEligible`, the pre-claim gate), `dispatch/exhaustion.go:93-94`, `adminapi/health_store.go:262-263, :361, :393`, and `platformapi/campaign_report.go:178-179` — plus the partial index `accounts_eligibility_idx`'s `WHERE` clause. One shared definition (a SQL fragment constant, or a view the five sites select from) would replace them. | **The duplication is deliberate** — `eligibility.go:288-291` says so explicitly, so that exhaustion cannot silently drift from selection. But the mechanism that enforces agreement is *nobody forgetting*, and the failure mode when someone does is nasty and silent: a term added to four of five sites makes the pre-claim gate say work exists while the consume finds nothing, i.e. **a dispatcher that spins forever** rather than a failed send. Nothing fails, nothing errors, no test necessarily goes red. Deferred here because module 2 is closed and this is a refactor of load-bearing SQL on a system with no new consumers. **Flagged as a prerequisite for whoever adds the next exclusion term** — one shared definition is cheaper than five tests asserting they agree. Module 3 hits this immediately: its Phase 1 adds `deleted_at` and its Phase 4 adds a From-verification term, making it the *second and third* phases to pay this cost (registered there as M3-P4-OD-3). |

---

## 3. Operator surface

| Item | What it is | Why deferred |
|---|---|---|
| **`/v1/admin/workers` CRUD** | Registering, listing, and decommissioning workers over the admin API. | There is **no worker route on the admin surface at all** today (verified: the served route set is accounts, pools, config, health). A worker is provisioned by seeding a `send_managed.boxes` row with a bcrypt token hash — `tools/provision-worker` mints the pair — and decommissioned by clearing `token_hash`, which refuses it on next connect regardless of a still-valid cert. That works and is documented in [`install-and-deploy.md`](./install-and-deploy.md), but it is a DB operation where every other operator task is an API call. Note the security shape any implementation must preserve: the API may only ever return the token **once, at mint time** (the hash is what is stored), exactly like `smtp_secret`'s write-only posture. |
| **Server-side bulk credential re-encryption** | Rotating the master key without re-entering every SMTP password by hand. | This is *why* master-key rotation is manual re-entry today, and it is a consequence of a load-bearing security property rather than an oversight: `adminapi` holds an **Encrypt-only** primitive with no `Decrypt` method, and the sole `Decrypt` capability is wired only into the send path (CLAUDE.md Rule 6). A bulk re-encrypt needs a Control-side path that can decrypt-under-old and encrypt-under-new — i.e. a **second** decrypt call site, in the admin surface. That is exactly what Rule 6 forbids adding casually, so building it requires a ruling on where such a path may live and how it is audited. Procedure as it stands: [`disaster-recovery.md`](./disaster-recovery.md) §3. |
| **Multi-key credential keyring** | A Control that resolves more than one `credential_key_id` at a time. | A Control resolves exactly one label. That constraint is what forces master-key rotation to be freeze-first-then-cut-over (there is no window where rows under two labels are both sendable). A keyring would allow a rolling rotation with no send freeze, and would pair naturally with the item above. |
| **Admin RBAC / multiple operator roles** | Distinct operator identities with distinct permissions, plus an audit trail keyed to a human. | V1 has **one omnipotent operator key** (Domain B). Any holder can do anything on the admin surface, and `account_events` records the request id, not *who*. Fine for a small trusted team; a real limitation stated in [`security-posture.md`](./security-posture.md) §3.6. |
| **Rate limiting / lockout on the API surfaces** | Throttling or locking out repeated failed auth attempts. | The external code `rate_limited` (429) exists in the vocabulary but nothing emits it — reserved deliberately so adding it later is additive. Both surfaces are meant to sit on a private network behind a proxy; the proxy is the intended enforcement point today. [`security-posture.md`](./security-posture.md) §3.7. |
| **A metrics endpoint** | Prometheus exposition or an equivalent time series. | `GET /v1/admin/health` is a JSON snapshot, not a series. Several fields are already windowed and graph cleanly if you poll and store them — [`monitoring.md`](./monitoring.md) §5 says how, and why this was left out. |
| **A "cleared" alert notification** | A webhook POST when a fired condition resolves. | Alerts are rising-edge only, with no all-clear. Receivers are meant to treat the health endpoint as current truth rather than pairing up/down events. Deliberate: an all-clear implies a state machine the alerting layer does not keep. |
| **All three shadow dials in the health snapshot** | `/v1/admin/health`'s `shadow` block reports `autoquarantine_mode` and its `would_quarantine_count` / `recent[]`, but not `autopause_mode` or `retention_mode` — since `send_managed_0026` there are three dials on one ladder and only one of them is visible here. | Deferred in fix-plan item 13 rather than built, because it is an **API-contract change** (a new response field plus the counters behind it) and item 13's subject was the safety property, not the surface. It is not a silent gap: both new dials leave a real trail — `shadow_would_pause:` rows in `account_events` and a per-tick `retention_shadow_would_delete` log line carrying the true backlog count — and [`operator-runbook.md`](./operator-runbook.md) §"The three shadow dials" carries the queries. Whoever builds it should note that `retention_mode`'s counter is deliberately uncapped by `retention_delete_batch_size` (ADR-0014 decision 7), so it must not be sourced from the sweep's batch. |

---

## 4. Data at rest and retention

| Item | What it is | Why deferred |
|---|---|---|
| **`send_logs` at-rest encryption** | Encrypting the rendered body, subject, headers, and recipient address that `send_logs` stores in plaintext. | The **single privacy control** on that table today is retention: content is dropped after `debug_log_retention_days`. Everything else about the table is hardened (write-once via a reject trigger plus `REVOKE UPDATE`; `DELETE` retained only for the sweep) — the content is simply not encrypted. Stated plainly in [`security-posture.md`](./security-posture.md) §3.2, and it is the reason retention is a **privacy** dial and not only a storage one. Encrypting it needs a key-management answer for a *second* class of data, and a decision about whether debugging can survive it. |
| **Per-tenant data isolation beyond RLS** | Anything stronger than `FORCE ROW LEVEL SECURITY` on `send_managed.accounts`. | RLS with a non-login, non-superuser schema owner is the V1 boundary and it genuinely enforces. Stronger isolation (separate schemas or databases per tenant) is a deployment-topology decision. |
| **A one-call tenant-erasure path** | Making `DELETE FROM public.users` (or an admin endpoint) erase a tenant's coldmail-send-managed footprint in one operation. | Erasure works today but is a **three-step sequence in this order: accounts → pools → user**. A one-statement user delete is refused by `pools_user_fk`, which declares no referential action — verified by query on real Postgres against a tenant with job history, not assumed. Each step now succeeds (the account step was wedged until `send_managed_0024`; see ADR-0012). Making the user delete cascade `send_managed.pools` is a **tenant data-lifecycle ruling**, not a bug fix: it decides whether deleting a platform user may silently destroy coldmail-send-managed configuration and audit history. Note the audit asymmetry it would inherit — deleting an **account or worker** keeps the `job_events` rows with the FK nulled (`SET NULL` + `send_managed_0024`'s carve-out), whereas deleting the **user** removes them outright (`job_events_user_fk` is `ON DELETE CASCADE`). Both are intended, but a one-call path must document which one the caller gets. |

---

## 4a. Security hardening (raised by Phase 10's R3 security review)

Every item here was found by walking `security-posture.md`'s claims against the code. Each one is a
**behavior change**, which is why Phase 10 corrected the documentation instead — the residual risk is
now stated honestly at the section linked in the last column, and the capability that would close it
is registered here.

| Item | What it is | Why deferred |
|---|---|---|
| **Zero the worker's in-flight credential on shutdown and on an uncaught exception** | The worker's SIGTERM/SIGINT handler stops the stream and exits without zeroing the credential the current send holds, and no `uncaughtException`/`unhandledRejection` handler is registered at all. The three send-path exits (accept, SMTP error, thrown exception) *are* covered by a `finally` and are tested. | A memory image captured at that instant — core dump, container checkpoint, `/proc/<pid>/mem` — may hold one in-flight plaintext. The fix is small (give `stop()` a reference to the live credential; add the two process handlers) but it changes the shutdown path of the service that holds credentials, and it needs its own behavioral test per Rule 3. Consequence documented at [`security-posture.md`](./security-posture.md) §3.12, which is what stops an operator believing `systemctl stop` is a scrub. |
| **Revoke-on-live-stream: terminate or periodically re-authorize a connected worker** | The token/status check runs **only** in the register handshake. Job pushes are served from an in-memory worker map with no re-read of `send_managed.boxes`, and the heartbeat path returns a `stale` worker to `active` without re-checking the token. | So clearing `token_hash` refuses the next connect but does not disconnect a worker that is already streaming — an attacker who keeps the (outbound, worker-initiated) stream open keeps receiving one plaintext credential per job. Closing it means either a re-authorization read on the push path (a DB read per push, on the hottest path in the system) or a Control-side stream-kill triggered by a config/DB signal — both real design decisions, not polish. The interim answer is procedural and now documented: isolate the host **first**, revoke second ([`disaster-recovery.md`](./disaster-recovery.md) §4a, [`security-posture.md`](./security-posture.md) §3.1). |
| **Field-level before/after diffs in `account_events`** | An `updated` audit row today carries identical before/after flag snapshots (`paused`, `pause_reason`, `quarantined`, `disabled`) and no list of what actually changed. | A `PATCH` that rewrote `smtp_host` is therefore indistinguishable in the audit trail from a no-op one, which is what makes the item below undetectable after the fact. `account_events` is append-only with enumerated grants, so widening what it stores is an additive schema change plus a write-path change — doable, but behavior. [`security-posture.md`](./security-posture.md) §3.6. |
| **Close the `smtp_host` credential-redirect path** | Require `smtp_secret` re-entry when `smtp_host` changes, and/or an allowlist of permitted SMTP hosts. | A Domain B key holder can repoint an existing account at a host they control **without** re-entering the secret, and the next dispatch delivers the decrypted credential to that host. No DB access, no key access, no Domain A token needed. The Encrypt-only admin surface bounds the API *response*, not this. Either fix changes the semantics of a documented `PATCH` (omit-means-unchanged is deliberate), so it is a contract decision. [`security-posture.md`](./security-posture.md) §3.11. |
| **Reject private-range `smtp_host` at write time, or gate the self-signed affordance behind an explicit dev flag** | The worker disables TLS certificate verification for loopback/RFC-1918/link-local SMTP hosts — a test affordance so the harness can reach the local sink — while the Control-side validator accepts any bare IP literal. | The two together mean a real account can be pointed at a private address and will AUTH over an unverified connection. Neither half is wrong on its own; the gap is that nothing enforces the boundary the worker-side comment assumes. Fixing it either rejects a currently-valid value (contract change) or adds a new dev-only flag (config surface). Interim: a deploy-time check in [`security-posture.md`](./security-posture.md) §5, and the residual risk at §3.13. |
| **Restrict the alert webhook's destination** | Reject loopback, RFC-1918, and link-local (`169.254.0.0/16`, i.e. cloud metadata) destinations for `alert_webhook_url`, or take an allowlist. | Validation today is a scheme-prefix check only, so the Control will POST from inside the trust boundary to any reachable address — a blind SSRF available to a Domain B holder. Bounded (only an already-privileged principal can set it; the response is never returned or logged) but real. A blocklist has to be a config decision rather than a hardcoded constant, per Rule 1. [`security-posture.md`](./security-posture.md) §3.14. |
| **An append-only `config_events` audit table** | A durable, tamper-resistant row per config write, with the key, the before value, the after value, and the request id. | Config writes are recorded **only** as a Control process-log line today — no DB row, no before/after. So "when was `debug_log_retention_days` widened?" (§3.2 makes that a privacy question) is answerable only from shipped logs, for as long as their retention holds. New table + new write path on every `PUT /v1/admin/config`. [`security-posture.md`](./security-posture.md) §3.6. |
| **Per-send attestation** | Something that lets the Control distinguish a truthful worker report from a fabricated one. | The outcome guards check the reporting worker against `assigned_box_id` and the `attempt_token` against the current attempt, which stops replay across attempts and workers — but a compromised worker can still report any outcome for the job it legitimately holds: `accepted` for mail never sent, or fabricated `auth_failure`s that quarantine healthy accounts. §3.9's absent delivery feedback means neither is detectable here. This is a protocol-level design problem (what could a worker sign that the Control cannot already forge on its behalf?), not an implementation gap. [`security-posture.md`](./security-posture.md) §3.15. |
| **A credential-leak scan that runs against a production log stream** | An operator-runnable check that a real deployment's shipped logs contain no credential. | The blocking `no_secret_leak` CI gate reads `.env` and compares the **dev worker token and its hash** against *dev compose* logs; the SMTP-credential canary scan lives in the send-path harness and needs the whole dev stack. Against a production deployment the gate either aborts on the missing `.env` or prints `OK` having compared nothing — a vacuous green, which is worse than no check. Interim: §5's deploy-time list now tells the operator to write the grep themselves and says plainly that the shipped script does not cover them. |
| **Scrub the DSN out of `tests/harness/harness.go`'s two connect failures** *(registered 2026-08-07)* | `harness.go:39` (`Connect`, `t.Fatalf("connect: %v", err)`) and `harness.go:87` (`mustConnect`, `panic(err)`) hand a rejected connection string straight to test output. Same MEASURED pgx v5.7.4 defect the shipped code was just fixed for: a DSN the driver rejects is echoed **whole** into the error, and pgx's own redaction misses `?password=`, `?sslpassword=` and the honored spaced `password = SECRET`. | Smaller than the shipped-code leak and deliberately left: the credential is the **per-run dev password** `scripts/dev.sh` generates, and the sink is test output rather than a service log — so it is not a production credential in a shipped log line. It is not nothing either, because CI uploads test output as an artifact. **Why not fixed with the rest:** `tests/harness` is a third Go module and cannot import `services/control/internal/dsnscrub` any more than the migrate tool can, so closing it means a **THIRD copy** of the scrub and a drift control that polices three files instead of two. Fix shape, in preference order: (1) extract a shared `libs/dsnscrub` module that all three `require`, which also retires `TestDSNScrub_MigrateCopyHasNotDriftedFromControl` entirely; (2) a third copy plus an extended drift test. **Do NOT wrap only the `t.Fatalf` call site** — `mustConnect` panics with the raw error and is the one used by the clone path, so fixing one of the two leaves the other open and makes the file look done. The blocking `dsn_scrub_wiring` gate deliberately scopes to `services/` and `tools/` and says so in its header, so this is an acknowledged exclusion rather than a hole the gate silently permits. |

---

## 5. Build infrastructure

| Item | What it is | Why deferred |
|---|---|---|
| ~~**Go toolchain upgrade to clear two module vulns**~~ — **RESOLVED 2026-11-07** | `GO-2026-5004` (SQL injection via placeholder confusion in `pgx/v5`) and `GO-2026-5970` (infinite loop in `golang.org/x/text`), plus the further `x/text`, `x/net` and Go-stdlib advisories `govulncheck` reports against the old toolchain. | **Done, not deferred.** Builder image `golang:1.23-alpine` → `golang:1.26-alpine` (go1.26.10) in both Dockerfiles and `real-infra.yml`; `pgx/v5` v5.7.4 → v5.10.0, `x/text` v0.21.0 → v0.43.0; `go` directive 1.23 → 1.26.0 in the three modules that need it. `govulncheck`, live call paths, before → after: `services/control` 41 → **0**, `send-managed-migrate` 27 → **0**, `tests/harness` 27 → **0**, `provision-worker` 0 → **0**. Full harness and all gates re-run on the new toolchain. **This is a dated result, not a permanent one** — the advisory database keeps moving, so re-run `govulncheck` at pilot time (go-live checklist §9). |
| ~~**`scripts/dev.sh` merges the RAM-backed Postgres unconditionally** (module 3's #131, not ported)~~ — **RESOLVED 2026-11-08: ported, and both follow-up verifications done on a real runner (last column)** | `tests/harness/compose.test.yml` puts Postgres' data directory on a **tmpfs** with `fsync=off`, and `scripts/dev.sh` merges it on every invocation. A `docker stop postgres` therefore frees the tmpfs and the next start brings up an **empty** database — so an outage drill on the default dev stack destroys the data and reads as a recovery bug. Module 3 made the override opt-in (`DEV_RAM_PG=1`, set by CI), pointed every standalone script at `dev.sh compose`, and added a blocking gate (`tests/harness/checks/postgres_durability.sh`) that reads the rendered compose config in both modes. | **Dev/test tooling only — production is unaffected.** **Done:** `scripts/dev.sh` now runs the base compose file alone; `DEV_RAM_PG=1` merges the RAM override and is set job-wide in `real-infra.yml`, so CI runs the stack it always did. `dev.sh compose` prints the invocation, and `load_e11.sh` and the four check scripts that run compose take it from there instead of re-deriving it. `tests/harness/checks/postgres_durability.sh` (a blocking CI step) renders the config in both modes and fails if the default one is ever RAM-backed, with a built-in control that the RAM mode really renders a tmpfs. **Verified on GitHub Actions** (the result that counts): the per-push CI in the RAM mode (62 passed, 0 failed), and — via the on-demand `outage-drill` workflow ([run 42499590634](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42499590634)) — **(1) the whole harness in the default (non-RAM) mode: 62 passed, 0 failed** (it had never been run in that mode for this module), and **(2) the real `docker stop postgres` outage drill: 22 passed, 0 failed** (`tests/harness/drills/postgres_outage.sh`). With a job genuinely in flight on a paused worker and a snapshot taken first (`jobs=6 job_events=22 send_logs=4 max(seq)=22`), a 50 s outage left the Control running with 0 restarts, health and run failing closed with only `{code, message, request_id}`, a bounded `loops remain PAUSED, no state invented` backoff, `db outage RESOLVED after 55s`; afterwards jobs 6→6, events 22→22, `max(seq)` 22→22, the refused run created no job, and job B was delivered **exactly once** after its worker was released (mailpit 4→5, one `send_logs` row). The gate proves the rendered config is not RAM-backed; the drill proves the data then survives and the Control invents nothing. The local VM could not hold these runs (the host reaped them twice for memory), which is why they run on a runner. |
| **A RESERVED-marker EXPIRY rule for `config_consumption`** | This module's ledger syntax is `key\|RESERVED` with **no expiry field**, so a RESERVED marker here never comes due: the gate re-confirms the marker is documented, forever, and that is the whole check. Module 3 added exactly this rule (`key\|RESERVED\|<gate-or-phase>`, failing once `Docs/current-phase.txt` reaches the named label) after finding two keys RESERVED "by accident" with no expiry — and when it fired at its Phase 8 bump both keys were **dropped**, so the mechanism is proven end to end there. **This module still carries those same two keys unexpiring:** `retry_count_4xx` (§6.2 ruling 4 — needs the avoid-set/attempt-tracker Phase 5 deferred) and `variation_zero_width_rate_pct` (P9.5-OD-3 — a documented duplicate of the wired `variation_zero_width_rate`). | Adopting it is two changes, not one: a **behavioural change to a blocking gate** (new required ledger field, so every RESERVED line must be rewritten), plus a **disposition decision on two live seeded keys** — and module 3's precedent says the honest disposition at the last phase is to *drop* the key, since a key with no future phase left to name has no honest marker available. That is a seeded-config removal on a `/v1` surface, needing an Open Decision, not a gate tweak. Recorded 2026-08-05 while writing this gate's paired control; the control's closing NOTE points here. Nothing is silently unguarded meanwhile — both keys are marked RESERVED and both carry a written reason in [`config-reference.md`](./config-reference.md), which is what the existing rule enforces. |
| **A dependency/vulnerability scan in CI** | `govulncheck` (and an equivalent for the worker) as a blocking or reporting gate. | Scanning was done by hand in Phase 10. The five existing CI gates all catch *drift between code and docs*; a vuln gate is a different kind of check whose failure mode is "a new advisory published upstream breaks your build with no code change," which needs a policy decision (blocking vs reporting) before it is added. Note the worker needs no scanner: it ships `dependencies: {}` — `npm audit` has nothing to audit, which is the no-DB/no-KMS boundary showing up as an absence. |

---

## 6. Known defects, flagged not fixed

Found during Phase 10's verification passes, plus one gap found later while walking
[`send-managed-local-test-guide.md`](./send-managed-local-test-guide.md). Each is real and
reproduced; each fix is a **behavior change**, which is why none was made in a phase whose contract is
that behavior does not change. Listed most-likely-to-be-hit first.

> **Missing test infrastructure: there is no controllable SMTP sink on the dev stack.** Found walking
> Stage 9 (failure injection). The compose sink is Mailpit with `MP_SMTP_AUTH_ACCEPT_ANY: "true"`; it
> accepts any credential and cannot be made to emit a 535, 550, 4xx, or throttle, so the live stack can
> demonstrate the **success path only**. `docker-compose.yml`'s comment on that service used to claim
> the failure paths "use a controllable Go sink (`tests/harness`)" — **that sink was never built**; the
> comment described an intention. The comment is now corrected to state the limitation instead.
> The failure-path behaviour *is* genuinely covered — `internal/dispatch`'s `policy_test.go`,
> `score_test.go`, `statemachine_test.go`, `shadowmode_test.go`, and `classify_test.go` drive real SMTP
> response text through the real `OutcomeHandler` against real Postgres (see the Stage 9 preamble's
> step→test table) — so this is a **gap in live-stack demonstrability, not in verification**. Building
> the sink would let an operator *see* a 550 become a `bounced` job end-to-end rather than trust a Go
> suite. Not a deploy blocker; it is why Stage 9 steps 1–4 are marked "covered by suite" rather than
> hand-ticked.
>
> **Scope correction (2026-07-29, Stage 10 steps 3–4).** The gap is narrower than "no live failure
> injection." Pointing an account's `smtp_port` at a dead port makes the worker report `connect_refused`
> → class `timeout` — a real worker-reported penalizing failure that exercises the score model,
> `consecutive_failures`, the backoff ladder, and the whole auto-quarantine shadow/enforce ladder on
> the live stack, with no hand-written rows. Stage 10 steps 3–4 are closed that way. What a dead port
> cannot do is select a *class*: it is always `timeout`. So the missing capability is specifically
> **controlling the SMTP reply code** — which is what still blocks Stage 9 steps 1–4 (`auth_failure`,
> `bounce_permanent`, `transport_4xx`, `throttle`), and only those.

| Defect | Evidence | Why not fixed here |
|---|---|---|
| **`POST /v1/run` returns HTTP 500 when `public.job_items.campaign_id` is NULL.** External code `internal_error`, internal reason `read_job_item_failed`. | `services/control/internal/platformapi/store.go:69` scans a **nullable** platform column into a non-nullable Go `string`. Reproduced live: inserted a `job_items` row with `campaign_id = NULL`, called `/v1/run`, got `500`. | A new operator's very first `/v1/run` can hit this, so it is worth fixing early — but the fix changes a response code (most likely to `validation_failed`/`400`, since a job item with no campaign is a caller-side problem, not a server fault). Choosing *which* code is a contract decision on an additive-only surface. |
| **A worker exits permanently if `dev.sh up` runs ahead of `dev.sh seed`.** | The worker needs its `send_managed.boxes` row to register; `restart: "no"` in compose means a rejected worker stays dead. **Reproduced during Phase 10's own gate run** — the worker was found `exited 1` with `register rejected: unauthenticated`, and the fix was a manual `docker compose up -d --no-deps worker` after seeding. | Affects local dev only (a real install seeds the row before starting a worker, per the install guide). The fix is a compose/ordering change — behavior. |
| **A register rejection short-circuits the documented ~30-attempt retry budget.** | `services/worker/src/stream.js` calls `this.stop()` on `accepted:false`, which sets `stopped = true`, so `connectOnce` returns immediately at the top of its next iteration. **Measured:** exit after attempt 1, ~1s, not ~30 attempts over ~30s. | The budget is real but covers **dial/TLS failures only**; a rejection is treated as terminal on purpose (an unauthorized worker hammering the Control is worse than a worker that exits for its supervisor to restart). What is wrong is the *documentation* of the budget's scope, plus the interaction with the item above. Changing the retry behavior is behavior; the doc side is being corrected. |
| **Three inert variables in `.env.example`: `SEND_MANAGED_ALERT_WEBHOOK_URL`, `PLATFORM_SERVICE_TOKEN`, `PLATFORM_ADMIN_KEY_HASH`.** | No code reads any of the three. The webhook destination comes only from the `alert_webhook_url` config row; the live auth variables are `PLATFORM_TOKEN_HASHES` and `ADMIN_API_KEY_HASHES`, both comma-separated **bcrypt** lists — not a plaintext token, not a SHA-256. | Setting the env var and expecting alerts is a silent no-op — a config-not-code violation in an *example file*, not in the running system. Setting any of them expecting an effect is a silent no-op, and for the two auth ones the surface fails **closed** — a Domain A call returns `401` with no hint that the variable you set is not the one being read. Removing the lines is trivial but touches the example every install copies; all three are now labelled inert in `.env.example` itself, and noted in [`monitoring.md`](./monitoring.md) §3 and [`install-and-deploy.md`](./install-and-deploy.md) Step 6, so nobody is misled meanwhile. |

---

## 7. The open decision — RULED AND CLOSED 2026-08-07

Every item in the ledger across phases 0–10 (including 9.5) is ruled and closed. The last one, **R2-5**,
was ruled on 2026-08-07 and is recorded here rather than deleted, because the *reasoning* is the part
worth keeping.

- **R2-5 — stale `user_id` rows in a locked spec's quick-reference table. RULED: annotate, do not
  rewrite.** `Docs/history/phase-9-polish-and-documentation.md` §5.3 shows `run`'s body as
  `{job_item_id}`, omitting the REQUIRED `user_id`. Ruling: the row stays exactly as written (CLAUDE.md
  §8 — a closed spec is a historical record, and rewriting it to match today's code destroys the
  record of what was specified), and a **dated annotation** now sits above the table naming precisely
  what is stale and pointing at [`api-contract.md`](./api-contract.md) as the authority.
  - **A second stale row was found while ruling it.** `GET /v1/report`'s selector is shown as
    `?job_id=` | `?job_item_id=` | `?campaign_id=`, but `?user_id=` is a REQUIRED tenant selector too.
    The item as filed named `run` only. Both were verified against the code
    (`internal/platformapi/handlers.go:144` and `:219` — each returns `400 validation_failed` /
    `user_id_required`) and against the living contract, which is correct on both routes.
  - **Why an annotation and not a gate.** `contract_drift` verifies the served routes against
    `api-contract.md`; it does not read closed phase specs, and it should not — they are frozen by
    design. So a stale example inside one is invisible to every automated check in this repo, and no
    mechanism can be added that would catch the next one without unfreezing the specs. That makes this
    the rare case where the honest control is a **reader-facing warning at the point of use** rather
    than a test. Stated plainly so it is not mistaken for the usual "a comment is not a control": here
    there is no mechanism available, and the annotation is placed where someone about to implement from
    the wrong table will read it.
  - Not a build blocker and not a contract gap: an operator or UI team reads the living contract.

Full ledger with every ruling: [`Docs/history/open-decisions.md`](./history/open-decisions.md).

---

## 7a. A markdown link-integrity gate over `Docs/` (registered 2026-08-07)

**Registered after MEASURING the current state, not supposing it.** A one-off walk of every relative
markdown link under `Docs/` in all three repos on 2026-08-07: **`coldmail-send-managed` 228 local links
/ 2 broken · `coldmail-send-byo` 263 / 2 · `coldmail-verify` 485 / 0.**

**What the two broken ones in this repo were.** Rename fallout. `Docs/README.md:19` and
`Docs/future-work.md:114` both pointed at `coldmail-send-managed-local-test-guide.md` — the prose was
updated to the new repo prefix while the file kept its shorter name,
[`send-managed-local-test-guide.md`](./send-managed-local-test-guide.md). **Both are fixed**; a
re-measurement reads 0 broken. This item is the *gate*, not the fix.

**A second, sharper instance found the same day, which is why this is worth a gate.**
`Docs/history/open-decisions.md` cites `Docs/pre-live-gates.md` **twice** — at the `OD-1` row and in
the "Pre-live gates" section — after that file moved to `Docs/history/pre-live-gates.md` during the
documentation reorganisation. Every *other* citation in the repo (`advisory-locks.md:35`,
`go-live-checklist.md:10`, `history/README.md:32`) already uses the new path, which is precisely why
nobody noticed: the majority were correct. That one sits in a **frozen** ledger, so it was **annotated
rather than repaired** — CLAUDE.md forbids editing a locked document as build work. Nothing was lost
(G1–G4 are all carried in the living [`go-live-checklist.md`](./go-live-checklist.md) §2/§3/§4), but it
makes the point exactly: **a link can break inside a file that policy forbids anyone from touching, and
the only way to learn that is to check.**

**Why the failure mode deserves a mechanism.** It is silent and one-directional. A reader follows a
citation, finds nothing, and cannot distinguish "renamed" from "moved" from "never existed" — and the
third case is real: module 3's sweep the same day found `FW-M3-6` cited by two documents while the id
was **defined nowhere**, because a ruling whose whole disposition was *"register it"* was never carried
out. A link-checker would not have caught that one, but it is the same family, and neither has any
mechanism watching it.

**Why it is not built here.** New gate, new work; this closeout closes what is open rather than adding
mechanisms. It also needs a scoping decision first: whether to cover `Docs/history/`. Frozen documents
**will** accumulate stale links by design — the tree moves under them and they may not be edited — so a
blocking gate over them would go permanently red, which is worse than no gate. The honest shape is
probably **blocking on live docs, reporting on frozen ones**, and that is a policy call, not an
implementation detail.

**Fix shape.** Extract `](<relative path>)` from every `Docs/**/*.md`, skip `http`/`#`/`mailto`, strip
any `#anchor`, assert the target exists relative to the **citing** file's directory. Verified workable —
the measurement above is that exact algorithm run by hand. Control: point one link at a nonexistent
file and watch it go red.

---

## 8. Related

| Doc | For |
|---|---|
| [`go-live-checklist.md`](./go-live-checklist.md) | What must be true before real volume — including the items above that gate a pilot. |
| [`security-posture.md`](./security-posture.md) | The residual risks several of these items would reduce. |
| [`operator-runbook.md`](./operator-runbook.md) | Working within these limits today. |
| [`Docs/history/open-decisions.md`](./history/open-decisions.md) | Every decision ruled across eleven phases, with its disposition. |
| [`CLAUDE.md`](../CLAUDE.md) | The rules any of this work must not break. Read before picking an item up. |

## Every image is pinned by digest (ruled 2026-11-09)

`postgres:16-alpine`, `golang:1.26-alpine`, `alpine:3.20`, `node:20-alpine` (compose, `services/*/Dockerfile`, `tools/send-managed-migrate/Dockerfile`)
and `alpine:3` in `tools/own-secrets.sh` now carry `@sha256:`; `tests/harness/checks/image_pins.sh` rejects **any** reference without one (no
exception list), and its scope now includes `tools/*/Dockerfile`, workflow `image:` lines and a heuristic scan of the shell tooling (23-case
self-test). Cost: a pinned builder gets no base-image security patches until a reviewed commit bumps the digest. **Verify support status:**
`node:20-alpine` and `alpine:3.20` are old releases (my recollection is that both are at or past upstream end-of-life — not verified from the
build host); moving them is a separate decision.
