package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// setWorkerHeartbeat forces a worker's last_heartbeat_at into the past (or future) to simulate silence /
// liveness for the reclaim detectors. Written via the admin conn (a test fixture, not the app path).
func (e *dispatchEnv) setWorkerHeartbeat(t *testing.T, workerID string, ago time.Duration) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(),
		`UPDATE send_managed.boxes SET last_heartbeat_at = NOW() - make_interval(secs => $2) WHERE id=$1`,
		workerID, int(ago.Seconds())); err != nil {
		t.Fatalf("set worker heartbeat: %v", err)
	}
}

// workerStatus reads a worker's current status.
func (e *dispatchEnv) workerStatus(t *testing.T, workerID string) string {
	t.Helper()
	var s string
	if err := e.admin.QueryRow(context.Background(),
		`SELECT status::text FROM send_managed.boxes WHERE id=$1`, workerID).Scan(&s); err != nil {
		t.Fatalf("read worker status: %v", err)
	}
	return s
}

// reclaimEvents returns the (to_state, actor, error_class) of every job_events row for a job whose
// actor is 'reclaim_loop', in order — the auditable provenance a reclaim must leave (§2.4).
func (e *dispatchEnv) reclaimEvents(t *testing.T, jobID string) [][3]string {
	t.Helper()
	rows, err := e.admin.Query(context.Background(),
		`SELECT to_state::text, actor::text, COALESCE(error_class::text,'')
		   FROM send_managed.job_events WHERE job_id=$1 AND actor='reclaim_loop' ORDER BY created_at`, jobID)
	if err != nil {
		t.Fatalf("read reclaim events: %v", err)
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var st, ac, ec string
		if err := rows.Scan(&st, &ac, &ec); err != nil {
			t.Fatalf("scan reclaim event: %v", err)
		}
		out = append(out, [3]string{st, ac, ec})
	}
	return out
}

// TestReclaim_DeadWorkerMassReclaims (Detector A, §2.2) — a worker silent past
// heartbeat_interval×missed_threshold is marked 'stale' and ALL its in-flight jobs are reclaimed to
// queued via the locked reclaim transition (actor='reclaim_loop', error_class='timeout', lease
// NULL, saturating attempt_count). This is the core dead-worker behavior behind T3.
func TestReclaim_DeadWorkerMassReclaims(t *testing.T) {
	e := newDispatchEnv(t, "reclaimdead")
	ctx := context.Background()
	pool := e.seedPool(t)
	// Over-provision accounts so several jobs can be in flight on the one worker at once.
	for i := 0; i < 3; i++ {
		e.seedAccount(t, pool, "sec")
	}

	// Drive THREE jobs to sending on the default worker (each freed between pushes so the single-worker
	// recordingPusher admits the next). All three end up assigned_box_id = e.px.workerID, state=sending.
	var jobs []string
	for i := 0; i < 3; i++ {
		j := e.seedQueuedJob(t, "dead@example.com")
		e.dispatchToSending(t, j)
		e.px.freeAllWorkers()
		jobs = append(jobs, j)
	}

	// Baseline attempt_count for a later saturation cross-check.
	_, _, _, _, attempt0 := e.jobState(t, jobs[0])

	// The worker goes silent past the window (default 120s × 3 = 360s; use 400s to be safely past).
	e.setWorkerHeartbeat(t, e.px.workerID, 400*time.Second)

	// One reclaim tick.
	cfg, n, err := e.d.ReclaimTick(ctx)
	if err != nil {
		t.Fatalf("ReclaimTick: %v", err)
	}
	_ = cfg
	if n != 3 {
		t.Fatalf("ReclaimTick reclaimed %d jobs, want 3 (all in-flight on the dead worker)", n)
	}

	// (1) Worker is now stale.
	if s := e.workerStatus(t, e.px.workerID); s != "stale" {
		t.Fatalf("worker status=%s, want stale after heartbeat-miss reclaim", s)
	}

	// (2) Every job: queued, lease NULL, attempt_count incremented (saturating), one reclaim_loop
	//     job_events row with to_state=queued, error_class=timeout.
	for _, j := range jobs {
		state, _, leaseNull, completed, attempt := e.jobState(t, j)
		if state != "queued" || !leaseNull || completed {
			t.Fatalf("job %s: state=%s leaseNull=%v completed=%v, want queued/true/false", j, state, leaseNull, completed)
		}
		if attempt != attempt0+1 {
			t.Fatalf("job %s: attempt_count=%d, want %d (saturating +1)", j, attempt, attempt0+1)
		}
		evs := e.reclaimEvents(t, j)
		if len(evs) != 1 {
			t.Fatalf("job %s: %d reclaim_loop events, want exactly 1", j, len(evs))
		}
		if evs[0] != [3]string{"queued", "reclaim_loop", "timeout"} {
			t.Fatalf("job %s: reclaim event = %v, want [queued reclaim_loop timeout]", j, evs[0])
		}
	}
}

// TestReclaim_LiveWorkerNotReclaimed (Detector A negative control / anti-vacuity) — a worker that is
// still heartbeating within the window is NOT marked stale and its in-flight job is NOT reclaimed.
// Paired with the positive case above, this proves Detector A fires on silence, not unconditionally.
func TestReclaim_LiveWorkerNotReclaimed(t *testing.T) {
	e := newDispatchEnv(t, "reclaimlive")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	job := e.seedQueuedJob(t, "live@example.com")
	e.dispatchToSending(t, job)

	// Worker heartbeated 10s ago — well within the 360s window.
	e.setWorkerHeartbeat(t, e.px.workerID, 10*time.Second)

	_, n, err := e.d.ReclaimTick(ctx)
	if err != nil {
		t.Fatalf("ReclaimTick: %v", err)
	}
	if n != 0 {
		t.Fatalf("ReclaimTick reclaimed %d jobs, want 0 (worker is alive)", n)
	}
	if s := e.workerStatus(t, e.px.workerID); s != "active" {
		t.Fatalf("worker status=%s, want active (never silent long enough)", s)
	}
	if state, _, leaseNull, _, _ := e.jobState(t, job); state != "sending" || leaseNull {
		t.Fatalf("job state=%s leaseNull=%v, want sending/false (not reclaimed)", state, leaseNull)
	}
	if len(e.reclaimEvents(t, job)) != 0 {
		t.Fatalf("live-worker job has reclaim_loop events, want none")
	}
}

// expireLease forces a job's lease_expires_at into the past (a hung/stuck send), keeping it
// in-flight — the setup for Detector B. It bypasses the state trigger (no state change), a fixture
// write via the admin conn.
func (e *dispatchEnv) expireLease(t *testing.T, jobID string) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(),
		`UPDATE send_managed.jobs SET lease_expires_at = NOW() - INTERVAL '1 second' WHERE id=$1`, jobID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

// TestReclaim_HungJobOnLiveWorkerReclaimed (Detector B, §2.3 — T4 core) — a job whose lease expires is
// reclaimed EVEN THOUGH its worker is still heartbeating; Detector A never flags the live worker, and a
// SECOND healthy in-flight job on the same live worker is NOT reclaimed. This proves the per-job
// backstop fires on lease expiry independent of worker liveness — the anti-vacuity teeth.
func TestReclaim_HungJobOnLiveWorkerReclaimed(t *testing.T) {
	e := newDispatchEnv(t, "reclaimhung")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.seedAccount(t, pool, "sec")

	// Two jobs in flight on the one (live) worker.
	hung := e.seedQueuedJob(t, "hung@example.com")
	e.dispatchToSending(t, hung)
	e.px.freeAllWorkers()
	healthy := e.seedQueuedJob(t, "healthy@example.com")
	e.dispatchToSending(t, healthy)

	// Worker is alive (recent heartbeat). Only the hung job's lease has expired.
	e.setWorkerHeartbeat(t, e.px.workerID, 5*time.Second)
	e.expireLease(t, hung)

	_, n, err := e.d.ReclaimTick(ctx)
	if err != nil {
		t.Fatalf("ReclaimTick: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d, want 1 (only the lease-expired job)", n)
	}
	// (1) Detector A never flagged the live worker.
	if s := e.workerStatus(t, e.px.workerID); s != "active" {
		t.Fatalf("worker status=%s, want active (still heartbeating)", s)
	}
	// (2) The hung job is reclaimed by Detector B.
	state, _, leaseNull, _, _ := e.jobState(t, hung)
	if state != "queued" || !leaseNull {
		t.Fatalf("hung job state=%s leaseNull=%v, want queued/true", state, leaseNull)
	}
	evs := e.reclaimEvents(t, hung)
	if len(evs) != 1 || evs[0] != [3]string{"queued", "reclaim_loop", "timeout"} {
		t.Fatalf("hung job reclaim events = %v, want one [queued reclaim_loop timeout]", evs)
	}
	// (3) The healthy job on the same live worker is untouched.
	if state, _, leaseNull, _, _ := e.jobState(t, healthy); state != "sending" || leaseNull {
		t.Fatalf("healthy job state=%s leaseNull=%v, want sending/false (not reclaimed)", state, leaseNull)
	}
	if len(e.reclaimEvents(t, healthy)) != 0 {
		t.Fatalf("healthy job has reclaim events, want none")
	}
}

// TestReclaim_AssignedNeverPushedReclaimed (Detector B, §2.3) — a job stranded in 'assigned' (the
// Phase 4 TXN1↔TXN2 crash window: claimed, lease set, never pushed to a worker, assigned_box_id NULL)
// is invisible to Detector A but reclaimed by the per-job lease backstop once its lease expires.
func TestReclaim_AssignedNeverPushedReclaimed(t *testing.T) {
	e := newDispatchEnv(t, "reclaimassigned")
	ctx := context.Background()

	// Seed a job and move it to 'assigned' with an expired lease and NO worker (mirrors the crash
	// window: claimForUser set state=assigned + lease, before TXN2 recorded a worker).
	job := e.seedQueuedJob(t, "assigned@example.com")
	// Strand in 'assigned' with an expired lease inside ONE tx so the tx-local actor GUC is set for
	// the state-change trigger (set_config(...,true) is transaction-scoped).
	tx, err := e.admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor','dispatcher',true)"); err != nil {
		t.Fatalf("set actor: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE send_managed.jobs SET state='assigned', lease_expires_at=NOW()-INTERVAL '1 second' WHERE id=$1`, job); err != nil {
		t.Fatalf("strand assigned: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit strand: %v", err)
	}
	// Sanity: no worker assigned.
	var workerID *string
	if err := e.admin.QueryRow(ctx, `SELECT assigned_box_id FROM send_managed.jobs WHERE id=$1`, job).Scan(&workerID); err != nil {
		t.Fatalf("read worker: %v", err)
	}
	if workerID != nil {
		t.Fatalf("precondition: assigned_box_id=%v, want NULL", *workerID)
	}

	_, n, err := e.d.ReclaimTick(ctx)
	if err != nil {
		t.Fatalf("ReclaimTick: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d, want 1 (the stranded assigned job)", n)
	}
	state, _, leaseNull, _, _ := e.jobState(t, job)
	if state != "queued" || !leaseNull {
		t.Fatalf("assigned job state=%s leaseNull=%v, want queued/true", state, leaseNull)
	}
	if evs := e.reclaimEvents(t, job); len(evs) != 1 || evs[0] != [3]string{"queued", "reclaim_loop", "timeout"} {
		t.Fatalf("assigned job reclaim events = %v, want one [queued reclaim_loop timeout]", evs)
	}
}

// TestReclaim_NotExpiredNotReclaimed (Detector B negative control) — a job whose lease is still in
// the future is NOT reclaimed, proving Detector B keys on lease expiry, not on in-flight state alone.
func TestReclaim_NotExpiredNotReclaimed(t *testing.T) {
	e := newDispatchEnv(t, "reclaimfresh")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	job := e.seedQueuedJob(t, "fresh@example.com")
	e.dispatchToSending(t, job) // lease = NOW() + 120s (job_max_processing_time_sec), still valid
	e.setWorkerHeartbeat(t, e.px.workerID, 5*time.Second)

	_, n, err := e.d.ReclaimTick(ctx)
	if err != nil {
		t.Fatalf("ReclaimTick: %v", err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d, want 0 (lease still valid, worker alive)", n)
	}
	if state, _, leaseNull, _, _ := e.jobState(t, job); state != "sending" || leaseNull {
		t.Fatalf("job state=%s leaseNull=%v, want sending/false", state, leaseNull)
	}
}

// TestReclaim_RedispatchesToDifferentWorkerNoDoubleSend (§2.5/§2.6 — T5) — a reclaimed job re-enters
// dispatch and sends via a DIFFERENT worker, exactly once. And (T5b) a reclaimed job that ALREADY has
// a provider_message_id persisted is short-circuited by the idempotency gate — a late duplicate
// accepted report is a clean no-op (no second push, no raw 23505), never re-sent.
func TestReclaim_RedispatchesToDifferentWorkerNoDoubleSend(t *testing.T) {
	e := newDispatchEnv(t, "reclaimredisp")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.addWorker(t) // a second worker so re-dispatch can land elsewhere

	// (T5a) Dispatch, reclaim (dead worker), then re-dispatch → sends exactly once.
	job := e.seedQueuedJob(t, "redisp@example.com")
	e.dispatchToSending(t, job)
	firstWorker := e.px.workerID
	e.setWorkerHeartbeat(t, firstWorker, 400*time.Second) // kill the first worker
	if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 1 {
		t.Fatalf("reclaim n=%d err=%v, want 1/nil", n, err)
	}
	if st, _, _, _, _ := e.jobState(t, job); st != "queued" {
		t.Fatalf("post-reclaim state=%s, want queued", st)
	}
	pushesBefore := e.px.pushCount()
	// Re-dispatch: the dead worker is stale so AnyConnectedWorker returns the healthy extra worker.
	e.px.freeAllWorkers()
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
		t.Fatalf("reset next: %v", err)
	}
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	if st, _, _, _, _ := e.jobState(t, job); st != "sending" {
		t.Fatalf("re-dispatch state=%s, want sending", st)
	}
	if got := e.px.pushCount() - pushesBefore; got != 1 {
		t.Fatalf("re-dispatch produced %d pushes, want exactly 1 (no double-send)", got)
	}

	// (T5b) Persist the send (accepted with a provider_message_id).
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-REDISP-1", SMTPResponse: "250 2.0.0 Ok"})
	if st, hasPMID, _, _, _ := e.jobState(t, job); st != "sent" || !hasPMID {
		t.Fatalf("after accept state=%s hasPMID=%v, want sent/true", st, hasPMID)
	}
	// A late DUPLICATE accepted report (the crash-after-250 worker that finally reported) is a clean
	// no-op — the idempotency gate (provider_message_id already set) short-circuits it.
	pushesNow := e.px.pushCount()
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-REDISP-DUP", SMTPResponse: "250 2.0.0 Ok"})
	var pmid string
	if err := e.admin.QueryRow(ctx, `SELECT provider_message_id FROM send_managed.jobs WHERE id=$1`, job).Scan(&pmid); err != nil {
		t.Fatalf("read pmid: %v", err)
	}
	if pmid != "PMID-REDISP-1" {
		t.Fatalf("duplicate report changed provider_message_id to %q — idempotency gate failed", pmid)
	}
	if e.px.pushCount() != pushesNow {
		t.Fatalf("duplicate report triggered a new push — must be a clean no-op")
	}
}

// TestReclaim_DropReconnectWithinWindowNoThrash (§3, P6-3 — T6) — worker disconnect is
// heartbeat-timeout-driven, NOT eager: a stream drop followed by a reconnect WITHIN the silence
// window reclaims nothing (no thrash), while sustained silence PAST the window does reclaim. The
// drop itself is modeled by last_heartbeat_at ceasing to advance (the DB timestamp is the single
// reclaim trigger, §3.1 — a dropped mTLS stream changes no DB state). Paired positive/negative in
// one test so it can't pass merely because reclaim never runs.
func TestReclaim_DropReconnectWithinWindowNoThrash(t *testing.T) {
	e := newDispatchEnv(t, "reclaimblip")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	job := e.seedQueuedJob(t, "blip@example.com")
	e.dispatchToSending(t, job)

	// (negative) Stream "dropped" but only silent 100s — WITHIN the 360s window. No reclaim.
	e.setWorkerHeartbeat(t, e.px.workerID, 100*time.Second)
	if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 0 {
		t.Fatalf("within-window blip: reclaimed n=%d err=%v, want 0/nil (no thrash)", n, err)
	}
	if s := e.workerStatus(t, e.px.workerID); s != "active" {
		t.Fatalf("worker status=%s during a brief blip, want active (never silent long enough)", s)
	}
	if state, _, leaseNull, _, _ := e.jobState(t, job); state != "sending" || leaseNull {
		t.Fatalf("job state=%s leaseNull=%v during blip, want sending/false (not reclaimed)", state, leaseNull)
	}

	// (reconnect) A heartbeat arrives → last_heartbeat_at fresh again; still no reclaim ever fired.
	e.setWorkerHeartbeat(t, e.px.workerID, 1*time.Second)
	if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 0 {
		t.Fatalf("post-reconnect: reclaimed n=%d, want 0", n)
	}
	if len(e.reclaimEvents(t, job)) != 0 {
		t.Fatalf("blip produced reclaim events, want none (heartbeat-driven, not eager — P6-3)")
	}

	// (positive) Now the worker stays silent PAST the window — reclaim DOES fire. Proves the negative
	// case above wasn't vacuous.
	e.setWorkerHeartbeat(t, e.px.workerID, 400*time.Second)
	if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 1 {
		t.Fatalf("past-window silence: reclaimed n=%d err=%v, want 1/nil", n, err)
	}
	if s := e.workerStatus(t, e.px.workerID); s != "stale" {
		t.Fatalf("worker status=%s after sustained silence, want stale", s)
	}
}

// TestReclaim_DisabledWorkerExcluded (§2.2) — a 'disabled' worker (admin verb, intentionally offline) is
// excluded from Detector A even when silent: it is not re-marked and its scan is skipped. (Its
// jobs, if any, still expire by lease via Detector B — Chunk 3.)
func TestReclaim_DisabledWorkerExcluded(t *testing.T) {
	e := newDispatchEnv(t, "reclaimdisabled")
	ctx := context.Background()

	// Disable the default worker (status='disabled' requires disabled_at non-null — boxes_disabled_at_check).
	if _, err := e.admin.Exec(ctx,
		`UPDATE send_managed.boxes SET status='disabled', disabled_at=NOW() WHERE id=$1`, e.px.workerID); err != nil {
		t.Fatalf("disable worker: %v", err)
	}
	e.setWorkerHeartbeat(t, e.px.workerID, 400*time.Second) // silent AND disabled

	_, n, err := e.d.ReclaimTick(ctx)
	if err != nil {
		t.Fatalf("ReclaimTick: %v", err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d, want 0 (disabled worker excluded)", n)
	}
	if s := e.workerStatus(t, e.px.workerID); s != "disabled" {
		t.Fatalf("worker status=%s, want disabled (not re-marked stale)", s)
	}
}
