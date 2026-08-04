package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// drain_test.go — Phase 7 §6 T4 (graceful shutdown). REAL infra: a real Postgres clone, the real
// dispatch loop, the real outcome handler, real job rows. The "SIGTERM" is the real code path the
// signal handler in main.go runs (Dispatcher.Drain / StartDraining), not a stand-in for it, and the
// "SIGKILL" control is the real absence of that path — the drain is simply never called.

// TestDrain_StopsClaimingThenLeavesInFlightInPlace is T4 assertions (1)(2)(3)(4). One job reports
// WITHIN the drain window (and resolves normally); another does NOT (its worker stalls past the
// timeout) and must be LEFT exactly as stored — never eagerly requeued.
func TestDrain_StopsClaimingThenLeavesInFlightInPlace(t *testing.T) {
	e := newDispatchEnv(t, "drainleave")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.addWorker(t) // a second worker so two jobs can be in flight at once

	// Two jobs driven into flight the real way (claim → resolve → push → sending).
	reporter := e.seedQueuedJob(t, "reports-in-window@example.com")
	staller := e.seedQueuedJob(t, "stalls-past-timeout@example.com")
	if _, _, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("pre-drain tick: %v", err)
	}
	for _, j := range []string{reporter, staller} {
		if state, _, _, _, _ := e.jobState(t, j); state != "sending" {
			t.Fatalf("precondition: job %s state=%s, want sending (both must be in flight before SIGTERM)", j, state)
		}
	}
	// A THIRD job stays queued: it is the witness for assertion (1) — a draining Control must not claim it.
	stillQueued := e.seedQueuedJob(t, "must-not-be-claimed@example.com")
	e.px.freeAllWorkers() // workers are idle again, so ONLY the drain latch can explain a non-claim

	if n, err := e.d.InFlightCount(ctx); err != nil || n != 2 {
		t.Fatalf("precondition: InFlightCount=%d err=%v, want 2", n, err)
	}

	// ---- SIGTERM: step 1, stop-claim ----
	e.d.StartDraining()
	if !e.d.Draining() {
		t.Fatal("StartDraining did not latch")
	}

	// (1) No new queued→assigned claim occurs while draining. Tick is a pure no-op: nothing pushed,
	// the queued job untouched, and NO new job_events (a claim would write one).
	queuedEventsBefore := e.eventCount(t, stillQueued)
	pushesBefore := e.px.pushCount()
	for i := 0; i < 3; i++ {
		_, pushed, err := e.d.Tick(ctx)
		if err != nil {
			t.Fatalf("draining tick returned an error: %v", err)
		}
		if pushed != 0 {
			t.Fatalf("draining tick pushed %d job(s), want 0 — dispatch must stop claiming and pushing on SIGTERM (§4.1 step 1)", pushed)
		}
	}
	// TryDispatch (the Phase-3 primitive, still a live claim path) must honor the drain too.
	if id, err := e.d.TryDispatch(ctx); err != nil || id != "" {
		t.Fatalf("TryDispatch while draining returned (%q, %v), want (\"\", nil) — the second claim path must also stop", id, err)
	}
	if state, _, _, _, _ := e.jobState(t, stillQueued); state != "queued" {
		t.Fatalf("the queued witness job is now %s — a draining Control claimed new work (§4.1 step 1 violated)", state)
	}
	if got := e.eventCount(t, stillQueued); got != queuedEventsBefore {
		t.Fatalf("the queued witness job gained %d job_events while draining, want 0 new", got-queuedEventsBefore)
	}
	if got := e.px.pushCount(); got != pushesBefore {
		t.Fatalf("pushes went %d→%d while draining, want unchanged", pushesBefore, got)
	}

	// ANTI-VACUITY for (1): the ONLY reason nothing was claimed is the drain latch. Prove the very same
	// tick DOES claim once the latch is cleared — otherwise this test would pass on a broken fixture
	// (no eligible account, no worker, empty queue) rather than on the behavior under test.
	e.d.draining.Store(false)
	if _, pushed, err := e.d.Tick(ctx); err != nil || pushed != 1 {
		t.Fatalf("with draining cleared the same tick pushed %d (err=%v), want 1 — the non-claim above was NOT caused by the drain latch (vacuous test)", pushed, err)
	}
	if state, _, _, _, _ := e.jobState(t, stillQueued); state != "sending" {
		t.Fatalf("witness job is %s after the un-drained tick, want sending", state)
	}
	e.d.StartDraining() // back to draining for the rest of the test
	e.px.freeAllWorkers()

	// (2) A job that reports WITHIN the drain window resolves normally and leaves {assigned,sending}.
	// Report it from a goroutine mid-drain, exactly as a real worker would during the window. Resolve the
	// reporting worker BEFORE the goroutine starts (this test runs two workers, so the report must come from
	// the one the job was actually pushed to — see jobWorker).
	reporterWorker := e.jobWorker(t, reporter)
	go func() {
		time.Sleep(300 * time.Millisecond)
		e.oh.Handle(reporterWorker, workerproto.Report{
			Type: workerproto.TypeReport, JobID: reporter, Outcome: workerproto.OutcomeAccepted,
			ProviderMessageID: "drain-pmid-0001", SMTPResponse: "250 2.0.0 OK queued as drain-pmid-0001",
		})
	}()

	// Snapshot the staller BEFORE the drain so "left exactly as stored" is a byte comparison, not a
	// state-name comparison.
	sState, sPMID, sLeaseNull, sCompleted, sAttempt := e.jobState(t, staller)
	sLease := e.leaseOf(t, staller)
	sEvents := e.eventCount(t, staller)

	// ---- step 2: the bounded drain. 2s is far shorter than any report the staller will send (never). ----
	left := e.d.Drain(ctx, 2*time.Second)

	// The reporter resolved normally.
	if state, hasPMID, _, completed, _ := e.jobState(t, reporter); state != "sent" || !hasPMID || !completed {
		t.Fatalf("the in-window reporter is state=%s pmid=%v completed=%v, want sent/true/true (§4.1 step 2: it resolves normally during the drain)", state, hasPMID, completed)
	}

	// (3) The staller is LEFT EXACTLY AS STORED — no sending→queued, no marking, no new event.
	if left < 2 {
		t.Fatalf("Drain reported %d job(s) left in flight, want >=2 (the staller and the witness job both stalled)", left)
	}
	state2, pmid2, leaseNull2, completed2, attempt2 := e.jobState(t, staller)
	if state2 != sState || pmid2 != sPMID || leaseNull2 != sLeaseNull || completed2 != sCompleted || attempt2 != sAttempt {
		t.Fatalf("the stalled in-flight job CHANGED across shutdown: state %s→%s pmid %v→%v leaseNull %v→%v completed %v→%v attempt %d→%d — §4.2 requires leave-in-place (an eager requeue would manufacture a duplicate)",
			sState, state2, sPMID, pmid2, sLeaseNull, leaseNull2, sCompleted, completed2, sAttempt, attempt2)
	}
	if state2 != "sending" {
		t.Fatalf("the stalled job is %s, want sending — shutdown must not move it", state2)
	}
	if lease2 := e.leaseOf(t, staller); lease2 == nil || !lease2.Equal(*sLease) {
		t.Fatalf("the stalled job's lease changed across shutdown (%v → %v) — the drain writes nothing", sLease, lease2)
	}
	if got := e.eventCount(t, staller); got != sEvents {
		t.Fatalf("the stalled job gained %d job_events during shutdown, want 0 — shutdown invents no transition", got-sEvents)
	}

	// (4) The drain itself neither errored nor hung: it returned at its bound, not later.
	// (Timing is implied by Drain returning; a hang would have tripped the test timeout.)
}

// leaseOf reads a job's raw lease_expires_at so a test can assert byte-equality across shutdown
// (jobState only reports whether it is NULL).
func (e *dispatchEnv) leaseOf(t *testing.T, jobID string) *time.Time {
	t.Helper()
	var lease *time.Time
	if err := e.admin.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM send_managed.jobs WHERE id=$1`, jobID).Scan(&lease); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	return lease
}

// TestDrain_ReturnsEarlyWhenEverythingReports is T4 assertion (2) in its clean-deploy form: when every
// in-flight job reports inside the window, the drain ends EARLY (it does not burn the whole timeout)
// and leaves nothing behind. This is the paired positive control for the timeout case above — without
// it, a Drain that always waited the full window and always reported "left" would still pass that test.
func TestDrain_ReturnsEarlyWhenEverythingReports(t *testing.T) {
	e := newDispatchEnv(t, "drainearly")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	job := e.seedQueuedJob(t, "prompt-report@example.com")
	e.dispatchToSending(t, job)

	jobWorker := e.jobWorker(t, job) // report as the worker it was pushed to (see jobWorker)
	go func() {
		time.Sleep(250 * time.Millisecond)
		e.oh.Handle(jobWorker, workerproto.Report{
			Type: workerproto.TypeReport, JobID: job, Outcome: workerproto.OutcomeAccepted,
			ProviderMessageID: "drain-early-0001", SMTPResponse: "250 2.0.0 OK queued as drain-early-0001",
		})
	}()

	start := time.Now()
	left := e.d.Drain(ctx, 30*time.Second) // a generous window it must NOT burn
	elapsed := time.Since(start)

	if left != 0 {
		t.Fatalf("Drain left %d job(s), want 0 (everything reported inside the window)", left)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Drain took %s of a 30s window after the only in-flight job reported — it must return as soon as in-flight hits zero", elapsed)
	}
	if state, _, _, _, _ := e.jobState(t, job); state != "sent" {
		t.Fatalf("job state=%s after the drain, want sent", state)
	}
}

// TestDrain_SigkillAndGracefulConvergeOnTheSameRecoveredState is T4 assertion (5)(6) plus its
// ANTI-VACUITY control (§6 T4): recovery must NOT depend on the drain — a SIGKILL (no drain at all)
// must reach the SAME recovered state via the startup rescan alone; the drain only reduces how much is
// left behind.
//
// Two independent job/environment arms, driven through the two shutdown paths, then BOTH restarted
// (the real RunStartupRescan + the real reclaim loop) and both resolved by a late worker replay:
//
//	arm A — graceful: StartDraining + Drain (the worker does not report in time) → left in place
//	arm B — SIGKILL:  no drain call at all, the process simply vanishes → left in place
//
// Assert: identical post-shutdown job state, identical post-rescan posture, and identical final
// terminal state after the worker replays. No job stranded in either arm.
func TestDrain_SigkillAndGracefulConvergeOnTheSameRecoveredState(t *testing.T) {
	e := newDispatchEnv(t, "drainkill")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.addWorker(t)

	graceful := e.seedQueuedJob(t, "graceful-arm@example.com")
	killed := e.seedQueuedJob(t, "sigkill-arm@example.com")
	if _, _, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	for _, j := range []string{graceful, killed} {
		if state, _, _, _, _ := e.jobState(t, j); state != "sending" {
			t.Fatalf("precondition: %s is %s, want sending", j, state)
		}
	}

	// ---- arm A: GRACEFUL. Drain runs; the worker never reports inside the window. ----
	if left := e.d.Drain(ctx, 1*time.Second); left < 2 {
		t.Fatalf("graceful drain left %d, want >=2 (neither job reported)", left)
	}
	gState, gPMID, gLeaseNull, gCompleted, gAttempt := e.jobState(t, graceful)

	// ---- arm B: SIGKILL. Nothing at all happens to the process — no drain, no cleanup. ----
	// (Represented faithfully by simply not calling Drain: a SIGKILL runs no code, so there is no
	// code path to invoke. The assertion is that the job's state is INDISTINGUISHABLE from arm A's.)
	kState, kPMID, kLeaseNull, kCompleted, kAttempt := e.jobState(t, killed)

	if gState != kState || gPMID != kPMID || gLeaseNull != kLeaseNull || gCompleted != kCompleted || gAttempt != kAttempt {
		t.Fatalf("graceful and SIGKILL left DIFFERENT states: graceful(%s,pmid=%v,leaseNull=%v,done=%v,att=%d) vs killed(%s,pmid=%v,leaseNull=%v,done=%v,att=%d) — the drain must not write anything, so both paths must leave a stalled job identically (§4.3)",
			gState, gPMID, gLeaseNull, gCompleted, gAttempt, kState, kPMID, kLeaseNull, kCompleted, kAttempt)
	}
	if gState != "sending" {
		t.Fatalf("both arms are %s, want sending (left in place, not requeued, not marked)", gState)
	}

	// ---- the NEXT BOOT: a fresh Dispatcher over the same DB, the real startup rescan. ----
	// Force both leases stale first (the shutdown was long enough that the original lease expired) so
	// the rescan has something to do and Detector B would otherwise reclaim immediately.
	e.expireLease(t, graceful)
	e.expireLease(t, killed)

	boot := NewDispatcher(e.pool, e.dec, e.px)
	e.setConfigInt(t, "startup_reconnect_window_sec", 120)
	n, err := boot.StartupRescan(ctx)
	if err != nil {
		t.Fatalf("next-boot rescan: %v", err)
	}
	if n != 2 {
		t.Fatalf("rescan extended %d lease(s), want 2 (both arms' leftovers)", n)
	}
	// A fresh Control is NOT draining — the latch is per-process state, not persisted.
	if boot.Draining() {
		t.Fatal("a freshly-constructed Dispatcher reports draining — the drain latch must not survive a restart")
	}

	// (5) The workers reconnect and REPLAY their buffered outcomes; both jobs resolve with the worker's own
	// provider_message_id and NO duplicate send_logs row.
	for i, j := range []string{graceful, killed} {
		pmid := []string{"replay-graceful-0001", "replay-killed-0001"}[i]
		if !e.oh.Handle(e.jobWorker(t, j), workerproto.Report{
			Type: workerproto.TypeReport, JobID: j, Outcome: workerproto.OutcomeAccepted,
			ProviderMessageID: pmid, SMTPResponse: "250 2.0.0 OK queued as " + pmid,
		}) {
			t.Fatalf("replayed outcome for %s was not acked after the restart", j)
		}
		state, hasPMID, _, completed, _ := e.jobState(t, j)
		if state != "sent" || !hasPMID || !completed {
			t.Fatalf("job %s after replay: state=%s pmid=%v completed=%v, want sent/true/true", j, state, hasPMID, completed)
		}
		if got := e.sendLogCount(t, j); got != 1 {
			t.Fatalf("job %s has %d send_logs rows after the recovery replay, want exactly 1 (no duplicate)", j, got)
		}
	}

	// (6) No job stranded: nothing is left in {assigned,sending}.
	if left, err := boot.InFlightCount(ctx); err != nil || left != 0 {
		t.Fatalf("after recovery InFlightCount=%d err=%v, want 0 — no job may be stranded by either shutdown path", left, err)
	}
}

// TestDrain_UsesSeededTimeoutAndNeverBlocksOnASickDB covers the two remaining shutdown properties:
// the drain window comes from the SEEDED shutdown_drain_timeout_sec (config-not-code — main.go reads
// it, so the value must actually load and be non-zero), and a drain attempted while the DB is
// unreachable ENDS IMMEDIATELY rather than hanging a deploy (there is nothing to drain: no report can
// be persisted, and nothing is written either way).
func TestDrain_UsesSeededTimeoutAndNeverBlocksOnASickDB(t *testing.T) {
	e := newDispatchEnv(t, "draincfg")
	ctx := context.Background()

	cfg := e.mustLoadConfig(t)
	if cfg.ShutdownDrainTimeoutSec != 30 {
		t.Fatalf("seeded shutdown_drain_timeout_sec is %d, want 30 (send_managed_0019 seed drifted; main.go's drain window comes from this key, not a constant)", cfg.ShutdownDrainTimeoutSec)
	}

	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "sickdb-drain@example.com")
	e.dispatchToSending(t, job)

	proxy, proxyDSN := startPGProxy(t, e.moduleDSN)
	ppool := proxiedPool(t, proxyDSN)
	pd := NewDispatcher(ppool, e.dec, e.px)

	proxy.cut()
	start := time.Now()
	left := pd.Drain(ctx, 30*time.Second) // a long window it must NOT wait out on a dead DB
	elapsed := time.Since(start)

	if elapsed > 15*time.Second {
		t.Fatalf("Drain waited %s with the DB unreachable — a shutdown must never block on a sick DB", elapsed)
	}
	if left != -1 {
		t.Fatalf("Drain returned %d with the DB unreachable, want -1 (count unknown ⇒ end the drain, leave everything in place)", left)
	}
	// And it invented nothing: with the DB back, the job is still exactly `sending`.
	proxy.restore()
	if state, hasPMID, _, completed, _ := e.jobState(t, job); state != "sending" || hasPMID || completed {
		t.Fatalf("job is state=%s pmid=%v completed=%v after a drain during an outage, want sending/false/false untouched", state, hasPMID, completed)
	}
}
