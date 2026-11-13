# CLAUDE.md — Operating Rules (Module 2, Email SendManaged)

This file is the /init anchor and the single source of build discipline for this
repository (one repo; Go "Control" orchestrator + Node.js "SendManaged Workers" + centrally
owned SQL migrations). Read it before writing code, tests, migrations, or docs.
These rules are NON-NEGOTIABLE and outlive any individual phase. If a task appears
to require breaking one, STOP and file an Open Decision (Rule 2) — do not proceed.

> **Status (2026-07-28, Phase 10 close):** all eleven phases (0, 1–9, 9.5, 10) are implemented,
> tested on real infra, and closed. The locked phase specs live under `Docs/history/` and MUST NOT
> be edited as part of any build work. Living operational + reference docs are the top level of
> `Docs/` — see `Docs/README.md`. Deferred work is registered in `Docs/future-work.md`, not left
> open. Rules below are unchanged and outlive the build.

## 1. Config not code
- Every tunable — timeout, window, limit, batch size, interval, mode — is a seeded
  row in `send_managed.config` with a documented `default_value` and a `value_type` hint.
  Values are JSONB-wrapped `{"v": ...}`. There are NO hardcoded tuning constants in
  Go, Node, or SQL. A literal that governs runtime behavior is a bug.
- `default_value` is never mutated. Phase 9 §4 is the authoritative 50-key reference. (2026-11-09: the
  living reference is now `Docs/config-reference.md`, currently 57 keys — the `config_drift` gate, not a
  count written in prose, is what keeps it exact.)
- New tunable => new seeded config key in a migration + an entry in the Phase 9 §4
  reference in the same change. No key, no constant.
- TWO config-ordering INVARIANTS are enforced at config-load, not assumed:
    (a) smtp_operation_timeout_sec  <  job_max_processing_time_sec
    (b) box_outcome_buffer_ttl_sec  >= startup_reconnect_window_sec
  A config set that violates either is REJECTED at load with external code
  `config_ordering_violation` (Phase 7 §5.3 authoritative). Do not "clamp" or
  auto-correct — reject.
- The ONE narrow, documented exception: the migration runner's own `lock_timeout` /
  `statement_timeout` are CLI-flag defaults, because `send_managed.config` does not exist
  until the bootstrap migration creates it. This exception is the runner only — never
  Control/worker runtime behavior.

## 2. Flag, don't silently fix
- Contradictions, ambiguities, and locked-fact conflicts are NOT resolved by
  inventing an answer. Record them in the Open Decisions log (`Docs/open-decisions.md` — the living
  ledger; `Docs/history/open-decisions.md` is the frozen phase-0–10 predecessor — + an ADR in `Docs/adr/`) and await the user's ruling.
- Never resolve a locked contradiction on your own authority. "Locked" means locked.
- An Open Decision blocks only the ambiguous slice; keep unrelated work moving.

## 3. Real behavioral tests only
- Tests exercise real behavior against real infrastructure (real Postgres, real
  Control<->worker channel). NO syntax-only tests. NO mock-the-DB / mock-the-worker tests
  standing in for behavior.
- This is absolute for: claim/consume (exactly-once job pickup), idempotency
  (no double-send under retry/replay), and the job state machine
  (queued/assigned/sending/sent/failed/bounced transitions).
- The real-infra harness (`tests/harness/`) is the home for these; new behavior ships
  with a behavioral test in that harness. A test that cannot fail is not a test:
  every test asserts a concrete observed post-state, and guard tests carry a paired
  positive/negative control.

## 4. Documentation is part of every change
- No change is complete until its docs are updated in the same PR. Docs never go
  stale; a diff that changes behavior without changing docs is incomplete.
- The docs discipline (structure, ownership, staleness gate) is defined in the
  Phase-0 documentation section (`Docs/history/phase-0-groundwork.md` §3) and the living docs
  under `Docs/`. Follow it.

## 5. Contracts, errors, and honesty
- Contracts (API request/response shapes, event/message schemas, DB columns, enums)
  are ADDITIVE-ONLY and VERSIONED. Never rename; never repurpose; never remove.
  The 7 enums (job_state, error_class, pool_kind, routing_strategy, box_status,
  event_actor, pause_reason) are additive-only forever.
- TWO-VOCABULARY ERROR RULE: an internal reason is NEVER serialized to a client.
  Client error envelopes carry ONLY {external code, message, request_id}. NEVER
  emit credentials, ciphertext, key-ids, SQL, SQLSTATE, stack traces, internal
  hostnames, or internal reason strings across a client boundary. Internal detail
  goes to logs/traces keyed by request_id, never to the caller.
- The alert webhook (alert_webhook_url) is an UNTRUSTED OUTBOUND EGRESS across a
  trust boundary and is subject to the same two-vocabulary rule: its payload carries
  ONLY condition codes, subject ids, and timestamps — never send content, credentials,
  internal reasons, SQL, or internal hostnames.
- Never invent an outcome the worker did not report. If the worker did not confirm a
  send/failure/bounce, the system does not claim one. No optimistic fabrication.

## 6. Load-bearing security properties (never weakened)
- Control is the SOLE decryptor. KMS Decrypt is Control-only. The platform and operators
  hold NO Decrypt capability. Do not add one.
- The worker holds SMTP credentials in MEMORY ONLY and zeroes them on EVERY exit path
  (success, error, panic, shutdown, timeout). A credential is never logged, never
  persisted, never serialized to a client.
- The worker has NO DB access and NO KMS access. Ever. It talks only to Control. This is
  structural: the worker service ships no DB driver and no KMS SDK, and its container
  mounts neither DB credentials nor KMS key material.
- The worker<->Control channel is mutual-TLS (mTLS). Do not downgrade or bypass.
- SMTP: STARTTLS-before-AUTH on ports 587/25; implicit TLS on 465. AUTH never
  precedes TLS.
- Worker authorization authority is the registration token (hash seeded in
  send_managed.boxes.token_hash; plaintext never stored); the client cert is transport
  auth/confidentiality. A decommissioned worker (token_hash cleared/rotated) is refused
  on next connect regardless of cert.
- Auth domains never share a credential: Domain A = platform service token
  (quote/run/report/describe); Domain B = operator platform_admin bearer key
  (stored hashed, constant-time compared) for /v1/admin/*.
- The `send_managed` schema is owned by a dedicated NON-LOGIN, NON-SUPERUSER role
  (`send_managed_owner`) so that `FORCE ROW LEVEL SECURITY` on `send_managed.accounts` actually
  enforces. The app role `send_managed_app` is login-only and holds the enumerated grants,
  never ownership.

## 7. Autonomous-subsystem safety controls are respected
- Every autonomous subsystem obeys its kill-switch / mode dial. These are honored by
  every current and future phase; no subsystem is "always on":
    - dispatch_mode               (serving | accept_and_defer | refuse_new | stopped;
                                    default serving) — the graduated panic switch. Each
                                    rung stops strictly more than the one above:
                                    accept_and_defer stops sending but keeps intake and
                                    the reclaim sweep; refuse_new also answers
                                    POST /v1/run with 503 paused; stopped also stops
                                    reclaim. Halt cleanly, zero job loss, resumable.
                                    Unrecognized/unreadable resolves to accept_and_defer,
                                    never to serving and never to stopped.
    - dispatch_enabled            (panic switch; default true) — RETAINED (Rule 5) and
                                    still honored: composed as a severity FLOOR of
                                    accept_and_defer, not a second independent lever.
                                    false alone still halts dispatch cleanly, zero job
                                    loss, resumable. NOT dropped in favour of
                                    dispatch_mode — it has many live consumers, so
                                    removing it would be a breaking change for no gain
                                    (ADR-0013).
    - autoquarantine_mode         (off | shadow | enforce; default shadow) — shadow
                                    logs the action it WOULD take before any phase makes
                                    it act; default is never enforce.
    - autopause_mode              (off | shadow | enforce; default shadow) — the same
                                    ladder on auto-pause (send_managed_0026). shadow
                                    EVALUATES the pause decision and records it as an
                                    account_events observation row (from_flags ==
                                    to_flags), then does not pause. Unrecognized
                                    resolves to shadow, never enforce and never off.
    - retention_mode              (off | shadow | enforce; default shadow) — the same
                                    ladder on the send_logs retention sweep. shadow
                                    counts the expired rows and logs the count, deleting
                                    nothing. NOTE the standing cost: at shadow,
                                    send_logs grows without bound, so promoting this to
                                    enforce is a REQUIRED go-live step
                                    (Docs/go-live-checklist.md §12), not a nicety.
    - autopause_enabled           (panic switch; default true) — RETAINED (Rule 5) and
                                    still honored: composed as a severity CEILING of
                                    autopause_mode (false caps the effective mode at
                                    off), not a second independent lever. Same rule as
                                    dispatch_enabled's FLOOR — a boolean can only ever
                                    reduce what the system does — stated on the inverse
                                    scale, because these two ladders order by ACTIVITY
                                    while dispatch_mode orders by SEVERITY (ADR-0014).
    - Every autonomous mutator — every write the Control makes on its own initiative,
      as distinct from transcribing a worker report or executing an operator request —
      is on one of these ladders or explicitly excluded with a recorded reason. That is
      held by a MECHANISM, not by this list: TestMutatorMode_EveryAutonomousMutator
      IsDialledOrExplicitlyExcluded discovers every file in internal/dispatch that
      writes to send_managed.* and fails naming any file in neither map. A new
      autonomous mutator therefore cannot ship enforcing on day one. A dial gates an
      ACTION, never its REVERSAL: auto-unpause runs at every value of autopause_mode.
    - variation_enabled           (default true)
    - alert_webhook_url           ("" = alerting off)
- Control runs as a SINGLETON guarded by a session-level Postgres advisory lock on a
  fixed key, acquired at startup with bounded wait
  (brain_singleton_acquire_timeout_sec) then fail. SIGKILL auto-releases it (no stale
  lock). Never add a second dispatch path or distributed dispatch.

## 8. How this file is maintained
- CLAUDE.md changes ONLY by a deliberate, reviewed PR. No silent edits.
- Newly locked invariants are APPENDED (new numbered rule or a dated sub-point),
  never quietly rewritten. Existing rules are not softened or reworded to fit a
  convenient implementation; if a rule genuinely must change, that is an Open
  Decision (Rule 2) with an ADR, ruled on by the user, then appended here.
- Every rule above maps to a locked platform decision. Treat weakening any of them
  as an incident, not a refactor.

## 9. Account→job lock ordering (deadlock-avoidance invariant)
- Any transaction that acquires an EXCLUSIVE account-row lock (`SELECT … FROM
  send_managed.accounts … FOR UPDATE`) AND also writes a `send_managed.jobs` row MUST acquire the
  account lock FIRST — account → job. This is load-bearing and codebase-wide, not a
  per-phase detail.
- WHY (structural, permanent): the `send_managed.jobs` `AFTER UPDATE OF state` trigger
  (`send_managed_0008`) inserts `NEW.assigned_account_id` into `job_events`, so EVERY job-row
  UPDATE that leaves the account assigned takes an FK `KEY SHARE` on the account row
  AFTER the job-row lock. A path that separately grabs an exclusive account `FOR UPDATE`
  after the job row cycles (`40P01`) with one that grabbed it before. This class has bitten
  three times (Phase 4 double-book, Phase 5 lost-update-un-quarantine, Phase 6 R4-P6-1
  reclaim) — see ADR-0010.
- Precise scope: a path that writes a job row but touches the account ONLY via the
  trigger's implicit `KEY SHARE` (no exclusive account lock of its own) is safe without
  pre-locking, PROVIDED its only concurrent same-job counterparty acquires the job with
  `FOR UPDATE SKIP LOCKED` (skipped, never blocked-on). When in doubt, take the account
  `FOR UPDATE` first — it only ever serializes more, never less.
- Every phase that adds or moves an account/job lock ships a genuine two-connection race
  test that drives the two lock orders concurrently and proves no `40P01` (ADR-0010).
  That test must satisfy Rule 10 — it must PROVE it contended.
- The current both-locking paths and their confirmed orders are enumerated in ADR-0010.
- (2026-07-26, Phase 7 R4) TWO POSTGRES SEMANTICS that decide whether this rule even
  applies to a path. Both were learned the hard way — by writing negative controls that
  silently did NOT trip a lock-order test — so they are recorded rather than re-derived:
    (a) An `UPDATE` that leaves `state` UNCHANGED does NOT produce a `job_events` row.
        `trg_job_events_on_state_change` is `AFTER UPDATE OF state`, and its function
        RETURNS EARLY when `OLD.state = NEW.state`. So `SET state = state` is not an event.
    (b) An `UPDATE` that leaves an FK key value UNCHANGED does NOT take `KEY SHARE` on the
        referenced row — Postgres skips the FK re-check when the key did not change. So
        `SET assigned_account_id = assigned_account_id` does not lock the account.
  CONSEQUENCE, and the reason this matters: a job-row `UPDATE` that touches NEITHER `state`
  NOR an FK key (the Phase 7 startup rescan's lease extension is exactly this) is
  EVENT-FREE and takes NO account lock, so Rule 9 does not apply to it. That premise is
  load-bearing, so it is asserted behaviorally in BOTH directions
  (`internal/dispatch/rescan_lockorder_test.go`), not left as a comment. Corollary for test
  authors: to make such a test fail on purpose you must inject an EXPLICIT account
  `FOR KEY SHARE`/`FOR UPDATE` — the two no-op `SET`s above are NOT valid negative controls.

## 10. Anti-vacuity for concurrency tests (Rule 3 reinforcement)
- (2026-07-26, Phase 7 R4) A concurrency test that never actually contends is a test that
  CANNOT FAIL. Phase 7's rescan-vs-outcome race test ran 40/40 rounds in which neither
  connection ever blocked: it passed, proved nothing, and would have passed just as
  happily if the rescan had taken an account lock in the wrong order.
- Therefore: every two-connection race test MUST PROVE CONTENTION, not hope for it. One of:
    (a) FORCE a deterministic interleave — hold one transaction open at the exact point of
        interest and drive the counterparty against it (preferred: no timing luck), or
    (b) ASSERT the contention it claims — e.g. observe a genuinely blocked backend via
        `pg_stat_activity WHERE wait_event_type='Lock' AND state='active'`, or assert the
        blocked side does not complete while the lock is held.
  A test whose only assertion is "no `40P01` occurred" is not evidence unless it also shows
  the two orders genuinely met.
- The failure mode this closes is silent: an uncontended race test is indistinguishable
  from a passing one in CI output. Prefer (a); when a test must rely on real concurrency,
  it fails if it observes no contention.
- This applies to EVERY remaining phase and is part of the Definition of Done (DoD-2).
- (2026-07-26, Phase 8 closeout) A `pg_stat_activity` contention check MUST be scoped to the
  SPECIFIC blocking backend, not merely "some backend somewhere" or "some backend in this
  database." Filter by `datname = current_database()` AND a `query ILIKE '%...%'` fragment
  unique to the statement under test. `datname` alone is necessary but not sufficient — every
  dispatch-package test clones its own isolated database, so a `datname`-only filter usually
  works in practice, but it does not by itself prove the match is the RIGHT backend rather than
  an unrelated one that happens to share the database (e.g. an admin setup statement still
  in-flight). Precedent: `eligibility_test.go`'s sampler already used both filters; Phase 8's
  `attempttoken_lockorder_test.go` initially added only `datname` and a Phase 8 closeout
  verification pass found the gap, then discovered on the FIRST attempt to add the query filter
  that the guessed statement was wrong (the actual blocking statement was a different UPDATE
  than assumed) — proving the stronger check was not merely cosmetic, it caught a real mistake
  a `datname`-only check could not have caught.

## 11. Review gates run SEQUENTIALLY, never in parallel
- (2026-07-26, Phase 8 closeout) Multiple review gates over the SAME body of work (e.g. R1–R4
  covering different scopes of one phase) MUST run one at a time, in sequence — never launched
  concurrently. Each later pass's job is to INDEPENDENTLY RE-VERIFY, against the actual working
  tree, that every prior pass's claimed fix genuinely landed — not just that the prior pass
  reported a fix. Running gates in parallel makes that cross-verification structurally
  impossible: a later gate cannot re-check a fix that does not exist yet at the moment it runs.
- Phase 8's four review gates (R1–R4) were run in parallel. R3 and R4 found four real issues (one
  HIGH: a config-validation gap that could wedge the Control from a routine admin write). All four
  fixes were committed in a single pass with NO independent re-verification, because no
  subsequent gate ran afterward to check them. A dedicated CLOSEOUT pass (this one) later found
  that one of the four "fixes" was only partially applied (the `pg_stat_activity` scoping added
  `datname` but not the query-text filter the fix's own commit message claimed to add) — exactly
  the class of gap sequential re-verification exists to catch, and exactly the failure mode this
  repo has already been burned by once before (Phase 4: a claimed `readLease` fix that never
  landed). Do not assume a claimed fix is real. Verify it in the tree, every time, including your
  own claimed fixes from an earlier turn in the same session.
- This is now a standing requirement for every future multi-gate review, not a one-time
  correction: the LAST gate in any review sequence is not done until it has re-run (not merely
  re-read) every earlier gate's claimed fix and can report CONFIRMED-IN-TREE or MISSING for each.

## Definition of Done (every phase is held to this)
See `Docs/history/definition-of-done.md`. A phase is not done until all nine DoD items are
green: spec written; real behavioral tests green on real infra; docs updated in the
same change; config keys seeded + documented; no hardcoded tuning constants; Open
Decisions discharged; additive-only + migration chain intact; security properties
re-verified; control-plane safety controls honored.
