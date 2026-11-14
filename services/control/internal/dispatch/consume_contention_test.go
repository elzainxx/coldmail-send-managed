package dispatch

// FORCED contention for the daily-limit ceiling (FW-M3-22).
//
// WHY THIS FILE EXISTS. TestAccountConsume_NeverExceedsDailyLimit used to *hope* its eight
// workers contended: it released them together and then asserted, after the fact, that at least two of them had
// landed a success and that a pg_stat_activity sampler had once seen two backends mid-query. Under FOR UPDATE
// SKIP LOCKED nothing makes the lock fair, so on a fast runner one worker could take all fifty slots before a peer
// got a turn, and the test failed with "only 1 worker(s) ever succeeded" on a commit that changed no executable line
// (module 3, run 43445719147). That is the opposite of what the test is for: it guards the double-booking ceiling, and a race
// that does not race is a test that cannot fail. The invariants (exactly 50, never more) held in the failing attempt;
// what failed was the proof that the race was real.
//
// THE FIX IS NOT A BETTER STATISTIC. It is to stop leaving contention to scheduling at all. forceLastSlotContention
// builds the contended state by construction (CLAUDE.md Rule 10(a), "hold one transaction open at the exact point of
// interest and drive the counterparty against it"):
//
//  1. the account has exactly ONE slot left (daily_sent_count = daily_limit - 1);
//  2. a test-only BEFORE UPDATE trigger on that account's row waits on a Postgres advisory lock the TEST holds, so a
//     consume that has already locked the row and is about to take the last slot PARKS inside its own statement;
//  3. one worker (the holder) is started and is PROVEN parked — a backend of this database, not this session, whose
//     wait_event is Lock/advisory and whose query is the consume statement (Rule 10(b), scoped by datname + query
//     fragment + wait event, never "some backend somewhere");
//  4. the other seven workers are then released at once against a row that is locked and not yet consumed. They must
//     ALL be refused — while capacity visibly remains (daily_sent_count is still limit-1), so a refusal here can only
//     be contention, never exhaustion — and they must complete while the holder is still parked ("the blocked side does
//     not complete while the lock is held": a peer that waited on the row instead of skipping it would hang here);
//  5. the gate is released, the holder completes and takes the last slot, and every worker is refused again.
//
// Nothing in steps 1-5 depends on timing: the only waits are on observed state, and the deadlines are safety bounds
// that a healthy run never approaches (it finishes in milliseconds). The paired controls live in
// tests/harness/checks/consume_contention_selftest.sh, which builds three mutants of the production code and requires
// each to turn THIS test red with its own signature: SKIP LOCKED removed, the consume serialised behind one mutex, and
// the daily-limit predicate removed.
//
// WHAT THIS DOES NOT FORCE, said plainly. The MVCC re-check path (a peer whose statement snapshot predates the
// holder's commit but whose scan follows it) cannot be driven deterministically from outside the statement: SKIP
// LOCKED means a peer never waits between snapshot and scan, so there is no point at which to hold it. The volume
// phase of the test (eight workers racing fifty slots) still exercises that path, and still asserts every invariant
// (exactly 50, none after the limit, no SQLSTATE 23514), but it no longer asserts HOW MUCH overlap occurred — that is
// logged, not judged, because it is exactly what scheduling decides.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// consumeGateKey is the advisory-lock key the gate trigger waits on. Advisory locks are scoped to a database and every
// dispatch test runs in its own cloned database, so this key cannot collide with another test's.
const consumeGateKey int64 = 7311042

// The gate: a function that waits for a SHARED advisory lock, and a trigger that calls it before the account row is
// updated. The test holds the EXCLUSIVE lock, so every consume that reaches the UPDATE parks until the test releases it.
// The WHEN clause scopes it to the one account under test.
const consumeGateFuncDDL = `
CREATE OR REPLACE FUNCTION public.zz_consume_gate() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
  PERFORM pg_advisory_xact_lock_shared(%d::bigint);
  RETURN NEW;
END
$fn$`

const consumeGateTriggerDDL = `
CREATE TRIGGER aaa_zz_consume_gate BEFORE UPDATE OF daily_sent_count ON send_managed.accounts
FOR EACH ROW WHEN (OLD.id = '%s'::uuid) EXECUTE FUNCTION public.zz_consume_gate()`

// consumeResult is one worker's outcome: ok means a slot was consumed, err means something other than a refusal.
type consumeResult struct {
	worker int
	ok     bool
	err    error
}

// blockedConsumes counts consume statements, in THIS database and not in this session, that are parked on the gate.
// The three filters are the ones CLAUDE.md Rule 10 requires: datname, a query fragment unique to the statement under test,
// and the specific wait event. pid <> pg_backend_pid() keeps this poll from matching itself (its own text contains the
// fragment).
func blockedConsumes(t *testing.T, e *dispatchEnv) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity
		  WHERE datname = current_database() AND pid <> pg_backend_pid()
		    AND state = 'active' AND wait_event_type = 'Lock' AND wait_event = 'advisory'
		    AND query ILIKE '%eligible_accounts%'`).Scan(&n); err != nil {
		t.Fatalf("blockedConsumes: %v", err)
	}
	return n
}

// forceLastSlotContention drives the five steps in the file comment against the ONE account acct. consume[w] is worker
// w's own call into accountConsume (each over its own pre-connected pool); consume[0] is the holder. On return the gate
// trigger is dropped and the account sits at its limit; the caller resets it for whatever runs next.
func forceLastSlotContention(t *testing.T, e *dispatchEnv, acct string, limit int, consume []func() (bool, error)) {
	t.Helper()
	ctx := context.Background()
	workers := len(consume)
	if workers < 3 {
		t.Fatalf("forceLastSlotContention needs a holder and at least two peers, got %d workers", workers)
	}
	const safety = 20 * time.Second

	// Step 1 and 2: one slot left, the gate installed, then the TEST takes the exclusive gate lock.
	e.setAccountDailyState(t, acct, limit, limit-1, todayUTC(), false, nil)
	if _, err := e.admin.Exec(ctx, fmt.Sprintf(consumeGateFuncDDL, consumeGateKey)); err != nil {
		t.Fatalf("install the gate function: %v", err)
	}
	if _, err := e.admin.Exec(ctx, fmt.Sprintf(consumeGateTriggerDDL, acct)); err != nil {
		t.Fatalf("install the gate trigger: %v", err)
	}
	if _, err := e.admin.Exec(ctx, `SELECT pg_advisory_lock($1)`, consumeGateKey); err != nil {
		t.Fatalf("take the gate lock: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if _, err := e.admin.Exec(ctx, `SELECT pg_advisory_unlock($1)`, consumeGateKey); err != nil {
			t.Errorf("release the gate lock: %v", err)
		}
	}
	defer release()

	var wg sync.WaitGroup
	// fail releases the gate and lets every goroutine finish before failing, so a red run never leaves a worker parked
	// on a pool the test is about to close.
	fail := func(format string, args ...any) {
		t.Helper()
		release()
		wg.Wait()
		t.Fatalf(format, args...)
	}

	// Step 3: the holder, proven parked before anything else is released.
	holderCh := make(chan consumeResult, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ok, err := consume[0]()
		holderCh <- consumeResult{worker: 0, ok: ok, err: err}
	}()
	parkedBy := time.Now().Add(safety)
	for blockedConsumes(t, e) != 1 {
		select {
		case r := <-holderCh:
			fail("the holder returned (ok=%v err=%v) instead of parking on the gate — the forced interleave did not engage, so this run proves nothing", r.ok, r.err)
		default:
		}
		if time.Now().After(parkedBy) {
			fail("no consume was ever observed parked on the gate within %s — the forced interleave did not engage, so this run proves nothing", safety)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Step 4: the peers, released together against a row that is locked and not yet consumed.
	peerCh := make(chan consumeResult, workers-1)
	start := make(chan struct{})
	for w := 1; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			ok, err := consume[w]()
			peerCh <- consumeResult{worker: w, ok: ok, err: err}
		}(w)
	}
	close(start)
	peers := make([]consumeResult, 0, workers-1)
	deadline := time.After(safety)
	for len(peers) < workers-1 {
		select {
		case r := <-peerCh:
			peers = append(peers, r)
		case <-deadline:
			fail("FORCED CONTENTION BROKEN: only %d of %d peer consumes completed while the holder was parked on the gate — a peer must be REFUSED by SKIP LOCKED, never wait on the locked row or on another consume", len(peers), workers-1)
		}
	}
	for _, r := range peers {
		if r.err != nil {
			fail("FORCED CONTENTION BROKEN: peer %d returned an error instead of a refusal while the holder was parked (a peer that waits on the locked row times out here): %v", r.worker, r.err)
		}
		if r.ok {
			fail("DOUBLE BOOKING: peer %d consumed a slot while the holder held the last one (daily_limit=%d)", r.worker, limit)
		}
	}
	select {
	case r := <-holderCh:
		fail("the holder completed (ok=%v err=%v) while the gate was still held — the park was not real", r.ok, r.err)
	default:
	}
	if n := blockedConsumes(t, e); n != 1 {
		fail("expected exactly one consume still parked after the peers were refused, saw %d", n)
	}
	if c := e.accountDailySentCount(t, acct); c != limit-1 {
		fail("daily_sent_count=%d while the holder is parked, want %d — capacity must still remain, or the peers' refusals were exhaustion and not contention", c, limit-1)
	}

	// Step 5: release the gate; the holder takes the last slot; capacity is then gone for everyone.
	release()
	select {
	case r := <-holderCh:
		if r.err != nil {
			fail("the holder errored after the gate was released: %v", r.err)
		}
		if !r.ok {
			fail("the holder lost the last slot it had locked")
		}
	case <-time.After(safety):
		fail("the holder did not complete within %s of the gate being released", safety)
	}
	wg.Wait()
	if c := e.accountDailySentCount(t, acct); c != limit {
		t.Fatalf("daily_sent_count=%d after the holder completed, want exactly %d", c, limit)
	}
	for w := range consume {
		ok, err := consume[w]()
		if err != nil {
			t.Fatalf("worker %d errored once capacity was gone (SQLSTATE 23514 here would mean the CHECK, the last resort, was needed): %v", w, err)
		}
		if ok {
			t.Fatalf("OVER-CONSUMPTION: worker %d consumed a slot after the daily_limit of %d was reached", w, limit)
		}
	}
	if c := e.accountDailySentCount(t, acct); c != limit {
		t.Fatalf("daily_sent_count=%d after the confirm round, want exactly %d", c, limit)
	}
	if _, err := e.admin.Exec(ctx, `DROP TRIGGER aaa_zz_consume_gate ON send_managed.accounts`); err != nil {
		t.Fatalf("drop the gate trigger: %v", err)
	}
	t.Logf("FORCED CONTENTION: 1 holder parked on the last slot (observed via pg_stat_activity), %d peers released against it and ALL refused while daily_sent_count was still %d; holder then took the slot, %d workers refused again",
		workers-1, limit-1, workers)
}
