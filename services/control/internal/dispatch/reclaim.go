// reclaim.go — Phase 6 §2: the continuous reclaim loop. A THIRD single-Control polling loop (beside
// the Phase 4 dispatch loop and the Phase 6 lane pacing), on its own reclaim_tick_interval_ms.
// Each tick runs two indexed detectors:
//   - Detector A (§2.2, this file): dead-worker heartbeat scan → mark stale + mass-reclaim its jobs.
//   - Detector B (§2.3, Chunk 3): per-job lease-expiry backstop, worker-liveness-independent.
//
// Both converge on the ONE locked reclaim transition (§2.4, reclaimJobsWhere): assigned|sending →
// queued, actor='reclaim_loop', lease cleared, saturating attempt_count, error_class='timeout',
// next_attempt_at from Phase 5's timeout backoff. The account is deliberately NOT touched — no flag,
// no score recompute — because a reclaim is a Control-DECLARED timeout with no send outcome to
// attribute (P6-10; the carve-out is structural — this file's transition writes only send_managed.jobs).
//
// Race-safety rests on the single-Control model (identical to Phase 4 §6): exactly one reclaim loop
// and one dispatch loop in one process. To be safe against the tiny window where a lane worker is
// completing a handoff on a job the reclaim loop also sees, every reclaim UPDATE is STATE-GUARDED
// (WHERE state IN ('assigned','sending')): if a report already moved the job to sent/bounced, the
// guard matches nothing and the reclaim is a clean no-op. Detector A runs first; its UPDATE moves
// the job to queued, so Detector B's WHERE no longer matches it — reclaimed once, never twice.
package dispatch

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// reclaimJobLocked applies the §2.4 locked reclaim transition to ONE job by id, INSIDE the caller's
// tx (which must have set the actor GUC to 'reclaim_loop'). BOTH detectors reclaim through this one
// per-job path (Detector A row-by-row over a dead worker's jobs — spec §2.4 explicitly endorses
// "row-by-row under SKIP LOCKED for large fan-outs"; Detector B per lease-expired job), so there is
// exactly one reclaim code path and one lock discipline.
//
// LOCK ORDER (deadlock avoidance — ADR-0010, the module's recurring defect zone). The job UPDATE
// below fires the AFTER UPDATE OF state trigger, whose job_events INSERT carries assigned_account_id
// and so takes a FK KEY SHARE lock on that account row. Phase 5's applyClassifiedFailure locks the
// account FOR UPDATE *before* its job requeue (account→job). If reclaim instead let the trigger grab
// the account lock AFTER already holding the job row (job→account), the two would cycle
// (40P01) on a shared {job, account}. So reclaim mirrors applyClassifiedFailure EXACTLY:
//  1. read the job's assigned_account_id (stable while in-flight — requeue clears only the worker);
//  2. lock that account FOR UPDATE *first* (account→job order — matches the outcome handler);
//  3. lock the job row FOR UPDATE SKIP LOCKED and re-assert the in-flight guard — SKIP LOCKED so a
//     lane worker mid-handoff (markSending, job→account) is SKIPPED (retried next tick), never
//     blocked-on, so that opposite-order pair can't cycle either;
//  4. apply the transition on the now-locked row (the trigger's KEY SHARE on the account is a no-op
//     re-request of the FOR UPDATE we already hold — no upgrade, no cycle).
//
// Locking the account is NOT a write — the P6-10 carve-out (a Control-declared reclaim performs no
// account UPDATE and no score recompute) is fully preserved; a shared/exclusive row lock touches no
// column. This function reads assigned_account_id and WRITES ONLY send_managed.jobs columns.
//
// The transition itself is Phase 5's timeout JOB policy declared by the Control: state→'queued' (the
// one locked reassignment, P4-20); lease NULL (jobs_lease_valid_check); attempt_count saturates via
// LEAST(+1,max) (§6.2, never violates jobs_attempt_count_check); last_error_class='timeout' (§1.1
// row 6, copied into job_events by the trigger → auditable 'reclaim_loop' provenance);
// last_error_message = the internal reason (≤2000, two-vocabulary, never crosses a client boundary);
// next_attempt_at = NOW() + the Phase 5 backoff, REFERENCED not redefined (ladder+cap from
// cfg.Backoff, indexed by the job's own saturating post-increment attempt, byte-equivalent to
// BackoffSchedule.backoffForAttempt). assigned_box_id/assigned_account_id are left as-is so routing
// re-picks freely on the next dispatch. Returns 1 if reclaimed, 0 if skipped/no-op (row locked by a
// worker, or already left in-flight — a clean idempotent no-op, retried next tick).
func reclaimJobLocked(ctx context.Context, tx pgx.Tx, cfg Config, jobID, reason string) (int, error) {
	ladder := cfg.Backoff.LadderSeconds
	if len(ladder) == 0 {
		ladder = backoffScheduleDefault.LadderSeconds
	}
	maxSecs := cfg.Backoff.MaxSeconds
	if maxSecs <= 0 {
		maxSecs = backoffScheduleDefault.MaxSeconds
	}

	// (1) Read the account this in-flight job used (unlocked; assigned_account_id is stable while the
	// job is in {assigned,sending} — no path mutates it there). Gate on the in-flight state so a job
	// that already left is a clean no-op.
	var acctID *string
	if err := tx.QueryRow(ctx,
		`SELECT assigned_account_id FROM send_managed.jobs WHERE id=$1 AND state IN ('assigned','sending')`, jobID).Scan(&acctID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil // already left in-flight (reported sent/bounced/requeued) — nothing to reclaim
		}
		return 0, err
	}

	// (2) Lock the account FOR UPDATE FIRST (account→job order, mirroring applyClassifiedFailure).
	if acctID != nil {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM send_managed.accounts WHERE id=$1 FOR UPDATE`, *acctID); err != nil {
			return 0, err
		}
	}

	// (3) Lock the job row, SKIP LOCKED (never block on a lane worker mid-handoff), re-asserting the
	// in-flight guard under the lock. No row → skipped/raced → clean no-op this tick.
	var locked string
	if err := tx.QueryRow(ctx,
		`SELECT id FROM send_managed.jobs WHERE id=$1 AND state IN ('assigned','sending') FOR UPDATE SKIP LOCKED`, jobID).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}

	// (4) Apply the §2.4 transition on the locked row.
	tag, err := tx.Exec(ctx, `
UPDATE send_managed.jobs
   SET state            = 'queued',
       lease_expires_at = NULL,
       attempt_count    = LEAST(attempt_count + 1, max_attempts),
       last_error_class = 'timeout',
       last_error_message = $2,
       next_attempt_at  = NOW() + make_interval(secs => LEAST(
           ($3::int[])[LEAST(GREATEST(LEAST(attempt_count + 1, max_attempts), 1), array_length($3::int[], 1))],
           $4::int)),
       updated_at       = NOW()
 WHERE id = $1
   AND state IN ('assigned','sending')`, jobID, truncate(reason, 2000), ladder, maxSecs)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ReclaimTick runs one full reclaim cycle (§2.1): re-read config fresh (Phase 2 OD-14 — a config
// change takes effect next tick, no redeploy), then Detector A followed by Detector B. Returns the
// Config it read (so the caller times the next wait) and the number of jobs reclaimed this tick.
// A per-detector APPLICATION error is logged and does not abort the other detector or the tick — a
// transient failure degrades to "retry next tick", never crashes the singleton Control (Rule 7).
//
// Phase 7 §2.2 (P7-3): a detector error that is TRANSPORT unavailability is instead RETURNED, so the
// caller's outage gate trips and the loop pauses. Without this, an outage that began mid-tick (after
// LoadConfig succeeded) would be swallowed per-detector and the loop would keep hammering a dead
// socket every interval instead of halting. It changes no write behavior: both detectors are
// state-guarded and simply do nothing when their statements cannot reach the DB.
func (d *Dispatcher) ReclaimTick(ctx context.Context) (Config, int, error) {
	cfg, err := LoadConfig(ctx, d.db)
	if err != nil {
		return Config{}, 0, err
	}
	// Fix-plan item 12: the DEEPEST rung freezes the reclaim sweep. This is the only column that
	// distinguishes `stopped` from `refuse_new`, and it is deliberately the last thing to stop: with a
	// dead worker holding an expired lease, refuse_new returns that job to `queued` and stopped leaves
	// it `sending` until the rung is released.
	//
	// NOTHING IS STRANDED BY THIS. Both detectors are DB-predicate-driven — Detector A reads
	// boxes.last_heartbeat_at, Detector B reads `lease_expires_at < NOW()` — so a skipped sweep loses
	// no information whatsoever: the worker is still silent and the lease is still expired on the
	// first tick after release, and the job has not moved. It is safe to freeze *because* claiming
	// already stopped two rungs earlier: no new lease is taken while frozen, so every in-flight row
	// belongs to a pre-pause attempt. (M1's ADR-0015 §3 reaches the same conclusion about its own
	// drainer's reclaim sweep, on the same invariant.)
	//
	// WHY AN OPERATOR WOULD WANT THIS. Reclaim REASSIGNS in-flight jobs (sending→queued). During an
	// incident where you suspect the send path itself — a provider mangling messages, a pool-wide
	// credential problem — requeueing is not neutral: it is the system queueing up another attempt at
	// the thing you are trying to stop. Freezing the sweep holds the jobs exactly as they are for
	// inspection. The cost is real and stated in the docs: a job whose worker died during a `stopped`
	// pause waits out the pause before anyone reclaims it.
	//
	// Nothing here writes, so a frozen tick is a pure no-op — no state change, no job_events row.
	if !cfg.ReclaimAllowed() {
		d.logReclaimFrozenByOperator(cfg)
		return cfg, 0, nil
	}
	reclaimed := 0
	if nA, err := d.reclaimDeadWorkers(ctx, cfg); err != nil {
		if IsOutage(err) {
			return cfg, reclaimed, err
		}
		log.Printf("reclaim: dead-worker detector failed: %v", err)
	} else {
		reclaimed += nA
	}
	if nB, err := d.reclaimExpiredLeases(ctx, cfg); err != nil {
		if IsOutage(err) {
			return cfg, reclaimed, err
		}
		log.Printf("reclaim: lease-expiry detector failed: %v", err)
	} else {
		reclaimed += nB
	}
	return cfg, reclaimed, nil
}

// reclaimDeadWorkers is Detector A (§2.2): the periodic dead-worker scan Phase 3 deferred (P3-9),
// superseding Phase 3's lazy-only staleness. A worker is dead iff it has been silent past
// heartbeat_interval_sec × heartbeat_missed_threshold (default 360s). Each dead worker is marked
// 'stale' (the enum value whose Phase 1 comment is literally "Missed heartbeats, jobs being
// reclaimed" — which also stops new dispatch to it, since the dispatcher pushes only to active
// workers) and ALL its in-flight jobs are mass-reclaimed. 'disabled' workers are excluded (an admin
// verb — intentionally offline; their jobs, if any, still expire by lease via Detector B).
//
// Served by the Phase 1 boxes_liveness_idx (status, last_heartbeat_at) — no new index.
func (d *Dispatcher) reclaimDeadWorkers(ctx context.Context, cfg Config) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	silenceSecs := cfg.HeartbeatIntervalSec * cfg.HeartbeatMissedThreshold

	// 1. Find silent-but-not-disabled workers (served by boxes_liveness_idx). Read them in a plain
	//    query (no long-held lock) — the per-worker reclaim below re-checks status='active' under the
	//    UPDATE, so a worker that changed state between this read and the reclaim is handled correctly.
	rows, err := d.db.Query(ctx,
		`SELECT id FROM send_managed.boxes
		  WHERE status <> 'disabled'
		    AND last_heartbeat_at < NOW() - make_interval(secs => $1)`, silenceSecs)
	if err != nil {
		return 0, err
	}
	var deadWorkers []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		deadWorkers = append(deadWorkers, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	total := 0
	for _, worker := range deadWorkers {
		n, err := d.reclaimJobsOnWorker(ctx, worker, cfg)
		if err != nil {
			log.Printf("reclaim: worker %s mass-reclaim failed: %v", worker, err)
			continue // a per-worker error must not abort the scan of the other dead workers
		}
		total += n
	}
	return total, nil
}

// reclaimExpiredLeases is Detector B (§2.3): the per-job lease-expiry backstop, which fires
// REGARDLESS of worker liveness. Detector A cannot catch a job that hangs on a worker that is still
// heartbeating (a wedged SMTP session, a stuck send); the per-job lease is the backstop. A job
// whose lease_expires_at has passed is presumed stuck and reclaimed even if its worker is alive — the
// "hung job on a live worker" case (test T4). This also catches the leftover 'assigned' jobs from a
// Control crash in Phase 4's TXN1↔TXN2 window (assigned_box_id IS NULL, invisible to Detector A).
//
// Each expired job is reclaimed in its OWN transaction under FOR UPDATE SKIP LOCKED: the row is
// re-selected and locked (skipping any a lane worker is mid-handoff on), then the locked §2.4
// transition applies with the state guard re-asserted. A job Detector A already moved to 'queued'
// this tick no longer matches (state guard) — reclaimed once, never twice (§2.1). Served by the
// Phase 1 jobs_reclaim_idx (lease_expires_at, state) WHERE state IN ('assigned','sending') AND
// lease_expires_at IS NOT NULL — no new index.
func (d *Dispatcher) reclaimExpiredLeases(ctx context.Context, cfg Config) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Snapshot the currently-expired job ids (plain read; the per-job tx below re-checks + locks, so
	// a job that changes state between this read and its reclaim is handled by the guard). Bounded by
	// nothing here — a large backlog is reclaimed across ticks naturally; each row is cheap.
	rows, err := d.db.Query(ctx,
		`SELECT id FROM send_managed.jobs
		  WHERE state IN ('assigned','sending') AND lease_expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	var expired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	total := 0
	for _, jobID := range expired {
		n, err := d.reclaimOneJob(ctx, jobID, cfg)
		if err != nil {
			log.Printf("reclaim: lease-expiry reclaim of job %s failed: %v", jobID, err)
			continue // a per-job error must not abort the others
		}
		total += n
	}
	return total, nil
}

// reclaimOneJob reclaims a single lease-expired job (Detector B). Thin wrapper over
// reclaimOneJobReason with the lease-expiry reason.
func (d *Dispatcher) reclaimOneJob(ctx context.Context, jobID string, cfg Config) (int, error) {
	return d.reclaimOneJobReason(ctx, jobID, cfg, "lease expired")
}

// reclaimOneJobReason reclaims a single job by id in its own tx via the shared account→job-ordered,
// SKIP-LOCKED reclaimJobLocked path, tagging the internal reason ('lease expired' / 'worker heartbeat
// missed'). Returns 1 if reclaimed, 0 if skipped/no-op (locked by a worker, or already left
// in-flight — retried next tick). Both detectors funnel through here, so there is exactly one
// reclaim lock discipline.
func (d *Dispatcher) reclaimOneJobReason(ctx context.Context, jobID string, cfg Config, reason string) (int, error) {
	var n int
	err := d.inActorTx(ctx, "reclaim_loop", func(tx pgx.Tx) error {
		cnt, err := reclaimJobLocked(ctx, tx, cfg, jobID, reason)
		if err != nil {
			return err
		}
		n = cnt
		return nil
	})
	return n, err
}

// reclaimJobsOnWorker marks one dead worker 'stale' and reclaims every in-flight job on it. The stale-mark
// is one small tx; each job is then reclaimed ROW-BY-ROW in its own tx via the shared account→job-
// ordered reclaimJobLocked path (spec §2.4 explicitly endorses "row-by-row under SKIP LOCKED for
// large fan-outs"). This is the ADR-0010 deadlock fix (R4-P6 HIGH): the previous single bulk UPDATE
// let the job_events trigger take the account KEY SHARE AFTER the job row (job→account), cycling
// with applyClassifiedFailure's account→job order on a shared {job,account}; reclaimJobLocked now
// locks the account FIRST, matching the outcome handler. Marking stale FIRST also stops new dispatch
// to the worker (the dispatcher pushes only to active workers) before the reclaims run.
//
// Per-job tx (not one big tx) is deliberate: it bounds lock-hold time, lets a job a worker holds
// (SKIP LOCKED) be skipped without rolling back the others, and makes the worker-stale mark durable
// even if a later job's reclaim races. The old "atomic worker+jobs" property is not needed — a worker
// marked stale with a not-yet-reclaimed in-flight job is self-correcting (the job is caught this
// tick's later rows or next tick, and by Detector B via its lease regardless).
func (d *Dispatcher) reclaimJobsOnWorker(ctx context.Context, workerID string, cfg Config) (int, error) {
	// Mark stale (active → stale only, idempotent). A worker that reconnected to 'active' between the
	// scan and here is re-marked, but the per-job guarded reclaim below only touches still-in-flight
	// rows and the over-mark self-heals on the worker's next heartbeat (Phase 3 §3.2).
	if err := d.inActorTx(ctx, "reclaim_loop", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE send_managed.boxes SET status='stale' WHERE id=$1 AND status='active'`, workerID)
		return err
	}); err != nil {
		return 0, err
	}

	// Enumerate the worker's in-flight jobs (served by jobs_assigned_box_id_idx), then reclaim each in
	// its own tx via the shared account→job-ordered path.
	rows, err := d.db.Query(ctx,
		`SELECT id FROM send_managed.jobs WHERE assigned_box_id=$1 AND state IN ('assigned','sending')`, workerID)
	if err != nil {
		return 0, err
	}
	var jobIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		jobIDs = append(jobIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	total := 0
	for _, jobID := range jobIDs {
		n, err := d.reclaimOneJobReason(ctx, jobID, cfg, "worker heartbeat missed")
		if err != nil {
			log.Printf("reclaim: worker %s job %s reclaim failed: %v", workerID, jobID, err)
			continue // a per-job error must not abort the rest of the worker's jobs
		}
		total += n
	}
	return total, nil
}
