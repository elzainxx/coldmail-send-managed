package dispatch

import (
	"context"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// TestHang_WorkerReportedTimeoutQuarantinesBounded (T8 — the P6-11 ordering invariant's teeth) — a
// hanging account (one whose worker operation timeout fires BEFORE the job lease, per
// smtp_operation_timeout_sec < job_max_processing_time_sec) surfaces as a worker-REPORTED timeout, so
// Phase 5's outcome handler SCORES it and, after a bounded number of consecutive timeouts,
// auto-quarantines it — removing it from eligibility. Crucially the job is NOT stuck in an endless
// unscored Control-reclaim loop: the reclaim_loop event count stays ZERO (the worker reported first).
// Then the job routes to a healthy account and sends. This is the exact behavior the ordering
// invariant guarantees (contrast the negative control, TestConfigOrdering_RejectsInvertedPair,
// which proves an inverted pair is rejected at load rather than silently thrashing).
func TestHang_WorkerReportedTimeoutQuarantinesBounded(t *testing.T) {
	e := newDispatchEnv(t, "hangquar")
	ctx := context.Background()
	pool := e.seedPool(t)
	accHang := e.seedAccount(t, pool, "sec-hang")

	// Drive worker-reported timeouts on the hanging account until it auto-quarantines. Each iteration:
	// dispatch the one job → it lands on accHang (the only eligible account) → the worker "operation
	// timeout" fires and reports error_reason=timeout → Phase 5 scores accHang. Bounded loop guards
	// against an infinite test if quarantine never happened (which would itself be the failure).
	job := e.seedQueuedJob(t, "hang@example.com")
	const maxAttempts = 10
	attempts := 0
	quarantined := false
	for i := 0; i < maxAttempts; i++ {
		// Make the requeued job immediately claimable again.
		if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
			t.Fatalf("reset next_attempt_at: %v", err)
		}
		if _, err := e.d.TryDispatch(ctx); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		st, _, _, _, _ := e.jobState(t, job)
		if st != "sending" {
			// The account quarantined mid-loop → the job can no longer be placed on it (no eligible
			// account), so TryDispatch left it queued. That IS the terminal condition we're testing.
			break
		}
		attempts++
		// The worker's per-operation SMTP timeout fired first (invariant holds) → a REPORTED timeout.
		e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
			ErrorReason: workerproto.ReasonTimeout, SMTPResponse: ""})
		e.px.freeAllWorkers()
		_, quar, _ := e.accountFlagsNow(t, accHang)
		if quar {
			quarantined = true
			break
		}
	}

	if !quarantined {
		t.Fatalf("accHang was NOT quarantined after %d worker-reported timeouts — a hanging account must degrade & quarantine (P6-11)", attempts)
	}
	// Bounded: it took a small, sane number of attempts (score 75 → crosses 30 at ~cf=4), never an
	// unbounded thrash.
	if attempts > 6 {
		t.Fatalf("accHang took %d attempts to quarantine — expected bounded (~4-5); not a tight degrade", attempts)
	}

	// ZERO Control reclaims for this job: the worker reported every timeout (actor='box'), so the reclaim
	// loop never declared one. This is the invariant's whole point — a reported timeout, not an
	// unscored reclaim.
	if evs := e.reclaimEvents(t, job); len(evs) != 0 {
		t.Fatalf("job accumulated %d reclaim_loop events — a hanging account must be caught by worker-REPORTED timeouts, not Control reclaims (P6-11)", len(evs))
	}

	// The scoring provenance is worker-reported: a system quarantine event with the score-threshold
	// reason exists on accHang (Phase 5 §4.2b), actor='system' (the auto-quarantine), not a reclaim.
	evs := e.systemAccountEvents(t, accHang)
	sawQuarantine := false
	for _, ev := range evs {
		if ev[0] == "quarantine" {
			sawQuarantine = true
		}
	}
	if !sawQuarantine {
		t.Fatalf("no system quarantine account_event on accHang, got %v", evs)
	}

	// Now a healthy account joins the pool; the job routes to it and sends (accHang is out of
	// eligibility). Proves the campaign is NOT stuck — it fails over to good capacity.
	accGood := e.seedAccount(t, pool, "sec-good")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
		t.Fatalf("reset next_attempt_at: %v", err)
	}
	e.px.freeAllWorkers()
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("failover dispatch: %v", err)
	}
	st, _, _, _, _ := e.jobState(t, job)
	if st != "sending" {
		t.Fatalf("after a healthy account joined, job state=%s, want sending (failover)", st)
	}
	var usedAcct string
	if err := e.admin.QueryRow(ctx, `SELECT assigned_account_id FROM send_managed.jobs WHERE id=$1`, job).Scan(&usedAcct); err != nil {
		t.Fatalf("read account: %v", err)
	}
	if usedAcct != accGood {
		t.Fatalf("failover used account %s, want the healthy accGood %s", usedAcct, accGood)
	}
	// And it can complete cleanly on the good account.
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-HANG-OK", SMTPResponse: "250 2.0.0 Ok"})
	if st, hasPMID, _, _, _ := e.jobState(t, job); st != "sent" || !hasPMID {
		t.Fatalf("final state=%s hasPMID=%v, want sent/true (delivered via healthy account)", st, hasPMID)
	}
}
