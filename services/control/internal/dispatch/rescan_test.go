package dispatch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// jobLeaseAndEvents reads a job's raw lease_expires_at + its total job_events count — the two data a
// rescan assertion needs (lease was extended; NO event was written).
func (e *dispatchEnv) jobLeaseAndEvents(t *testing.T, jobID string) (lease *time.Time, events int) {
	t.Helper()
	if err := e.admin.QueryRow(context.Background(),
		`SELECT lease_expires_at, (SELECT count(*) FROM send_managed.job_events WHERE job_id=$1)
		   FROM send_managed.jobs WHERE id=$1`, jobID).Scan(&lease, &events); err != nil {
		t.Fatalf("read lease+events: %v", err)
	}
	return lease, events
}

// workerHeartbeat reads a worker's raw last_heartbeat_at.
func (e *dispatchEnv) workerHeartbeat(t *testing.T, workerID string) time.Time {
	t.Helper()
	var hb time.Time
	if err := e.admin.QueryRow(context.Background(),
		`SELECT last_heartbeat_at FROM send_managed.boxes WHERE id=$1`, workerID).Scan(&hb); err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}
	return hb
}

// TestRescan_ExtendsLeasesAndRefreshesHeartbeats (§1.2 steps 2–3, the core rescan behavior) — on a
// simulated boot, the rescan (a) extends every orphaned in-flight lease to ~NOW()+window and (b)
// refreshes non-disabled worker heartbeats to ~NOW(), while (c) leaving already-terminal/queued jobs
// untouched. Anti-vacuity: a queued job (not in-flight) is NOT touched, and a 'disabled' worker's
// heartbeat is NOT refreshed — proving the rescan is scoped to the recovery targets, not blanket.
func TestRescan_ExtendsLeasesAndRefreshesHeartbeats(t *testing.T) {
	e := newDispatchEnv(t, "rescanextend")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.seedAccount(t, pool, "sec")

	// Two orphaned in-flight jobs (crash left them 'sending' with a near-expired lease), one control
	// 'queued' job that the rescan must not touch.
	sending := e.seedQueuedJob(t, "orphan1@example.com")
	e.dispatchToSending(t, sending)
	e.px.freeAllWorkers()
	assigned := e.seedQueuedJob(t, "orphan2@example.com")
	e.dispatchToSending(t, assigned)
	e.px.freeAllWorkers()
	// Force both leases near-expired (the crash was long enough that the original 120s lease is stale).
	e.expireLease(t, sending)
	e.expireLease(t, assigned)
	queuedCtl := e.seedQueuedJob(t, "queued-control@example.com") // never in-flight

	// A disabled worker (admin-offline) that must NOT get its heartbeat refreshed; and a stale worker that
	// MUST (non-disabled). Force both heartbeats far into the past first.
	e.setWorkerHeartbeat(t, e.px.workerID, 500*time.Second)
	// disabled requires disabled_at (boxes_disabled_at_check); set both in one statement.
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.boxes SET status='disabled', disabled_at=NOW() WHERE id=$1`, e.px.workerID); err != nil {
		t.Fatalf("disable worker: %v", err)
	}
	disabledHBBefore := e.workerHeartbeat(t, e.px.workerID)

	liveWorkerID := seedWorker(t, e.admin) // non-disabled, silent
	e.setWorkerHeartbeat(t, liveWorkerID, 500*time.Second)

	const window = 90
	e.setConfigInt(t, "startup_reconnect_window_sec", window)

	n, err := e.d.StartupRescan(ctx)
	if err != nil {
		t.Fatalf("StartupRescan: %v", err)
	}
	if n != 2 {
		t.Fatalf("rescan extended %d leases, want 2 (the two orphaned in-flight jobs)", n)
	}

	// (a) Both in-flight leases now sit ~NOW()+window in the FUTURE (extended past expiry), so
	// Detector B will not reclaim them during the window.
	now := time.Now()
	for _, j := range []string{sending, assigned} {
		lease, _ := e.jobLeaseAndEvents(t, j)
		if lease == nil {
			t.Fatalf("job %s lease is NULL after rescan, want extended (still in-flight)", j)
		}
		delta := lease.Sub(now)
		if delta < time.Duration(window-30)*time.Second || delta > time.Duration(window+30)*time.Second {
			t.Fatalf("job %s lease delta=%s, want ~%ds in the future (rescan window)", j, delta, window)
		}
	}

	// (b) The non-disabled silent worker's heartbeat is refreshed to ~NOW(); (c) the disabled worker's is NOT.
	if hb := e.workerHeartbeat(t, liveWorkerID); now.Sub(hb) > 30*time.Second {
		t.Fatalf("live worker heartbeat not refreshed (%.0fs ago), want ~NOW()", now.Sub(hb).Seconds())
	}
	if hb := e.workerHeartbeat(t, e.px.workerID); !hb.Equal(disabledHBBefore) {
		t.Fatalf("disabled worker heartbeat was refreshed (%v -> %v), want left untouched", disabledHBBefore, hb)
	}

	// The queued control job is entirely untouched (still queued, no lease, no extra event).
	if st, _, leaseNull, _, _ := e.jobState(t, queuedCtl); st != "queued" || !leaseNull {
		t.Fatalf("queued control job state=%s leaseNull=%v, want queued/true (rescan must not touch it)", st, leaseNull)
	}
}

// TestRescan_LeaseExtensionIsEventFree (THE headline Chunk-1 property, §0/§1.2 + the Phase 6 lesson)
// — the rescan's lease-extension UPDATE writes ZERO job_events rows. This is what makes it take no
// account FK KEY SHARE and therefore unable to deadlock (proven separately below). We snapshot the
// event count of an in-flight job before the rescan and assert it is byte-identical after — the
// lease moved, but not one audit row was appended.
func TestRescan_LeaseExtensionIsEventFree(t *testing.T) {
	e := newDispatchEnv(t, "rescaneventfree")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	job := e.seedQueuedJob(t, "eventfree@example.com")
	e.dispatchToSending(t, job)
	e.px.freeAllWorkers()
	e.expireLease(t, job)

	leaseBefore, eventsBefore := e.jobLeaseAndEvents(t, job)
	if eventsBefore == 0 {
		t.Fatal("precondition: job has no events (should have queued+assigned+sending genesis events)")
	}

	e.setConfigInt(t, "startup_reconnect_window_sec", 120)
	if _, err := e.d.StartupRescan(ctx); err != nil {
		t.Fatalf("StartupRescan: %v", err)
	}

	leaseAfter, eventsAfter := e.jobLeaseAndEvents(t, job)
	// The lease genuinely moved (anti-vacuity: the UPDATE did something)...
	if leaseBefore != nil && leaseAfter != nil && !leaseAfter.After(*leaseBefore) {
		t.Fatalf("lease not extended (%v -> %v), want later — the rescan did nothing", *leaseBefore, *leaseAfter)
	}
	// ...but NOT ONE job_events row was written (the event-free guarantee).
	if eventsAfter != eventsBefore {
		t.Fatalf("rescan wrote %d job_events row(s) (%d -> %d), want ZERO — lease extension MUST be event-free (§1.2, the deadlock-safety premise)",
			eventsAfter-eventsBefore, eventsBefore, eventsAfter)
	}
	// The job is still in-flight (state unchanged, lease non-NULL) — the rescan reclaimed nothing.
	if st, _, leaseNull, _, _ := e.jobState(t, job); st != "sending" || leaseNull {
		t.Fatalf("job state=%s leaseNull=%v, want sending/false (rescan does NOT reclaim — that's the loop's job)", st, leaseNull)
	}
}

// TestRescan_LeaseExtensionVsOutcomeNoDeadlock (ADR-0010 mandate — the two-connection deadlock test
// for the new job-touching path Phase 7 adds, §1 + R4). The rescan's bulk lease-extension UPDATE runs
// CONCURRENTLY with applyClassifiedFailure (account→job, exclusive account FOR UPDATE then a job
// requeue) on the SAME in-flight job, on genuinely separate connections, over many rounds. Because
// the rescan's UPDATE is event-free (no state change → no job_events → no account FK KEY SHARE), it
// holds NO account lock and cannot cycle (40P01) with the outcome handler's account→job order. Assert:
// no round errors (a deadlock aborts one side), and the job ends coherent — either requeued by the
// classified failure, or still in-flight with an extended lease.
//
// SCOPE — read this before trusting it as the lock-order proof (CLAUDE.md Rule 10, added 2026-07-26).
// Instrumented, this test resolves 40/40 rounds with the classified failure winning: the rescan's
// UPDATE usually runs when the job has already left 'sending', so it matches zero rows and the two
// statements NEVER ACTUALLY CONTEND for the same job row. It is therefore a real test of OUTCOME
// COHERENCE under concurrency, and NOT evidence about the two lock ORDERS — it would pass unchanged
// even with the ordering wrong. The lock-order proof is rescan_lockorder_test.go, which forces each
// interleave with held transactions and fails if it observes no contention. Do not delete this test
// (coherence is worth asserting) and do not cite it for lock ordering.
func TestRescan_LeaseExtensionVsOutcomeNoDeadlock(t *testing.T) {
	e := newDispatchEnv(t, "rescandeadlock")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	e.setConfigInt(t, "startup_reconnect_window_sec", 120)
	// Keep the account high-scored (recent success) so the classified failures across many rounds
	// don't quarantine it and stall dispatchToSending — we're testing the LOCK ORDER, not scoring.
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
	oh := NewOutcomeHandler(p1, e.cfg)          // applyClassifiedFailure: account FOR UPDATE, then job requeue
	rescanner := NewDispatcher(p2, e.dec, e.px) // event-free bulk lease extension

	const rounds = 40
	failWins, extendWins := 0, 0
	for i := 0; i < rounds; i++ {
		job := e.seedQueuedJob(t, "dl@example.com")
		e.dispatchToSending(t, job)
		e.px.freeAllWorkers()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// A classified (transient) failure — requeues the job AND takes the exclusive account lock.
			oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
				SMTPResponse: "451 4.3.0 temporary local error"})
		}()
		go func() {
			defer wg.Done()
			if _, err := rescanner.StartupRescan(ctx); err != nil {
				t.Errorf("round %d: StartupRescan errored (possible deadlock 40P01): %v", i, err)
			}
		}()
		wg.Wait()

		st, _, _, _, _ := e.jobState(t, job)
		switch st {
		case "queued":
			failWins++ // the classified failure requeued it (rescan's extend was a no-op on a now-queued row)
		case "sending", "assigned":
			extendWins++ // still in-flight — rescan extended its lease; the failure had not committed yet
		default:
			t.Fatalf("round %d: unexpected state %q after rescan/outcome race", i, st)
		}
	}
	if failWins+extendWins != rounds {
		t.Fatalf("only %d/%d rounds resolved coherently", failWins+extendWins, rounds)
	}
	t.Logf("rescan-vs-classified-failure race: %d requeued, %d still-in-flight over %d rounds (all coherent, no deadlock)", failWins, extendWins, rounds)
}

// TestRescan_Idempotent (§1.4) — running the rescan twice (a Control that restarts again mid-window) is
// safe: it simply re-extends leases + re-refreshes heartbeats from the new boot, writing no events
// and corrupting nothing. Two back-to-back rescans leave the job in-flight with a fresh lease and no
// job_events growth.
func TestRescan_Idempotent(t *testing.T) {
	e := newDispatchEnv(t, "rescanidem")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.setConfigInt(t, "startup_reconnect_window_sec", 120)

	job := e.seedQueuedJob(t, "idem@example.com")
	e.dispatchToSending(t, job)
	e.px.freeAllWorkers()
	e.expireLease(t, job)
	_, eventsBefore := e.jobLeaseAndEvents(t, job)

	for i := 0; i < 2; i++ {
		if _, err := e.d.StartupRescan(ctx); err != nil {
			t.Fatalf("StartupRescan #%d: %v", i+1, err)
		}
	}
	_, eventsAfter := e.jobLeaseAndEvents(t, job)
	if eventsAfter != eventsBefore {
		t.Fatalf("two rescans wrote %d events, want 0 (idempotent + event-free)", eventsAfter-eventsBefore)
	}
	if st, _, leaseNull, _, _ := e.jobState(t, job); st != "sending" || leaseNull {
		t.Fatalf("after 2 rescans job state=%s leaseNull=%v, want sending/false", st, leaseNull)
	}
}
