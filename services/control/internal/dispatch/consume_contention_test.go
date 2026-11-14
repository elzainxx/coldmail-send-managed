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
// tests/harness/checks/consume_contention_selftest.sh, which builds four mutants for the test in this paragraph and
// requires each to turn it red with its own signature: SKIP LOCKED removed, the consume serialised behind one mutex,
// the daily-limit predicate removed, and the gate trigger disarmed.
//
// THE SECOND PATH, forced by its own test (ADR-0015). Everything above forces what happens while a holder keeps the row
// LOCKED: the peers are refused by SKIP LOCKED. The other way a peer is refused is the MVCC RE-CHECK: a peer whose statement
// snapshot PREDATES the holder's commit but whose row lock FOLLOWS it. Its scan saw capacity; the holder then took the last
// slot and committed; when the peer reaches the row it is no longer locked, it was UPDATED, so Postgres follows the update
// chain and re-evaluates the eligibility predicate against the committed version (EvalPlanQual). Only that re-check refuses
// the peer. There is no point OUTSIDE the statement at which to hold a peer between its snapshot and its lock, but there is one
// INSIDE it: a restrictive row-level-security policy whose predicate calls a volatile function is evaluated in the peer's scan —
// after its snapshot, before LockRows. TestAccountConsume_StaleSnapshotPeerIsRefused installs one (test-only DDL; production
// code is untouched), parks ONLY the backend whose application_name is zz_mvcc_peer on a test-held advisory lock, lets the holder
// take the last slot and COMMIT, releases the peer, and requires it to be refused (no error, the count stays at the limit). That
// the peer's scan saw capacity is proved from the gate's own log (a row with seen_count = limit-1 written by the peer, which can
// only be the version that existed before the holder's commit); that it was refused after the holder's commit is the outcome.
// Its mutants (consume_contention_selftest.sh): the row lock removed, which removes the re-check, so only the CHECK constraint
// is left between the peer and a double booking; and the gate disarmed, so the test must say the interleave was not forced.
//
// What stays outside both tests, said plainly: the volume phase of TestAccountConsume_NeverExceedsDailyLimit
// (eight workers racing fifty slots) asserts every invariant (exactly 50, none after the limit, no SQLSTATE 23514) but only LOGS
// how much the workers overlapped, because that is exactly what scheduling decides.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// ---------------------------------------------------------------------------------------------------------------------------
// The MVCC re-check path (ADR-0015).

const (
	mvccPeerApp   = "zz_mvcc_peer"
	mvccHolderApp = "zz_mvcc_holder"
)

// mvccGateKey is the advisory-lock key the MVCC gate waits on; it differs from consumeGateKey so the two tests can never interfere.
const mvccGateKey int64 = 7311043

// mvccGateLogDDL is the gate's own log. The gate writes a row for every version of the account row it is asked about, from inside the
// statement under test; the rows become visible when that statement commits, which is when the test reads them.
const mvccGateLogDDL = `
CREATE TABLE public.zz_mvcc_gate_log (
	n           bigserial PRIMARY KEY,
	app         text NOT NULL,
	backend_pid int  NOT NULL,
	seen_count  int  NOT NULL,
	seen_limit  int  NOT NULL
)`

// mvccGateFuncDDL: args are the account id, the application_name that must park, and the advisory-lock key. The function is VOLATILE
// and SECURITY DEFINER (so the module role may append to the log without any grant), answers true for every row of every other
// account, and for the account under test records what it was shown and — for the peer ONLY — waits for a SHARED advisory lock the
// test holds exclusively. It always returns true: it never decides eligibility, it only decides WHEN the peer's scan proceeds.
const mvccGateFuncDDL = `
CREATE OR REPLACE FUNCTION public.zz_mvcc_gate(rid uuid, seen_count int, seen_limit int) RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, public AS $fn$
DECLARE who text := current_setting('application_name');
BEGIN
  IF rid <> '%[1]s'::uuid THEN RETURN true; END IF;
  INSERT INTO public.zz_mvcc_gate_log (app, backend_pid, seen_count, seen_limit) VALUES (who, pg_backend_pid(), seen_count, seen_limit);
  IF who = '%[2]s' THEN PERFORM pg_advisory_xact_lock_shared(%[3]d::bigint); END IF;
  RETURN true;
END
$fn$`

// mvccGatePolicyDDL puts the gate INSIDE the consume statement without touching the production SQL: a RESTRICTIVE policy is ANDed with
// the existing permissive "sender_module_full_access" one, and its qual runs in the scan of eligible_accounts (and again when Postgres
// re-checks a row version it followed an update chain to). The mutant "nopolicy" rewrites this line, so keep it on one line.
const mvccGatePolicyDDL = `CREATE POLICY zz_mvcc_gate ON send_managed.accounts AS RESTRICTIVE FOR SELECT TO send_managed_app USING (public.zz_mvcc_gate(id, daily_sent_count, daily_limit))`

// newAppConsume opens a pool of its own whose connections carry application_name=app, pings it so the physical connection exists before
// anything is parked or released, and returns a function that runs ONE accountConsume over it. The name is what lets the gate park the
// peer and nobody else, and what scopes the pg_stat_activity check to that one backend.
func newAppConsume(t *testing.T, e *dispatchEnv, app, poolID string) func() (bool, error) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(poolDSN(t, e))
	if err != nil {
		t.Fatalf("%s: parse the module DSN: %v", app, err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = app
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("%s: pool: %v", app, err)
	}
	t.Cleanup(p.Close)
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("%s: ping (the physical connection must exist before the interleave starts): %v", app, err)
	}
	d := NewDispatcher(p, e.dec, e.px)
	return func() (bool, error) {
		_, ok, err := d.accountConsume(context.Background(), poolID, defaultAccountOrderBy, nil, "UTC", 0, 900)
		return ok, err
	}
}

// blockedMVCCPeers counts consume statements parked on the MVCC gate. Scoped as CLAUDE.md Rule 10 requires and one step tighter than
// blockedConsumes: this database, not this session, the peer's own application_name, the wait event, and the consume statement's text.
func blockedMVCCPeers(t *testing.T, e *dispatchEnv) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity
		  WHERE datname = current_database() AND pid <> pg_backend_pid() AND application_name = $1
		    AND state = 'active' AND wait_event_type = 'Lock' AND wait_event = 'advisory'
		    AND query ILIKE '%eligible_accounts%'`, mvccPeerApp).Scan(&n); err != nil {
		t.Fatalf("blockedMVCCPeers: %v", err)
	}
	return n
}

// TestAccountConsume_StaleSnapshotPeerIsRefused forces the MVCC re-check path (ADR-0015; the comment at the top of this file says how).
func TestAccountConsume_StaleSnapshotPeerIsRefused(t *testing.T) {
	e := newDispatchEnv(t, "mvccrecheck")
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	forceStaleSnapshotRecheck(t, e, acct, 50, newAppConsume(t, e, mvccPeerApp, pool), newAppConsume(t, e, mvccHolderApp, pool))
}

// forceStaleSnapshotRecheck drives the interleave against the ONE account acct, which has exactly one slot left:
//
//  1. the gate (log table, function, restrictive policy) is installed and the TEST takes the exclusive gate lock;
//  2. the PEER starts a consume. It takes its statement snapshot, reads the account row WITH capacity (limit-1 < limit), asks the gate,
//     and parks. It is PROVEN parked: a backend of this database, not this session, application_name zz_mvcc_peer, wait_event
//     Lock/advisory, running the consume statement;
//  3. the HOLDER, whose connection the gate never parks, runs a whole consume while the peer is parked: it takes the last slot and
//     commits. daily_sent_count is now the limit, and the peer is still parked;
//  4. the gate is released. The peer resumes with the old row version in hand, passes its predicate on it, reaches the row lock, finds
//     the row updated by a COMMITTED transaction, follows the chain and re-checks the predicate against the committed version. It
//     must be REFUSED: no error, no slot, count unchanged at the limit;
//  5. the gate's log must show the peer was shown a row with capacity (seen_count = limit-1): the only such version is the one that
//     existed before the holder's commit, so the peer's snapshot predated it. A peer whose snapshot predates the holder's commit,
//     refused only after that commit, was refused by the re-check — there is no other way for it to be refused.
//
// Nothing here depends on timing. The waits are on observed state; the deadlines are safety bounds a healthy run (milliseconds) never
// approaches. Note accountConsume's own 10s context bounds how long the peer can stay parked.
func forceStaleSnapshotRecheck(t *testing.T, e *dispatchEnv, acct string, limit int, peer, holder func() (bool, error)) {
	t.Helper()
	ctx := context.Background()
	const safety = 20 * time.Second

	// Step 1.
	e.setAccountDailyState(t, acct, limit, limit-1, todayUTC(), false, nil)
	for _, ddl := range []string{mvccGateLogDDL, fmt.Sprintf(mvccGateFuncDDL, acct, mvccPeerApp, mvccGateKey), mvccGatePolicyDDL} {
		if _, err := e.admin.Exec(ctx, ddl); err != nil {
			t.Fatalf("install the MVCC gate: %v", err)
		}
	}
	if _, err := e.admin.Exec(ctx, `SELECT pg_advisory_lock($1)`, mvccGateKey); err != nil {
		t.Fatalf("take the MVCC gate lock: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if _, err := e.admin.Exec(ctx, `SELECT pg_advisory_unlock($1)`, mvccGateKey); err != nil {
			t.Errorf("release the MVCC gate lock: %v", err)
		}
	}
	defer release()

	var wg sync.WaitGroup
	fail := func(format string, args ...any) {
		t.Helper()
		release()
		wg.Wait()
		t.Fatalf(format, args...)
	}

	// Step 2: the peer, proven parked between its snapshot and its row lock.
	peerCh := make(chan consumeResult, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ok, err := peer()
		peerCh <- consumeResult{worker: 1, ok: ok, err: err}
	}()
	parkedBy := time.Now().Add(safety)
	for blockedMVCCPeers(t, e) != 1 {
		select {
		case r := <-peerCh:
			fail("MVCC forced interleave did not engage: the peer returned (ok=%v err=%v) instead of parking between its snapshot and its row lock, so this run proves nothing", r.ok, r.err)
		default:
		}
		if time.Now().After(parkedBy) {
			fail("MVCC forced interleave did not engage: no peer consume was observed parked on the gate within %s, so this run proves nothing", safety)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Step 3: the holder takes the last slot and commits while the peer is parked.
	holderCh := make(chan consumeResult, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ok, err := holder()
		holderCh <- consumeResult{worker: 0, ok: ok, err: err}
	}()
	select {
	case r := <-holderCh:
		if r.err != nil {
			fail("FORCED CONTENTION BROKEN: the holder errored (or was held up past the consume's own 10s timeout) while the peer was parked on the gate: %v", r.err)
		}
		if !r.ok {
			fail("the holder was refused the last slot while the peer was parked — capacity was visibly there (daily_sent_count=%d, limit=%d), so the gate is parking more than the peer", limit-1, limit)
		}
	case <-time.After(safety):
		fail("FORCED CONTENTION BROKEN: the holder did not complete within %s while the peer was parked on the gate — the gate must park ONLY the peer, and nothing may serialise the two consumes", safety)
	}
	if c := e.accountDailySentCount(t, acct); c != limit {
		fail("daily_sent_count=%d after the holder committed, want %d", c, limit)
	}
	select {
	case r := <-peerCh:
		fail("the peer returned (ok=%v err=%v) while the gate was still held — the park was not real", r.ok, r.err)
	default:
	}
	if n := blockedMVCCPeers(t, e); n != 1 {
		fail("expected the peer to still be parked after the holder committed, saw %d parked", n)
	}

	// Step 4: release the gate; the peer must be refused by the re-check.
	release()
	var pr consumeResult
	select {
	case pr = <-peerCh:
	case <-time.After(safety):
		fail("the peer did not return within %s of the gate being released", safety)
	}
	wg.Wait()
	if pr.err != nil {
		t.Fatalf("MVCC RE-CHECK PATH FAILED: the stale-snapshot peer ERRORED instead of being refused (an accounts_daily_sent_check violation, SQLSTATE 23514, means it tried to take a slot the holder had already taken and only the last-resort CHECK stopped it): %v", pr.err)
	}
	if pr.ok {
		t.Fatalf("MVCC RE-CHECK PATH FAILED: DOUBLE BOOKING — the stale-snapshot peer consumed a slot the holder had already taken (daily_limit=%d)", limit)
	}
	if c := e.accountDailySentCount(t, acct); c != limit {
		t.Fatalf("daily_sent_count=%d after the peer was refused, want exactly %d", c, limit)
	}

	// Step 5: the evidence that this really was the stale-snapshot path, from the gate's own log.
	rows, err := e.admin.Query(ctx, `SELECT seen_count FROM public.zz_mvcc_gate_log WHERE app = $1 ORDER BY n`, mvccPeerApp)
	if err != nil {
		t.Fatalf("read the gate log: %v", err)
	}
	var peerSaw []int
	for rows.Next() {
		var c int
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan the gate log: %v", err)
		}
		peerSaw = append(peerSaw, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the gate log: %v", err)
	}
	sawCapacity := false
	for _, c := range peerSaw {
		if c == limit-1 {
			sawCapacity = true
		}
	}
	if !sawCapacity {
		t.Fatalf("MVCC PATH NOT PROVEN: the peer was refused, but the gate never saw it read the row with capacity (daily_sent_count=%d); it saw %v, so its refusal may have been exhaustion, not the re-check", limit-1, peerSaw)
	}

	for _, ddl := range []string{`DROP POLICY zz_mvcc_gate ON send_managed.accounts`, `DROP FUNCTION public.zz_mvcc_gate(uuid, int, int)`, `DROP TABLE public.zz_mvcc_gate_log`} {
		if _, err := e.admin.Exec(ctx, ddl); err != nil {
			t.Fatalf("remove the MVCC gate (%s): %v", ddl, err)
		}
	}
	t.Logf("MVCC RE-CHECK FORCED: peer was shown counts %v (capacity at %d) and parked before its row lock; holder took the last slot and COMMITTED (count=%d); peer REFUSED, no error, count stayed %d",
		peerSaw, limit-1, limit, limit)
}
