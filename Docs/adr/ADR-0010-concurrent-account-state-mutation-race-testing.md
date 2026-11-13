# ADR-0010: account-state mutation under concurrent outcomes requires a two-connection race test (and any locking fix requires a two-connection deadlock test)

- **Status:** accepted
- **Date:** 2026-07-24

## Context

Two separate phases hit the **same class of defect** — a data race on `sender.accounts` state
when more than one outcome/consume touches the same account concurrently:

- **Phase 4** (dispatcher & routing): the double-booking-prevention core. The whole point of the
  atomic consume (`FOR UPDATE SKIP LOCKED` + increment-inside-the-admitting-UPDATE +
  `accounts_daily_sent_check`) is that two concurrent workers can never both spend an account's
  last unit of daily headroom. The correctness of that path is *only* observable under genuine
  concurrency — a serial test passes vacuously. Phase 4 therefore shipped
  `TestAccountConsume_NeverExceedsDailyLimit` with 8 real `pgxpool` connections racing, plus a
  live `pg_stat_activity` sampler proving genuine time-overlap (a serial impl would sit at
  concurrency 1 and pass a naive `<= limit` check).

- **Phase 5** (error taxonomy, status & quarantine): the account-effect write path. Review gate R4
  found that `applyAccountEffect` read the account's flags **without a row lock** and then wrote
  back whole-column literals computed in Go from that snapshot. Two concurrent outcomes on the
  same account (allowed — per-user lanes let one account serve several in-flight jobs, and each
  box report is processed in an independent transaction) could **lost-update**: an `auth_failure`
  commits `quarantined=TRUE`, and a concurrent `throttle` outcome that read the pre-quarantine
  snapshot then writes its stale `quarantined=FALSE` literal — **silently un-quarantining** the
  account, violating the LOCKED "quarantine is manual-recovery-only" invariant (Phase 5 §4.2). The
  fix was `SELECT … FOR UPDATE` on the account row. But the *naive* placement of that lock then
  **deadlocked** (40P01): the job requeue's `job_events` trigger takes a FK `KEY SHARE` lock on
  the account, and two transactions each holding `KEY SHARE` then trying to upgrade to `FOR UPDATE`
  cycle. That deadlock was **caught by a two-connection regression test on its first run**, and
  fixed by acquiring the account `FOR UPDATE` at transaction entry — before the job requeue — for a
  consistent account→job lock order.

The recurring theme: **account-state mutation is the module's most concurrency-sensitive surface,
and every defect in it has been invisible to serial tests and visible only to a genuine
multi-connection race.** A locking *fix* is itself a new concurrency hazard (deadlock), so it too
must be exercised concurrently.

## Decision (standing requirement for all future phases)

1. **Every phase that mutates `sender.accounts` state under an outcome/consume/claim path ships at
   least one genuine two-connection (or N-connection) concurrency-race behavioral test** — real
   separate `pgxpool` connections against real Postgres, never one shared connection and never a
   mock. The test asserts a concrete post-state that a lost-update / double-spend / stale-overwrite
   would violate (e.g. "quarantine survives", "count never exceeds limit", "exactly one system
   event"), and carries the anti-vacuity proof that concurrency was genuinely exercised (more than
   one connection succeeded, or a sampler observed real overlap).

2. **Any locking change (adding/moving `FOR UPDATE`, `FOR KEY SHARE`, advisory locks, or reordering
   locked statements) is itself checked for deadlock with a two-connection test** that drives the
   two lock orders concurrently. A lock added to fix a race is not "done" until a concurrent test
   proves it does not deadlock. Document the lock order in a comment at the acquisition site.

3. This applies to Phase 6+ (timeout detection / reclaim loop — which will mutate account and job
   state from a background sweeper concurrently with live dispatch, a prime candidate for this
   class of bug) and every later phase touching account state.

## Codebase-wide lock-ordering invariant (added Phase 6 closeout, 2026-07-25)

Phase 6's review gate R4 hit this class a **third** time (P6-R4-1): Detector A's dead-box mass
reclaim was a bulk `UPDATE sender.jobs` whose `job_events` trigger (`sender_0008`) inserts
`assigned_account_id` and so takes an FK `KEY SHARE` on the account row **after** the job row
(job→account), cycling `40P01` with `applyClassifiedFailure`'s `FOR UPDATE`-then-requeue
(account→job). The root cause is **structural and permanent**: any `UPDATE sender.jobs` that leaves
`assigned_account_id` set fires the trigger and therefore takes an account lock *after* the job-row
lock — so a path is implicitly job→account unless it pre-locks the account.

**THE INVARIANT (load-bearing, codebase-wide):**

> **Any transaction that acquires an EXCLUSIVE account-row lock (`SELECT … FROM sender.accounts …
> FOR UPDATE`) AND also writes a `sender.jobs` row MUST acquire the account lock FIRST
> (account → job).** A path that writes a job row while touching the account only via the trigger's
> implicit `KEY SHARE` (no exclusive account lock of its own) is safe without pre-locking, PROVIDED
> its only concurrent same-job counterparty acquires the job with `FOR UPDATE SKIP LOCKED` (so it is
> skipped, never blocked-on). When in doubt, take the account `FOR UPDATE` first — it only ever
> serializes more, never less.

**Why the precise scoping (not simply "account-first always"):** three current paths write a job
row job-first (`applyAccepted`, `applyBounced`, `markSending`) and are nonetheless deadlock-free,
because they never hold an *exclusive* account lock while waiting on a job — their account
interaction is the trigger's `KEY SHARE` (plus, for `applyAccepted`, an in-place upgrade to
`NO KEY UPDATE` on a row it already holds `KEY SHARE` on). The cycle in P6-R4-1 required one side to
hold an *exclusive* account lock (`FOR UPDATE`) while blocked on a job the other side held. So the
invariant that actually prevents the cycle is scoped to exclusive account locks; stating it as a
blanket "account-first always" would be *false* of the three job-first paths and would mislead a
future phase into thinking they violate it.

**The current paths that lock BOTH an account and a job row (enumerated + confirmed, Phase 6
closeout):**

| Path | File | Acquires exclusive account lock? | Order | Verdict |
|---|---|---|---|---|
| `applyClassifiedFailure` | `outcome.go` | Yes (`FOR UPDATE` at tx entry) | **account → job** | ✓ follows the invariant |
| `reclaimJobLocked` (both detectors) | `reclaim.go` | Yes (`FOR UPDATE` first, then job `FOR UPDATE SKIP LOCKED`) | **account → job** | ✓ follows the invariant |
| atomic consume | `eligibility.go` | Yes (`FOR UPDATE SKIP LOCKED`) | account-only (**no job row touched** — the job was claimed in a separate earlier tx) | ✓ trivially (no job lock to invert) |
| `applyAccepted` | `outcome.go` | No (trigger `KEY SHARE` → in-place `NO KEY UPDATE` upgrade) | job → account | ✓ safe by construction (sole mutator of its job — idempotent-gated; only same-job racer is reclaim, which uses `SKIP LOCKED`) |
| `applyBounced` | `outcome.go` | No (trigger `KEY SHARE` only; no account mutation — bounce carve-out) | job → (account KEY SHARE) | ✓ safe (never an exclusive account lock) |
| `markSending` | `dispatch.go` | No (trigger `KEY SHARE` only) | job → (account KEY SHARE) | ✓ safe (never an exclusive account lock) |

All exclusive-account-lock paths (`applyClassifiedFailure`, `reclaimJobLocked`, the consume) follow
account-first; the three job-first paths take no exclusive account lock and are deadlock-free by the
construction above (confirmed empirically by `TestLockOrderAudit_ConcurrentAcceptedSameAccount`,
`TestLockOrderAudit_AcceptedVsClassifiedSameAccount`, `TestReclaimRace_OutcomeVsReclaim`, and
`TestReclaimRace_DetectorAVsClassifiedFailure` — all real two-connection tests, all green).

**`reclaimJobLocked` is the SINGLE reclaim locking path** — both Detector A (dead-box, row-by-row
over the box's in-flight jobs) and Detector B (lease-expiry) funnel through it via
`reclaimOneJobReason`; there is no second, divergent reclaim UPDATE. Confirmed Phase 6 closeout.

**Standing requirement for future phases:** any NEW path that will `SELECT … FROM sender.accounts …
FOR UPDATE` and also write a job row acquires the account lock first, and ships a two-connection
race test (per items 1–2 above). Any new job-first path that adds an exclusive account lock must
move to account-first. This is item DoD-10 and CLAUDE.md Rule 9.

## Consequences

- A small, permanent test-authoring cost per phase — offset by catching a whole defect class
  (Phase 4's double-book, Phase 5's lost-update un-quarantine and its fix's deadlock) before it
  reaches production, where it would silently shed real sending capacity or over-send.
- The tests are slower (real connections, real contention) but run inside the existing real-infra
  harness, consistent with CLAUDE.md Rule 3 (no mocks for the claim/consume/state-machine paths).
- Recorded in the Open Decisions ledger (`Docs/open-decisions.md`, Phase 5 review-gate section) so
  the requirement is discoverable from the ledger, not only from this ADR.

## Amendment (Phase 7 closeout, 2026-07-26): a race test must PROVE it contended, and the two
## Postgres semantics that decide whether Rule 9 applies at all

Phase 7's review gate R4 found the requirement above being satisfied in letter and not in substance.
`TestRescan_LeaseExtensionVsOutcomeNoDeadlock` was a genuine two-connection test on real Postgres,
driving the rescan's lease `UPDATE` against a concurrent outcome, and it was green. Instrumented, it
turned out that in **40 of 40 rounds neither connection ever blocked**: the two paths never met, so
the test asserted only that two unrelated statements can both succeed. It would have passed unchanged
if the rescan had taken an account lock in the wrong order — precisely the defect this ADR exists to
catch. An uncontended race test is indistinguishable from a passing one in CI output, which is what
makes this failure mode worth an amendment rather than a note.

**Amended requirement (now CLAUDE.md Rule 10, DoD-2).** A two-connection race test must prove
contention, not hope for it. Either:

1. **Force a deterministic interleave** — hold one transaction open at the exact point of interest and
   drive the counterparty against it. Preferred: it removes timing luck entirely and is repeatable.
2. **Assert the contention it claims** — observe a genuinely blocked backend
   (`pg_stat_activity WHERE wait_event_type='Lock' AND state='active'`), or assert the blocked side
   cannot complete while the lock is held — and FAIL if no contention is observed.

"No `40P01` occurred" is not evidence on its own. Closed for Phase 7 by
`internal/dispatch/rescan_lockorder_test.go`, which drives both orders as forced interleaves and fails
on `40P01`, on a block, and on vacuity.

**The two Postgres semantics.** Establishing that test's teeth took three attempts, and the two
failures are recorded here because they determine whether Rule 9 applies to a path at all:

- **(a) An `UPDATE` that leaves `state` unchanged produces NO `job_events` row.**
  `trg_job_events_on_state_change` is `AFTER UPDATE OF state`, and `fn_job_events_on_state_change`
  RETURNS EARLY when `OLD.state = NEW.state` (`sender_0008`). So `SET state = state` is not an event.
- **(b) An `UPDATE` that leaves an FK key value unchanged takes NO `KEY SHARE` on the referenced row.**
  Postgres skips the FK re-check when the key did not change. So
  `SET assigned_account_id = assigned_account_id` does not lock the account.

**Consequence for the invariant.** A job-row `UPDATE` that touches neither `state` nor an FK key is
**event-free** and takes no account lock, so Rule 9's precondition never triggers for it. Phase 7's
startup rescan (`SET lease_expires_at, updated_at WHERE state IN ('assigned','sending')`) is exactly
this shape, which is why the whole phase satisfies Rule 9 vacuously rather than by ordering discipline.
Because that premise is load-bearing, it is asserted behaviorally in BOTH directions rather than
documented in a comment.

**Consequence for test authors.** Neither no-op `SET` above is a valid negative control for a
lock-ordering test — both leave the test green while changing nothing observable. To make such a test
fail on purpose, inject an EXPLICIT `SELECT … FROM sender.accounts … FOR KEY SHARE` (or `FOR UPDATE`)
into the transaction under test. That is what made `rescan_lockorder_test.go` fail with *"the rescan's
lease UPDATE BLOCKED behind a held exclusive account lock — it is NOT event-free"*, proving the teeth.
