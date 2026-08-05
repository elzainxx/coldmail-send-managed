# Open Decisions ledger ("flag, don't fix")

> **OWNER: MODULE 2 (`coldmail-send-managed`), and this is module 2's OWN copy — the original.** Ids here are module 2's (`OD-4`, `OD-13`, `P8-OD-1`, …), never `M3-P*-OD-*`. A verbatim copy of this file was carried into module 3 (`coldmail-send-byo/Docs/history/open-decisions.md`) at this same relative path so inherited-code citations keep resolving; that copy carries the mirror-image warning, since in module 3's tree it sits one directory below module 3's own living ledger and the shared filename made it easy to mistake for module 3's history. Nothing in module 3 is ruled here (added 2026-08-05).

Every contradiction, ambiguity, or assumption that needs a ruling — never silently resolved
(CLAUDE.md Rule 2). Items ruled by the user are marked **RULED** with the disposition
implemented. Items where Phase 0 applied the plan's *recommended* disposition as its build
default are marked **APPLIED (recommended)**. Items still needing a ruling are **OPEN**.

## Ruled this session (implemented, do not reopen)

| # | Decision | Ruling | Where implemented |
|---|---|---|---|
| OD-4 | Migration tooling | **RULED:** thin self-tracking runner; do NOT adopt golang-migrate. | `tools/sender-migrate/` keeps `sender.schema_migrations` self-`INSERT`/`DELETE` in each migration; runner only orders/applies. |
| OD-11 | Canonical operating-rules file | **RULED:** `CLAUDE.md` (repo root) is canonical; Claude Code reads it. | `/CLAUDE.md`; CODEOWNERS + README point at it. |
| OD-13 | Dedicated table-owner role | **RULED:** create a non-superuser `sender_owner` that owns the schema so `FORCE RLS` enforces. | `db/migrations/sender_0000_bootstrap` creates `sender_owner` (NOLOGIN), owns `sender` schema; `sender_module` is login-only with grants. |
| OD-8 | External-table shapes | **RULED:** build the pre-flight existence check against ASSUMED shapes; confirm real shapes before Phase 1 live. | `db/preflight/check_platform_tables.sql`; assumption noted in ADR-0007. |
| OD-5 | SMTP sink | **RULED:** Mailpit. | `docker-compose.yml` `smtp-sink: axllent/mailpit`. |

## Applied recommended disposition (plan §10; build default, override available)

| # | Decision | Applied disposition |
|---|---|---|
| OD-1 | Migration runner DB identity | Runner connects as a superuser/owner-capable admin role (env `MIGRATE_DATABASE_URL`), distinct from `sender_module`; bootstrap `CREATE ROLE` then ownership handed to `sender_owner`. **Pre-live gate G1** (`Docs/pre-live-gates.md`): confirm the real admin role + secret-store credential source with platform-db before the first live run. Not a build blocker. |
| OD-2 | Runtime patch pins | Go 1.23 / Node 20 LTS pinned in go.mod + Dockerfiles + package.json engines. Host has newer (Go 1.26 / Node 24); CI/containers use the pins. |
| OD-3 | config-reference generate-vs-verify | **Verify** (Option B) for Phase 0: the drift gate diffs the doc table against a phase-aware manifest + live `sender.config`. Full generate is revisited when the first keys seed (Phase 1). |
| OD-6 | config-drift expected-key count | Phase-aware manifest (`docs/config-manifest.txt`), not a hardcoded 50. Phase 0 seeds ZERO config keys, so the manifest is empty and the gate asserts emptiness. |
| OD-7 | E11 ceiling storage | Committed versioned doc artifact (deferred to when E11 runs; Phase 0 only scaffolds the load suite dir). |
| OD-9 | O6 two-Brain test placement | Blocking `real-infra` CI job (the singleton guard is load-bearing and the test is fast). Implemented in the harness. |
| OD-10 | Platform migrator lock key | **RULED 2026-07-23:** the sender module reserves the **`9_000_000_000–9_999_999_999`** advisory-lock band (`Docs/advisory-locks.md`); the platform avoids it. **Pre-live gate G2** confirms this with platform-db before the first live run. Not a build blocker. |
| OD-12 | Tool/dir naming | Standardized on `sender-migrate`, `provision-box`, `gen-certs`, `tests/harness/`. |
| OD-14 | Prod cloud KMS provider | Local surface is a dev key file (KMS-API-shaped abstraction). Provider choice deferred to prod sign-off (S15). |
| OD-15 | Dev box id/token under scale | One dev box id for the Phase-0 loop; per-replica minting deferred until a multi-box dev scenario is needed. |

## New this build (Phase 0 execution surfaced these)

| # | Decision | Status | Detail |
|---|---|---|---|
| OD-16 | `make` unavailable on Windows dev host | **APPLIED** | Portable entrypoint is `scripts/dev.sh`; `Makefile` delegates to it. CI (Ubuntu) has `make`; both call the same script, so dev==CI holds. |
| OD-17 | Phase 0 owns a bootstrap migration (schema+roles); how to number it vs the locked feature chain | **RULED (2026-07-23) — renumbered to `sender_0000`** | The bootstrap is **`sender_0000_bootstrap`** (pre-feature groundwork). This keeps the LOCKED feature chain **exactly `sender_0001..sender_0020`** as every Phase 1–9 spec and the Phase 9 §4 config reference assume — ZERO edits to any locked spec, and the feature chain is NOT shifted. Phase 1 begins at `sender_0001_create_schema`, unchanged. The contiguity guard + `migration_lint` accept `0000` as the chain start (`0000 → 0001` contiguous). Supersedes the earlier "shift Phase 1 to 0002" approach. See ADR-0002 (amended). |
| OD-18 | Pre-`sender.config` bootstrap values | **APPLIED** | `sender.config` is created in Phase 1 (`sender_0010`). At Phase 0 the Brain/box read their few bootstrap values from env — the same documented bootstrap-exception class as the runner's `lock_timeout`. Enumerated env bootstrap values: `BRAIN_SINGLETON_ACQUIRE_TIMEOUT_SEC`, `BRAIN_HEALTH_ADDR`, `BRAIN_BOXLINK_ADDR`, `KMS_DEV_KEY_FILE`, the mTLS cert/key/CA paths, `DEV_BOX_ID`/`DEV_BOX_TOKEN`/`DEV_BOX_TOKEN_HASH`, and **`BOX_HEARTBEAT_INTERVAL_SEC`** (heartbeat cadence — a behavioral dial, so it MUST become a seeded `sender.config` key in Phase 1 when box heartbeat lands; until then it is an env bootstrap default, tracked here so it does not silently persist as a hardcoded-in-spirit value). The singleton keeper's internal 5s liveness-check interval (`internal/singleton/singleton.go`) is infra wiring, not a product dial; a candidate for a future seeded key if Phase 1+ wants maximal strictness. No behavioral dial is hardcoded beyond these tracked bootstrap values. Phase 1 moves box `token_hash` into `sender.boxes`. |
| OD-19 | Host antivirus TLS-inspection breaks host→localhost mTLS probes | **APPLIED — dev-host artifact, not a module defect** | On the Windows dev host, Norton intercepts TLS on `localhost:8443` and re-signs the Brain's cert with an untrusted root, so a Go/curl mTLS client on the HOST sees "unknown authority". The box↔Brain mTLS is unaffected INSIDE the Docker network. Resolution: the harness probes the mTLS boxlink routes **in-network** (from the box container), which is the correct, real test anyway. CI (Ubuntu, no interception) is unaffected. No code change; documented so a future dev doesn't chase a phantom cert bug. |

## Phase 1 build (data model & schema)

| # | Decision | Status | Detail |
|---|---|---|---|
| OD-20 | How Phase 1's drafted `sender_0001`/`0002` (schema/role creation) reconcile with Phase 0's `sender_0000` bootstrap, which already created the schema + roles + grants | **RULED (2026-07-23) — see ADR-0008** | Feature chain stays at its LOCKED numbers `sender_0001..0012` (config at `0010`, per Phase 9 §4.1) with ZERO locked-spec edits. `0001`/`0002` are idempotent confirmations of the bootstrap ground (their DOWN un-records only the version, never drops bootstrap-owned objects). Feature objects are created by the admin runner then `ALTER ... OWNER TO sender_owner` so `FORCE RLS` enforces. `0012` issues EXPLICIT DML grants (default privileges don't fire for reowned tables — verified on real PG), which makes `job_events` born with zero `sender_module` DML. Append-only REVOKEs land in the creating migration (ADR-0002). Proven green by `TestRunnerLifecycle` + full harness. |

### Phase 1 closeout (2026-07-23 — two guards + one carried question)

| # | Decision | Status | Detail |
|---|---|---|---|
| OD-21 | The ADR-0008 grants trap is manual discipline every future phase must remember | **RULED — converted to an enforced invariant** | `TestGrantsInvariant` (`tests/harness/phase1_grants_invariant_test.go`) enumerates every `sender.*` table from the catalog and asserts `sender_module`'s exact privileges per class; a future table added without grants FAILS. `TestGrantsInvariant_AntiVacuity` proves the teeth. Recorded in ADR-0008 + DoD-7 ("new table ⇒ explicit grants"). |
| OD-22 | `send_logs` write-once vs the Phase 7 retention sweep | **RULED — verified, no migration needed** | Live grants already are the required end state: `send_logs` = INSERT+SELECT+DELETE, UPDATE revoked. Phase 7 §3.1 runs the sweep IN THE BRAIN as `sender_module` (it "uses the `send_logs` DELETE privilege Phase 1 `sender_0009` reserved exactly for this") — no separate role, no additive migration. `TestSendLogsWriteOnceButRetentionCanDelete` asserts UPDATE rejected + retention-path DELETE succeeds. |
| OD-3 (AAD) | AAD row-binding adopt/decline | **LOCKED-ADOPTED** (Phase 2 §14 OD-3 "DECIDED-NOW (adopt)") | AAD = `account.id ‖ credential_key_id`. Phase 1 implements + tests it (T4 part D); Phase 2 carries it forward (its test T2b). The earlier "provisional/Phase-2-owns-adopt/decline" flag in the Phase 1 test comments is superseded — this is not re-opened. |

**Phase 1's own §14 decision log is CLOSED** in `Docs/phase-1-data-model.md` (every item
DECIDED-NOW or DEFER-TO-PHASE-N; NEEDS-USER empty). No Phase 1 build decision is left open.
Carried, still-current dispositions (unchanged by this closeout): **T5c** pause-reason
daily-reset test is **DEFER-TO-PHASE-5** (its lazy-reset UPDATE is Phase 4/5 logic; the Phase 1
`pause_reason` CHECK it needs is present + tested by T7), and the `jobs_reclaim_idx`
reconciliation note (0011 canonical over `02-claim-and-indexes.md`) stands as-is.
Two rulings carry forward as owned, scheduled work: confirm platform column shapes before
Phase 2 (pre-live gate G3 / OD-8), and the §9 credential-handoff preconditions (mTLS box
channel, memory-only + zeroed credential, residual-risk note) are LOCKED requirements Phase 3
must implement.

## Phase 2 build (account management & operator admin API)

Phase 2's own §10 decision log is **CLOSED** in `Docs/phase-2-account-management.md` (every item
DECIDED-NOW / DEFER-TO-PHASE-N / DROPPED; the needs-user list is empty). The carried-forward
rulings were **implemented as locked**, not re-opened:

| # | Decision (carried, locked) | Where implemented |
|---|---|---|
| Admin model | Operator-only, one scope `platform_admin`, env-configured bcrypt-hashed bearer key(s); no per-principal table (OD-9 dropped); no admin-layer tenant isolation. | `internal/adminapi/auth.go` (constant-time, fail-closed); mounted only when `ADMIN_API_KEY_HASHES` set. |
| Reserved system user (§0.1) | `SENDER_SYSTEM_USER_ID` = all-zero UUID owns shared pools/accounts; `user_id` stays NOT NULL; module has SELECT-only on `public.users` so the platform must seed it; pre-flight → clean `409 system_user_not_provisioned`. | `internal/adminapi/server.go` (`SystemUserID`), `pool_store.go` (`systemUserProvisioned`), tests T13. |
| OD-2 per-pool uniqueness | `UNIQUE (pool_id, smtp_host, smtp_username)` (`sender_0013`); dup within pool → `409`, across pools allowed; residual (two counters) accepted. | `sender_0013`; `account_create.go`/`account_update.go` pre-check + 23505 backstop; tests T7/T12. |
| OD-1 audit sink | `sender.account_events` (`sender_0014`), append-only; one row per audited op in-tx; reuses `event_actor` enum (no enum change). | `sender_0014`; `audit.go`; tests T8/T8b/T9. |
| OD-3/4/6/7 crypto | AAD = `account.id ‖ credential_key_id`; app-side UUID; namespaced `credential_key_id` alias; admin **Encrypt-only, never Decrypt**. | `crypto.go` (no Decrypt method), `uuid.go`, `kms.LoadEncryptKey`; tests T1/T2/T2b. |
| OD-5/8/10/12/13/14/15 | disabled-pool assign allow+warn; live SMTP verify DEFER-TO-PHASE-3; rate-limit off; `owner_user_id`/`kind` immutable; fallback kind-agnostic (Phase 4 owns kind policy); config reload DEFER-TO-PHASE-4; manual pause/unpause included. | pool/account handlers + `pool_list_update.go` (immutability), `config_*` (reload not built), `account_lifecycle.go`. |

### Phase 2 build surfaced (flagged, not silently fixed)

| # | Decision | Status | Detail |
|---|---|---|---|
| OD-23 | Phase 2 §12's `account_events` DDL specifies BOTH an unconditional `BEFORE UPDATE` reject-trigger AND an `account_id` FK `ON DELETE SET NULL` — a direct contradiction (the FK's `SET NULL` is an UPDATE the trigger blocks, so the account delete §2.5/§7.4 mandate becomes impossible; the 500 was caught by tests T5/T9). | **RULED — see ADR-0009** | Resolved on the spec's own terms + the Phase 1 `#23` lesson §7.4 already cites: the reject-trigger is made **FK-aware** — it permits ONLY the `account_id` non-null→NULL maintenance transition (all other columns unchanged) and rejects every genuine content mutation. Content immutability + defense-in-depth both preserved (T9 proves teeth: a superuser content UPDATE and a bundled null+content UPDATE are both rejected). No locked spec edited; fix lives in `sender_0014` + ADR-0009. |
| OD-24 | Phase 2 §3 (validation table) and §6.4 say `daily_limit` is `> 0` **"and ≤ a configurable ceiling — §6"**, and `daily_limit_default` is `> 0` **"and ≤ the ceiling used to sanity-bound per-account `daily_limit`"** — but **no ceiling config key was ever seeded** (Phase 1 `sender_0010` seeds 14 keys; none is a `daily_limit` ceiling, and the Phase 9 §4 50-key reference does not name one either). The cross-reference points at a dial that does not exist. | **RULED (2026-07-23) — option (c): a hardcoded sanity BOUND, not a config key** | The user ruled: do NOT seed a `daily_limit_ceiling` config key (that would edit the locked Phase 9 §4 reference + the `config_drift` gate for a dial nobody would turn), and do NOT drop the bound. Instead enforce a **generous hardcoded sanity ceiling** at the admin validation layer — a fat-finger guard, same category as the fallback-chain `maxDepth` guard, NOT a tuning dial. Implemented as `adminapi.DailyLimitCeiling = 100_000` (`account_validate.go`), applied to per-account `daily_limit` (create + PATCH) and to `daily_limit_default` config writes (`config_validate.go`); a violation returns a clean `400 out_of_range`, never a raw DB error. **Rationale on the record:** 100k comfortably admits every real per-mailbox provider limit (Google Workspace ~2k/day; SES account quotas far higher but a single SMTP *mailbox* never sends ~100k/day) while catching an extra digit or two. A too-high limit degrades **gracefully** at send time — a provider throttle → throttle class → pause; a provider suspension → auth_failure → quarantine, both already handled downstream — so this is a **reputation guard, not a correctness guard**. The §3/§6.4 "configurable" wording is satisfied by a **bound** rather than a dial; **promoting it to a real seeded config key later is a trivial additive change** (new key + wire the validator to it). No locked spec edited; no config key added (config_drift still 14 keys). Proven by `TestDailyLimitSanityCeiling` (above-ceiling create/PATCH/config → clean 400; realistic 50k accepted; >0 lower bound intact). |

**Needs-user: empty** (this closeout settled OD-24 via ruling option (c) — a hardcoded sanity
bound). Two owned/scheduled items carry forward (not open questions): the platform coordination
item to seed `SENDER_SYSTEM_USER_ID` into `public.users` (guarded by the §0.1 pre-flight, surfaced
as pre-live gate **G5**), and the OD-7 IAM precondition (admin service = Encrypt-only, never KMS
Decrypt) to verify before Phase 3 alongside G4.

## Phase 3 build (minimal send path)

Phase 3's own §10 decision log is **CLOSED** in `Docs/phase-3-minimal-send-path.md` (every item
DECIDED-NOW / DEFER-TO-PHASE-N / CONFIRMED; the needs-user list is empty). The decisions were
**implemented as specified**, not re-litigated:

| # | Decision (from Phase 3 §10) | Where implemented |
|---|---|---|
| P3-1 | box↔Brain = box-initiated persistent mTLS stream (register/heartbeat/push_job/report on one channel) | `internal/boxlink/stream.go` + `services/box/src/stream.js`; new `:8444` listener; Phase-0 `:8443` HTTP routes unchanged |
| P3-2 | SMTP failure → requeue to `queued` (`error_class='unknown'`, lease cleared, `attempt_count++`, no auto re-dispatch) | `internal/dispatch/outcome.go` `applyError`; test `TestOutcome_ErrorRequeues` + live T5 note |
| P3-3 | provider id capture; fall back to Brain `Message-ID`; `accepted_without_id` only as impossible-case backstop | `services/box/src/stream.js` (`parseProviderID` → Message-ID); `outcome.go` guards empty id |
| P3-4 | box id collision = upsert-by-identity (same id+token = re-register; wrong token = auth fail) | `stream.go` register handler (markActive upsert; reject on token mismatch) |
| P3-5 | `From` address-only when `from_name` NULL | `internal/dispatch/headers.go` `formatFrom` |
| P3-6 | `box_metadata` = `{box_version,hostname,started_at}`, diagnostics only | `stream.go` `boxMetadata`; box sends optional fields on register |
| P3-7 | §8 minimum guaranteed headers (From/To/Subject/Reply-To/Message-ID/Date) | `headers.go` `buildHeaders`; box sends exactly these + MIME framing |
| P3-11 | HELO name derived from `from_address` (`mail.<domain>`); configurability DEFERRED | box `smtp.js` `sendingDomain`; deferred to deliverability phase |
| U1 / OD-7 | box runs SMTP; Brain SOLE decryptor; admin Encrypt-only; box no DB/KMS | `internal/credential` (Decrypt-only, separate from `adminapi`), wired only into `internal/dispatch`; live suite S1/OD-7 |

**Phase 3 surfaced no new Open Decision** — no locked-fact conflict arose (unlike Phase 1's `#23`
or Phase 2's OD-23/OD-24). The one operational note recorded during build: on a fresh live stack
the dev box row (`sender.boxes`) must be seeded before the box's bounded register-retry window
expires (`scripts/dev.sh seed`); the behavioral harness seeds fixtures before starting the box,
so this is a dev-ergonomics note, not a defect. **No new config key** was added — Phase 3 CONSUMES
already-seeded keys (`job_max_processing_time_sec` = lease, `debug_log_retention_days` =
`send_logs` TTL), read at runtime in `internal/dispatch/config.go` (config_drift still 14 keys).
The per-operation SMTP timeout and outcome-buffer TTL keys are owned by Phases 6/7 (coordinated
notes in the Phase 3 spec), not seeded early.

### Phase 3 build surfaced (flagged, not silently resolved — CLAUDE.md Rule 2)

| # | Decision | Status | Detail |
|---|---|---|---|
| OD-25 | Phase 3 §6 (and `Docs/api-contract.md`) state the Domain-A surface enforces per-tenant isolation ("a caller for user A never sees user B's jobs"), but the request shapes as first built carried **no tenant selector** — `run {job_item_id}`, `report ?job_id=`/`?job_item_id=` — and Domain A authenticates a **single shared platform service token**, so the module had no request-borne principal to scope `readReport`/`createJob` by and could not *structurally* enforce user-A-can't-see-user-B from the API alone. Surfaced by both Phase 3 review gates; flagged (not silently resolved) per Rule 2. | **RULED (2026-07-23) — option (b): module-enforced tenancy** | The user ruled **module-enforced tenancy**. `run` (body) and `report` (query) now take an explicit `user_id` tenant selector (UUID, REQUIRED — missing/malformed → clean `400 validation_failed`), and every platform-facing query scopes `WHERE user_id = <tenant>`: `createJob` inserts only when `public.job_items.user_id = <tenant>`; `readReport`/`jobStateByItem`/`jobIDByItem` filter by it. A `run`/`report` for an id **owned by another tenant** returns the SAME `404 not_found` as a non-existent id (**never `403`** — no cross-tenant existence disclosure). **Reasoning on the record:** the module already receives `user_id` on `run` (frozen into `sender.jobs.user_id` from `public.job_items`), so scoping by it is natural; the **Domain-A contract has no consumers yet**, so adding a required selector is free now and breaking later; and this is **defense-in-depth** — even a platform-side bug that passes the wrong `user_id` cannot cross tenant boundaries in the module. This is an **additive** Domain-A contract change (a new required request field/param), permitted by the ruling; §6 of the Phase 3 spec + `api-contract.md` are updated so the isolation claim is now TRUE and enforced, not asserted. Implemented in `internal/platformapi/{handlers,store,server}.go`; proven by the cross-tenant behavioral tests (chunk 2). No other locked spec edited. |

**Needs-user: empty.** OD-25 is RULED (option b) and implemented. The two carried-forward
cross-system sign-offs remain **owned, scheduled** verification items (not open questions): G4 (prod
KMS Decrypt granted to the Brain role only; the admin service Encrypt-only) and G5
(`SENDER_SYSTEM_USER_ID` seeded into `public.users`) — both are going-live gates the harness
exercises via seeded fixtures, not build blockers.

### Phase 4 build surfaced (flagged, not silently resolved — CLAUDE.md Rule 2)

| # | Decision | Status | Detail |
|---|---|---|---|
| P4-OD-1 | Phase 4 spec §3 specifies the `one_by_one` strategy's account ordering as `ORDER BY account_status_score DESC, RANDOM() LIMIT 1`, and both the prose ("concurrent lanes naturally stick to the same account until it becomes ineligible... exactly one-by-one") and the T3b test plan ("A reaches 15 before B's first send") assume this order is STABLE across repeated calls while scores are tied — the documented pre-Phase-5 condition (all `account_status_score` default to 100 until Phase 5 computes real scores). Empirically verified against real Postgres this is FALSE: `RANDOM()` re-evaluates on every query execution, so two equal-score rows reshuffle on nearly every call (a direct 10-call trial returned an alternating/unstable sequence, not a sticky one). Under the literal §3 SQL, `one_by_one` behaves exactly like `random` whenever scores are tied — i.e. always, pre-Phase-5 — contradicting its own name and test plan. Surfaced while writing Chunk 3's routing tests; flagged (not silently resolved) per Rule 2. | **RULED (2026-07-23) — deterministic tiebreak: `a.id ASC`, not `RANDOM()`** | The user ruled a deterministic tiebreak. `defaultAccountOrderBy` (the `one_by_one` / fallback ordering, `internal/dispatch/eligibility.go`) is `account_status_score DESC, a.id ASC`. This makes the lowest-id account among score-ties the leader deterministically until it is genuinely removed from eligibility (hits `daily_limit` / becomes paused/quarantined/disabled), at which point the next-lowest tied id becomes the new leader — real "exhaust one, then move on," not incidental. Once Phase 5 computes distinct scores, `score DESC` already dominates and this tiebreak is inert on ties — so the change has **zero effect post-Phase-5** and does not weaken or contradict anything Phase 5 introduces. `random` and `round_robin` are unaffected (their own `ORDER BY` fragments do not use this constant). Proven by `TestRouting_OneByOne` (`internal/dispatch/routing_test.go`): account A (limit 15) is fully exhausted strictly before account B's first consume, across 25 draws. |

**Needs-user: empty.** P4-OD-1 is RULED and implemented; no other Phase 4 item is open.

### Phase 4 closeout (2026-07-24) — hardcoded-timeout triage (Rule 1 debt, pre-existing since Phase 0)

Every hardcoded timeout/interval literal across Brain and the box, enumerated and classified as
either **(a) a safety bound** (kept hardcoded, documented — the `MAX_FALLBACK_HOPS` precedent: a
guard rail, not an operator-facing behavior dial) or **(b) an operational tuning dial** (should be
a seeded config key; flagged for the owning future phase, never silently promoted here — a new
seeded key is DDL, and Rule 2/8 forbid this closeout from stealing a later phase's migration
number).

**(a) Safety bounds — kept hardcoded, now documented at each site (or already were):**

| Site | Value | Why it's a bound, not a dial |
|---|---|---|
| `internal/dispatch/pool.go` `maxFallbackHops` | 8 | Pre-existing Phase 4 exemption (§10 P4-19) — bounds an already-acyclic walk (Phase 2 cycle check), defense-in-depth only. |
| `internal/dispatch/dispatch.go` `sharedPoolNotConfiguredLogInterval` | 1 min | Pre-existing Phase 4 exemption — governs an internal LOG line's noise floor, not send/dispatch behavior. |
| `internal/singleton/singleton.go` keeper ticker | 5 s | Pre-existing OD-18 exemption — infra liveness-check cadence, not a product dial. |
| `internal/singleton/singleton.go` `backoff` (lock poll) | 200 ms | Same class as the keeper ticker — bounds how fast a losing Brain polls the advisory lock; not operator-facing. |
| `internal/boxlink/stream.go` register-handshake read deadline | 15 s | A DoS/hang guard on an unauthenticated peer ("a silent peer cannot hold a slot forever") — the register protocol itself has no variable-length legitimate case; this bounds abuse, not a real workload. |
| `internal/boxlink/stream.go` write deadline (push_job frame) | 10 s | Same class — bounds a wedged-peer write, not a tunable send behavior. |
| `internal/boxlink/boxlink.go`, `internal/httpapi/health.go` `ReadHeaderTimeout` | 5 s | Standard Go `http.Server` hardening against slow-header DoS; a platform-wide Go idiom, not a product dial. |
| `cmd/brain/main.go` shutdown grace period | 10 s | A drain-on-SIGTERM bound, not a runtime behavior a campaign/operator would ever want to tune per-deployment. |
| `services/box/src/smtp.js` QUIT-after-DATA timeout | 2000 ms (hardcoded, NOT `timeoutMs`) | QUIT is explicitly "best-effort" (the send already succeeded by the time QUIT runs) — this bound only affects how long the box waits before moving on, never affects delivery outcome. Flagged as inconsistent with the rest of the file (every other SMTP step uses the caller-supplied `timeoutMs`) — a cosmetic inconsistency, not a correctness bug, since QUIT's outcome is discarded either way. |
| `services/box/src/index.js` register-retry cap/interval | 30 attempts / 1000 ms | Startup-only bounded retry while the Brain may still be coming up; not a runtime behavior dial (a box that can't register in ~30s is a deployment-ordering problem, not something an operator tunes per campaign). |

**(b) Operational tuning dials — FLAGGED for promotion, not silently added here:**

| Site | Current value | Should become | Owning migration |
|---|---|---|---|
| `services/box/src/index.js`/`docker-compose.yml` `BOX_SMTP_TIMEOUT_MS` | env var default 30000 ms, hardcoded in compose | `smtp_operation_timeout_sec` — **already planned** in `Docs/phase-6-pacing-and-dead-box-detection.md` (§5, P6-11), default 30, delivered to the DB-less box via the additive `push_job.smtp.operation_timeout_sec` wire field. **This is not new scope** — Phase 6 already owns it, migration `sender_0018`. Flagging here only to record that Phase 4 leaves it exactly as Phase 6 expects to find it (still a bootstrap env default), and to surface the CRITICAL sub-check below. |
| `services/box/src/index.js`/`docker-compose.yml` `BOX_HEARTBEAT_INTERVAL_SEC` | env var default 10s (compose sets `"10"`) | **Genuine pre-existing gap, not previously reported this precisely:** `heartbeat_interval_sec` was ALREADY seeded in Phase 1 (`sender_0010`, default `120`) per OD-18's own plan ("MUST become a seeded key... when box heartbeat lands"), but the box was never wired to read it — it still uses its own hardcoded env-var default of `10`, completely decoupled from the seeded key (which the Brain-side heartbeat-staleness logic presumably reads or will read). No migration is needed — the key already exists; only the box's wiring (reading it from `push_job`/register-ack, or some other delivery path, since the box has no DB) is missing. **Not fixed here** — wiring a DB-less box to a DB-only config key is a delivery-mechanism decision (mirroring exactly the `push_job.smtp.operation_timeout_sec` pattern Phase 6/9 already designed for the timeout key) that belongs to whichever phase owns box↔config delivery, not a surgical Phase-4 closeout fix. Flagged for that phase's owner. |
| `services/box/src/stream.js` reconnect backoff (none currently — the box's `main` loop retries registration at a flat 1000ms; there is no NAMED "reconnect window" dial yet) | n/a (flat retry, not a window) | `startup_reconnect_window_sec` — **already planned** in `Docs/phase-7-runtime-hardening.md` (§5, P7-11), default 120, migration `sender_0019`. Not new scope; recorded so the eventual Phase 7 implementer knows the box currently has no reconnect-window concept at all (a flat retry loop), not even an unwired placeholder. |

**CRITICAL sub-check (explicitly required by this closeout): does any hardcoded timeout in the send
path or on the box↔Brain channel interact with the job lease, and could it silently violate
`smtp_operation_timeout_sec < job_max_processing_time_sec`?**

**Yes — one genuine, reportable exposure, though not yet a live violation.** `BOX_SMTP_TIMEOUT_MS`
(currently 30000ms = 30s, compose-set) is the box's per-operation SMTP timeout — structurally the
SAME dial `smtp_operation_timeout_sec` will become in Phase 6, just not yet coordinated with
anything. Today it numerically satisfies the eventual invariant against the current lease default
(`30s < job_max_processing_time_sec` = 120s), **but nothing enforces this relationship**:
`job_max_processing_time_sec` is ALREADY a live, operator-settable `sender.config` key (seeded
Phase 1, mutable via the Phase 2 admin config endpoint right now) — an operator could lower it to,
say, 20s today, and the box's hardcoded 30s SMTP timeout would then EXCEED the lease with **zero
reject-check, zero warning, anywhere in the running system**. The failure mode: an account hang
would blow through the shorter lease first, causing an unscored Brain-side reclaim/timeout
(Phase 6, not yet built) rather than the box's own scored timeout report — silently defeating the
exact ordering Phase 6/P6-11 is designed to guarantee, for however long between now and Phase 6
shipping the coordinated key + config-load reject check. This is NOT a Phase-4 regression (Phase 4
introduces no new interaction with this pair — the lease has been operator-mutable since Phase 1,
and the box's fixed SMTP timeout has been unenforced since Phase 3) — it is a **pre-existing,
now-explicitly-reported latent gap** between two already-shipped pieces, closed only when Phase 6
ships `smtp_operation_timeout_sec` + its config-load `config_ordering_violation` reject check
exactly as already planned. **Not fixed here** — promoting a value to a seeded config key is DDL
that belongs to Phase 6's own migration (`sender_0018`), not a surgical Phase-4 closeout change;
fixing the SYMPTOM (e.g. hardcoding a lower box timeout) without the coordinated reject-check would
just relocate the gap, not close it. Flagged for Phase 6, with the explicit interim risk recorded:
**an operator lowering `job_max_processing_time_sec` below 30s between now and Phase 6 would create
a live, currently-undetectable violation.** No other hardcoded timeout in the send path or the
box↔Brain channel interacts with the lease (the boxlink handshake/write deadlines are connection-
level hardening on the register/push frame itself, not per-SMTP-operation timing).

**No new config key added by this closeout.** `config_drift` remains 14 keys. Both flagged
promotions above are Phase 6/7's own already-planned keys (`sender_0018`/`sender_0019`) — this
closeout adds nothing, steals no migration number, and records the interim risk instead of
papering over it.

## Phase 5 build (error taxonomy, account status & quarantine)

Phase 5's spec §8 shipped a **closed decision log with an empty needs-user list** — every item
DECIDED-NOW / DEFER-TO-PHASE-N / FLAGGED-NOT-BUILT / CONFIRMED. Build surfaced the items below;
none required a user ruling (they are deliberate build-structure notes recorded per CLAUDE.md
Rule 2, not contradictions).

### Phase 5 build surfaced (recorded, not silently resolved — CLAUDE.md Rule 2)

| # | Decision | Status | Detail |
|---|---|---|---|
| P5-OD-1 | The auto-quarantine / auto-pause decision seams for Phase 9.5 | **APPLIED (deliberate)** | Chunk 4's structural requirement: auto-quarantine and auto-pause each go through ONE explicit predicate — `quarantineDecision()` and `pauseDecision()` in `services/brain/internal/dispatch/account_effect.go` — with a single call site each. Every automatic quarantine/pause flows through exactly those two functions. Phase 9.5's `autoquarantine_mode` (off\|shadow\|enforce) and `autopause_enabled` kill-switches can wrap the decision by editing ONLY those functions, with NO surgical re-plumbing across the outcome handler. **9.5's config keys are NOT seeded or wired here** (they live in 9.5's own migration `sender_0021`, per the locked plan); this is purely the seam they will wrap. Recorded as deliberate scope discipline. |
| P5-OD-2 | Migrations `0016` (columns) + `0017` (config) both land in the same build chunk | **APPLIED (chain contiguity)** | The locked plan associates `0016` with the scoring work (spec Chunk 3) and `0017` with the backoff key (spec Chunk 2). The migration chain must be contiguous (`migration_lint`) and the runner won't apply `0017` with `0016` absent, so BOTH landed together in build Chunk 2a; build Chunk 3 then implemented the scoring *formula* that consumes the `consecutive_failures` column. No spec edit; a build-sequencing note only. |
| P5-OD-3 | `attempt_count` saturation must apply to EVERY requeue path | **APPLIED (bug fix)** | §6.2 mandates `attempt_count` saturates at `max_attempts`. The outcome handler and the pre-existing dispatch-side requeue paths (`requeueNoAccount`/`requeuePushFailed`/header-injection, `dispatch.go`) both increment the same counter; an unconditional `+1` on the dispatch side would violate `jobs_attempt_count_check` (23514) once an account auto-quarantines mid-retry and its job requeues past the cap. Both now use `LEAST(attempt_count+1, max_attempts)`. Caught by a real behavioral test before it could reach production. |

**P5-13 (the coordinated Phase 4 §2.1 predicate edit) is now IMPLEMENTED, not just documented.**
The Phase 4 spec already carried the throttle-expired OR-term in prose (applied in the Phase 4
closeout); Phase 5's `sender_0016` adds the `throttle_paused_at` column and `sender_0017` the
`throttle_pause_seconds` key it depends on, so the term is now live in BOTH the atomic consume
(`accountConsumeQueryTmpl`, which additionally self-clears the pause + writes one
`account_events(system, unpause)`) and the read-only `poolHasEligible` predicate — the two kept
byte-identical so the pre-claim gate and the §5 exhaustion classifier can never drift from the
real selection logic. Reason-scoped: never admits a `manual` pause, never an in-window throttle
pause. The double-booking core (`SKIP LOCKED` + `accounts_daily_sent_check`) is untouched.

**Three new config keys added (`sender_0017`): `status_score_weights`, `retry_backoff_schedule`,
`throttle_pause_seconds`.** `config_drift` moves 14 → 17. `default_value` never mutated; both
`json` keys validated at write (admin endpoint) AND re-validated on read (fall back to the
documented default, never crash — §7). No locked spec touched; only additive columns + config.

### Phase 5 review gates (R1–R4) — findings

Four narrowly-scoped review passes ran after the build (R1/R2 architect, R3/R4 code), each
independently re-verifying against the working tree (the Phase-4 R4 lesson). R1, R2, R3 returned
zero findings. R4 (code review of scoring / state machine / decision seams) returned two REAL,
tree-verified findings on the account-flag write path, both fixed + covered by a new regression test:

| # | Finding | Severity | Fix |
|---|---|---|---|
| P5-R4-1 | `applyAccountEffect` read account flags WITHOUT `FOR UPDATE`, then wrote back whole-column literals computed in Go from that snapshot. Two concurrent outcomes on the SAME account (allowed — per-user lanes, no per-account in-flight lock, independent report transactions) could lost-update: an `auth_failure` commits `quarantined=TRUE`, a concurrent `throttle` then writes its stale `quarantined=FALSE` literal — **silently un-quarantining**, violating the LOCKED manual-recovery-only invariant (§4.2). | **HIGH** | `readAccountEffectState` now `SELECT … FOR UPDATE`; the account row is locked at tx entry in `applyClassifiedFailure` BEFORE the job requeue (whose `job_events` trigger takes a FK KEY SHARE on the account) — a consistent account→job lock order that also prevents the deadlock the naive lock would have caused. New `TestConcurrentOutcomes_QuarantineSurvivesThrottle` (two real connections) proves quarantine survives + is stable across repeated runs. |
| P5-R4-2 | In the consume CASE, the expired-throttle self-clear branch (`WHEN ea.was_throttle THEN FALSE`) preceded the `>= daily_limit` branch, so a send that both cleared an expired throttle pause AND reached the daily cap ended `paused=FALSE` while at-limit, plus a spurious `unpause` audit row. No safety/double-booking impact (next consume's count guard + `chainHasSelfRecovering` check the count directly; self-heals at reset) but an observable account-state/audit deviation. | **MEDIUM** | Reordered the CASE so daily-limit-reached wins over the throttle self-clear; the throttle-unpause audit now fires only when the row ended un-paused (`now_paused = FALSE`), so a throttle→daily_limit transition writes no spurious `unpause`. |

No additional findings. The consume's `SKIP LOCKED` + `accounts_daily_sent_check` double-booking
core remained untouched by both fixes (verified: the double-booking test still lands exactly 50).

## Phase 6 build (pacing & dead-box detection)

Phase 6's spec shipped a **closed decision log with an empty needs-user list** (§7 — every item
DECIDED-NOW/DECIDED-DEFER/FLAGGED/DEFER-TO-PHASE-7/CONFIRMED). One implementation-surfaced
reconciliation is recorded here (flagged per CLAUDE.md Rule 2, not silently resolved).

### Phase 6 build surfaced (recorded, not silently resolved — CLAUDE.md Rule 2)

| # | Item | Disposition | Detail |
|---|---|---|---|
| P6-OD-1 | The config-ordering invariant `smtp_operation_timeout_sec < job_max_processing_time_sec` (P6-11) has two texts that appear to differ on enforcement: **CLAUDE.md Rule 1** (NON-NEGOTIABLE) says a violating set is **REJECTED at config-LOAD** with external code `config_ordering_violation` — "Do not clamp or auto-correct — reject"; **Phase 6 spec §5.1** says the module "logs an internal warning ... does not hard-reject, to avoid coupling two independently-tuned dials **at write time**." | **RECONCILED — reject at LOAD, no coupling at WRITE (no spec conflict)** | The two texts are scoped to different enforcement points and do not actually conflict. §5.1's "no hard-reject" is explicitly about the **admin write path** (Phase 2 config-update validates ONE key at a time and cannot see the other dial). CLAUDE.md Rule 1's reject is at **config-LOAD** — the Brain reading the whole set together and refusing to act on a violating one. The **Phase 4 closeout already committed Phase 6 to ship exactly this**: "closed only when Phase 6 ships `smtp_operation_timeout_sec` + its config-load `config_ordering_violation` reject check" (Docs/open-decisions.md, Phase 4 closeout, line ~195). Implemented as: `validateConfigOrdering` in `services/brain/internal/dispatch/config.go` — `LoadConfig` returns a `config_ordering_violation` error (strict `<`; equality also rejected) so the Brain never runs on an inverted pair; `config_validate.go` (the write path) is left uncoupled per §5.1. `TestConfigOrdering_RejectsInvertedPair` proves reject-on-inversion + load-on-valid. No locked spec edited; Rule 1 (non-negotiable) governs where the two texts meet. |
| P6-OD-2 | `smtp_operation_timeout_sec` is *consumed by the DB-less box*, but the box had no delivery path for it (it used its own `BOX_SMTP_TIMEOUT_MS` env default, decoupled from the seeded/enforced key — the exact latent gap the Phase 4 closeout flagged). | **APPLIED (delivery wiring, additive)** | Wired via the additive `push_job.smtp.operation_timeout_sec` wire field (the delivery path the Phase 4 closeout named, line ~171): the Brain stamps the seeded value into every `push_job` (`dispatch.go`), and the box prefers it over its bootstrap env default (`services/box/src/stream.js`). `omitempty` + a `>0` guard keep it additive (older frames/tests fall back to the env default). This makes the Brain-side reject-check meaningful end-to-end: the value the box applies is always the enforced-strictly-below-lease one. |

**Needs-user: empty.** P6-OD-1 is a reconciliation (no conflict once scoped to load-vs-write, and
the Phase 4 closeout already pre-committed the reject check); P6-OD-2 is additive delivery wiring.
The locked P4-15 daily-slot refund stays **DEFERRED (P6-4 DECIDED-DEFER)** — not implemented, per
spec. Every other Phase 6 item is DECIDED in the spec's own §7 log.

### Phase 6 review gates (R1–R4) — findings

Four narrowly-scoped review passes ran after the build (R1/R2 architect, R3/R4 code), each
independently re-verifying against the working tree (the Phase-4/5 lesson, ADR-0010 / memory). R1
(pacing) and R3 (pacing + reclaim core) returned zero blocking findings (R3 noted the Go-vs-SQL
backoff-ladder formula duplication as a LOW maintainability suggestion — currently byte-equivalent,
verified). R2 (reclaim + carve-out + invariant + scope) returned zero defects, plus two
forward-looking non-blocking observations for Phase 9.5 (below). R4 (the deepest gate — carve-out +
disconnect + concurrency) returned ONE REAL, tree-verified HIGH finding on the reclaim lock order,
fixed + covered by a new two-connection regression test:

| # | Finding | Severity | Fix |
|---|---------|----------|-----|
| P6-R4-1 | Detector A's dead-box MASS reclaim (`reclaimJobsOnBox`) was a single bulk `UPDATE sender.jobs ... WHERE assigned_box_id=$1`, whose `AFTER UPDATE OF state` trigger inserts `assigned_account_id` into `job_events` — taking an FK `KEY SHARE` on the account row AFTER already holding the job row (**job→account** order). Phase 5's `applyClassifiedFailure` locks the account `FOR UPDATE` BEFORE its job requeue (**account→job**). Opposite orders on a shared `{job, account}` → a `40P01` deadlock cycle, reachable when a heartbeat-dead box still has an in-flight job whose penalizing box report is processed concurrently. Self-healing (the reclaim tx rolls back + retries next tick; Detector B still catches the job by lease) but a genuine deadlock in exactly the ADR-0010 recurring defect zone — and the original race test (`TestReclaimRace_OutcomeVsReclaim`) only exercised the SAFE half (Detector B + an *accepted* outcome, both job→account), never this pair. | **HIGH** | Both detectors now reclaim ROW-BY-ROW through one shared `reclaimJobLocked` that mirrors `applyClassifiedFailure` EXACTLY: read `assigned_account_id`, lock that account `FOR UPDATE` FIRST (account→job), then lock the job `FOR UPDATE SKIP LOCKED` (so a lane worker mid-handoff — `markSending`, job→account — is skipped not blocked, closing that opposite-order pair too), then apply the §2.4 transition. Locking the account is NOT a write, so the P6-10 carve-out (no account mutation, no score recompute) is fully preserved — verified: `reclaimJobLocked` reads `assigned_account_id` and writes only `sender.jobs`. Detector A's mass path is now spec §2.4's explicitly-endorsed "row-by-row under SKIP LOCKED for large fan-outs" (stale-mark in its own small tx first). New `TestReclaimRace_DetectorAVsClassifiedFailure` (two real connections) races Detector A's mass reclaim against a *classified* failure on the same job/account over 40 rounds — the exact combination the old test missed — asserting no `40P01` and a coherent terminal state. |

**Non-blocking forward-looking notes (recorded for Phase 9.5, NOT Phase 6 defects — the keys they
depend on do not exist yet):** (1) a runtime `config_ordering_violation` (an operator edits one dial
post-startup into a violating pair) degrades dispatch+reclaim to a silent skip-and-retry loop with
only a log line — no health/alert surface until `alert_webhook_url` lands (9.5). (2) `ReclaimRunner`
has no `dispatch_enabled` kill-switch gate — correct now (the dial is a 9.5 key, unwired everywhere),
but 9.5's wiring must gate the reclaim loop too, not only the dispatch `Runner` (CLAUDE.md Rule 7).

**Needs-user: empty** after the gates. P6-R4-1 is fixed + regression-tested; the two forward notes
are Phase 9.5 wiring reminders, not open decisions.

## Phase 7 build (runtime hardening: rescan, buffering, outage, retention, shutdown)

Phase 7's spec shipped **closed**, and the build implemented it exactly — schema-neutral apart from
`sender_0019`'s six config seeds (P7-1's derive-on-demand summaries mean there is NO summary table
and no `sender_0020`). No locked spec was edited. Nothing out-of-phase was built: no content
variation (Phase 8), no 9.5 operability keys, no Phase-9 quote/describe polish.

### Phase 7 build surfaced (recorded, not silently resolved — CLAUDE.md Rule 2)

| # | Item | Disposition | Detail |
|---|------|-------------|--------|
| P7-OD-1 | §3.1's retention sweep must `DELETE` from `sender.send_logs`, but `sender_0009` makes that table write-once. Does the sweep need an additive grants migration? | **CONFIRMED — no migration needed (verified by behavior, not by reading)** | `sender_0009` revokes UPDATE and installs a BEFORE UPDATE reject trigger but DELIBERATELY RETAINS DELETE; `sender_0012_grants_and_rls.up.sql:26` grants `SELECT, INSERT, DELETE` to `sender_module` with a permissive RLS policy. Discharges OD-22 (above) with a paired privilege test: `TestRetention_ModuleRoleCanDeleteButStillCannotUpdate` — the app role's DELETE affects 1 row (positive), its UPDATE errors and the subject is unchanged (negative control). NOTE recorded during the build: the reject trigger fires for **every** role INCLUDING the table owner, so no fixture can age a row by UPDATE — the retention test instead sets `debug_log_retention_days = 0` (config-not-code) and lets the real send path write an at-boundary row. |
| P7-OD-2 | Three TEST-side defects surfaced while closing the phase (Chunk 6). Recorded rather than quietly patched, because two of them had been reported as "pre-existing failures" in earlier commit messages and the third means a CI gate was under-collecting. | **FIXED at the cause; none is a product defect** | (a) `TestAccountConsume_NeverExceedsDailyLimit` built its 8 worker pools INSIDE the race, so one worker could drain all 50 slots during its peers' handshake — an UNDER-count (over-consumption was never at risk). Now pools are pre-connected + `Ping()`ed and released by a start barrier, and the fixed attempt budget is replaced by race-to-capacity + a CONFIRM phase where all 8 workers must be refused — strictly stronger (it proves the 51st consume is impossible). (b) Harness **T3** seeded a job `queued`/`next_attempt_at=NOW()` then moved it to `sending` in a second statement; Phase 4's polling loop (T3 predates it) claimed it in the gap and churned it, so the assertion could sample `queued`. `provider_message_id` was NULL throughout. Fixed by seeding `next_attempt_at` in the future, and the assertion now scans the whole event history for any `sent`/`bounced` transition. (c) Harness SUITE 7 hand-listed `smtp.test.mjs`, so Chunk 2b's `buffer.test.mjs` never ran in CI; it now globs `test/*.test.mjs` and FAILS a zero-test run. |

**Needs-user: empty.** P7-OD-1 is a confirmation (the required grants already exist, now proven by a
paired test); P7-OD-2 is test-infrastructure repair with no product change. The one guarantee the
phase exists to protect — the Brain never fabricates an outcome the box did not report — is unaffected
by all three and is asserted under restart, outage, and shutdown.

## Phase 9.5 build (operability & control-plane safety)

### Phase 9.5 build surfaced (flagged, not silently resolved — CLAUDE.md Rule 2)

| # | Decision | Status | Detail |
|---|---|---|---|
| P9.5-OD-1 | The Chunk 1 execution instructions directed "gate the ReclaimRunner with `dispatch_enabled` (the Phase 6 wiring reminder)" — the two forward-looking notes Phase 6's closeout recorded (`Docs/open-decisions.md`, Phase 6 section). But the authoritative Phase 9.5 spec (`phase-9_5-operability-and-control-plane-safety.md` §1.2) states the opposite in plain prose: *"The reclaim loop (Phase 6) still reclaims dead-box / lease-expired jobs back to `queued` — a paused dispatcher must not strand in-flight work."* Its own **O1** behavioral test asserts *"reclaim still runs"* while `dispatch_enabled=false`, and the Phase 0 groundwork spec's O1 entry says the same. Gating `ReclaimRunner` on `dispatch_enabled` would directly contradict the spec's own text and fail its own O1 as written. Surfaced before writing any gating code; flagged per Rule 2 rather than silently picking one side. | **RULED (2026-07-26) — follow the spec: `ReclaimRunner` is NOT gated on `dispatch_enabled`** | The user ruled that the Phase 6 closeout's forward note was written before §1.2's exact wording existed in the now-authoritative Phase 9.5 spec, and does not override it. `Tick()` (dispatch) and `TryDispatch` (the Phase-3 legacy claim path) both check `dispatch_enabled` at their top and perform no claim/route/push when it is `false`; `ReclaimTick()`/`ReclaimRunner` carry NO `dispatch_enabled` reference and run unconditionally, exactly matching §1.2's "still reclaims" language and the O1 test. The outcome handler, heartbeats, and config reads are likewise never gated (they were never claim paths). |

**Needs-user: empty.** P9.5-OD-1 is RULED and implemented as the spec's own §1.2 requires.

| # | Decision | Status | Detail |
|---|---|---|---|
| P9.5-OD-2 | While wiring `autoquarantine_mode`/`autopause_enabled` into the outcome-handling path (§2.1/§2.2), discovered that `OutcomeHandler.cfg` was a Config snapshot captured ONCE at `NewOutcomeHandler` construction (Phase 5) and never refreshed — unlike `Tick`/`ReclaimTick`/the startup rescan/the retention sweep, which all call `LoadConfig` fresh on every iteration. This meant `quarantine_threshold`, `throttle_pause_seconds`, the backoff ladder, and the score weights were ALREADY silently stale-since-boot on the report-handling path (a pre-existing Phase 5/6/7 gap, not something this phase introduced), and the new B2 keys would have inherited the same staleness — directly contradicting §2.1's explicit "takes effect on the next evaluation; no restart" promotion requirement for the shadow ladder. `retention_test.go`'s own comment ("the handler must be rebuilt to see the changed key") independently confirms the same staleness was already a known test-side workaround. Surfaced before writing any B2 gating logic; flagged per Rule 2 with the three candidate fixes (thread fresh config through the call chain / patch only the two new keys / document as a known restart-required limitation). | **RULED (2026-07-26) — thread fresh config through the whole call chain** | `Handle()` now calls `LoadConfig(ctx, h.db)` fresh at entry (mirroring `Tick`'s own pattern) and passes it as an explicit `cfg Config` parameter down through `applyAccepted`/`applyBounced`/`applyClassifiedFailure`/`applyAccountEffect`/`recomputeScoreOnSuccess`/`recomputeScoreOnFailure`/`bumpAndRecompute`/`backoffSecondsForClass`/`writeSendLog` — none of them read a cached struct field anymore. `NewOutcomeHandler(db, cfg)`'s signature is UNCHANGED (many existing call sites), but the `cfg` parameter is now accepted-and-discarded (documented on the struct) since Handle no longer needs a stored copy. Fixes staleness for every dial on this path at once, consistently — `quarantine_threshold`, `throttle_pause_seconds`, the backoff ladder, and the score weights (pre-existing Phase 5/6/7 dials) as well as the new `autoquarantine_mode`/`autopause_enabled`/`variation_enabled` (Phase 9.5) — with no data race (a local parameter, never a shared mutable field). |

**Needs-user: empty.** P9.5-OD-2 is RULED and implemented; the full dispatch/adminapi/platformapi suites and the live harness (53/53) are green with the fix in place.

| # | Decision | Status | Detail |
|---|---|---|---|
| P9.5-OD-3 | Review gate R2 (architect) found that S10 (`unit_price_credits`) and S11 (`variation_comment_intensity_rates`/`variation_zero_width_rate_pct`/`variation_css_micro_params`) were seeded and loaded into `Config` but NEVER actually consumed downstream — `quote`/`describe` still used the old hardcoded `QuoteUnitCredits=1` constant, and `variationSnapshot` never referenced any of the three S11 fields — directly contradicting spec §2.4's own text and the DoD-4/DoD-5 closeout's affirmative (and, at the time, incorrect) claim that these were wired. On investigating S11 specifically, a SECOND issue surfaced: `variation_zero_width_rate_pct` and `variation_css_micro_params.delta_px` are, on inspection, DUPLICATES of the already-wired Phase 8 `variation_zero_width_rate`/`variation_css_magnitude_px` parameters (P8-OD-1 RULED-WIDEN) under a different unit/shape (percent-int vs fraction-float for the same zero-width rate; a second `delta_px` for the same CSS magnitude) — wiring both would create two competing config keys driving the identical box behavior. Flagged per Rule 2 rather than silently picking a disposition for either half. | **RULED (2026-07-26) — S10: wire `quote`/`describe` to read `unit_price_credits` live; S11: wire only the two genuinely-new sub-values, treat the two duplicate sub-values as documented duplicates (not re-wired)** | **S10:** `platformapi/handlers.go` gained `readUnitPriceCredits(ctx)`, a local `sender.config` reader (mirrors `adminapi`'s own local-reader pattern — `platformapi` does not import `internal/dispatch`, same package-isolation precedent as OD-7/B3) — `handleQuote`/`handleDescribe` now read it fresh per request instead of the removed `QuoteUnitCredits` constant. New test `TestUnitPrice_QuoteAndDescribeReflectLiveConfig` (O7) proves the default (1) and a live write (3) both reach `quote`/`describe` with no restart. **S11:** `boxproto.Variation` gained two additive fields — `comment_intensity_rates` (object) and `css_skip_below_px` (int, `omitempty`) — populated by `variationSnapshot` from `cfg.VariationCommentIntensityRates`/`cfg.VariationCSSMicroParams.SkipBelowPx`; box-side, `variation.js`'s `resolveVariation` + `VARIATION_DEFAULTS` gained matching resolvers (percent-to-fraction conversion for the rates table, per-bucket graceful degradation on malformed input), and `variation-content.js`'s `injectComments`/`applyCssVariation` gained optional trailing parameters (`rates`, `skipBelowPx`) that override their hardcoded tables/thresholds when supplied, falling back to the original hardcoded values when omitted (backward compatible with every existing Phase 8 call site and test). `variation_zero_width_rate_pct` and `variation_css_micro_params.delta_px` are deliberately left unconsumed — the box's zero-width rate and CSS magnitude continue to be driven exclusively by Phase 8's `variation_zero_width_rate`/`variation_css_magnitude_px`, so there remains exactly one source of truth per behavior. New tests: `TestVariation_S11RatesReachPushJob` (Go), plus box-side `resolveVariation`/`injectComments`/`applyCssVariation` override tests (Node) — full dispatch suite (75.8s) and full box suite (78/78) green after the fix. |

**Needs-user: empty.** P9.5-OD-3 is RULED and implemented; S10 and the two genuinely-new S11 sub-values are now live-consumed, closing the gap R2 found before R3/R4 ran.

| # | Decision | Status | Detail |
|---|---|---|---|
| P9.5-OD-4 | Review gate R3 (code-level), tasked specifically with verifying the P9.5-OD-3 fix rather than trusting its description, found TWO further defects: (1) `boxproto.Variation.CSSSkipBelowPx` was tagged `omitempty` — its own doc comment claimed this was safe because "0 is not in the documented range," but the spec's §6 table documents the range as 0–20 (0 IS in range) and `readCSSMicroParams`'s own validation accepts 0 — so an operator-set `skip_below_px=0` ("vary every declaration, no matter how small") was silently discarded by Go's JSON encoder and the box fell back to its own hardcoded default (3) with no error, no log, no operator-visible signal — the EXACT bug class P8-OD-1 already fixed for the three sibling numeric fields in the same struct, now reintroduced on a fourth field the P9.5-OD-3 fix itself added. (2) A SEPARATE, previously-unflagged dial: `brain_singleton_acquire_timeout_sec` was seeded (`sender_0021`) and loaded into `dispatch.Config`, but `cmd/brain/main.go` actually calls `singleton.Acquire` using `internal/config.Config.SingletonAcquireTimeout` — sourced from the `BRAIN_SINGLETON_ACQUIRE_TIMEOUT_SEC` env var — BEFORE `dispatch.LoadConfig` is ever invoked, and the two values are never reconciled. An operator writing `PUT brain_singleton_acquire_timeout_sec` (following the spec's own "no redeploy" promise for every Phase 9.5 dial) had zero effect on the actual acquire timeout — the same seeded-but-unconsumed defect class R2 found for S10/S11, on a key outside that fix's scope. Also found (MEDIUM): `unit_price_credits`/`health_metrics_window_sec`/`brain_singleton_acquire_timeout_sec`/`variation_zero_width_rate_pct`/`variation_comment_intensity_rates`/`variation_css_micro_params` all lacked write-time semantic range validation despite the spec's §6 table documenting explicit allowed ranges for each (contrast `quarantine_threshold`/`daily_limit_default`/`variation_css_magnitude_px`, which all get dedicated bounds checks) — a negative `unit_price_credits` or an out-of-range JSON sub-field was silently accepted at write time. | **RULED (2026-07-26) — fix all four; treat as code-review findings on the SAME P9.5-OD-3 fix, not a new scope decision** | **(1)** Removed `omitempty` from `CSSSkipBelowPx` (`boxproto.go`), matching its three siblings; corrected the self-contradictory doc comment. New test `boxproto/variation_wire_test.go`'s `TestVariation_ZeroValueNumericFieldsSurviveJSONRoundTrip` asserts all four amended numeric fields (`CSSMagnitudePx`/`ZeroWidthRate`/`UserAgentRate`/`CSSSkipBelowPx`) survive a REAL `json.Marshal`/`Unmarshal` round trip at their zero value — confirmed this test fails against the pre-fix `omitempty` tag (verified by temporarily re-adding it) and passes after, so the test is genuinely non-vacuous. **(2)** `cmd/brain/main.go` gained `readSingletonAcquireTimeout(ctx, pool, envDefault)` — a minimal local `sender.config` reader (mirrors `adminapi`/`platformapi`'s own local-reader pattern) called on the already-connected pool BEFORE `singleton.Acquire`, falling back to the env value on any read/parse failure (a genuinely pre-`sender_0021` database, or a transient blip) — the singleton guard itself must never block startup on a config problem unrelated to it. Verified LIVE against the running stack: `PUT brain_singleton_acquire_timeout_sec=3`, then a real second `docker compose run brain` process exited in ~3s (not the 60s env default), proving the live value is genuinely read. **(3)** Added six missing semantic range checks to `config_validate.go`'s per-key switch (`unit_price_credits` 0–1000, `health_metrics_window_sec` 60–86400, `brain_singleton_acquire_timeout_sec` 0–600, `variation_zero_width_rate_pct` 0–100, plus two new inner-shape validators `validateIntensityRates`/`validateCSSMicroParams` for the two S11 json keys, mirroring `validateScoreWeights`/`validateBackoffSchedule`'s established pattern) — all six ranges match the spec's own §6 table exactly, and all seeded defaults were re-verified to fall within their new bounds. New test file `adminapi/phase95_config_range_test.go` (6 tests) proves each boundary (reject just-outside, accept just-inside, including the legitimate `0` boundaries for `unit_price_credits`/`brain_singleton_acquire_timeout_sec`/`skip_below_px`). Full suite re-verified after all fixes: `dispatch` (346s under heavy host load, still green), `adminapi` (92.9s), `platformapi` (12.7s), `boxproto` (new package, 0.4s), box Node suite (78/78) — all green; `gofmt`/`go vet` clean. |

**Needs-user: empty.** P9.5-OD-4 is RULED and implemented; all four R3 findings (2 HIGH, 1 MEDIUM covering 6 keys, plus the boundary-value test gap) are fixed and re-verified in the tree — not merely reported as fixed, per CLAUDE.md Rule 11's own standing requirement that this closeout not repeat.

### R4 (final gate) — independent re-verification of R1-R3's claimed fixes + B3/B4 concurrency review

Per CLAUDE.md Rule 11, R4 re-ran (not merely re-read) every earlier gate's claimed fix directly
against the tree: **P9.5-OD-1 CONFIRMED-IN-TREE** (`reclaim.go`/`runner.go` carry zero
`dispatch_enabled` references); **P9.5-OD-2 CONFIRMED-IN-TREE** (`outcome.go`'s `Handle` loads
`Config` fresh and threads it through the whole call chain, no cached field); **P9.5-OD-3
CONFIRMED-IN-TREE** (`readUnitPriceCredits` is genuinely called from `handleQuote`/`handleDescribe`;
`variationSnapshot` genuinely populates `CommentIntensityRates`/`CSSSkipBelowPx` from `Config`);
**P9.5-OD-4 CONFIRMED-IN-TREE** (`CSSSkipBelowPx`'s `omitempty` is genuinely gone;
`readSingletonAcquireTimeout` is genuinely called before `singleton.Acquire`, not dead code; all six
range/shape validators are present and their tests genuinely pass — R4 ran
`TestPhase95_*` (6/6), `TestVariation_ZeroValueNumericFieldsSurviveJSONRoundTrip`, and the full
`internal/adminapi` suite live against real Postgres, not just read the diffs).

R4's own scope (B3/B4 health/alert paths + concurrency): confirmed structurally read-only
(`health_alerts.go`/`health_store.go` contain zero `INSERT`/`UPDATE`/`DELETE`); confirmed the
account-before-job lock order (Rule 9/ADR-0010) is genuinely unchanged by B1/B2 — `applyAccountEffect`
still runs inside the transaction that already holds the account `FOR UPDATE` from
`readAccountEffectState`, and B2's shadow/enforce/off switch adds no new SQL statement or lock, only
Go-level conditional wrapping — so no new forced-interleave test is required, confirming rather than
merely trusting R1's original claim. Confirmed `readSingletonAcquireTimeout`'s pre-lock config read is
harmless under a two-Brain race (independent read-only SELECTs, no contention with the advisory lock).
One **LOW, non-blocking** finding: `health_alerts.go`'s `alertDedup.clearStale`+`rising` do not provide
a strict "fires exactly once until it clears" guarantee under CONCURRENT `/v1/admin/health` pollers
specifically (each request's own goroutine computes its own snapshot and can race another request's
`clearStale`/`rising` pair) — bounded impact (at most an extra two-vocabulary-clean webhook POST, no
state mutation), not fixed in this pass; noted for a future B4 hardening pass if webhook volume ever
makes it operationally relevant.

**Phase 9.5 review gates R1-R4 are CLOSED. APPROVE.** All four gates ran sequentially (never in
parallel, per CLAUDE.md §11), each later gate independently re-verified in the tree that every prior
gate's claimed fix actually landed, and the one class of defect this discipline exists to catch (a
"fixed" claim that turns out to be partial or absent) was found TWICE in this same sequence — R2 on
the original build's own claims (P9.5-OD-3), then R3 on R2's own fix (P9.5-OD-4's `CSSSkipBelowPx`
half) — and both times caught and closed before the NEXT gate ran, exactly as the discipline intends.

### Phase 9.5 closeout (Chunk 5 — B5 re-verification + B6/E11 load test)

B5 (Brain singleton) was confirmed ALREADY BUILT in Phase 0, not rebuilt this phase — re-verified
live with a genuine second Brain process (`docker compose run --rm --no-deps brain` against the
SAME Postgres): it exits non-zero within its bounded `brain_singleton_acquire_timeout_sec`, logs
"another Brain is active; exiting cleanly", and issues zero claim/push; the first Brain's `/readyz`
confirms `singleton_lock:true` throughout, unaffected. This is also `tests/harness/run.sh`'s
pre-existing SUITE 4, re-run green as part of the full harness.

B6/E11 (the single-Brain throughput-ceiling scale gate) shipped as `tests/harness/load_e11.sh`,
recording its numbers in the new `Docs/load-test-results.md` per **OD-7's ruling** (a committed,
versioned doc artifact, not a dashboard). One methodological correction surfaced and fixed WHILE
authoring the script, recorded here rather than silently patched: the first draft measured
`send_rate_per_min` with the Phase 6 per-lane pacing delay (`send_delay_min_sec`/`send_delay_max_sec`,
default 3-7s) still active, which throttles throughput at roughly `lanes/avg_delay` long before the
Brain's own per-tick capacity is stressed — confounding exactly the variable §7 wants isolated (the
same reasoning Phase 6's own T1 test uses in the opposite direction to prove its pacing IS
delay-bound). Fixed by zeroing both keys for the run's duration (restored on exit, via the script's
EXIT trap) and by shrinking `health_metrics_window_sec` to the run's own measurement window (the
seeded 3600s default otherwise dilutes a short burst's rate into "per hour," reading near-zero). A
second draft's saturation heuristic (comparing raw queue depth step-over-step) was also corrected:
because each ramp step seeds MORE volume than the last, a bigger queue is expected regardless of
saturation, so the real signal used is whether `send_rate_per_min` itself plateaus/regresses as
lanes grow with boxes still active — exactly §7's own "backlog growth despite idle boxes" reasoning,
applied to the metric that is actually comparable across steps.

**Needs-user: empty.** No new Open Decision from Chunk 5; both B5 and B6/E11 close out per the
spec's own §5/§7 without contradiction. Phase 9.5's five chunks are complete; see
`Docs/definition-of-done.md`'s Phase 9.5 status block for the full nine-item closeout.

### Phase 9.5 CLOSEOUT (2026-07-27) — config consumption sweep (all 53 keys) + omitempty/zero-value sweep

R2 and R3 each found, by accident, a config key seeded/documented/passing `config_drift` but never
read by any code — this CLOSEOUT swept all 53 keys deliberately rather than trusting there were no
more, and separately swept every wire struct for the omitempty/zero-value bug class R3 also found
by accident (`CSSSkipBelowPx`).

| # | Decision | Status | Detail |
|---|---|---|---|
| P9.5-OD-5 | The config consumption sweep (Chunk 1) found a THIRD instance of the seeded-but-unconsumed defect class, independent of R2/R3's two: `daily_reset_hour` was seeded and write-time-validated since `sender_0010`, documented in Phase 1 §11 ("resetting at `daily_reset_hour`"), but every daily-reset-boundary computation (eligibility/consume, pool exhaustion, account scoring, the admin health endpoint) computed "today" as plain midnight in `daily_reset_timezone`, ignoring the hour dial entirely — an operator turning this dial got no behavior change. Two further keys were found ORPHANED with no locked-spec path to wire them: `retry_count_4xx` (Phase 5 §6.2 documents it as a soft early-life retry hint, but V1 has no per-job avoid-set/attempt-tracker to hook it into — Phase 5's own §6.3 defers that mechanism) and `variation_zero_width_rate_pct`/`variation_css_micro_params.delta_px` (already ruled DUPLICATES of Phase 8's wired dials under P9.5-OD-3 — re-confirmed, not re-litigated). | **RULED (2026-07-27) — wire `daily_reset_hour` for real; mark the two orphans RESERVED / NOT-YET-CONSUMED, do not invent a mechanism for either** | `daily_reset_hour` wired into all four call sites via the SAME boundary formula (`(NOW() - INTERVAL '1 hour' * hour) AT TIME ZONE tz)::date`), never re-derived separately, matching the codebase's existing "one predicate, shared everywhere" discipline (`eligibility.go`, `exhaustion.go`, `account_score.go`, `adminapi/health_store.go`). Seeded default (0) is fully behavior-preserving — no existing operator's behavior changes. Two behavioral tests (`daily_reset_hour_test.go` in both `dispatch` and `adminapi`) prove an identical row's eligibility genuinely flips when only the hour dial changes; both were verified non-vacuous by reverting the SQL wiring and confirming the tests fail loudly, then restoring. `retry_count_4xx` and the two S11 duplicate sub-values are marked **RESERVED / NOT-YET-CONSUMED** in `Docs/config-reference.md` with the reasoning inline — wiring `retry_count_4xx` requires designing the avoid-set/attempt-tracker Phase 5 explicitly deferred, an Open Decision for whichever phase takes it on, not a surgical fix here. |
| P9.5-OD-6 | The omitempty/zero-value sweep (Chunk 2) checked every remaining config-derived/operator-settable field for the P8-OD-1/P9.5-OD-4 bug class (a JSON `omitempty` tag silently dropping an operator-set 0/false/"" and substituting the receiver's own hardcoded default). Two candidates found structurally identical in shape to the already-fixed `CSSSkipBelowPx`: `boxproto.RegisterAck.OutcomeBufferTTLSec` and `boxproto.SMTP.OperationTimeoutSec` — both numeric, both `omitempty`, both fall back to the box's bootstrap env default when zero/absent. | **RULED (2026-07-27) — flag, do not fix; the two locked specs never document 0 as meaningful** | Unlike `CSSSkipBelowPx` (Phase 9.5 §6's own table explicitly names the 0–20 range, so 0 is a documented legitimate value), the locked Phase 7 spec (§5.3/P7-11, `box_outcome_buffer_ttl_sec`) and the locked Phase 6 spec (§5/P6-11, `smtp_operation_timeout_sec`) never state that 0 means anything ("disable buffering/timeout entirely" is not a documented feature for either). Removing `omitempty` here would not correct a documented gap, it would invent new semantics for a locked spec unilaterally — reserved for an Open Decision ruled by the user (CLAUDE.md Rule 2), not a surgical fix. Both fields' doc comments in `boxproto.go` now record this reasoning inline ("CHECKED, NOT CHANGED") so a future reader does not re-discover and re-flag the same non-issue. Every other omitempty field was confirmed safe by inspection: bool toggles' `VARIATION_DEFAULTS` fallback is already `false` (absent and explicit-false are behaviorally identical), and pointer-typed fields (`*int`, etc.) check nil-ness under `omitempty`, not the pointed-to value. |

**Needs-user: empty.** P9.5-OD-5 and P9.5-OD-6 are both RULED and implemented. The `config_consumption`
CI gate (new, this CLOSEOUT) makes the defect class P9.5-OD-5 found structurally unable to recur
silently: a future seeded-but-unconsumed key now fails CI rather than waiting for a review gate to
find it by accident.

## Phase 9 build (polish and documentation — the terminal phase)

### Chunk 5 — the cross-phase consistency audit (§6): one structural fact re-confirmed, zero new contradictions

Eight properties (enum emitters, state-transition ownership, account-before-job lock ordering, the
reclaim-does-not-score carve-out + bounce-never-penalizes, wait-vs-fail ordering + rescan
event-freedom, all-toggles-off byte-identical + the single-authoritative-duplicate-window,
migration-chain contiguity + the 53-key reconciliation, coordinated cross-phase doc edits, and a
docs-vs-behavior spot check) were verified against the ACTUAL CODE — not the docs — with every
finding independently adversarially re-checked by a second pass reading the same files itself.

| # | Decision | Status | Detail |
|---|---|---|---|
| P9-OD-1 | The enum-emitter audit found `box_status='disabled'` has NO live application-code writer anywhere in `services/brain` or `services/box` — the only two places that literal appears are `tools/provision-box/main.go`'s `revoke` subcommand (which only PRINTS decommission SQL text for an operator to run out-of-band; confirmed no DB driver import in that file) and test fixtures. Surfaced by the audit rather than assumed clean. | **CONFIRMED-CONSISTENT with the existing P9-A ruling — no new decision needed** | This is not a new gap: Phase 9 §5.4a step 4 already documents box decommission as an out-of-band operator procedure (rotate/clear `token_hash`, which the SAME `provision-box revoke` command emits), and P9-A already ruled `/v1/admin/boxes` CRUD is a deliberate post-V1 item. The audit's independent re-verification (reading `provision-box/main.go` directly, confirming zero DB access in that binary, confirming no `/v1/admin/boxes/*` route exists) CONFIRMS the documented disposition holds in the tree, rather than merely restating it — exactly this chunk's mandate ("if the audit reveals a genuinely missing capability, flag it — do not build it"). No code change; recorded here for traceability since this is the one non-trivial fact the audit's 16 total agent-passes surfaced. |

**Net audit result: zero contradictions found across eleven phases.** Per this chunk's own
instruction, a zero-finding result is a reason for extra scrutiny, not celebration — the mandatory
adversarial second pass on every property (not just a sample) is what makes this a defensible
"clean" result rather than an assumed one. All eight properties independently re-verified true; the
one fact worth recording (P9-OD-1) was already covered by an existing ruling, re-confirmed rather
than re-litigated.

**Needs-user: empty.**

### Chunk 6 — end-to-end verification (§8): the write-path config-ordering reject is a genuinely missing capability, FLAGGED not built

| # | Decision | Status | Detail |
|---|---|---|---|
| P9-OD-2 | E4 (§8) requires: "PUT smtp_operation_timeout_sec >= job_max_processing_time_sec → 400 config_ordering_violation, no write" — and Phase 9 §4.6/§5.4 states as fact that "on a live change the PUT is rejected with the clean external code config_ordering_violation." Live-tested against the actual running Brain (rebuilt image, real Postgres): `PUT /v1/admin/config/smtp_operation_timeout_sec` to `999999` (≥ `job_max_processing_time_sec`=120) returned **HTTP 200 and the write succeeded** — no rejection, no `config_ordering_violation`, no `400`. Same result for `PUT box_outcome_buffer_ttl_sec` to `1` (< `startup_reconnect_window_sec`=120, violating invariant b) — HTTP 200, write succeeded. Grepped `internal/adminapi` for `config_ordering_violation`/`validateConfigOrdering`: **zero matches** — the string and the check exist ONLY in `internal/dispatch/config.go` (the config-LOAD path, read by `Tick`/`ReclaimTick`/rescan/retention on their own cadence), never in `internal/adminapi/config_validate.go` (the write path). This is EXACTLY the gap P7-OD-5 (Phase 7 closeout) and P6-OD-1 flagged and explicitly deferred: "Building it in Phase 7 would be out-of-phase work... it closes when Phase 9 ships §4.6 + E4." Phase 9 §4.6/§5.4's own prose already asserts the write-path reject as an accomplished fact ("the PUT is rejected") — that assertion is FALSE in the current tree; only the load-path half exists. | **FLAGGED — genuinely missing capability per this phase's own mandate ("if the audit reveals a genuinely missing capability, flag it — do not build it"); NOT built here** | **Live-observed consequence (matches P7-OD-5's own prediction exactly, re-confirmed by direct observation rather than by re-reading the prior analysis):** during the ~12 seconds the bad `smtp_operation_timeout_sec=999999` value was live, both `dispatch: tick error` and `reclaim: tick error` logged `config_ordering_violation` once per tick (the config-LOAD path correctly refusing to run on the bad set — confirmed via `docker compose logs brain`); `GET /readyz` stayed `db:true, singleton_lock:true` throughout (no crash, no corruption); restoring the value via the SAME `PUT` immediately cleared the errors on the very next tick — the Brain self-heals the instant a valid config is written, exactly as designed. **No job was lost or corrupted** during the window (dispatch/reclaim simply skip-and-retry on the normal cadence while the set is invalid, per the existing load-path design) — the cost is purely that the operator discovers the mistake at the next tick's log line rather than at the moment of the `PUT`, which is precisely the "safe but not instant" characterization P7-OD-5 already gave this gap. **Disposition:** this is Phase 9's OWN deliverable (§4.6/§5.4/E4) reporting itself unbuilt when checked against the actual code — an honest audit finding, not a contradiction between two docs. Per this phase's explicit governing rule, it is flagged rather than fixed in this pass. The fix, when undertaken, is additive-only and narrowly scoped: `adminapi/config_validate.go`'s `validateConfigValue` would need to read the OTHER key of each pair (a query the current single-key validation signature does not have access to) and return `codeConfigOrderingViolation`/`400` for either pair, mirroring `dispatch/config.go`'s existing `validateConfigOrdering` logic exactly — no schema change, no new config key, additive to the existing write path only. |

**RULED (2026-07-27, closeout): close it now.** Rationale on the record: an operator who `PUT`s an
invalid pair got a clean `200 OK` while the system silently ignored the write — the same
"false-promise-of-control" defect class the 9.5 consumption sweep already closed twice (P9.5-OD-5).
Worse, the inverted pair PERSISTED in `sender.config`, so the load-path rejection then surfaced as a
LATENT STARTUP/TICK FAILURE at the next deploy or crash-recovery — far from the change that caused
it. Fail at the point of the mistake, not later.

**P9-OD-2 is RULED-AND-CLOSED.** Implemented:

- **New package `internal/configordering`** — the two invariant checks
  (`CheckSMTPTimeoutVsLease`, `CheckBufferTTLVsReconnectWindow`) extracted as pure,
  dependency-free functions with no DB/HTTP import. `internal/dispatch/config.go`'s
  `validateConfigOrdering` (the load path) now CALLS these same functions instead of
  reimplementing the comparisons inline — the two enforcement points can never silently define
  "valid" two different ways. The load path's error type/string (`configOrderingError`,
  `"config_ordering_violation: <detail>"`) is byte-identical to before (verified: the three
  existing dispatch tests that assert on the exact string —
  `TestConfigOrdering_RejectsInvertedPair`, `TestConfigOrdering_IsNotADBOutage`,
  `TestConfigOrdering_RejectsShortOutcomeBufferTTL` — all still pass unchanged).
- **New file `internal/adminapi/config_ordering_write.go`** — `checkConfigOrderingOnWrite`, called
  from `handleConfigPut` (`config_handlers.go`) after the existing value_type/semantic validation
  and BEFORE the DB write, for exactly the four keys that have an ordering counterpart
  (`smtp_operation_timeout_sec`/`job_max_processing_time_sec`,
  `box_outcome_buffer_ttl_sec`/`startup_reconnect_window_sec`). It reads the counterpart key's
  LIVE current value fresh (never cached) inside the same request, calls the SAME
  `internal/configordering` functions the load path uses, and on violation returns a clean
  `400 config_ordering_violation` (new external code, added to the additive-only enum) whose
  `message` NAMES the conflicting counterpart key, its current value, and the required relation in
  plain English (e.g. *"This value conflicts with job_max_processing_time_sec (currently 120):
  smtp_operation_timeout_sec must stay strictly below job_max_processing_time_sec."*) — an operator
  is never left guessing. All 49 other config keys are unaffected (no counterpart, `nil` fast-path).
- **Ordered widening is a documented consequence, not a bug.** Raising BOTH keys of a pair upward
  (e.g. widening the SMTP timeout AND the lease) requires ordering the two `PUT`s, because each
  individual step is validated against the OTHER key's value at that moment — raise the boundary
  key first (the lease, or the reconnect window), then the dependent key (the timeout, or the
  buffer TTL). Documented inline in `config_ordering_write.go` and in the new test
  `TestConfigOrdering_OrderedTwoStepWideningSucceeds` (below), which proves the wrong order fails
  and the right order succeeds.
- **Tests** (`internal/adminapi/config_ordering_write_test.go`, real Postgres, real HTTP against
  the actual production Server): `TestConfigOrdering_InvertedPUTRejectedAndNotPersisted` (both
  directions of both invariants rejected with `config_ordering_violation`, the error names the
  counterpart key+value, and — anti-vacuity — the stored value is read back and confirmed
  UNCHANGED after the rejected write); `TestConfigOrdering_ValidPUTSucceeds` (positive control — a
  legal pair on either key of either pair succeeds normally, proving the boundary check does not
  over-reject); `TestConfigOrdering_OrderedTwoStepWideningSucceeds` (the wrong order fails, the
  right order succeeds, for both invariants); `TestConfigOrdering_LoadPathBackstopStillRejectsDirectSQL`
  (defense in depth intact — a value that reaches `sender.config` by a path OTHER than the admin
  `PUT`, e.g. a direct SQL write, is still genuinely present in the table for the Brain's own
  `LoadConfig` to catch on its next tick; the load path's own test suite in `internal/dispatch`
  — unmodified by this change — is what actually exercises that catch). A REAL bug was caught and
  fixed while writing these tests: the first draft of `checkConfigOrderingOnWrite` named the WRONG
  key in the error message for the `job_max_processing_time_sec`/`startup_reconnect_window_sec`
  write direction (it read `configordering.Violation`'s own `CounterpartKey` field, which is fixed
  to the invariant's semantic dependent/boundary structure, not to "the other key from this
  particular write's perspective") — caught by
  `TestConfigOrdering_InvertedPUTRejectedAndNotPersisted`'s own counterpart-naming assertion before
  it ever reached a live test, fixed by building the message from the write-specific local
  variables instead of the shared `Violation` struct's own key labels.
- **Live-verified against the actual running Brain** (rebuilt image): the exact `PUT` that
  previously returned `200` for `smtp_operation_timeout_sec=999999` now returns a clean
  `400 config_ordering_violation` naming `job_max_processing_time_sec (currently 120)`; the stored
  value is confirmed unchanged (`30`, the seeded default) via a follow-up `GET`; the symmetric
  `box_outcome_buffer_ttl_sec=1` `PUT` is likewise rejected; a valid `PUT` (`45`, still `< 120`)
  still succeeds normally. Full harness re-run 53/53 green; all 5 CI gates green.

**Needs-user: none — RULED.**

### Review gates R1/R2 (sequential, per CLAUDE.md §11) — findings and dispositions

R1 (architect-review, Chunk 5's audit rigor) independently spot-checked four of the eight audited
properties against the actual source (not the audit's own citations) and confirmed all four hold.
One LOW wording-precision finding: `architecture-overview.md`'s Chunk 5 summary said `applyAccepted`
touches `sender.accounts` "only via the implicit FK KEY SHARE," same as `markSending` — but
`applyAccepted` also calls `recomputeScoreOnSuccess`, which issues real (non-exclusive) `UPDATE
sender.accounts` statements; the underlying safety property (no exclusive lock, proven deadlock-free)
still holds, so this was a prose-precision fix, not a code or property change. **FIXED** (commit
`16b65a8`).

R2 (code-review, contract completeness — read as a UI-consumer) first independently re-verified R1's
fix in the tree (confirmed the corrected wording is present and re-derived the underlying
`recomputeScoreOnSuccess`/`bumpAndRecompute` claim from source itself, not from the commit message),
then found five real documentation gaps a UI dev would actually hit:

| # | Finding | Severity | Disposition |
|---|---|---|---|
| R2-1 | `api-contract.md` never stated B8's pagination REQUEST params (`?cursor=`/`?page_size=` names, default 100/max 200, silent clamp-not-error on out-of-range) — a UI dev would have to read a second file or experiment. | MEDIUM | **FIXED** — added the explicit param line under the B8 JSON block. |
| R2-2 | `provider_message_id`/`completed_at` are OMITTED (key absent) on the single-job `report` shape but ALWAYS PRESENT as `null` on the campaign-items shape — same field names, different presence semantics, undocumented either way. | MEDIUM | **FIXED** — documented both semantics explicitly at their respective shapes. |
| R2-3 | No inlined `error.code` enum/table in `api-contract.md` itself — it lived only in a companion doc (`phase-9-polish-and-documentation.md` §5.2) with no forward pointer from the per-endpoint rows. | MEDIUM | **FIXED** — inlined the full envelope shape + code enum into `api-contract.md` directly (a new section right after the auth-domains section), including the `config_ordering_violation` row's honest note pointing at P9-OD-2. |
| R2-4 | `handleRun`/`handleReport`'s `user_id` check conflated empty and malformed into a single `issueRequired`, inconsistent with the `job_id`/`job_item_id`/`campaign_id` checks in the SAME handlers, which correctly split empty (`issueRequired`) from malformed (`issueMalformed`). | LOW | **FIXED** — split both `user_id` checks the same way (`internal/platformapi/handlers.go`); covered by the existing `TestTenancy_MissingOrMalformedSelectorRejected` (asserts status code only, unaffected by the internal-reason split). |
| R2-5 | `phase-9-polish-and-documentation.md` §5.3's quick-reference table lists `run`'s request body as `{job_item_id}` only, omitting the REQUIRED `user_id` (OD-25) — stale since before OD-25's ruling. `api-contract.md` (the authoritative living mirror) already states the body correctly. | LOW | **FLAGGED, not fixed** — `phase-9-polish-and-documentation.md` is one of the nine LOCKED phase specs; CLAUDE.md forbids editing a locked spec during build work. The living mirror (`api-contract.md`) is correct and is what `contract_drift` verifies against; this is a stale example in a frozen document, not a contract gap. Recorded here rather than silently edited. |

**Needs-user:** R2-5 alone, if the user wants the locked spec's stale example corrected via a
deliberate, reviewed edit (CLAUDE.md §8) — not a build blocker; the authoritative living contract
is already correct.

**Phase 9's two sequential review gates are CLOSED. APPROVE.** Both ran one at a time (never in
parallel, per CLAUDE.md §11); R2 independently re-verified R1's claimed fix in the tree before
starting its own review. Every MEDIUM/LOW finding from both gates is now FIXED except R2-5, which is
correctly FLAGGED (a locked-spec edit, not a build action) rather than silently patched.

## Ledger-wide confirmation (Phase 9 CLOSEOUT, 2026-07-27) — needs-user is EMPTY across Phases 0-9.5

Every dated section above (Phase 0 through Phase 9.5, plus this CLOSEOUT) was re-scanned for its own
"Needs-user" line. Result: every single one reads **empty**, **none**, or a completed **RULED**
disposition — with exactly ONE deliberate exception:

- **R2-5** (§ "Review gates R1/R2" above) — a stale request-body example in
  `phase-9-polish-and-documentation.md`, one of the nine LOCKED phase specs. CLAUDE.md forbids
  editing a locked spec during build work; the authoritative living contract (`api-contract.md`,
  the document `contract_drift` actually verifies against) is already correct. This is intentionally
  left open for the user's own call on whether to spend a deliberate, reviewed edit (CLAUDE.md §8)
  on a frozen document's stale example — **not a build blocker, not a contract gap, not a defect any
  operator or UI team would ever hit** (they read `api-contract.md`, which is correct).

No other item across the entire ledger — OD-1 through OD-25, P4-OD-1, P5-OD-1..3, P6-OD-1..2,
P7-OD-1..8, P8-OD-1, P9.5-OD-1..6, P9-OD-1, P9-OD-2 — has an open needs-user. **The build has zero
outstanding decisions blocking launch.**

## Pre-live gates (required before the first LIVE run; NOT before Phase 1 build)

All consolidated in **`Docs/pre-live-gates.md`**. These are confirmed against the real shared
platform DB before the module first points at it — Phase 1 is implemented and proven in the
harness without them.

- **G1 / OD-1** — migration admin credential source (real admin role + secret store), distinct
  from `sender_module`.
- **G2 / OD-10** — platform avoids the reserved `9_000_000_000–9_999_999_999` advisory-lock band.
- **G3 / OD-8** — confirm real `public.users` / `public.job_items` COLUMN shapes. The Phase 0
  pre-flight checks table EXISTENCE only (a column-level check would break whenever the platform
  legitimately adds a column), so this is a going-live confirmation, not a harness blocker.
- **G4 / S15** — prod KMS key policy grants Decrypt to the Brain role only (sole-decryptor IAM).
  Phase 2 adds a paired precondition: the **admin service's** IAM grant is **Encrypt/DEK-wrap
  only, never Decrypt** (OD-7) — verified against the KMS key policy before Phase 3 ships.
- **G5 / OD (§0.1)** — the platform seeds the reserved `SENDER_SYSTEM_USER_ID` (all-zero UUID)
  row into `public.users` before shared-pool operations are used live. The module has SELECT-only
  on `public.users` and cannot seed it; the §0.1 pre-flight fails shared-pool creates with a
  clean `409` until the row exists, so this is a going-live coordination item, not a harness
  blocker (the behavioral suite seeds it via the admin connection to exercise the provisioned path).

## Status

Every Open Decision above is **RULED or APPLIED** — none is left open pending a build
decision. Phase 0, Phase 1 (data model & schema, `sender_0001..0012`), Phase 2 (account
management & operator admin API, `sender_0013..0014` + `internal/adminapi`), and Phase 3 (the
minimal send path — box↔Brain persistent mTLS stream, credential handoff + sole-decryptor scoping,
SMTP session on the box, idempotency, `quote`/`run`/`report`/`describe`; NO new migration —
schema-neutral) are all built and green on real infra (a real email is delivered to Mailpit).
Phase 2 surfaced two flagged items, both **RULED**: **OD-23** (the `account_events`
FK-vs-trigger contradiction) — resolved via ADR-0009 with no locked-spec edit — and **OD-24** (the
`daily_limit` ceiling referenced a never-seeded config key) — ruled option (c), a hardcoded sanity
bound (`DailyLimitCeiling = 100_000`), not a config key, not dropped. **Phase 3's §10 log was
closed before build; the review surfaced one genuine spec-vs-contract gap — OD-25** (the Domain-A
per-tenant isolation model), which the user **RULED option (b): module-enforced tenancy** and which
is now implemented (explicit `user_id` selector on `run`/`report`; every platform query scoped
`WHERE user_id=<tenant>`; cross-tenant access → 404-style not-found). **Phase 4's own §10 log
shipped with an empty needs-user list; build surfaced one genuine spec self-contradiction —
P4-OD-1** (the `one_by_one` routing strategy's literal `ORDER BY` does not produce its own
documented "exhaust one, then move on" behavior — verified empirically against real Postgres),
which the user **RULED: a deterministic `a.id ASC` tiebreak** (inert once Phase 5 gives accounts
distinct scores). Every Open Decision is now RULED or APPLIED; **needs-user is empty.**
**Phase 4's closeout (2026-07-24)** re-verified two claimed properties with real instrumentation
(genuine `pg_stat_activity`-observed concurrency, and confirmed `accounts_daily_sent_check` firing
zero times across repeated runs — both held as claimed, no weaker-than-believed finding) and
triaged every hardcoded timeout across Brain and the box: 11 safety bounds now explicitly
documented at their sites, and 3 operational dials flagged for their rightful owning phase (two
already planned by Phase 6/7; one genuine pre-existing gap — `heartbeat_interval_sec` was seeded
in Phase 1 but the box was never wired to read it, flagged for whichever phase owns box↔config
delivery). One CRITICAL interim risk is recorded explicitly, not papered over: an operator
lowering `job_max_processing_time_sec` below the box's hardcoded 30s SMTP timeout today would
create a live, currently-undetectable violation of the Phase-6-planned ordering invariant — closed
only when Phase 6 ships `smtp_operation_timeout_sec` + its config-load reject check. No new config
key was added by the closeout (`config_drift` stays 14); no locked spec was touched. The
remaining items are the five **pre-live confirmations** (G1–G5), gates on going live, not on
implementing a phase in the harness. Nothing here blocks the next phase from building on this
ground.
**Phase 5 (error taxonomy, account status & quarantine, `sender_0016..0017` + the classifier/
scoring/state-machine/exhaustion logic in `internal/dispatch`)** is built and green on real infra
(live harness 40/40, all 4 CI gates green, `config_drift` 14 → 17). Its spec §8 shipped with an
empty needs-user list; the four review gates (R1/R2 architect, R3/R4 code) each re-verified against
the working tree — R1/R2/R3 clean, **R4 found two real defects** on the account-flag write path (a
HIGH lost-update that could silently un-quarantine an account under concurrent outcomes, and a
MEDIUM consume CASE-order bug), both fixed and covered by a new two-connection regression test (the
naive locking fix's deadlock was itself caught by that concurrent test before shipping). **Phase 5's
closeout (2026-07-24)** verified the two carve-outs: (a) bounce-never-penalizes held, but the test
was under-specified — a paired positive control was added so it can no longer pass vacuously; (b)
the wait-vs-fail ordering (empty→WAIT and self-recovering→WAIT strictly before the terminal-fail
branch) was verified real and well-covered, no change needed. The closeout also recorded a standing
requirement (**ADR-0010**): every phase that mutates `sender.accounts` state under an outcome/
consume/claim path ships a genuine two-connection concurrency-race test, and any locking fix is
itself checked for deadlock with a two-connection test — because account-state mutation under
concurrent outcomes is the recurring defect surface (Phase 4's double-book and Phase 5's
lost-update un-quarantine + its fix's deadlock all live there). Needs-user remains **empty**; no
locked spec was touched by Phase 5 or its closeout.

**Phase 6 (pacing & dead-box detection, `sender_0018` + the pacing/reclaim logic in
`internal/dispatch`)** is built and green on real infra (live harness 40/40, all 4 CI gates green,
`config_drift` 17 → 19). Its spec §7 shipped with an empty needs-user list; build recorded P6-OD-1
(the config-ordering reject-at-load vs warn-at-write reconciliation — no real conflict; the Phase 4
closeout had pre-committed the load-time reject check) and P6-OD-2 (additive box-timeout delivery
via `push_job.smtp.operation_timeout_sec`). The four review gates (R1/R2 architect, R3/R4 code) each
re-verified against the working tree — R1/R2/R3 clean, **R4 found one real HIGH** (P6-R4-1): Detector
A's bulk reclaim UPDATE was job→account (its `job_events` trigger takes the account FK KEY SHARE
after the job row), cycling `40P01` with `applyClassifiedFailure`'s account→job order — the third
appearance of the ADR-0010 defect class its own §3 predicted for the reclaim loop. Fixed by routing
both detectors through one shared `reclaimJobLocked` that locks account-first (mirroring the outcome
handler) then job `FOR UPDATE SKIP LOCKED`; covered by a new two-connection regression test driving
the exact combination the original test missed. **Phase 6's closeout (2026-07-25)** promoted the
account→job lock ordering to a codebase-wide invariant (CLAUDE.md Rule 9, ADR-0010 with a full path
table, DoD-2) — honestly scoped to *exclusive* account locks after enumerating that three job-first
paths (`applyAccepted`/`applyBounced`/`markSending`) are safe without pre-locking (they take no
exclusive account lock; only the trigger's KEY SHARE), confirmed by new two-connection audit tests;
and confirmed the reclaim-does-not-score carve-out is non-vacuous (paired box-reported-timeout
positive control + no-cascade-quarantine assertion). Two forward-looking Phase-9.5 wiring reminders
are recorded (alert on a runtime `config_ordering_violation`; gate `ReclaimRunner` with
`dispatch_enabled`) — reminders, not open decisions. Needs-user remains **empty**; no locked spec
was touched by Phase 6 or its closeout.

**Phase 7's review gates (2026-07-25) changed the needs-user line for the first time since Phase 4.**
R1 (architect) produced P7-OD-3/P7-OD-4 (both resolved, needs-user none). R2 (architect) produced
**P7-OD-5** (scope: §5.3's live-`PUT` reject is owned by Phase 9 §4.6/E4, so Phase 7 deliberately ships
only the config-LOAD half — flagged, not built ahead) and **P7-OD-6**, whose first half is FIXED (a
config-ordering violation is no longer misclassified as a DB outage) and whose second half is
**NEEDS-USER**: the Brain has no per-attempt identity on the report path, so a stale replay from a
superseded attempt of the same job on the SAME box is indistinguishable from the live attempt's report.
The box-side mitigations shipped in Phase 7; the Brain-side fix is a contract+schema decision and Phase 7
is schema-neutral. R4 (code review) produced **P7-OD-7** (the day-rollup's bucket boundary follows the
implicit SESSION timezone while every other day-boundary path pins `daily_reset_timezone` explicitly —
a contract question, and `SummarizeByPeriod` has no production caller yet, so it is filed rather than
guessed at). **Both were RULED on 2026-07-26 in the closeout:** P7-OD-7 → follow `daily_reset_timezone`
(implemented + tested here); P7-OD-6 → RULED-ADOPT, persist per-attempt identity, **owner Phase 8**
(residual risk accepted for Phase 7, which stays schema-neutral) — **now APPLIED in Phase 8 Chunk 1
(2026-07-26): `sender_0020` adds `sender.jobs.attempt_token` and `OutcomeHandler.Handle` rejects a
provable token mismatch without mutating state or scoring the account.** **Needs-user is therefore EMPTY
again** — every Phase 7 item is RULED or APPLIED. No locked spec was edited by Phase 7, its gates, or
its closeout. The closeout also appended CLAUDE.md **Rule 10** (a concurrency test must PROVE it
contended) and a Rule 9 sub-point recording two Postgres semantics (an unchanged `state` fires no
trigger; an unchanged FK key takes no `KEY SHARE`) — see the ADR-0010 amendment. It additionally FLAGGED
**P7-OD-8** (`accounts.daily_reset_day DEFAULT CURRENT_DATE` uses the server date, not
`daily_reset_timezone`) — surfaced by a test failure, low severity under the shipped UTC default, no
ruling required to proceed.

**Phase 8 (content variation & header engineering, 2026-07-26) is built, reviewed, and closed.** All
six chunks landed (per-attempt identity, the variation-layer skeleton + identity-function guarantee,
body/content toggles, encoding/header toggles + deterministic seeding, config seeds + Brain wiring,
and the full closeout suite). Four review gates (R1–R4) surfaced **one HIGH finding** (the admin
config surface's numeric value_type gates accepted a quoted-numeric-string write, which would
silently wedge every subsequent Brain polling tick) and **three MEDIUM findings** (a QP
trailing-whitespace RFC 2045 gap; an unscoped `pg_stat_activity` contention check; a concurrency test
whose direction (b) never actually called the function its doc comment claimed to test) — all four
fixed. Phase 8 also surfaced **P8-OD-1**, filed and **RULED-WIDEN**: three toggle parameters were
promoted from fixed-in-code values to independent `sender.config` dials, growing `sender_0020` from
14 to 17 keys and additively amending TWO locked specs (this document's own §4.5 in
`phase-9-polish-and-documentation.md`, and `phase-8-content-variation.md`'s own "Post-Chunk-5
amendment" section) — see P8-OD-1 below for the full disposition. A dedicated CLOSEOUT pass
(2026-07-26) then found that the four review-gate fixes had been committed in parallel with NO
independent re-verification (the gates themselves ran concurrently, so no gate could check another's
fix) — one of the four ("fixed") findings turned out to be only half-applied. All four are now
independently re-verified in the tree; the standing lesson (review gates run sequentially, each pass
re-verifying every earlier pass's claimed fix) is recorded as CLAUDE.md **Rule 11** and DoD-2. The
closeout also found two claims from the original Phase 8 mandate that were weaker than stated: the
forced-interleave stale-replay test never asserted account-non-scoring (only a separate, non-racing
test did) — FIXED, now asserted under the forced interleave too; and no test anywhere proves "exactly
one message reaches Mailpit" for the stale-replay-vs-live-attempt scenario specifically — FLAGGED,
not built (the property IS proven at the Postgres/state layer, which is the layer that actually
decides whether a duplicate send occurs; a live-Mailpit version of this exact scenario has no clean
deterministic realization with the harness's current primitives without either violating the
single-registration invariant or introducing a hoped-for race Rule 10 would then require re-litigating).
**Needs-user: empty** — every Phase 8 item is RULED, APPLIED, or a recorded, non-blocking FLAG. No
locked spec was edited; both amendments were additive per CLAUDE.md §8.

**Phase 9.5 (operability & control-plane safety, 2026-07-26) is built and closed.** All five
chunks landed (B1 the global dispatch panic switch, B2 the shadow-quarantine ladder + auto-pause/
variation kill-switches, B3 the read-only `/v1/admin/health` endpoint, B4 minimal alerting incl. the
optional two-vocabulary-clean webhook, B5 re-verification + B6/E11 the single-Brain throughput-
ceiling load test). Two items were filed rather than silently resolved: **P9.5-OD-1** (the build
mandate's own instruction to gate `ReclaimRunner` on `dispatch_enabled` directly contradicted the
spec's own §1.2 text and its O1 test) — **RULED: follow the spec, reclaim is NOT gated** — and
**P9.5-OD-2** (a genuine pre-existing bug found while wiring B2: `OutcomeHandler.cfg` was captured
once at construction and never refreshed, so several already-seeded Phase 5/6/7 dials were silently
stale-since-boot on the report-handling path) — **RULED: thread fresh config through the whole call
chain**, fixing every dial on that path at once. B5 (the Brain singleton) was confirmed
ALREADY BUILT in Phase 0, not rebuilt — re-verified live with a genuine second Brain process. B6/E11
shipped as `tests/harness/load_e11.sh`, recording a real measured ceiling (46→194→238→370→322
sends/min across a 6-box ramp) in the new `Docs/load-test-results.md` per **OD-7's ruling**
(committed, versioned doc artifact). The full clean-slate harness ran **52/53 green** — the one
failure is the documented, pre-existing Phase 8 `comment_injection` intermittent (unrelated to and
unaffected by this phase; that test file is locked). **Needs-user: empty** — both P9.5-OD-1 and
P9.5-OD-2 are RULED and implemented; no locked spec was edited (only additive config seeds, one
read-only route, one optional webhook, per the spec's own §10 confirmation).

### P7-OD-3 — the two bounds R1 required had no seeded key (RESOLVED without inventing one)

**Raised by:** review gate R1 (architect), Phase 7 closeout.

Two fixes R1 demanded needed a knob the phase does not seed:

1. a **replay cadence** for §2.3's live-stream replay ("keeps replaying until it receives a
   post-commit ack" — with the stream up there is no reconnect to trigger a replay), and
2. the **max entry count** that §1.3 names as the second half of the buffer's bound.

Phase 7 §5 seeds exactly six keys and neither of these is among them, so adding either as a config key
would mean inventing a seventh key and a §4 reference entry the locked spec does not contain.

**Resolved as follows, with no new key and no hardcoded tuning constant:**

- The replay cadence reuses the **existing** `heartbeat_interval_sec` — already seeded, already
  delivered to the box, and already the box's "prove liveness on a schedule" tick. Replay is idempotent
  by construction (§1.3: pre-persist idempotency + `provider_message_id UNIQUE`), so piggybacking it is
  a no-op once the outcome is persisted. No new tunable is introduced, so config-not-code's "new
  tunable ⇒ new seeded key" is satisfied by having no new tunable at all.
- The max entry count is a **structural safety bound** (1024), in the same category as the box's
  30-attempt connect budget: it is not an operator dial and it does not govern steady-state behavior
  (a box holds at most one in-flight job, so >1 buffered entry already means outcomes are stacking up
  against an unreachable Brain, and the TTL is the bound that fires in practice). Its consequence is
  the one the spec names for overflow: the outcome is dropped and the job falls back to the continuous
  reclaim loop.

**Needs-user:** none. Recorded because the reasoning — why neither became a config key — is the part
worth auditing, not the values.

### P7-OD-4 — stale-replay mis-attribution: guard added, and why it acks rather than retains

**Raised by:** review gate R1 (architect), Phase 7 closeout.

The buffer TTL (900s) deliberately outlives a job lease (120s), so an outcome can outlive its own
attempt: the job gets reclaimed and re-dispatched, possibly to another box, and the original box's
replay then lands on a *different* attempt that is legitimately `sending`. Measured on real Postgres,
applying it requeues the live attempt, burns one of its retries, writes a phantom `send_logs` row, and
drops the account score 100 → 63 for a failure that never happened.

Two sub-decisions, both taken conservatively rather than by preference:

- **The guard rejects only a PROVABLE mismatch** (`assigned_box_id` and the reporting box both known
  and different). A NULL `assigned_box_id` — claimed but never pushed, or assignment cleared by a
  requeue — is NOT treated as a mismatch, so the guard can never reject a legitimate outcome; those
  cases stay with the apply paths' existing `state='sending'` guards.
- **A stale report is ACKED (`resolved=true`), not retained.** It can never become applicable (the job
  has moved to a new attempt whose outcome the Brain will record), so withholding the ack would make the
  box replay a permanently-dead outcome until its TTL for no benefit. Acking here does not weaken the
  persist-ack contract: that contract exists so the box does not drop an outcome the Brain FAILED to
  persist — this is an outcome the Brain has correctly determined it must not persist at all.

**Needs-user:** none.

### P7-OD-5 — §5.3's config_ordering_violation on a live `PUT` is OWNED BY PHASE 9 (deliberately not built here)

**Raised by:** review gate R2 (architect), Phase 7 closeout.

Phase 7 §5.3 states the P7-11 invariant is enforced with external code `config_ordering_violation`
without naming an enforcement point. Phase 7 implements the **config-LOAD** half only
(`validateConfigOrdering` in `LoadConfig`, zeroed Config on reject, never a clamp). The admin
**write-path** half — `PUT /v1/admin/config/{key}` refusing a value that would break the pair — is
**not** implemented, and that is a scope decision, not an omission:

- `Docs/phase-9-polish-and-documentation.md` **§4.6** is the authoritative statement of both ordering
  invariants for the admin surface; its **error-code table** lists `config_ordering_violation` with the
  trigger "A config `PUT` would break invariant §4.6"; its **endpoint table** maps the config `PUT`
  route to "invariant checks (§4.6) → `config_ordering_violation`"; and Phase 9 test **E4** is exactly
  that assertion, for **both** pairs, including the clean-envelope check.
- Building it in Phase 7 would therefore be out-of-phase work (the mandate: no Phase-9 polish, flag
  instead of building ahead), and would land the behavior without Phase 9's envelope/logging test.

This is the same load-vs-write split P6-OD-1 already reconciled for the P6-11 pair (reject at LOAD;
the write path validates one key at a time and is left uncoupled until the phase that owns it). P7-OD-5
records that the split now applies to P7-11 as well, so the divergence from §5.3's unqualified sentence
is on the record rather than silently resolved.

**Consequence while it is unbuilt (and why it is safe):** an operator CAN `PUT` a violating value. The
Brain then REFUSES to run on it — `LoadConfig` rejects, every loop's tick errors on the normal cadence
and claims nothing, and (see below) it is correctly reported as a configuration fault, not a DB outage.
The unsafe pair can never take effect; the cost is that the rejection is discovered at the next tick
rather than at the `PUT`. No duplicate, no invented outcome.

**Needs-user:** none — this is a scope boundary, not a contradiction. It closes when Phase 9 ships §4.6
+ E4.

### P7-OD-6 — a config-ordering violation is NOT a DB outage (fixed), and Brain-side ATTEMPT identity — **RULED-ADOPT 2026-07-26, owner: PHASE 8 → APPLIED 2026-07-26 (Phase 8 Chunk 1)**

**Raised by:** review gate R2 (architect), Phase 7 closeout.

**Fixed in Phase 7.** `IsOutage` defaults UNKNOWN errors to "outage" (the safe direction for transport
faults), which swept up `*configOrderingError` — an error raised *after* a completely successful read
from a completely healthy DB. Consequences, all real: a false `db outage DETECTED` line naming a healthy
DB; the halt then cleared by the next tick's successful `SELECT 1` probe, which resets `failures` to 0,
so the §2.2 bounded exponential ladder could never grow and all three loops would flap
DETECTED/RESOLVED at the 500ms base forever, burying the one line an operator needs (the offending
pair). `IsOutage` now returns false for it, routing it to the callers' application-error branch: log
and retry on the normal cadence, with the loops still correctly declining to act (a rejected load hands
back a ZEROED Config, so nothing runs on bad values). Covered by
`TestConfigOrdering_IsNotADBOutage` — the real dispatch Runner over a real violating row, asserting no
outage line, no halt, nothing claimed, then an anti-vacuity resume once the pair is fixed and a real
severed-connection positive control proving the log assertions can fail.

**THE DESIGN GAP (was OPEN; ruled below, now owned by Phase 8): the Brain has no ATTEMPT identity on
the report path.** The §1.3
attribution guard added by R1 rejects a stale replay only when the reporting box differs from
`assigned_box_id`. When a reclaimed job is re-dispatched to the **SAME** box, a late replay from the
superseded attempt is indistinguishable from the live attempt's report and WILL be applied. The wire
already carries what would be needed — `push_job.request_id` is minted per attempt and echoed in
`report.request_id` — but the Brain neither reads it in `OutcomeHandler.Handle` nor persists it, and
`sender.jobs` has no per-attempt identity column (only `attempt_count`/`max_attempts`). A real fix is a
CONTRACT + SCHEMA decision (persist the in-flight attempt's `request_id`, or add an attempt token, both
additive-only per Rule 5) and Phase 7 is deliberately **schema-neutral** (`sender_0019` seeds config
only, no `sender_0020`). Per Rule 2 this is filed, not invented.

**What Phase 7 DID fix on the box side** (both no-schema, both covered by new tests in
`services/box/test/buffer.test.mjs`, each with a paired control):

- A job pushed AGAIN to the same box no longer fails to buffer the LIVE attempt's outcome. The buffer is
  keyed by `job_id`, so the stale entry previously blocked it entirely — a lost send would then replay
  the dead attempt's result forever and the real outcome would be gone. The live attempt now REPLACES
  the entry (fresh TTL anchor) and the superseded one is discarded and counted.
- A `report_ack` echoing a DIFFERENT `request_id` than the buffered entry no longer drops that entry.
  Acking a superseded attempt would otherwise stop the replay of an outcome the Brain never committed —
  a silent loss, the precise failure P7-4 exists to prevent.

**Residual risk while the Brain-side half is open:** narrow but real — it needs an ack lost in flight,
a lease expiry, AND the re-dispatch landing on the same box. Its blast radius is one job's scoring/state
being attributed to the wrong attempt of the SAME job on the SAME box; it can never produce a second
SMTP send (that is bounded by `provider_message_id UNIQUE` + the pre-persist idempotency check).

**RULING (2026-07-26, user): RULED-ADOPT — persist per-attempt identity; OWNER is PHASE 8.** The
residual risk is accepted for the duration of Phase 7 (unchanged from the assessment above: it needs a
lost ack AND a lease expiry AND the re-dispatch landing on the same box, and it can never cause a second
SMTP send), with the box-side mitigations already shipped. Phase 7 remains schema-neutral; the additive
column/contract change and the same-box replay guard are Phase 8's to build, per the Phase 8 kickoff.

**Carried into Phase 8 as an inherited obligation** (so it is not re-litigated there):
- The wire already carries the identity — `push_job.request_id` is minted per attempt and echoed in
  `report.request_id`. Phase 8 reads and persists it (additive-only, Rule 5); it does not redesign it.
- The guard belongs in `OutcomeHandler.Handle`, alongside the existing different-box attribution check.
- Its test must cover the SAME-box case, which `staleattr_test.go` explicitly documents as the uncovered
  half (that scope comment is the pointer to this item and should be updated when Phase 8 closes it).

**Needs-user:** none (ruled). Tracking moves to Phase 8.

**FLAG (for the user, not a blocker):** no Phase 8 kickoff artifact exists in the tree yet — `Docs/`
holds `phase-8-content-variation.md` (the locked spec, untouched) but no kickoff/scope document, so this
ruling is recorded here on the user's instruction and this ledger is currently its only home. When the
Phase 8 kickoff lands, that document must carry this inherited obligation explicitly, or the ruling
lives only in a Phase 7 section and is easy to miss.

**APPLIED 2026-07-26 — Phase 8 CHUNK 1. The Brain-side half is BUILT; this item is DISCHARGED.** What
shipped, and where the two inherited obligations landed:

- **Migration `sender_0020`** adds `sender.jobs.attempt_token BIGINT NOT NULL DEFAULT 0` — additive-only
  (Rule 5), metadata-only on PG 11+, no CHECK constraint (that would force a validating scan for no
  benefit), and no new GRANT needed: it lands on `sender.jobs`, on which `sender_module` already holds
  table-level DML from `sender_0012`, so the ADR-0008 grants-invariant test stays green. The `.down.sql`
  drops it (re-opening the hole but never causing a false rejection). Existing rows read `0`.
- **The identity is a TOKEN, not `request_id`.** The inherited obligation said the wire already carried a
  per-attempt value (`push_job.request_id`) and Phase 8 should read and persist it. Phase 8 instead
  persists a Brain-minted monotonic counter and carries it as an additive field alongside `request_id`.
  Rationale (a deliberate strengthening, not a redesign): the token is minted **inside** the guarded
  `assigned → sending` UPDATE with `RETURNING`, so it is minted exactly once per attempt, is
  unforgeable by the box, and — being a stored column — can be re-asserted **inside** each apply
  statement, which a value living only in memory on the stream cannot. `request_id` remains exactly what
  it was (per-attempt request correlation, echoed in the report and in `report_ack`); nothing was
  renamed, repurposed, or removed. `attempt_count` was NOT used: Phase 5 made it SATURATE at
  `max_attempts` while requeue is unbounded until pool exhaustion, so long-lived jobs reuse the value —
  asserted as a contrast, not assumed, in `TestAttemptToken_MonotonicAndNonSaturating`.
- **The guard is in `OutcomeHandler.Handle`, alongside the different-box attribution check** — the
  second obligation, honored literally. It rejects only a PROVABLE mismatch (both tokens non-zero and
  different): log + `resolved=true` (so the box drops a permanently-inapplicable outcome instead of
  replaying it until TTL) and apply NOTHING. Because that pre-check is a read, the same predicate is
  ALSO carried inside every apply statement (`applyAccepted`, `applyBounced`, and both statements of
  `applyClassifiedFailure`), so a token that moves between the check and the apply tx makes the UPDATE
  match zero rows → a clean acked no-op. No state change, no `send_logs` row, no `job_events` row, no
  account scoring.
- **The SAME-box case is covered, and `staleattr_test.go`'s scope comment now points at that coverage**
  (the third obligation). `internal/dispatch/attempttoken_test.go`: mint-per-dispatch + on-the-wire
  agreement, monotonic-vs-saturating with anti-vacuity, the headline same-box stale-replay rejection
  (both a stale ACCEPTED and a stale FAILURE, with the box-identity and new-token preconditions asserted
  so it cannot pass vacuously, plus a positive control proving the live attempt still applies in full),
  and absent-token compatibility in BOTH directions (old box, old job row) so the guard can never
  discard a legitimate outcome. `internal/dispatch/attempttoken_lockorder_test.go` adds the
  Rule 10-compliant concurrency half: a FORCED interleave that holds the token bump uncommitted so the
  stale replay's pre-check reads a matching token, blocks on the job row (contention asserted via
  `pg_stat_activity`), and is then defeated by the in-UPDATE predicate — verified non-vacuous by a
  hand-run negative control (tautologising `applyAccepted`'s predicate makes it fail with
  `job state=sent`) — plus both lock-order directions against a held exclusive account `FOR UPDATE`,
  proving no `40P01` (Rule 9 / ADR-0010; `markSending` still takes no exclusive account lock, so it can
  only ever be the waiter).
- **The box's half is echo-only**, asserted on the real `BoxStream` over a real socket in
  `services/box/test/buffer.test.mjs`: the token is echoed VERBATIM in all four report shapes, a later
  attempt reports its OWN token (not a cached one), and an ABSENT token stays absent — the box never
  mints, defaults, or increments an attempt identity.
- Docs updated in the same change: the `attempt_token` frame contract in `Docs/api-contract.md`, and the
  column in `Docs/data-model.md`.

**Residual, unchanged and still accepted:** the crash-after-`250` window (the box dies after the SMTP
server accepted but before it can report) remains unavoidable and is NOT what this fixed — that one is
bounded by `provider_message_id UNIQUE` + the pre-persist idempotency check. What this closed is the
*fixable* half: a superseded attempt's buffered outcome being applied to a live one.

### P7-OD-7 — the day-rollup's bucket boundary depends on the SESSION timezone, not on config — **RULED 2026-07-26: FOLLOW `daily_reset_timezone`**

**Raised by:** review gate R4 (code review), Phase 7 closeout.

`SummarizeByPeriod` buckets with `date_trunc('<granularity>', created_at)` on `sender.job_events.created_at`,
which is a `TIMESTAMPTZ` (`sender_0008`). For a `TIMESTAMPTZ`, `date_trunc/2` truncates in the **session**
`TimeZone` — and nothing in `internal/db` sets one, so the boundary is whatever the server default or the
DSN happens to supply. Demonstrated on the live instance: `2026-07-25 02:30:00+00` buckets to `2026-07-25`
under UTC but to `2026-07-24` under `America/New_York` — a whole day's shift in every reported figure.

This is an inconsistency with the module's own established discipline, which is why it is filed rather than
shrugged off: **every other** day-boundary in this codebase pins the zone from config explicitly —
`eligibility.go`, `account_score.go`, `exhaustion.go` all compute `(NOW() AT TIME ZONE $n)::date` using
`daily_reset_timezone`. In Rule-1 terms the rollup currently depends on an implicit environment constant
where the rest of the module depends on a seeded key.

**Why it is NOT fixed here (Rule 2):** what a "per-day summary" *means* to a reporting caller is a contract
decision with three defensible answers — follow `daily_reset_timezone` (agrees with the daily-limit and
daily-reset accounting the operator already sees), pin explicit UTC (stable, but disagrees with that
accounting), or seed a new reporting-timezone key (which Phase 7 §5 does not authorize — it seeds exactly
six). Picking one silently would be inventing an answer to a contract question.

**Why it is MEDIUM, not HIGH:** `SummarizeByPeriod` has **no production caller** today — grep across
`cmd/`, `httpapi`, `platformapi` and `adminapi` finds none; it is reachable only from `retention_test.go`.
Nothing an operator or tenant can currently see is wrong. It must be pinned **before** any phase wires a
reporting endpoint to it.

Also noted by R4 and deliberately not built: `rollup_granularity` is validated at READ time by
`truncFieldFor`'s closed four-literal allow-list (which is also what makes the `fmt.Sprintf` injection-proof
— verified: no other interpolation path, and `user_id`/`from`/`to` are bound parameters), but the admin
`PUT` accepts an out-of-list value and the failure only surfaces later at summary time. That is the same
write-path validation gap P7-OD-5 already assigns to Phase 9 §4.6, so it is recorded there, not built here.

**RULING (2026-07-26, user): FOLLOW `daily_reset_timezone`.** Not UTC, and no new config key.

*Reasoning of record.* The system already has ONE authoritative definition of "a day", and that
definition governs daily limits, account scoring and pool exhaustion. A reported day whose boundaries
did not match the capacity-reset boundaries would show operators **partial days of capacity** — a day
showing 40 of a 100/day limit consumed because the other 60 fell in the report's neighbouring bucket.
Consistency with the existing definition beats introducing a second one. This also keeps the boundary
config-driven (Rule 1) instead of inherited from an implicit session/server setting.

**Implemented** in `retention.go` `SummarizeByPeriod`: `date_trunc('<field>', created_at AT TIME ZONE $4)`
with `cfg.DailyResetTimezone` as a **bound parameter** (the granularity stays the allow-listed
interpolation; the zone is never interpolated). `created_at AT TIME ZONE $tz` converts the `TIMESTAMPTZ`
to local wall-clock `TIMESTAMP` in that zone, and `date_trunc` over a plain `TIMESTAMP` is
zone-independent — so the boundary is pinned by config and cannot be inherited from the session.

`DaySummary.Bucket` is therefore **wall-clock time in `daily_reset_timezone`, not UTC** — documented on
the struct field, because a caller that re-labels it as UTC would reintroduce the same off-by-a-day.

**Standing note for any FUTURE reporting endpoint.** `SummarizeByPeriod` still has no production caller,
but its day definition is now pinned, and **any endpoint wired to it inherits that definition** — a
reported day is a `daily_reset_timezone` day. A future phase that wants a different reporting zone must
raise a new Open Decision and seed a key; it must not change this call site silently, because that would
re-desynchronize reports from capacity accounting.

**Tests** (`internal/dispatch/rollup_timezone_test.go`, real Postgres, real dispatch + real outcome path):
`TestRollup_BucketsFollowDailyResetTimezone` picks a zone whose local date differs from UTC's *at the hour
the suite runs* (Pacific/Midway before 11:00 UTC, else Pacific/Kiritimati — one always applies), drives a
real send, then asserts the summary bucket equals the `sender.accounts.daily_reset_day` the **real consume
path** stamped, and explicitly asserts the bucket is NOT the UTC day.
`TestRollup_ChangingDailyResetTimezoneShiftsBuckets` reads ONE `job_events` row at `02:30 UTC` under two
configs and asserts the bucket moves UTC-day → New-York-day (R4's exact demonstrated shift), with a paired
control that the UTC day is *gone* from the second read and the row total is conserved. Both were verified
to **FAIL** against the previous unpinned query (`no summary bucket for 2026-07-26 … buckets are
map[2026-07-25:4]`, and `bucketed as map[2026-07-24:1], want {2026-07-23:1}`).

**Needs-user:** none (ruled). P7-OD-6 was the one open item; it is **APPLIED as of 2026-07-26** (Phase 8
Chunk 1 — `sender_0020` + the `attempt_token` guard). P7-OD-8 below remains filed, not fixed.

### P7-OD-8 — `accounts.daily_reset_day DEFAULT CURRENT_DATE` uses the SERVER date, not `daily_reset_timezone` (FLAG, low severity)

**Raised by:** Phase 7 closeout Chunk 1, surfaced by a test failure rather than by inspection — which is
why it is worth recording.

While proving the P7-OD-7 ruling, `TestRollup_BucketsFollowDailyResetTimezone` failed its own
precondition (`state=queued, want sending`) under a zone WEST of the server. Cause: `sender_0005`
declares `daily_reset_day DATE NOT NULL DEFAULT CURRENT_DATE` — the **server's** date — while the
eligibility predicate compares that column against `(NOW() AT TIME ZONE daily_reset_timezone)::date`.
For a west-of-server zone a freshly-inserted account is born with `daily_reset_day` a day AHEAD of
today-in-tz, so neither eligibility branch matches (`<` fails, `=` fails) and the account is invisible
to dispatch until the server date catches up — up to ~a day of a brand-new account being silently
unusable.

**Why this is LOW and not a Phase 7 defect:** the seeded `daily_reset_timezone` is `UTC` and the
containers run UTC, so `CURRENT_DATE` and today-in-tz agree in every environment this module ships in
today. It only bites if an operator sets a west-of-UTC zone. The runtime is also self-healing in the
other direction: a STALE `daily_reset_day` is handled by the §2.1 lazy reset, which re-stamps it from
the config tz — so once an account transacts, the column is correct. Only the INITIAL default is wrong,
and only for a west-of-server zone.

**Not fixed here (Rule 2 + scope).** A fix is a migration changing a column default (Phase 7 is
deliberately schema-neutral, and the DEFAULT would have to read a config row, which a column default
cannot do — the honest fix is for the account-create path to compute the value, which is Phase 2's
surface, not Phase 7's). Flagging rather than reaching into another phase's write path during a closeout.

The test works around it explicitly and says so in a comment: it ages `daily_reset_day` so the §2.1
self-heal path runs, which is also the path that stamps the value the assertion then checks.

**Needs-user:** NO ruling required to proceed — recorded so a future phase (or the first operator who
sets a non-UTC zone) does not re-derive it. Worth pinning if account-create is next touched.

### P8-OD-1 — the execution mandate's "seed the intensities/rates too" collides with the LOCKED 14-key count — **RULED-WIDEN 2026-07-26**

**Raised by:** Phase 8 Chunk 5 (config seeds), surfaced while reconciling the current build mandate
against the closed spec set before writing `sender_0020`'s config-seed INSERTs.

**The contradiction.** The Chunk 5 execution instruction reads: *"Seed the toggle keys AND their
INTENSITIES/RATES (comment-injection rate, ZWJ rate, CSS-variation magnitude, etc.) as config with
documented defaults — an on/off toggle whose strength is hardcoded is only half a dial."* Taken
literally this adds new keys beyond the toggle set: a ZWJ-rate dial, a CSS-magnitude dial, a
UA-injection-rate dial (currently `ZWJ_RATE = 0.05`, the CSS `{-1,0,+1}` spread, and
`UA_INJECT_RATE = 0.3` in `services/box/src/variation-content.js` / `variation-headers.js`).

But `Docs/phase-8-content-variation.md` §6.1/§6.2 (LOCKED, closed, must not be edited per this
repo's operating rule) is explicit and enumerated: **"`sender_0020` seeds the FOURTEEN keys below
(twelve toggles + two toggle-parameters)."** Only `variation_comment_intensity` (an enum,
low/medium/high, whose 5/15/30% rates are fixed IN THE SPEC'S OWN TEXT for toggle 2) and
`variation_preheader_templates` are toggle-parameters; toggles 3 (CSS), 5 (ZWJ), and 10 (UA) are
listed as plain booleans with no accompanying rate/magnitude key, and their rates/magnitudes are
stated as fixed values in the toggle's own prose (toggle 3: "±1px... skips values <3px"; toggle 5:
"~5% of word-boundary spaces"; toggle 10 borrows the reference's "~70%-omission... reproducing the
reference's ~70%-omission behavior deterministically per job" — no key named for it anywhere in §5
or §6). `Docs/phase-9-polish-and-documentation.md` §4.5 (also locked) independently confirms the same
fourteen-key enumeration for `sender_0020` and treats it as closed/complete ("all default OFF/neutral
... See Phase 8 for per-toggle behavior; consolidated here for completeness") — it lists no
intensity/rate keys beyond `variation_comment_intensity`. Adding three more keys would grow Phase 8
from 14 to 17 and is a **locked-spec contradiction**, not a simple scope note: per CLAUDE.md Rule 2
("Never resolve a locked contradiction on your own authority") this is filed rather than silently
implemented either way.

**Disposition — proceed on the locked-spec-compliant side, flag the mandate's broader ask.**
`sender_0020`'s config seeds ship **exactly the fourteen keys** the locked spec enumerates (§6.2,
verbatim) — no ZWJ-rate, CSS-magnitude, or UA-rate key is added. The three currently-hardcoded
values (`ZWJ_RATE`, the CSS ±1 spread, `UA_INJECT_RATE`) remain literal constants in the box code,
each already documented in place as "a fixed toggle behavior per the locked spec's own text, not a
further config key" (see the comments already at their definition sites in
`variation-content.js`/`variation-headers.js`, Chunks 3–4). This keeps the Phase 8/Phase 9 §4.5 key
counts mutually consistent and the migration chain contiguous.

**What would change this:** if the user rules that the intensities/rates SHOULD become independent
dials, that is a genuine additive amendment to the closed Phase 8 spec (§6.1/§6.2) and to Phase 9
§4.5's consolidated table — both would need a deliberate, reviewed edit (CLAUDE.md §8: "Newly locked
invariants are APPENDED... never quietly rewritten"), and the box code would read them via the same
`push_job.variation` snapshot mechanism rather than a literal constant. Not built speculatively here.

**Needs-user:** yes — a ruling on whether toggles 3/5/10's rates should become independent
`sender.config` dials (widening Phase 8 to 17 keys and amending the two locked specs above) or stay
fixed-in-code as the current spec text already specifies. Proceeding on the locked-spec-compliant
14-key path in the meantime; this does not block any other Chunk 5/6 work.

**RULED (2026-07-26): WIDEN.** The user ruled to promote all three to independent config dials —
an on/off toggle whose strength is hardcoded is only half a dial (config-not-code, CLAUDE.md Rule
1). This is the additive amendment path this entry already anticipated, now executed:

- **`Docs/phase-8-content-variation.md`** — a new "Post-Chunk-5 amendment (2026-07-26, RULED)"
  section is APPENDED (not a rewrite) recording the ruling, adding
  `variation_css_magnitude_px` (default `1`, integer), `variation_zero_width_rate` (default
  `0.05`, number), and `variation_user_agent_rate` (default `0.3`, number) to §6.1's table and
  §6.2's seed SQL, and updating toggles 3/5/10's own prose to name the new parameter each rate now
  comes from. `sender_0020` grows from fourteen keys to **seventeen**.
- **`Docs/phase-9-polish-and-documentation.md`** §4.5's heading and table are amended in place with
  an explicit "amended post-Chunk-5, 14→17 keys" note (CLAUDE.md §8 — appended/dated, not silently
  rewritten); the module-wide count references in §4/§5.6 move 39→42 (and 39.5's forward-pointer
  moves 50→53 accordingly).
- **Migration:** the three new rows are appended to the SAME `sender_0020_jobs_attempt_token.up.sql`
  /`.down.sql` pair Chunk 5 already used (same rationale as the original fourteen — the version
  number is spent). `sender.jobs` gets no new column; this is config-only.
- **Brain (`config.go`/`variation.go`):** three new `Config` fields
  (`VariationCSSMagnitudePx int`, `VariationZeroWidthRate float64`,
  `VariationUserAgentRate float64`), two new readers (`readFloatKey` alongside the existing
  `readIntKey`), and `variationSnapshot(cfg)` extended to copy them into the wire shape.
- **Box (`variation-content.js`/`variation-headers.js`):** `applyCssVariation`, `insertZeroWidth`,
  and `maybeInjectUserAgent` now take the rate/magnitude as a parameter (threaded through
  `resolveVariation`'s toggle set) instead of reading the module-level `ZWJ_RATE`/`UA_INJECT_RATE`
  constants / the literal `{-1,0,+1}` spread — those constants are removed from the box, not merely
  overridden, so there is exactly one source of truth (config-not-code, no shadow default).
- **Admin API (`config_validate.go`):** a `"number"` `value_type` case (JSON float, Phase 8's first
  non-integer numeric key) plus semantic bounds (`variation_zero_width_rate`/
  `variation_user_agent_rate` ∈ `[0, 1]`; `variation_css_magnitude_px` ≥ `1`).
- **Config docs:** `config-manifest.txt` and `config-reference.md`'s Phase 8 section amended in
  place with the three new keys and the 39→42 count update; `config_drift` moves from 39 to 42.

**Needs-user:** none remaining — ruled. Tracking closes here.

---

## Phase 10 closeout (2026-07-28) — the ledger is confirmed empty; deferred work moved to a register

Phase 10's final chunk audited this ledger end to end across **every phase, 0 through 10 including
9.5**, and confirms the state below. The audit was a re-read of the actual file, not of a summary of
it.

**Confirmed EMPTY.** Zero items across the entire ledger await a ruling — OD-1..OD-25, P4-OD-1,
P5-OD-1..3, P6-OD-1..2, P7-OD-1..8, P8-OD-1, P9-OD-1..2, P9.5-OD-1..6, and every review-gate finding
(R1..R4 across phases 4–9) are RULED, APPLIED, FIXED, or explicitly closed with their disposition
implemented. P9-OD-2 — the last substantive one, the config-ordering write-path gap — was ruled and
closed in the Phase 9 closeout and is enforced at **both** boundaries today.

**One item is deliberately left open, and it is the user's call rather than a build decision:**

- **R2-5** — the stale `{job_item_id}` example in `phase-9-polish-and-documentation.md` §5.3, which
  omits the required `user_id`. It sits in one of the **locked** phase specs, and CLAUDE.md forbids
  editing a locked spec as part of build work. The authoritative living contract
  (`Docs/api-contract.md`, the document `contract_drift` actually verifies) is correct. Closing it
  means spending a deliberate, reviewed edit on a frozen document (CLAUDE.md §8). Listing it as open
  is the honest disposition — **not** a build blocker, **not** a contract gap, and not something an
  operator or UI team would ever hit.

**Deferred work no longer lives here.** The honest way to reach zero open items is not deletion, so
everything that was scoped out across eleven phases now has a labelled home in
**`Docs/future-work.md`** — bounce/complaint ingestion, open/click tracking, warm-up logic,
multi-shared-pool campaign binding, dedicated→dedicated fallback, Brain horizontal scaling (locked
out by Rule 7, not merely deferred), sequences, per-pool variation scope, `/v1/admin/boxes` CRUD,
server-side bulk credential re-encryption, a multi-key keyring, admin RBAC, API rate limiting, a
metrics endpoint, cleared-alert webhooks, `send_logs` at-rest encryption, the Go toolchain upgrade
that clears two live-call-path module vulns, and a CI vuln gate. Each carries one line on **what it
is** and **why it was deferred**, because the reason is the part that matters when someone picks one
up.

That register also carries the four defects Phase 10's verification found and **flagged rather than
fixed**, each because its fix is a behavior change and Phase 10 changes no behavior: the
`POST /v1/run` HTTP 500 on a NULL `public.job_items.campaign_id`
(`internal/platformapi/store.go:69` — a nullable column scanned into a non-nullable Go `string`), the
box's permanent exit when `dev.sh up` outruns `dev.sh seed` (reproduced during Phase 10's own gate
run), the register-rejection short-circuit of the documented ~30-attempt retry budget
(`services/box/src/stream.js` — measured exit after attempt 1, ~1s), and the inert
`SENDER_ALERT_WEBHOOK_URL` in `.env.example`. None was known-and-hidden; each was found by executing
the documented behavior rather than reading it.

**Tracking closes here.** Any future item opens a new dated section in this file with an ADR
(CLAUDE.md Rule 2); it does not reopen a closed one.

---

## Pre-deploy gate, Stage 11 (2026-07-29) — one item, RULED AND CLOSED

Opened per the rule directly above: a new dated section with an ADR, not a reopened item. Found while
walking Stage 11 of `Docs/sender-module-local-test-guide.md`.

**Status: DISCHARGED 2026-07-29.** The user ruled **option (B)** — pause dispatch, bounded
re-acquire. Implemented, tested on real infra, docs updated in the same change. The item's
`Disposition` column below is preserved as the state at the time of the ruling; the outcome is
recorded immediately after the table.

| Id | Item | Severity | Disposition |
|---|---|---|---|
| G11-OD-1 | **A Brain that LOSES the singleton advisory lock mid-life keeps dispatching, so two Brains can dispatch against one database.** `internal/singleton/singleton.go`'s keeper correctly notices a dead lock connection, logs `marking not-held`, and exits without re-acquiring — but `held=false` is read by **`readyz` only**. `cmd/brain/main.go:205` passes `guard` to the health server and to nothing else; no dispatch/reclaim/retention path consults `guard.Healthy()`. Observed live, from an ordinary documented drill rather than a contrived setup: Stage 9 step 8's outage dropped the lock at 00:46:55 (`57P01`), `pg_locks` then held **zero** advisory locks on key `9200000001`, and that same process (`RestartCount` 0) **kept dispatching for 56 minutes** — `dispatch: tick pushed …` at 01:26:33 and 01:43:03. Driven to its consequence: a second Brain started against the same DB **acquired the lock instantly** and came fully up (`brain ready`), so two Brains were live with the newcomer holding the lock — the "second dispatch path" Rule 7 exists to prevent. Nothing was corrupted here only because the compose DNS alias `brain` still resolved to the incumbent, so both boxes stayed on it — luck about DNS, not a property of the design. | **HIGH** (Rule 7 is a load-bearing locked invariant, and its guarantee is conditional in the current tree: it holds from startup and degrades silently on any DB restart, failover, connection reaper, or `pg_terminate_backend`) | **FLAGGED — NEEDS-USER, not fixed.** Three remediations differ in operational behavior, so the choice is a locked-invariant ruling (Rule 2), not an implementation detail: **(A)** exit on loss — most Rule-7-consistent, but converts a transient DB blip into a restart and discards the graceful-drain guarantees Phase 7 built and Stage 9 step 8 just proved (0 restarts, nothing invented); **(B)** pause dispatch while not-held and keep the process up, reusing the DB-outage pause that already exists and is already tested, with bounded re-acquisition; **(C)** re-acquire in the keeper alone — narrowest, but unsafe without (B), since resuming after a successor came and went would dispatch from stale in-memory state (box registrations, fairness cursor, leases). **Recommendation: (B) with bounded re-acquire** — the only option that closes the window while keeping every property the drills have proven. Full analysis, the live evidence, and the required anti-vacuous test shape in **ADR-0011**. |

**RULING (user, 2026-07-29): option (B) — "pause dispatch, bounded re-acquire."** Both halves shipped
together; a pause with no recovery leaves a live-but-useless process, and a re-acquire with no pause is
the unsafe (C). What landed:

- **Pause.** `Dispatcher.SetLeadership`/`IsLeader` (an `atomic.Pointer` to a predicate), wired in
  `main.go` to `guard.Healthy`. The three loop ticks (`Runner.tick`, `ReclaimRunner.tick`,
  `RetentionRunner.tick`) return `notLeaderRetryInterval` when it is false, checked immediately AFTER
  the Phase-7 outage gate — a lost lock is usually *caused by* a DB restart, so the outage gate is the
  one that should log and set the cadence. `logNotLeader` throttles a
  `singleton_lock_not_held … level=error` line (visible to the runbook's `grep 'level=error'`, but it
  cannot flood during a long pause).
- **Gate boundary.** `Dispatcher.Tick`/`ReclaimTick`/`RetentionTick` stay UNGATED on purpose: the gate
  must return a wait interval, and pushing it lower would force it to invent a `Config` and risk a zero
  interval / hot loop. Safe only because the loop ticks are the sole production callers — pinned
  behaviorally by `TestLeadershipGate_LowLevelTickIsDeliberatelyUngated`, not by a comment.
- **Deliberately NOT gated: the outcome path**, the mTLS listeners, and the admin/platform read
  surfaces. A box's report on an in-flight send is persisted even by a Brain that lost the lock: that
  job was pushed while this Brain WAS leader, the SMTP session really ran, and refusing it would invent
  a non-outcome (Rule 5) or force a duplicate send. Persisting is idempotent (`provider_message_id`
  UNIQUE + the pre-persist check). None of those surfaces CLAIM work, which is what the gate protects.
- **Re-acquire.** `Guard.SetReacquireInterval`, cadence from the new seeded
  `brain_singleton_reacquire_interval_sec` (default 5, `sender_0023`) — config-not-code (Rule 1). The
  fallback on an unreadable/invalid row is **0 = never re-acquire**: fail-closed, leaving the Brain
  paused-and-honest (`readyz` 503, container unhealthy, dispatching nothing) rather than resuming on a
  guessed cadence.
- **Tests** — `internal/dispatch/leadership_test.go`, 7 tests on real Postgres, all green, and every
  assertion confirmed to FAIL against a deliberately disabled gate. The core pair terminates the lock
  backend (`pg_terminate_backend` — exactly what the observed `57P01` did), waits for the loss to be
  *observed* (failing if the flag never flips, so "nothing changed" cannot read as "paused"), confirms
  `pg_locks` shows no lock, then asserts a job queued afterwards stays `queued`/attempt 0 with no push;
  the paired positive control flips one bool and nothing else. Recovery is asserted the same way, with
  `TestSingletonReacquireDisabledStaysPaused` (interval 0) as its negative control.
- **Verified on the LIVE stack too**, since the defect was originally observed in the real container:
  image rebuilt, lock backend terminated, and the full cycle observed — `pg_locks` to **zero** holders,
  pause logged, `lock RE-ACQUIRED … after 5s not-held`, back to **one** holder, `readyz` `ready`,
  container `healthy`, no restart. The 58-check harness is green against that binary. **That live pass
  found a bug the seven green tests missed:** the recovery line printed `after 2562047h47m16s not-held`
  (`time.Since(zero)`) because the loss was stamped only on a *failed* re-acquire and the first attempt
  normally succeeds. Fixed at the detection point, and now pinned by `Guard.NotHeldFor()` assertions in
  both directions (non-zero **and** bounded while paused; zero after recovery), each confirmed to fail
  against the reintroduced bug. A duration an operator reads during an incident is load-bearing output;
  green tests prove nothing about output no test reads.
- **A Rule-10 trap these tests exposed, recorded so it is not re-derived:** `notLeaderRetryInterval`
  is 1s and the seeded `dispatch_tick_interval_ms` is ALSO 1000ms, so a paused dispatch tick and a
  successful one return the SAME duration — `wait == notLeaderRetryInterval` is vacuous there, and
  `wait != …` is outright wrong (it failed on correct behavior, twice). Dispatch-side assertions are now
  observable-state only. Reclaim (15000ms) and retention (3600000ms) do not collide so their test may
  assert the wait, but it now GUARDS that precondition against the live config first, so a future
  reseed cannot silently re-introduce the hole — verified by seeding a colliding value and watching the
  guard fire.

Full analysis and the live evidence remain in **ADR-0011** (status: accepted).

**Not part of this item, and recorded so it is not conflated:** Stage 11 step 6 **passes on its own
terms**. Against a *healthy* incumbent, a second Brain waited the full bounded `1m0s`, logged
`singleton lock not acquired within 1m0s: another Brain is active; exiting cleanly`, and exited **code
3**, with the incumbent still `ready`/`healthy`. The startup guard works; the untested half was a
mid-life lock **loss**, which is a different event from a startup **contention**.

---

## Pre-deploy gate, Stage 12 (2026-07-29) — one item, RULED AND CLOSED

Opened per the same rule: a new dated section with an ADR, not a reopened item. Found while walking
Stage 12 step 7 of `Docs/sender-module-local-test-guide.md`.

> **RULING (2026-07-29). The user ruled option (A): the ADR-0009 pattern applied per-column.**
> Shipped as `sender_0024` — `CREATE OR REPLACE` of `sender.fn_job_events_reject_update`'s body,
> same signature/trigger/owner, **no** object added and no grant or FK action changed
> (additive-only, Rule 5). `sender_0008`'s `REVOKE UPDATE, DELETE … FROM sender_module` is
> untouched, so the app role still cannot issue *any* update; the carve-out is reachable only by
> the FK's own maintenance statement and by the schema owner.
>
> **Permitted iff all of:** `account_id` unchanged or non-NULL→NULL; `box_id` unchanged or
> non-NULL→NULL; **at least one actually nulled** (a bare no-op UPDATE still raises — no
> referential action performs it); and every content column byte-identical (`id`, `job_id`,
> `user_id`, `seq`, `from_state`, `to_state`, `actor`, `error_class`, `error_message`, `metadata`,
> `created_at`). Two per-column clauses, not the looser "an FK column changed" — that is what
> permits a box-and-account delete while still rejecting a nulled FK being **re-pointed at a
> different row**, which would rewrite provenance to blame an account that was never involved.
>
> **A Postgres semantic that decided the implementation, caught by a failing control rather than by
> reading.** The unchanged-side comparisons must be `IS NOT DISTINCT FROM`, not `=` — `NULL = NULL`
> is NULL, not TRUE, so `=` fails closed. A control with `=` substituted failed in **two** places:
> the anticipated one (the second of two sequential per-constraint updates — Postgres issues one
> `UPDATE ONLY … SET <col> = NULL` **per FK constraint**, not one combined statement), and an
> **unanticipated** one — a plain single-FK *box* delete, because the genesis event row is born with
> `account_id` NULL (`fn_job_events_on_insert` copies `NEW.assigned_account_id` from a job that is
> not yet assigned). The `=` form would therefore have wedged the *ordinary* box delete too. A
> reviewer reasoning only about "the combined case" would have shipped it.
>
> **Tests:** `internal/dispatch/job_events_fk_carveout_test.go` (5 tests, real Postgres) plus two
> driven probes in the migration lifecycle test. Jobs reach a terminal state through the REAL
> dispatch/outcome paths, so the rows under test carry trigger-written FK values. Content
> immutability is asserted against a per-row pre-delete fingerprint, with the FK columns
> fingerprinted **separately** — a whole-row hash could not distinguish "the FK was nulled"
> (permitted) from "the actor was rewritten" (forbidden). Each positive also asserts the *other* FK
> UNCHANGED (the per-column half) and the row count unchanged (nothing cascaded away). All negatives
> run as **superuser**: `sender_module` has UPDATE revoked, so a rejection seen as that role would
> prove the REVOKE and would pass with no trigger present.
>
> **Anti-vacuity, three controls actually run (Rule 10):** (1) revert to `sender_0008`'s
> unconditional body → all three positives failed with the exact wedge message; (2)
> `DROP TRIGGER` → the negative-controls test failed on the first content UPDATE; (3) `=` for
> `IS NOT DISTINCT FROM` → failed in the two places above. No single regression satisfies both
> halves. The lifecycle probe is paired the same way (permitted **and** content-still-rejected after
> Up; rejected again after `Down 0024`, proving the down is real), because `sender_0024` adds no
> object and every existence check is structurally blind to it.
>
> **ADR-0009 was corrected, not quietly patched around.** Its "the pattern is CLOSED" claim rested
> on a census that had not been carried out. That census was **re-run against the live schema**
> (migrated `0000..0024`), not against the migration files: exactly three immutability triggers
> (`account_events`, `job_events`, `send_logs`); six `ON DELETE SET NULL` FKs, of which the three on
> trigger-bearing tables are now all carved out and the other three sit on freely-updatable tables;
> and **zero** inbound FKs referencing any of the three event/log tables, confirming `send_logs`'
> "no inbound FK" claim by query rather than by assumption.

| Id | Item | Severity | Disposition |
|---|---|---|---|
| G12-OD-1 | **`DELETE /v1/admin/accounts/{id}` returns `500` for any account that has ever been assigned a job — the documented hard delete is unavailable.** `sender.job_events` declares TWO `ON DELETE SET NULL` FKs (`job_events_account_fk` → `accounts`, `job_events_box_fk` → `boxes`), and Postgres implements `SET NULL` as an internal `UPDATE ONLY … SET <col> = NULL`. The table's `BEFORE UPDATE` reject-trigger (`sender_0008`'s `fn_job_events_reject_update`) raises on **every** update with no carve-out, so the FK's own maintenance update aborts the parent delete: `reason=account_write_error: ERROR: sender.job_events is append-only: UPDATE forbidden (SQLSTATE 23000)`. Deleting a **box** with job history fails identically (`SET "box_id" = NULL`). This is the SAME defect ADR-0009 diagnosed and fixed for `account_events` — on the table ADR-0009 examined and **explicitly cleared**: its "pattern is CLOSED" section asserts `job_events`' inbound cleanup "is `CASCADE DELETE` … correct as-is", which is wrong about the schema as built (it is both kinds; the two SET NULL FKs were not enumerated). Reproduced at the SQL level as the schema **owner** in a rolled-back transaction, so it is not a grants or API-layer artifact, with a negative control proving the trigger is the blocker: an account with ZERO `job_events` rows deletes cleanly, because `SET NULL` then matches no row. That split is why the defect survived the build — Phase 2's T5/T9 delete accounts that were never dispatched to, and via the API a never-used account returns `204` while a used one returns `500`. Nothing is corrupted or dropped: the delete aborts atomically, so this is a **wedged operation**, not data loss. | **MEDIUM** (contradicts `api-contract.md:112` "hard delete (audit row survives)" and the Phase 2 locked §2.5 unconditional-`204`; leaves NO erasure path for a used account through any interface, including direct owner SQL — a GDPR-style hard delete is currently impossible. Not HIGH: no send-path, credential, or audit-integrity impact, and no silent wrong answer) | **RULED (A) 2026-07-29 AND CLOSED — fixed by `sender_0024`.** Was FLAGGED–NEEDS-USER because the fix **revises an accepted ADR's explicit conclusion** (ADR-0009's "pattern is CLOSED / correct as-is"), which is a ruling, not a refactor (Rule 2), and because `job_events` has TWO nullable FK columns where `account_events` has one, so the permitted set is strictly larger and deciding it is deciding how far the immutability guarantee bends. The user ruled the ADR-0009 pattern applied **per-column**. Landed: the carve-out permits only a non-NULL→NULL transition on `account_id`/`box_id` with **every** content column byte-identical and at least one FK actually nulled; a bundled content change, a no-op UPDATE, and re-pointing a nulled FK at a different row all still raise. 5 behavioral tests + 2 driven migration probes, **three** anti-vacuity controls run (unconditional body → positives fail; trigger dropped → negatives fail; `=` for `IS NOT DISTINCT FROM` → wedges in two places, one unanticipated). ADR-0009's census re-run against the live schema. **Re-driven end-to-end on the live dev stack through the API that produced the `500`**: runner-applied `0024`, throwaway account, 3 real sends (`sent`), then `DELETE` → **`204`**, with all 6 audit rows surviving, `account_id` NULL, and content + `box_id` verbatim. Full detail in the RULING block above and **ADR-0012**. |

**Recorded so it is not conflated:** tenant erasure (`DELETE FROM public.users`) also fails, but for
an unrelated reason — `pools_user_fk` declares no referential action and refuses first, before any
`job_events` trigger fires. It reaches the item above only by the longer path (pools → accounts).
**Still true after `sender_0024`**, and deliberately not changed by it: `sender_0024` fixes the
`job_events` collision only. A one-statement `DELETE FROM public.users` remains refused by
`pools_user_fk`; tenant erasure is an **accounts-then-pools-then-user** sequence, and each of those
three steps now works — driven end-to-end on real Postgres in a rolled-back transaction against a
tenant with `job_events` history (`DELETE 1` at each step, and the one-statement user delete still
refused with `violates foreign key constraint "pools_user_fk"`).

**One thing that probe surfaced, which is not obvious from the account-delete behavior:** the final
`DELETE FROM public.users` **cascades the tenant's `job_events` rows away entirely**
(`job_events_user_fk` is `ON DELETE CASCADE`; the probe's two event rows went to zero). So
"audit row survives" is a property of deleting an **account or box** — where the FK is `SET NULL` and
`sender_0024`'s carve-out applies — not of erasing a **tenant**, where a different FK with a different
referential action removes the rows before any UPDATE trigger is reachable. That asymmetry is
intentional as built (a GDPR erasure that left the tenant's per-job audit trail behind would be a
worse defect than the one just fixed), but it means the two operations must not be described in the
same breath. Making the user delete cascade *pools* on its own would be a separate ruling about
tenant data lifecycle, not a consequence of this one — registered in `Docs/future-work.md`, not
silently absorbed.
