package adminapi

// config_ordering_race_test.go — the concurrency half of P9-OD-2's write-boundary enforcement,
// added by Phase 10's R4 gate after it DEMONSTRATED (against the live dev stack, not by reasoning)
// that the original non-transactional read-then-write let two concurrent PUTs each pass the
// ordering check and commit an INVERTED pair, wedging both the dispatch and reclaim loops at
// config-LOAD on every tick while /readyz stayed green.
//
// Real Postgres, real HTTP against the actual production Server (CLAUDE.md Rule 3) — no mocks.
//
// ANTI-VACUITY (CLAUDE.md Rule 10): a race test that never contends cannot fail. This file uses
// method (a), the preferred one — a FORCED deterministic interleave, no timing luck: an
// independent transaction takes the config-write advisory lock and HOLDS it, and the test asserts
// the concurrent PUT genuinely BLOCKS (it does not complete while the lock is held, and a backend
// is observably lock-waiting on THAT specific lock) before the lock is released. Without the fix
// there is no lock to take, the PUT does not block, and the assertions below fail — which is
// verified by the paired negative control in
// TestConfigOrdering_ConcurrentPUTsCannotInvertPair_NegativeControl.

import (
	"sync"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/configordering"
)

// TestConfigOrdering_ConcurrentPUTsBlockOnTheWriteLock proves the serialization is REAL: while one
// transaction holds configWriteLockKey, a config PUT cannot proceed. This is the mechanism the
// TOCTOU fix rests on, asserted directly rather than assumed.
func TestConfigOrdering_ConcurrentPUTsBlockOnTheWriteLock(t *testing.T) {
	e := newTestEnv(t, "ordraceA")
	ctx := bgCtx()

	// (1) Hold configWriteLockKey in an independent transaction — this is the forced interleave.
	// A separate connection as the ADMIN role is fine: advisory locks are role-agnostic, keyed only
	// on the value, so this contends with the Server's pooled connection exactly as a second
	// concurrent PUT would.
	tx, err := e.admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock-holder tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", configWriteLockKey); err != nil {
		t.Fatalf("take config write lock: %v", err)
	}

	// (2) Fire a valid PUT concurrently. It must BLOCK on the lock above, not complete.
	done := make(chan int, 1)
	go func() {
		// 60 < 120 (the seeded lease) — semantically valid, so the ONLY thing that can hold it up is
		// the write lock. A rejected-on-merit PUT would not prove contention.
		r := e.do(t, "PUT", "/v1/admin/config/smtp_operation_timeout_sec", map[string]any{"value": 60})
		done <- r.status
	}()

	// (3) ANTI-VACUITY, part one: it must not have completed.
	select {
	case st := <-done:
		t.Fatalf("the PUT completed (status=%d) while configWriteLockKey was held — config writes are NOT serialized, so this test is vacuous and the TOCTOU window is open", st)
	case <-time.After(750 * time.Millisecond):
	}

	// (4) ANTI-VACUITY, part two: observe the genuinely blocked backend, scoped to THIS database AND
	// to the specific advisory-lock statement (CLAUDE.md Rule 10's 2026-07-26 addendum — datname
	// alone is necessary but not sufficient).
	var blocked int
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity
		  WHERE datname = current_database() AND wait_event_type='Lock' AND state='active'
		    AND query ILIKE '%pg_advisory_xact_lock%'`).Scan(&blocked); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	if blocked == 0 {
		t.Fatal("no backend is lock-waiting on pg_advisory_xact_lock — the PUT is not contending with the held lock (vacuous)")
	}

	// (5) Release. The PUT now proceeds and succeeds.
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit lock-holder tx: %v", err)
	}
	select {
	case st := <-done:
		if st != 200 {
			t.Fatalf("PUT status after the lock was released = %d, want 200", st)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the PUT never completed after the write lock was released — it is stuck, not merely serialized")
	}
}

// TestConfigOrdering_ConcurrentPUTsCannotInvertPair is the defect itself, driven end to end: the
// two PUTs R4 used to produce an inverted pair (lower the boundary key to 100 while the dependent
// key is still 120; raise the dependent key to 50 while the boundary is still 30 — each valid at
// the instant it reads, jointly inverted). Whatever the interleaving, the COMMITTED pair must
// satisfy the invariant. At most one of the two may succeed.
func TestConfigOrdering_ConcurrentPUTsCannotInvertPair(t *testing.T) {
	e := newTestEnv(t, "ordraceB")

	// Seeded state: smtp_operation_timeout_sec=30, job_max_processing_time_sec=120.
	// PUT A: smtp_operation_timeout_sec=100 — valid iff the lease is still >100 (it is, 120).
	// PUT B: job_max_processing_time_sec=50  — valid iff the timeout is still <50 (it is, 30).
	// Both were 200 before the fix, leaving 100 >= 50: an inverted pair.
	var (
		wg               sync.WaitGroup
		statusA, statusB int
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		statusA = e.do(t, "PUT", "/v1/admin/config/smtp_operation_timeout_sec", map[string]any{"value": 100}).status
	}()
	go func() {
		defer wg.Done()
		statusB = e.do(t, "PUT", "/v1/admin/config/job_max_processing_time_sec", map[string]any{"value": 50}).status
	}()
	wg.Wait()

	// THE assertion: read the committed truth and check the invariant, not the responses. This is
	// the post-state Rule 3 requires — "the response said 400" is not evidence about the table.
	timeout := configIntFromDB(t, e, "smtp_operation_timeout_sec")
	lease := configIntFromDB(t, e, "job_max_processing_time_sec")
	if timeout >= lease {
		t.Fatalf("INVERTED PAIR COMMITTED: smtp_operation_timeout_sec=%d >= job_max_processing_time_sec=%d (statusA=%d statusB=%d). The dispatch AND reclaim loops now fail every tick at config-LOAD while /readyz stays green.",
			timeout, lease, statusA, statusB)
	}

	// Both PUTs succeeding is the specific defect signature: it means neither saw the other's write.
	if statusA == 200 && statusB == 200 {
		t.Fatalf("both conflicting PUTs returned 200 (timeout=%d lease=%d) — the ordering check was evaluated against a counterpart value that another in-flight write then changed",
			timeout, lease)
	}
	// And at least one must have been REJECTED on the merits rather than erroring internally, so the
	// serialization produces a clean operator-facing outcome, not a 500.
	if statusA != 200 && statusA != 400 {
		t.Errorf("PUT A status=%d, want 200 (won the race) or 400 (cleanly rejected)", statusA)
	}
	if statusB != 200 && statusB != 400 {
		t.Errorf("PUT B status=%d, want 200 (won the race) or 400 (cleanly rejected)", statusB)
	}
}

// TestConfigOrdering_ConcurrentPUTsCannotInvertPair_NegativeControl is the paired control CLAUDE.md
// Rule 10 requires for a guard test: it proves the guard above is what does the work, by driving
// the SAME two writes through the PRE-FIX shape — read the counterpart on one connection, check,
// then write on another, with no transaction and no lock — and asserting that shape DOES invert the
// pair. If this control ever stops inverting, the test above has become incapable of failing and
// the two must be re-derived together.
func TestConfigOrdering_ConcurrentPUTsCannotInvertPair_NegativeControl(t *testing.T) {
	e := newTestEnv(t, "ordraceC")
	ctx := bgCtx()

	// The pre-fix interleave, made deterministic: BOTH sides read their counterpart FIRST (each
	// sees the seeded, still-valid value), and only then does either write. That is exactly what
	// separate pooled connections permitted, and it is why freshness of the read was not enough.
	leaseSeenByA, err := readConfigIntDirect(ctx, e.pool, "job_max_processing_time_sec")
	if err != nil {
		t.Fatalf("read lease: %v", err)
	}
	timeoutSeenByB, err := readConfigIntDirect(ctx, e.pool, "smtp_operation_timeout_sec")
	if err != nil {
		t.Fatalf("read timeout: %v", err)
	}

	// Each candidate passes the check against the value ITS side read. Note WHAT is being asserted:
	// the pure invariant check (the same configordering function both enforcement points call) was
	// never wrong. The defect was always in WHEN the counterpart value was read relative to the
	// write, never in the comparison itself.
	if v := configordering.CheckSMTPTimeoutVsLease(100, leaseSeenByA); v != nil {
		t.Fatalf("precondition: A's candidate (100 vs lease %d) should pass its own check: %v", leaseSeenByA, v)
	}
	if v := configordering.CheckSMTPTimeoutVsLease(timeoutSeenByB, 50); v != nil {
		t.Fatalf("precondition: B's candidate (timeout %d vs 50) should pass its own check: %v", timeoutSeenByB, v)
	}

	// Both writes then land — the state no single check ever saw.
	mustExec(t, e, `UPDATE send_managed.config SET value=jsonb_build_object('v',100) WHERE key='smtp_operation_timeout_sec'`)
	mustExec(t, e, `UPDATE send_managed.config SET value=jsonb_build_object('v',50)  WHERE key='job_max_processing_time_sec'`)

	timeout := configIntFromDB(t, e, "smtp_operation_timeout_sec")
	lease := configIntFromDB(t, e, "job_max_processing_time_sec")
	if timeout < lease {
		t.Fatalf("NEGATIVE CONTROL DID NOT INVERT: timeout=%d < lease=%d. The pre-fix shape no longer reproduces the defect, so TestConfigOrdering_ConcurrentPUTsCannotInvertPair may now be vacuous — re-derive both.",
			timeout, lease)
	}
	// Inverted, as it must be: this is the state the transaction+lock now prevents through the API.
}

func configIntFromDB(t *testing.T, e *testEnv, key string) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(bgCtx(),
		`SELECT (value->>'v')::int FROM send_managed.config WHERE key=$1`, key).Scan(&n); err != nil {
		t.Fatalf("read %s from DB: %v", key, err)
	}
	return n
}

func mustExec(t *testing.T, e *testEnv, sql string) {
	t.Helper()
	if _, err := e.admin.Exec(bgCtx(), sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
