package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-send-managed/control/internal/singleton"
)

// leadership_test.go — G11-OD-1 / ADR-0011 (option B, ruled by the user 2026-07-29).
//
// THE DEFECT THIS LOCKS DOWN. A Control that LOST the singleton advisory lock mid-life kept
// dispatching: singleton.keep() marked held=false and exited, but held=false was read by readyz and
// NOTHING else. Observed live on the dev stack — a DB restart dropped the lock at 00:46:55 and that
// same process (RestartCount 0) kept pushing jobs for 56 minutes, while a second Control acquired the
// now-free lock and came fully up. Two dispatchers, one database: exactly what CLAUDE.md Rule 7
// exists to make impossible.
//
// WHY THESE ASSERTIONS AND NOT THE OBVIOUS ONE (CLAUDE.md Rule 10). Asserting "the log says
// not-held" or "guard.Healthy() == false" would restate the behavior that ALREADY WORKED and would
// have passed green against the broken tree — the keeper always set the flag correctly. The only
// assertion that can distinguish fixed from broken is the OBSERVABLE CONSEQUENCE: a job queued while
// the lock is unheld must NOT be claimed or pushed. Every test below therefore asserts on job state
// and push count, and each carries the paired positive control that proves the same job DOES dispatch
// when leadership is held — without which "nothing dispatched" is indistinguishable from a broken
// fixture.

// TestLeadershipGate_NoDispatchWhileLockUnheld is the core anti-regression test: the same job, the
// same dispatcher, the same config — dispatched or not dispatched purely as a function of whether
// this Control holds the singleton lock.
func TestLeadershipGate_NoDispatchWhileLockUnheld(t *testing.T) {
	env := newDispatchEnv(t, "leadergate")
	ctx := context.Background()

	poolID := env.seedPool(t)
	env.seedAccount(t, poolID, "s3cret")
	env.addWorker(t)

	// --- NEGATIVE CONTROL FIRST (leadership held == false) -----------------------------------
	// Deliberately ordered before the positive control. If the gate were absent, this half would
	// dispatch and fail here; running it first means the test cannot accidentally pass because a
	// later positive control left the system in a dispatched state.
	// One mutable flag drives both halves, so the ONLY difference between the negative and positive
	// control is this bool — not a re-seeded job, a re-built dispatcher, or a second fixture.
	leader := false
	env.d.SetLeadership(func(context.Context) bool { return leader })

	jobID := env.seedQueuedJob(t, "gated@example.com")

	pushesBefore := env.px.pushCount()
	// Drive the REAL gated path: the Runner's tick. Dispatcher.Tick() is the low-level mechanism and
	// is deliberately NOT gated — the gate lives in the loop tick, where the pause/backoff decision
	// belongs (it must return a wait interval, and a gate inside Tick would have to invent a Config
	// to return, risking a zero interval and a hot loop). That placement is safe because the loop
	// ticks are the ONLY production callers of Tick/ReclaimTick/RetentionTick; asserted below.
	//
	// NOTE ON WHY THIS DOES NOT ASSERT THE RETURNED WAIT (Rule 10, learned by this test failing):
	// notLeaderRetryInterval is 1s and the seeded dispatch_tick_interval_ms is ALSO 1000ms, so a
	// paused tick and a fully successful leader tick return the SAME duration. `wait ==
	// notLeaderRetryInterval` therefore distinguishes nothing here — it passed for the paused half and
	// then failed the positive control, which is how the ambiguity surfaced. The gate's effect is
	// asserted on OBSERVABLE STATE (job state, attempt_count, push count, job_events) below, which
	// cannot collide with a config value.
	r := NewRunner(env.d)
	for i := 0; i < 3; i++ {
		r.tick(ctx)
	}

	state, hasPMID, _, _, attempt := env.jobState(t, jobID)
	if state != "queued" {
		t.Fatalf("job state = %q after 3 non-leader ticks, want %q — a Control that does not hold the singleton lock CLAIMED a job (this is the G11-OD-1 defect)", state, "queued")
	}
	if attempt != 0 {
		t.Fatalf("attempt_count = %d after 3 non-leader ticks, want 0 — dispatch burned an attempt without holding the lock", attempt)
	}
	if hasPMID {
		t.Fatal("job has a provider_message_id after non-leader ticks — an outcome was invented (Rule 5)")
	}
	if got := env.px.pushCount(); got != pushesBefore {
		t.Fatalf("pusher saw %d push(es) as non-leader, want %d — a lockless Control PUSHED a job to a worker with a decrypted credential", got-pushesBefore, 0)
	}
	// The job must be untouched, not merely un-sent: exactly the genesis event, no churn.
	if n := env.eventCount(t, jobID); n != 1 {
		t.Fatalf("job_events rows = %d after non-leader ticks, want exactly 1 (the genesis row) — the paused loop wrote job state", n)
	}

	// --- PAIRED POSITIVE CONTROL (leadership held == true) -----------------------------------
	// Without this, "nothing dispatched" above proves nothing: a broken fixture (no account, no worker,
	// unroutable pool) would produce identical assertions. Flip ONLY leadership — same job, same
	// dispatcher, same config, same gate function; one bool changes.
	leader = true
	r.tick(ctx)

	state, _, _, _, attempt = env.jobState(t, jobID)
	if state == "queued" {
		t.Fatal("POSITIVE CONTROL FAILED: the very same job did not dispatch even as leader, so the negative control above proves nothing about the leadership gate — fixture is broken (check pool/account/worker seeding)")
	}
	if got := env.px.pushCount(); got != pushesBefore+1 {
		t.Fatalf("POSITIVE CONTROL FAILED: pusher saw %d push(es) as leader, want exactly 1 — the negative control is not attributable to the gate", got-pushesBefore)
	}
	t.Logf("positive control: same job dispatched as leader (state=%q attempt=%d, 1 push) — the negative control above is attributable to leadership alone", state, attempt)
}

// TestLeadershipGate_LowLevelTickIsDeliberatelyUngated pins the GATE BOUNDARY, which the test above
// depends on and which a future reader could easily "fix" the wrong way.
//
// Dispatcher.Tick/ReclaimTick/RetentionTick are the low-level mechanisms and are NOT leadership-gated;
// the gate sits in the three Runner ticks. That is safe ONLY because those runners are the sole
// production callers (main.go wires nothing else to Tick), so this test states the boundary as
// executable fact rather than a comment: called directly, Tick still dispatches even with leadership
// false. If someone later moves the gate down into Tick, this test fails and tells them why the split
// exists — and if someone adds a second, ungated production dispatch path, the paired runner test
// above is what still catches the escape.
//
// It is written as "the ungated path DOES dispatch" rather than "no other caller exists" because the
// former is a behavioral assertion on real infra (Rule 3), while the latter would be a grep.
func TestLeadershipGate_LowLevelTickIsDeliberatelyUngated(t *testing.T) {
	env := newDispatchEnv(t, "leadergateboundary")
	ctx := context.Background()

	poolID := env.seedPool(t)
	env.seedAccount(t, poolID, "s3cret")
	env.addWorker(t)

	env.d.SetLeadership(func(context.Context) bool { return false })
	jobID := env.seedQueuedJob(t, "ungated@example.com")

	if _, _, err := env.d.Tick(ctx); err != nil {
		t.Fatalf("Dispatcher.Tick returned an error: %v", err)
	}
	if st, _, _, _, _ := env.jobState(t, jobID); st == "queued" {
		t.Fatal("Dispatcher.Tick did not dispatch with leadership=false — the gate has been moved INTO the low-level Tick. That is not automatically wrong, but the Runner-level gate is what returns the pause interval; if you moved it, update leadership_test.go's boundary reasoning and make sure the runner still backs off instead of hot-looping.")
	}
}

// TestLeadershipGate_ReclaimAndRetentionAlsoPause covers the other two gated loops. Reclaim is the
// one that matters most: it REASSIGNS in-flight jobs (sending -> queued), so a non-leader running it
// would yank a job out from under the worker a LEADER Control is currently pushing to — a double-dispatch
// rather than a merely-redundant one.
func TestLeadershipGate_ReclaimAndRetentionAlsoPause(t *testing.T) {
	env := newDispatchEnv(t, "leadergateloops")
	ctx := context.Background()

	// GUARD FIRST (Rule 10). Unlike the dispatch test above, this one CAN assert the returned wait —
	// but only because the seeded reclaim/retention intervals differ from notLeaderRetryInterval. That
	// is a config coincidence, not a property, and the identical assertion in the dispatch test was
	// VACUOUS for exactly this reason (dispatch_tick_interval_ms = 1000ms == notLeaderRetryInterval, so
	// a paused tick and a successful tick returned the same duration; the negative control passed and
	// the positive control then failed). So prove the two durations are distinguishable HERE, before
	// relying on them, rather than leaving a future reseed to silently re-introduce the same hole.
	cfg, err := LoadConfig(ctx, env.pool)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if d := time.Duration(cfg.ReclaimTickIntervalMS) * time.Millisecond; d == notLeaderRetryInterval {
		t.Fatalf("reclaim_tick_interval_ms is %s, which EQUALS notLeaderRetryInterval (%s) — the wait-duration assertions below cannot distinguish a paused tick from a successful one and would be vacuous. Assert on observable state instead (see TestLeadershipGate_NoDispatchWhileLockUnheld, which hit this exact collision).", d, notLeaderRetryInterval)
	}
	if d := time.Duration(cfg.RetentionSweepIntervalMS) * time.Millisecond; d == notLeaderRetryInterval {
		t.Fatalf("retention_sweep_interval_ms is %s, which EQUALS notLeaderRetryInterval (%s) — same vacuity as above.", d, notLeaderRetryInterval)
	}

	env.d.SetLeadership(func(context.Context) bool { return false })

	rr := NewReclaimRunner(env.d)
	if wait := rr.tick(ctx); wait != notLeaderRetryInterval {
		t.Fatalf("reclaim tick as non-leader returned wait %s, want %s — the reclaim loop is NOT gated on leadership, so a lockless Control can requeue a job a leader is sending", wait, notLeaderRetryInterval)
	}
	ret := NewRetentionRunner(env.d)
	if wait := ret.tick(ctx); wait != notLeaderRetryInterval {
		t.Fatalf("retention tick as non-leader returned wait %s, want %s — the retention sweep is NOT gated on leadership, so a lockless Control DELETEs send_managed.* rows", wait, notLeaderRetryInterval)
	}

	// Paired positive control: both loops run normally as leader. Otherwise a permanently-broken
	// tick (a panic recovered into the default interval, say) would satisfy the assertions above.
	env.d.SetLeadership(func(context.Context) bool { return true })
	if wait := rr.tick(ctx); wait == notLeaderRetryInterval {
		t.Fatal("POSITIVE CONTROL FAILED: reclaim tick as leader still returned the not-leader interval — the gate is stuck closed")
	}
	if wait := ret.tick(ctx); wait == notLeaderRetryInterval {
		t.Fatal("POSITIVE CONTROL FAILED: retention tick as leader still returned the not-leader interval — the gate is stuck closed")
	}
}

// TestLeadershipGate_UnwiredDispatcherIsLeader pins the default. Every dispatch-package test
// constructs a Dispatcher without a Guard and holds no advisory lock; if an un-wired Dispatcher
// defaulted to "not leader" the entire suite would silently stop dispatching and every behavioral
// assertion in the package would become vacuous. This asserts the default is permissive on purpose.
func TestLeadershipGate_UnwiredDispatcherIsLeader(t *testing.T) {
	env := newDispatchEnv(t, "leadergatedefault")
	if !env.d.IsLeader() {
		t.Fatal("a Dispatcher with no leadership gate wired reports NOT leader — this would silently vacuum every other test in this package")
	}
	// And an explicitly nil gate resets to the same permissive default rather than latching false.
	env.d.SetLeadership(func(context.Context) bool { return false })
	if env.d.IsLeader() {
		t.Fatal("SetLeadership(false-returning fn) did not take effect")
	}
	env.d.SetLeadership(nil)
	if !env.d.IsLeader() {
		t.Fatal("SetLeadership(nil) left the gate closed; it must reset to the permissive default")
	}
}

// TestSingletonRealLockLossPausesDispatch is the end-to-end version against a REAL Postgres advisory
// lock, killed the way production kills it. The test above proves the gate works given a leadership
// signal; this proves the signal itself goes false on a genuine loss and that the two halves are
// actually connected — which is precisely the wiring that was missing (guard reached readyz only).
//
// The loss is induced with pg_terminate_backend on the lock-holding backend, which is exactly the
// 57P01 that a DB restart delivered on the live stack.
func TestSingletonRealLockLossPausesDispatch(t *testing.T) {
	env := newDispatchEnv(t, "leaderreallock")
	ctx := context.Background()

	poolID := env.seedPool(t)
	env.seedAccount(t, poolID, "s3cret")
	env.addWorker(t)

	// A real Guard holding a real session-level advisory lock on this clone.
	guard, err := singleton.Acquire(ctx, env.moduleDSN, 5*time.Second)
	if err != nil {
		t.Fatalf("acquire singleton lock on the clone: %v", err)
	}
	defer guard.Release(context.Background())
	// Re-acquisition OFF for this test: we assert the PAUSE, and a keeper racing to re-acquire would
	// make "did it stay paused" timing-dependent. Re-acquisition gets its own test below.
	guard.SetReacquireInterval(0)

	env.d.SetLeadership(guard.Healthy)
	r := NewRunner(env.d)

	// ANTI-VACUITY PRECONDITION: prove the lock is genuinely held by this session before killing it.
	// Without this, a test that never acquired the lock would "pass" the pause assertions trivially.
	if !guard.Healthy(ctx) {
		t.Fatal("precondition failed: guard reports not-held immediately after Acquire")
	}
	if !advisoryLockHeld(t, env.admin) {
		t.Fatal("precondition failed: no advisory lock on the singleton key is visible in pg_locks — the test would prove nothing")
	}

	// Positive control FIRST here (the inverse order of the test above, deliberately): prove this
	// fixture dispatches while the lock is genuinely held, so the post-kill silence is attributable.
	warmup := env.seedQueuedJob(t, "before-kill@example.com")
	r.tick(ctx)
	if st, _, _, _, _ := env.jobState(t, warmup); st == "queued" {
		t.Fatal("POSITIVE CONTROL FAILED: a job did not dispatch while the singleton lock WAS held — fixture broken, the post-kill assertions would prove nothing")
	}
	pushesAfterWarmup := env.px.pushCount()

	// --- Kill the lock connection the way production does: terminate its backend. ---
	killLockBackend(t, env.admin)

	// The keeper notices on its 5s verify tick. Wait for held=false rather than sleeping a guessed
	// interval, and FAIL if it never flips — "the flag never changed" must not read as "paused".
	deadline := time.Now().Add(20 * time.Second)
	for guard.Healthy(ctx) {
		if time.Now().After(deadline) {
			t.Fatal("guard still reports held 20s after its lock backend was terminated — the keeper did not notice the loss")
		}
		time.Sleep(250 * time.Millisecond)
	}
	// The lock really is gone from the cluster, not just from our flag.
	if advisoryLockHeld(t, env.admin) {
		t.Fatal("pg_locks still shows the singleton advisory lock after the backend was terminated — the loss was not real")
	}

	// THE ASSERTION. A job queued now must not be claimed or pushed.
	//
	// Asserted on OBSERVABLE STATE only, never on the returned wait: notLeaderRetryInterval is 1s and
	// the seeded dispatch_tick_interval_ms is ALSO 1000ms, so a paused dispatch tick and a fully
	// successful one return the SAME duration. `wait == notLeaderRetryInterval` would therefore be
	// satisfied by BOTH — it would pass against the broken tree it exists to catch. (The reclaim and
	// retention loops do NOT have this collision, so the sibling test can assert their waits; it
	// guards that precondition explicitly rather than trusting the seed.)
	gated := env.seedQueuedJob(t, "after-kill@example.com")
	for i := 0; i < 3; i++ {
		r.tick(ctx)
	}
	if st, _, _, _, attempt := env.jobState(t, gated); st != "queued" || attempt != 0 {
		t.Fatalf("job after real lock loss: state=%q attempt=%d, want queued/0 — a Control with NO advisory lock claimed a job", st, attempt)
	}
	if got := env.px.pushCount(); got != pushesAfterWarmup {
		t.Fatalf("pusher saw %d additional push(es) after a real lock loss, want 0 — a lockless Control pushed a decrypted credential to a worker", got-pushesAfterWarmup)
	}
	t.Logf("real lock loss: dispatch paused, job %s stayed queued with 0 attempts", gated)
}

// TestSingletonReacquireResumesDispatch is the other half of option B: the Control does not merely
// pause forever, it comes back on its own. Asserted as a consequence (the previously-gated job
// dispatches) rather than as a log line or a flag flip.
func TestSingletonReacquireResumesDispatch(t *testing.T) {
	env := newDispatchEnv(t, "leaderreacquire")
	ctx := context.Background()

	poolID := env.seedPool(t)
	env.seedAccount(t, poolID, "s3cret")
	env.addWorker(t)

	guard, err := singleton.Acquire(ctx, env.moduleDSN, 5*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer guard.Release(context.Background())
	guard.SetReacquireInterval(1 * time.Second) // the seeded default is 5s; 1s keeps the test brisk
	env.d.SetLeadership(guard.Healthy)
	r := NewRunner(env.d)

	if !advisoryLockHeld(t, env.admin) {
		t.Fatal("precondition failed: lock not visible in pg_locks after Acquire")
	}
	killLockBackend(t, env.admin)

	// Wait for the loss to be observed...
	lost := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if !guard.Healthy(ctx) {
			lost = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !lost {
		t.Fatal("guard never reported not-held after its backend was terminated — nothing to re-acquire, test would be vacuous")
	}

	// THE REPORTED PAUSE DURATION IS REAL, not time.Since(zero). This assertion exists because the
	// first version of this code stamped the loss only inside logReacquireFailure — so on the common
	// path, where the FIRST re-acquire attempt succeeds, nothing ever stamped it and the recovery line
	// printed "after 2562047h47m16s not-held" (~292 years). Seven behavioral tests passed anyway,
	// because every one of them asserted on job state and none looked at the duration. It was caught
	// only by reading the live stack's log. An operator reads this number during an incident to decide
	// whether a pause is a 5-second blip or a half-hour outage, so it is load-bearing output, and it is
	// now pinned here rather than by eyeballing a log line.
	//
	// Bounds, not an exact value: it must be positive (something was stamped) and small (this loop
	// polls at 200ms and gives up at 20s, so a real span cannot exceed that by much). A one-sided
	// "> 0" check would NOT have caught the bug — time.Since(zero) is hugely positive. The upper bound
	// is what makes this test able to fail.
	if d := guard.NotHeldFor(); d <= 0 || d > time.Minute {
		t.Fatalf("Guard.NotHeldFor() = %s while the lock is genuinely not held, want a positive span under a minute — the not-held stamp is wrong (a value near 2562047h means time.Since(zero): the loss is not being stamped when it is DETECTED)", d)
	}

	// ...then for the keeper to get it back on its own. No restart, no operator action.
	regained := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if guard.Healthy(ctx) {
			regained = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !regained {
		t.Fatal("guard never re-acquired the lock within 30s despite brain_singleton_reacquire_interval_sec=1 — ADR-0011 option B's recovery half does not work")
	}
	if !advisoryLockHeld(t, env.admin) {
		t.Fatal("guard reports held but pg_locks shows no advisory lock on the singleton key — held=true is a lie")
	}
	// ...and the not-held span is CLEARED on recovery, so a healthy Control never reports a stale pause.
	// The pair of assertions (non-zero while paused, zero once recovered) is what makes each one
	// meaningful: a NotHeldFor that always returned 0 would satisfy this line, and one that never
	// cleared would satisfy the line above.
	if d := guard.NotHeldFor(); d != 0 {
		t.Fatalf("Guard.NotHeldFor() = %s after the lock was re-acquired, want 0 — the not-held stamp is not cleared on recovery, so a healthy Control reports a pause that ended", d)
	}

	// THE ASSERTION: dispatch actually resumes. A flag flip alone would not prove the loops recovered.
	// State-only, for the same reason as the test above — and here the duration check was not merely
	// vacuous but WRONG: a successful dispatch tick returns dispatch_tick_interval_ms = 1000ms, which
	// IS notLeaderRetryInterval, so `wait == notLeaderRetryInterval` fired on a correctly-resumed loop
	// and failed the test (observed). The job's state is the unambiguous evidence.
	jobID := env.seedQueuedJob(t, "after-reacquire@example.com")
	r.tick(ctx)
	if st, _, _, _, _ := env.jobState(t, jobID); st == "queued" {
		t.Fatal("job stayed queued after the lock was re-acquired — dispatch did not resume (ADR-0011 option B is only half-working: it pauses but never recovers)")
	}
	t.Log("re-acquired the lock without a restart and dispatch resumed")
}

// TestSingletonReacquireDisabledStaysPaused pins the fail-closed direction: interval 0 (the value
// main.go falls back to when the config row is unreadable) must NOT re-acquire. This is the negative
// control for the test above — it proves the re-acquisition is driven by the seeded interval rather
// than happening unconditionally, which is what makes the config key meaningful (Rule 1).
func TestSingletonReacquireDisabledStaysPaused(t *testing.T) {
	env := newDispatchEnv(t, "leadernoreacquire")
	ctx := context.Background()

	guard, err := singleton.Acquire(ctx, env.moduleDSN, 5*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer guard.Release(context.Background())
	guard.SetReacquireInterval(0) // disabled

	killLockBackend(t, env.admin)
	for deadline := time.Now().Add(20 * time.Second); guard.Healthy(ctx); {
		if time.Now().After(deadline) {
			t.Fatal("guard never reported not-held; nothing to assert")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Well past several 5s keeper ticks: it must still be not-held.
	time.Sleep(12 * time.Second)
	if guard.Healthy(ctx) {
		t.Fatal("guard re-acquired the lock with brain_singleton_reacquire_interval_sec=0 — re-acquisition ignores its config key, so the seeded value is decorative")
	}
	if advisoryLockHeld(t, env.admin) {
		t.Fatal("an advisory lock on the singleton key reappeared with re-acquisition disabled")
	}
}

// --- helpers ---

// advisoryLockHeld reports whether ANY session in this database holds the Control singleton advisory
// lock. Scoped to the current database (each dispatch test runs on its own clone) and to the exact
// key, reassembled from classid/objid the same way Docs/architecture.md#advisory-lock-key-registry documents.
func advisoryLockHeld(t *testing.T, admin *pgx.Conn) bool {
	t.Helper()
	var n int
	err := admin.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_locks l
		 JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory'
		  AND ((l.classid::bigint << 32) | l.objid::bigint) = $1
		  AND a.datname = current_database()`, singleton.LockKey).Scan(&n)
	if err != nil {
		t.Fatalf("pg_locks probe: %v", err)
	}
	return n > 0
}

// killLockBackend terminates the backend holding the singleton advisory lock — the same 57P01 a
// Postgres restart delivers, which is how this defect was found. Fails the test if there was no such
// backend to kill, so a vacuous run (nothing terminated, therefore nothing to notice) cannot pass.
func killLockBackend(t *testing.T, admin *pgx.Conn) {
	t.Helper()
	var killed int
	err := admin.QueryRow(context.Background(), `
		SELECT count(*) FROM (
		  SELECT pg_terminate_backend(l.pid)
		    FROM pg_locks l
		    JOIN pg_stat_activity a ON a.pid = l.pid
		   WHERE l.locktype = 'advisory'
		     AND ((l.classid::bigint << 32) | l.objid::bigint) = $1
		     AND a.datname = current_database()
		) AS t`, singleton.LockKey).Scan(&killed)
	if err != nil {
		t.Fatalf("pg_terminate_backend on the lock holder: %v", err)
	}
	if killed == 0 {
		t.Fatal("no backend held the singleton advisory lock, so nothing was terminated — this test would prove nothing (anti-vacuity, Rule 10)")
	}
}
