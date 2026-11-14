# ADR-0015: the MVCC re-check path of the consume is forced by a test-only row-level-security gate, not left to volume

<!-- file: ADR-0015-the-mvcc-recheck-path-is-forced-by-a-test-only-rls-gate.md -->


- **Status:** accepted
- **Date:** 2026-11-14
- **Supersedes:** —
- **Superseded-by:** —

## Context

The consume (`accountConsume`, `eligibility.go`) is one statement and its protection against booking more sends than `daily_limit` has three layers:

1. the `eligible_accounts` CTE: the eligibility predicate (including `daily_sent_count < daily_limit`) plus `ORDER BY … LIMIT 1 FOR UPDATE SKIP LOCKED`;
2. the `consumed` UPDATE that follows. It has **no guard of its own** (`WHERE a.id = ea.id` only), so layer 1 is the only thing that decides which row it touches;
3. the last resort, the CHECK constraint `accounts_daily_sent_check (daily_sent_count >= 0 AND daily_sent_count <= daily_limit)`, which must never fire in normal operation.

A peer consume that arrives while another consume is taking the last slot can be refused in **two different ways**:

- **(a) the row is locked** — the holder has it, and `SKIP LOCKED` skips it. FW-M3-22 (2026-11-14) made this deterministic: a consume parked mid-statement on a test-held advisory lock while it holds the slot, seven peers
  that must all be refused. `consume_contention_test.go`, ADR-less because it was a test fix.
- **(b) the row is not locked any more, but it changed** — the peer's statement snapshot was taken *before* the holder committed, so its scan saw the row with capacity (`limit-1 < limit`); by the time the peer reaches the
  row lock the holder has committed. The row it saw is now an old version, so `LockRows` finds it updated by a committed transaction, follows the update chain to the committed version, locks that and **re-evaluates the
  predicate against it** (`EvalPlanQual`). The committed version has `daily_sent_count = daily_limit`, so the peer is refused. This re-check is what stops the double booking on this path; nothing else in the statement does.

Path (b) was recorded as **not forced** in FW-M3-22: from outside the statement there is no point at which to hold a peer between its snapshot and its row lock, so the volume phase of the test exercised it only by chance.
"Covered statistically" is exactly what Rule 10 exists to remove, and the path guards the most important invariant in the sender.

The owner's instruction (2026-11-14, C3) was to try **Option A** first, bounded: in a test-only build, inject a volatile function into the consume predicate that blocks on a test-held advisory lock, so the peer pauses
*after* taking its snapshot and *before* locking the row; the holder commits and releases; the peer resumes and must re-check the changed row; prove the path was reached; add a mutant that goes red; production code must not
change. **Option B** was the fallback if Postgres's execution order prevented A: prove that `accounts_daily_sent_check` alone holds the limit.

## Decision

**Option A closed it, on the first runner attempt.** Postgres's execution order allows exactly the point we need: the qualifier of a row-level-security policy is part of the table scan's filter. It is evaluated per row version, after the statement's
snapshot and before `LockRows`. `accounts` already has `FORCE ROW LEVEL SECURITY` (`send_managed_0012`), so a test can add one more policy and have its function run **inside** the production statement without a character of the
production SQL changing.

`TestAccountConsume_StaleSnapshotPeerIsRefused` (`consume_contention_test.go`) does this, in the test's own cloned database:

1. a log table and a `VOLATILE SECURITY DEFINER` function `zz_mvcc_gate(id, daily_sent_count, daily_limit)`: for every other account it returns `true`; for the account under test it appends what it was shown to the log and, **only when
   `application_name = 'zz_mvcc_peer'`**, waits on a SHARED advisory lock. It always returns `true`: it decides when the scan proceeds, never what is eligible;
2. `CREATE POLICY zz_mvcc_gate ON send_managed.accounts AS RESTRICTIVE FOR SELECT TO send_managed_app USING (public.zz_mvcc_gate(…))` — a restrictive policy is ANDed with the existing permissive `sender_module_full_access`;
3. the test takes the exclusive lock, starts the **peer** (its own pool, `application_name=zz_mvcc_peer`) and waits until a `pg_stat_activity` row proves it parked — this database, not this session, that `application_name`, `wait_event_type='Lock'`
   and `wait_event='advisory'`, running the consume statement (`eligible_accounts`). The peer has its snapshot and has read the row **with capacity**;
4. the **holder** (`application_name=zz_mvcc_holder`, which the gate never parks) runs a complete consume while the peer is parked: it takes the last slot and **commits**. The test checks `daily_sent_count == limit` and that the peer is still parked;
5. the test releases the lock. The peer resumes holding an old row version, must be **refused — `ok=false`, no error** — and `daily_sent_count` must still equal the limit;
6. the peer's rows in the gate's log must include `seen_count = limit-1`. That value existed only before the holder's commit, so the peer's snapshot predated it. A statement whose snapshot predates the commit, refused only after the commit, with the CHECK
   never firing, was refused by the re-check — there is no other way for it to be refused.

Nothing in it depends on timing: every wait is on observed state; the deadlines (20 s) are safety bounds a healthy run (milliseconds) never approaches.

**The mutants** (`consume_contention_selftest.sh`; each applied to a scratch copy, verified to have landed, run against *this test only*, and required to go red with its own signature):

- `norecheck` — the row lock line is removed, so there is no `LockRows` and no re-check. The peer, released, reaches an UPDATE that has no guard of its own. Expected signature `MVCC RE-CHECK PATH FAILED`.
- `nopolicy` — the test's own policy is rewritten so the gate can never be called, so the peer never parks. Expected signature `MVCC forced interleave did not engage` (the vacuity guard: a run that did not force the interleave says so and fails).

## Consequences

- **Production code is unchanged.** Everything added is test code and CI scripts; the policy, function and log exist only in a per-test cloned database and are dropped at the end of the test.
- The re-check path is now covered by a deterministic test with a paired control, on every run, and by the 50-run repeat workflow (`consume-contention-repeat`), which now runs both tests.
- **What this does not prove, said plainly.** (i) It does not assert that the peer's predicate was re-evaluated *by the gate* against the committed version (`seen_count = limit`): the planner may order a user qualifier ahead of a policy qualifier,
  so the gate is not guaranteed to be called again; the log records what the peer was shown and the test prints it, but only the capacity sighting and the refusal are asserted. (ii) The injection point is row-level security: if `accounts` ever lost
  RLS the policy would have no effect, the peer would never park, and the test would go red with "forced interleave did not engage" — red, not silently green. (iii) It is tied to the Postgres behaviour of the pinned `postgres:16-alpine` image.
- **Option B is not the closing argument, but its property is visible.** With the row lock removed (`norecheck`) the stale-snapshot peer is stopped by `accounts_daily_sent_check` alone — SQLSTATE 23514 — so a double booking cannot be *committed* even if
  the re-check were lost (the runner shows the peer's error under that mutant as `ERROR: new row for relation …`, the start of Postgres's CHECK-violation message). That is a backstop that turns a silent over-send into an error, not a substitute for the re-check: the CHECK firing is a defect, and the test treats it as one.
- Reopen trigger: this test red on any runner, a mutant surviving, or a change to the consume statement's locking.

## Links

- Tests and gate: `services/control/internal/dispatch/consume_contention_test.go`, `tests/harness/checks/consume_contention.sh`, `consume_contention_selftest.sh`; the repeat workflow `.github/workflows/consume-contention-repeat.yml`.
- `Docs/future-work.md` FW-M3-22; CLAUDE.md Rules 3 and 10; the run record in `coldmail-platform/verification-runs.md` (seventh pass).
- Module 3 (`coldmail-send-byo`) proved this first, as its ADR-0019; this module's copy of the test, gate and mutants is a port of it, proven separately on the runner.
