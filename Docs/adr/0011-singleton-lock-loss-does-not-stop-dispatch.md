# ADR-0011: a Brain that LOSES the singleton lock mid-life keeps dispatching, so two Brains can dispatch at once

- **Status:** **accepted — option (B) ruled by the user 2026-07-29; implemented and tested on real
  infra the same day** (see "Ruling and what landed" below)
- **Date:** 2026-07-29
- **Found by:** Stage 11 of `Docs/sender-module-local-test-guide.md`, before its step 1 was reached —
  the dev stack was found already in the faulted state, left there by Stage 9 step 8's database-outage
  drill nearly an hour earlier.

## Context

CLAUDE.md Rule 7 locks the singleton: "Brain runs as a SINGLETON guarded by a session-level Postgres
advisory lock on a fixed key, acquired at startup with bounded wait
(`brain_singleton_acquire_timeout_sec`) then fail. SIGKILL auto-releases it (no stale lock). Never add
a second dispatch path or distributed dispatch."

`internal/singleton/singleton.go` implements the **acquire** half correctly and its keeper goroutine
notices loss correctly. On a dead lock connection it logs
`singleton: lock connection lost (...); marking not-held`, sets `held=false`, and **returns** — it
deliberately does not re-acquire, and the comment gives the right reason:

```go
// If the connection ever dies, it marks held=false (readyz then reports the
// truth) and exits — it does NOT try to silently re-acquire, because a dropped session lock
// means another Brain may have taken over, and re-acquiring would be a split-brain risk.
```

The gap is what happens **next**. `held=false` is consumed by exactly one thing: `readyz`. In
`cmd/brain/main.go` the `guard` is passed only to `httpapi.NewHealthServer(...)` (`main.go:205`); no
dispatch, reclaim, retention, or boxlink path consults `guard.Healthy()`. So the process keeps
claiming and pushing jobs after it has provably stopped holding the lock, and the released lock is now
free for any other Brain to take.

The keeper's own comment names the hazard it is avoiding ("another Brain may have taken over") but the
process does not act on it — it keeps dispatching in precisely the situation it just identified as
unsafe.

## Observed, live on the dev stack (2026-07-29)

The faulted state arose on its own, from an ordinary documented drill — not from a contrived setup:

1. Stage 9 step 8 stopped Postgres for 1m48s. At **00:46:55** the Brain logged
   `singleton: lock connection lost (FATAL: terminating connection due to administrator command
   (SQLSTATE 57P01)); marking not-held`. Correct so far.
2. Postgres came back. The keeper had already exited, so the lock was **never re-acquired**:
   `SELECT ... FROM pg_locks WHERE locktype='advisory' AND ((classid::bigint<<32)|objid::bigint)
   = 9200000001` returned **zero rows** — no lock held by anyone, anywhere in the cluster.
3. That Brain nonetheless **kept dispatching for the next 56 minutes**, with
   `dispatch: tick pushed 1 job(s)` at 01:26:33 (×3) and `dispatch: tick pushed 3 job(s)` at 01:43:03.
   `RestartCount` was 0 throughout — one continuous process, dispatching without the lock.
4. `readyz` on that container was honest — `{"db":true,"singleton_lock":false,"status":"not_ready"}`,
   HTTP 503 — and the container was `unhealthy` with `FailingStreak` 1189 (≈59 min at the 3s
   interval). So the state was *visible*; it just wasn't *acted on*.
5. **The consequence, driven rather than argued.** A second Brain was started against the same
   database while the first was still live and dispatching. It **acquired the lock immediately**
   (`singleton lock acquired (key=9200000001)`, no wait) and came fully up: startup rescan, both mTLS
   listeners, admin + platform surfaces, `brain ready`. Both processes were then live against one
   database, each believing it was entitled to dispatch, with a single lock holder (pid 9921) that was
   the *newcomer*. This is the "second dispatch path" Rule 7 exists to make impossible.
6. Removing the second Brain and restarting the first restored the invariant: lock re-acquired,
   `readyz` `ready`, container `healthy`.

A methodology note, because it nearly produced a false reading: `docker compose exec brain ...` also
matches a `docker compose run` container of the same service, and it selected the **newcomer**, whose
`readyz` said `singleton_lock: true`. Addressing the incumbent by container id
(`docker compose ps -q brain`) showed `false`. Probe a specific container id when two share a service
label, or the split-brain hides behind the healthy one.

Separately, and worth recording so this is not mistaken for a broader failure: **step 6 of Stage 11
passes on its own terms.** Against a *healthy* incumbent, a second Brain waited the full bounded
`1m0s`, logged `singleton lock not acquired within 1m0s: another Brain is active; exiting cleanly`,
and exited **code 3** while the incumbent stayed `ready`/`healthy`. The guard works. What is missing
is the mid-life half — a lock LOSS is not a startup CONTENTION, and only the latter was ever tested.

## Why this is filed, not fixed

Three defensible remediations exist and they differ in operational behavior, so choosing one is a
locked-invariant decision for the user (Rule 2), not an implementation detail:

- **(A) Exit.** The keeper, on loss, terminates the process (as a second Brain does at startup). Most
  consistent with Rule 7 — "one dispatcher or none" — and it self-heals under any supervisor with a
  restart policy, since a fresh boot re-acquires cleanly. But a transient DB blip then becomes a Brain
  restart, which costs the graceful-shutdown drain guarantees that Phase 7 built and that Stage 9
  step 8 just proved (the outage drill's whole point was that the Brain rides out an outage with 0
  restarts and invents nothing). Option A trades that away.
- **(B) Pause dispatch, stay up.** Dispatch/reclaim/retention consult `guard.Healthy()` and pause
  while not-held, exactly as they already pause on a DB outage (`no state invented`), with the
  keeper attempting bounded re-acquisition. Preserves the outage posture and the drain path, and it
  reuses a pause mechanism that already exists and is already tested. It leaves a process up that
  holds nothing, which is only safe *because* it dispatches nothing.
- **(C) Re-acquire in the keeper, and only then resume.** Narrowest change, but it is the option the
  keeper's own comment argues against unless paired with (B): re-acquiring while a successor holds
  the lock is impossible (`pg_try_advisory_lock` simply fails), yet re-acquiring *after* a successor
  has come and gone would resume dispatch in a process whose in-memory state — box registrations,
  fairness cursor, in-flight leases — is stale relative to what the successor did.

**(B), with the keeper re-acquiring on a bounded retry, is the recommendation.** It is the only one
that keeps every property the drills have already proven — 0 restarts across an outage, no invented
state, a real drain on shutdown — while closing the window in which a lockless Brain dispatches. (C)
alone is unsafe; (A) is safe but discards Phase 7 work to buy nothing (B) does not already buy.

Whichever is chosen, the test is behavioral per Rule 3 and must be anti-vacuous per Rule 10: kill the
lock connection out from under a live Brain (`pg_terminate_backend` on the lock pid is enough — that
is exactly what `57P01` did here), then assert the observable consequence — for (B), that a job
queued afterwards is **not** dispatched while the lock is unheld, with a paired positive control
proving the same job dispatches once the lock is back. Asserting only "the log says not-held" would
restate the behavior that already works and would have passed against today's broken tree.

## Ruling and what landed (2026-07-29)

**The user ruled option (B): "pause dispatch, bounded re-acquire."** Both halves shipped together —
a pause without recovery would leave a live process permanently useless, and a re-acquire without a
pause is the unsafe (C).

**The pause.** `Dispatcher` gained a leadership predicate (`SetLeadership`/`IsLeader`, an
`atomic.Pointer` to a func), and `main.go` wires it to `guard.Healthy`. The three loop ticks —
`Runner.tick`, `ReclaimRunner.tick`, `RetentionRunner.tick` — check it and return
`notLeaderRetryInterval` when it is false, immediately after the Phase-7 outage gate. The order is
deliberate: a lost lock is usually *caused by* a DB restart, so the outage gate should be the one
logging and setting the cadence. `logNotLeader` throttles a `singleton_lock_not_held … level=error`
line so a long pause does not flood the log while staying visible to the runbook's
`grep 'level=error'`.

**Where the gate sits, and why not lower.** `Dispatcher.Tick`/`ReclaimTick`/`RetentionTick` are the
low-level mechanisms and are **deliberately NOT gated**. The gate belongs in the loop tick because
that is what returns a wait interval; a gate inside `Tick` would have to invent a `Config` to return
and could yield a zero interval and a hot loop. That placement is only safe because the loop ticks are
the *sole* production callers of the three low-level ticks —
`TestLeadershipGate_LowLevelTickIsDeliberatelyUngated` pins that boundary behaviorally rather than
leaving it to a comment a future reader could "fix" the wrong way.

**Deliberately NOT gated: the outcome path**, plus the mTLS listeners and the admin/platform read
surfaces. A box reporting an in-flight send must always be persisted even by a Brain that no longer
holds the lock: that job was pushed while this Brain *was* the leader, the box really did run the SMTP
session, and refusing the report would either invent a non-outcome (Rule 5) or force a needless
duplicate send later. Persisting is idempotent (`provider_message_id` UNIQUE plus the pre-persist
check), so a successor Brain reaching the same row cannot double-count it. None of those surfaces
*claim* work, which is the property the gate actually protects.

**Two gates, not merged.** `DBHealth` asks "can I reach the DB" (halted ⇒ nothing *can* be written);
leadership asks "am I entitled to dispatch" (false ⇒ the DB is reachable and writes *would* succeed —
precisely the danger). Collapsing them would lose that distinction.

**The re-acquire.** `Guard.SetReacquireInterval` gives the keeper a bounded retry cadence, seeded as
`brain_singleton_reacquire_interval_sec` (default 5) by `sender_0023` — config-not-code (Rule 1).
`readSingletonReacquireInterval` falls back to **0 = never re-acquire** on any read/parse failure.
That direction is deliberate and fail-closed: 0 leaves the Brain paused-and-honest (`readyz` 503,
container unhealthy, dispatching nothing) until an operator acts, whereas guessing a cadence would
resume dispatch on a schedule nobody configured.

**An un-wired `Dispatcher` reports itself as leader** (`IsLeader` returns true with no predicate set).
Every dispatch-package test builds a `Dispatcher` with no `Guard`; defaulting to "not leader" would
silently vacuum the entire package. `TestLeadershipGate_UnwiredDispatcherIsLeader` pins that default
as intentional rather than incidental.

**Tests** (`internal/dispatch/leadership_test.go`, 7 tests, real Postgres). The core pair kills the
lock connection with `pg_terminate_backend` — exactly what the observed `57P01` did — waits for the
loss to be *observed* (failing if the flag never flips, so "nothing changed" cannot read as "paused"),
confirms `pg_locks` really shows no lock, then asserts a job queued afterwards stays `queued`/attempt
0 with no push. Its paired positive control flips **one bool** and nothing else. Recovery is asserted
the same way: after the keeper re-acquires unaided, a newly queued job dispatches;
`TestSingletonReacquireDisabledStaysPaused` is its negative control at interval 0, proving recovery is
driven by the seeded key rather than happening unconditionally. Every assertion was confirmed to FAIL
against a deliberately disabled gate.

**Verified on the live stack, not just in the test harness** — because the defect was originally
observed in the real container and `main.go`'s wiring is only exercised there. The image was rebuilt,
the lock backend terminated with `pg_terminate_backend`, and the whole cycle observed end to end:
`pg_locks` went to **zero** holders, the Brain logged the pause, `singleton_lock_not_held` appeared
once (throttled, from the dispatch loop), the keeper logged `lock RE-ACQUIRED … after 5s not-held`,
`pg_locks` returned to **one** holder, `readyz` returned to `ready`, and the container to `healthy` —
with no restart (`RestartCount` unchanged). Contrast the pre-fix behavior on the same drill: zero lock
holders and 56 minutes of continued dispatch. The 58-check harness is green against this binary.

**That live pass found a defect the seven green tests could not.** The first re-acquire log line read
`after 2562047h47m16s not-held` — ~292 years, i.e. `time.Since(zero)`. The loss was being stamped only
in `logReacquireFailure`, so on the common path (the *first* attempt succeeds) nothing ever stamped it.
Every test asserted job state; none looked at the duration, so all seven passed on nonsense output. The
stamp moved to where the loss is *detected*, and `Guard.NotHeldFor()` now exposes the span so a test can
pin it: asserted non-zero **and under a minute** while paused (a one-sided `> 0` would not have caught
it — `time.Since(zero)` is hugely positive), and asserted back to zero after recovery so the two
assertions validate each other. Both directions were confirmed to fail against the reintroduced bug.
The lesson generalizes: a number an operator reads *during an incident* to size the problem is
load-bearing output, and "the tests are green" says nothing about output no test reads.

**A Rule-10 trap found by these tests, recorded so it is not re-derived.** `notLeaderRetryInterval` is
1s and the seeded `dispatch_tick_interval_ms` is **also** 1000ms, so a paused dispatch tick and a fully
successful one return the *same* duration. Asserting `wait == notLeaderRetryInterval` on the dispatch
loop is therefore vacuous (satisfied by both), and asserting `wait != …` on a resumed loop is simply
wrong (it fired on correct behavior and failed the test — observed twice). The dispatch-side tests now
assert **observable state only** — job state, `attempt_count`, push count, `job_events`. The reclaim
(15000ms) and retention (3600000ms) loops have no such collision so their test *can* assert the wait,
but it now **guards that precondition explicitly** against the live config before relying on it, so a
future reseed cannot silently re-introduce the hole. That guard was itself verified by temporarily
seeding a colliding value and watching it fire.

## Consequences of leaving it open

*(Historical — this described the tree BEFORE the option-(B) fix above. Retained because it is the
evidence the ruling was made on. Every item is now closed.)*

- Rule 7's guarantee **was conditional**: it held from startup, but a mid-life
  lock loss silently degrades it. Any DB restart, failover, connection reaper, or
  `pg_terminate_backend` opens the window, and the window stays open until someone restarts the Brain.
- The window is *detectable* today — `readyz` 503, container `unhealthy` — so an orchestrator with a
  liveness probe wired to `readyz` would already restart into safety, which is effectively option (A)
  imposed from outside. The dev stack's healthcheck does not restart, which is exactly why the state
  persisted here for an hour and was available to be found.
- Nothing was corrupted in this instance: the queue drained to `sent`, and the second Brain never had
  a box registered to it (the compose DNS alias `brain` still resolved to the incumbent, so both boxes
  stayed on the first). That is **luck about DNS**, not a property of the design, and it is the reason
  this is filed as a real defect rather than a theoretical one.

## Related

- CLAUDE.md Rule 7 (singleton; never a second dispatch path).
- `internal/singleton/singleton.go` — `keep()` marks not-held and exits; `Healthy()` reads the flag.
- `cmd/brain/main.go:205` — the only consumer of `guard`.
- `Docs/sender-module-local-test-guide.md` Stage 9 step 8 (the drill that created the state) and
  Stage 11 step 6 (which passes on its own terms and did not cover mid-life loss).
- `Docs/future-work.md` — Brain horizontal scaling is locked out by Rule 7, which is what makes the
  singleton load-bearing rather than an optimization.
