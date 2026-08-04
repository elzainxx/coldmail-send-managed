package dispatch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// TestReclaimRace_DetectorAVsClassifiedFailure (R4-P6 HIGH regression, ADR-0010 §2) — the ONE
// lock-order combination the original reclaim-race test missed: Detector A's dead-worker MASS reclaim
// (reclaimJobsOnWorker — a plain multi-row UPDATE whose job_events trigger takes FK KEY SHARE on the
// account AFTER the job row: job→account order) racing a worker-REPORTED penalizing failure
// (applyClassifiedFailure — which locks the account FOR UPDATE BEFORE the job requeue: account→job
// order). Opposite orders on the same {job, account} pair = a 40P01 deadlock cycle. This test drives
// both concurrently, on genuinely separate connections, over many rounds: it must NEVER deadlock and
// must always leave the job in a single coherent state (queued via reclaim, or queued via the
// classified requeue — both legal; the point is no 40P01 and no torn state).
func TestReclaimRace_DetectorAVsClassifiedFailure(t *testing.T) {
	e := newDispatchEnv(t, "reclaimdeadlock")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	// Keep the score high via a recent success so the classified failure doesn't quarantine mid-loop
	// (we're testing the LOCK ORDER, not the account effect).
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET last_success_at=NOW() WHERE id=$1`, acct); err != nil {
		t.Fatalf("seed success: %v", err)
	}

	p1, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool1: %v", err)
	}
	defer p1.Close()
	p2, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool2: %v", err)
	}
	defer p2.Close()
	oh := NewOutcomeHandler(p1, e.cfg)
	racer := NewDispatcher(p2, e.dec, e.px)

	const rounds = 40
	for i := 0; i < rounds; i++ {
		job := e.seedQueuedJob(t, "dl@example.com")
		e.dispatchToSending(t, job) // assigned_box_id = e.px.workerID, assigned_account_id = acct, state=sending
		e.px.freeAllWorkers()
		// Kill the worker's heartbeat so Detector A (the mass-reclaim path) fires this tick.
		e.setWorkerHeartbeat(t, e.px.workerID, 400*time.Second)

		var wg sync.WaitGroup
		wg.Add(2)
		// Racer: Detector A mass reclaim (job→account lock order via the trigger).
		go func() {
			defer wg.Done()
			if _, _, err := racer.ReclaimTick(ctx); err != nil {
				t.Errorf("round %d: ReclaimTick errored (deadlock 40P01?): %v", i, err)
			}
		}()
		// Outcome: a penalizing classified failure (account→job lock order). transport_4xx keeps the
		// score path alive but won't quarantine a high-score account.
		go func() {
			defer wg.Done()
			oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
				SMTPResponse: "451 4.3.0 temporary local error"})
		}()
		wg.Wait()

		// Whoever won, the job is in a single coherent state (queued — both paths requeue) and the worker
		// got marked stale by the reclaim. No 40P01 (asserted via the t.Errorf above on tick error).
		st, _, leaseNull, _, _ := e.jobState(t, job)
		if st != "queued" {
			t.Fatalf("round %d: job state=%s after the race, want queued (both paths requeue)", i, st)
		}
		if !leaseNull {
			t.Fatalf("round %d: reclaimed/requeued job has a non-null lease", i)
		}
		// Reset the worker to active for the next round so dispatchToSending can push again.
		if _, err := e.admin.Exec(ctx, `UPDATE send_managed.boxes SET status='active', last_heartbeat_at=NOW() WHERE id=$1`, e.px.workerID); err != nil {
			t.Fatalf("reset worker: %v", err)
		}
	}
}
