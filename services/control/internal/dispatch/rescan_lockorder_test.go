package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// Phase 7 review gate R4 — the rescan lease-extension vs outcome lock-order proof, DETERMINISTICALLY
// INTERLEAVED.
//
// TestRescan_LeaseExtensionVsOutcomeNoDeadlock (rescan_test.go) races the two paths on wall-clock
// timing and, empirically, resolves 40/40 rounds with the classified failure winning — i.e. the
// rescan's UPDATE usually runs when the job has ALREADY left 'sending', so it matches zero rows and
// the two statements never actually contend for the same job row. That test therefore proves
// "coherent outcome", but it does NOT reliably prove the two lock ORDERS were driven against each
// other. These two tests force each interleaving with explicit barriers (held transactions) so the
// contention is guaranteed, not hoped for.
//
// Both directions are asserted, per CLAUDE.md Rule 9 / ADR-0010.
//
// TWO POSTGRES SEMANTICS, recorded here because they are what make the rescan event-free and what make
// the obvious negative controls for these tests USELESS (CLAUDE.md Rule 9 sub-point 2026-07-26,
// ADR-0010 amendment):
//
//	(a) An UPDATE that leaves `state` UNCHANGED produces NO job_events row. trg_job_events_on_state_change
//	    is AFTER UPDATE OF state, and fn_job_events_on_state_change RETURNS EARLY when
//	    OLD.state = NEW.state (send_managed_0008). So `SET state = state` is not an event.
//	(b) An UPDATE that leaves an FK key value UNCHANGED takes NO KEY SHARE on the referenced row —
//	    Postgres skips the FK re-check when the key did not change. So
//	    `SET assigned_account_id = assigned_account_id` does not lock the account.
//
// CONSEQUENCE: the rescan's `SET lease_expires_at, updated_at WHERE state IN ('assigned','sending')`
// touches neither `state` nor an FK key, so it is event-free and takes no account lock — which is why
// Rule 9's precondition never triggers for it. That premise is load-bearing, hence asserted here rather
// than asserted in a comment.
//
// CONSEQUENCE FOR WHOEVER EDITS THESE TESTS: neither no-op SET above will make them fail. Both were
// tried and both left the suite green. To verify the teeth, inject an EXPLICIT
// `SELECT … FROM send_managed.accounts … FOR KEY SHARE` into the rescan transaction — that produces the
// "BLOCKED behind a held exclusive account lock" failure below, which is how these tests were validated.

// TestRescan_ForcedInterleave_JobHeldThenAccountFirstOutcome forces: the rescan's bulk lease UPDATE
// takes the JOB row lock FIRST and holds it (open tx), then the outcome handler runs its
// account→job order (exclusive account FOR UPDATE, then the job requeue) and must BLOCK on the job
// row rather than cycle. When the rescan commits, the outcome proceeds and commits. A 40P01 in either
// direction fails the test.
func TestRescan_ForcedInterleave_JobHeldThenAccountFirstOutcome(t *testing.T) {
	e := newDispatchEnv(t, "rescanfi_jobfirst")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET last_success_at=NOW() WHERE id=$1`, acct); err != nil {
		t.Fatalf("seed success: %v", err)
	}

	job := e.seedQueuedJob(t, "fi1@example.com")
	e.dispatchToSending(t, job)
	e.px.freeAllWorkers()

	pRescan, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool rescan: %v", err)
	}
	defer pRescan.Close()
	pOutcome, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool outcome: %v", err)
	}
	defer pOutcome.Close()

	// (1) The rescan's EXACT statement (rescan.go:82-86) inside a HELD transaction: it now owns the
	// job row's write lock and will not release it until we commit.
	tx, err := pRescan.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rescan tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE send_managed.jobs
		    SET lease_expires_at = NOW() + make_interval(secs => $1),
		        updated_at = NOW()
		  WHERE state IN ('assigned','sending')`, 120); err != nil {
		t.Fatalf("rescan lease UPDATE: %v", err)
	}

	// (2) The outcome handler, on its own connection, in the opposite lock order (account FOR UPDATE
	// first, then the job requeue). It must BLOCK on the job row, not deadlock.
	type res struct {
		resolved bool
		state    string
	}
	done := make(chan res, 1)
	go func() {
		oh := NewOutcomeHandler(pOutcome, e.cfg)
		ok := oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
			SMTPResponse: "451 4.3.0 temporary local error"})
		st, _, _, _, _ := e.jobState(t, job)
		done <- res{resolved: ok, state: st}
	}()

	// Confirm it is genuinely BLOCKED (anti-vacuity: if it completed here, the interleaving never
	// happened and this test would prove nothing).
	select {
	case r := <-done:
		t.Fatalf("outcome handler completed (state=%s) while the rescan held the job row — the interleaving did not occur, so this test is vacuous", r.state)
	case <-time.After(750 * time.Millisecond):
	}
	var blocked int
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND state='active'`).Scan(&blocked); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	if blocked == 0 {
		t.Fatal("no backend is lock-waiting — the outcome handler is not contending with the held rescan lock (vacuous)")
	}

	// (3) Release the rescan. The outcome must then commit cleanly (no 40P01).
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit rescan tx: %v", err)
	}
	select {
	case r := <-done:
		if !r.resolved {
			t.Fatalf("outcome NOT resolved after the rescan released the job row (state=%s) — a deadlock/abort would look exactly like this", r.state)
		}
		if r.state != "queued" {
			t.Fatalf("job state=%s after the classified failure, want queued", r.state)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("outcome handler never completed after the rescan committed — the two paths are wedged")
	}
}

// TestRescan_ForcedInterleave_AccountHeldThenRescan forces the OTHER direction and is the direct
// behavioral proof of the event-free premise: an exclusive account FOR UPDATE is held open, and the
// rescan's lease UPDATE on a job assigned to THAT account must still complete. If the statement ever
// became event-ful (a SET that lists `state`, so the job_events trigger fires and its INSERT takes an
// FK KEY SHARE on the account), KEY SHARE conflicts with FOR UPDATE and this would block until the
// timeout — turning the header comment in rescan.go into an asserted property instead of a claim.
func TestRescan_ForcedInterleave_AccountHeldThenRescan(t *testing.T) {
	e := newDispatchEnv(t, "rescanfi_acctfirst")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	e.setConfigInt(t, "startup_reconnect_window_sec", 120)

	job := e.seedQueuedJob(t, "fi2@example.com")
	e.dispatchToSending(t, job)
	e.px.freeAllWorkers()
	// The job must genuinely be on the account whose row we pin, or the test is vacuous.
	var onAcct *string
	if err := e.admin.QueryRow(ctx, `SELECT assigned_account_id::text FROM send_managed.jobs WHERE id=$1`, job).Scan(&onAcct); err != nil {
		t.Fatalf("read assigned account: %v", err)
	}
	if onAcct == nil || *onAcct != acct {
		t.Fatalf("job is on account %v, want the pinned account %s (vacuous otherwise)", onAcct, acct)
	}

	pHold, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool hold: %v", err)
	}
	defer pHold.Close()
	pRescan, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool rescan: %v", err)
	}
	defer pRescan.Close()

	// Hold the EXCLUSIVE account lock open (exactly what applyClassifiedFailure / reclaimJobLocked
	// take as their FIRST step).
	tx, err := pHold.Begin(ctx)
	if err != nil {
		t.Fatalf("begin hold tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM send_managed.accounts WHERE id=$1 FOR UPDATE`, acct); err != nil {
		t.Fatalf("hold account FOR UPDATE: %v", err)
	}

	// The REAL rescan, on its own pool, while that exclusive account lock is held.
	d := NewDispatcher(pRescan, e.dec, e.px)
	type res struct {
		n   int
		err error
	}
	done := make(chan res, 1)
	go func() {
		n, err := d.StartupRescan(ctx)
		done <- res{n: n, err: err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			var pgErr *pgconn.PgError
			if errors.As(r.err, &pgErr) && pgErr.Code == "40P01" {
				t.Fatalf("rescan DEADLOCKED (40P01) against a held account FOR UPDATE: %v", r.err)
			}
			t.Fatalf("rescan failed while an account lock was held: %v", r.err)
		}
		if r.n < 1 {
			t.Fatalf("rescan extended %d leases, want >=1 (the in-flight job) — vacuous otherwise", r.n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the rescan's lease UPDATE BLOCKED behind a held exclusive account lock — it is NOT event-free (the job_events trigger's FK KEY SHARE conflicts with FOR UPDATE), which invalidates rescan.go's no-account-pre-lock argument")
	}

	// And the held tx is still healthy (nothing aborted it) — the rescan never touched the account.
	if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatalf("the account-holding tx was aborted by the rescan: %v", err)
	}
}
