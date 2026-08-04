// rescan.go — Phase 7 §1: the one-time startup rescan (Control-restart recovery). This is the
// deliberate belt-and-suspenders pass that runs ONCE per boot, BEFORE the dispatch/reclaim/lane
// loops start (main.go), and re-establishes a correct recovery posture for jobs the crash caught
// in-flight. It is strategy (b) reconnect-reconciliation (P7-2), NOT reclaim-all: it does NOT itself
// reclaim any job — actual reclaim stays the Phase 6 continuous loop's job, via the ONE §2.4
// transition (P6-8: the loop and the rescan share that transition; there is no second reclaim path).
//
// What the rescan does, in one pass:
//
//	(2) EXTEND the lease of every orphaned in-flight ({assigned,sending}) job by
//	    startup_reconnect_window_sec — opening a bounded reconnect window during which Detector B
//	    will NOT reclaim them (lease still in the future) and reconnecting workers can replay buffered
//	    outcomes (§1.3) and resolve their job with NO duplicate.
//	(3) REFRESH last_heartbeat_at on every non-disabled worker to NOW() — the CONTROL died, not the workers,
//	    so workers are presumed live until they miss a heartbeat measured FROM BOOT; this stops
//	    Detector A from instantly marking a reconnectable worker stale on the loop's first tick.
//
// THE CRITICAL PROPERTY (the Phase 6 lesson — ADR-0010): the lease-extension UPDATE (step 2) MUST be
// genuinely EVENT-FREE. It sets ONLY lease_expires_at/updated_at and leaves state unchanged, so:
//   - the AFTER UPDATE OF state trigger (send_managed_0008) does not fire on it at all (its trigger event
//     is UPDATE OF state; a SET that never lists `state` is not an OF-state update), AND
//   - even if it did fire, the trigger's first line is `IF OLD.state = NEW.state THEN RETURN NEW`
//     (no state change ⇒ no event) —
//
// so NO job_events row is inserted, so the statement takes NO FK KEY SHARE on assigned_account_id,
// so it CANNOT cycle (40P01) with a concurrent applyClassifiedFailure/reclaimJobLocked that holds an
// exclusive account lock. It therefore needs NO account pre-lock (CLAUDE.md Rule 9 is scoped to paths
// that take an EXCLUSIVE account lock AND write a job row — this path takes neither an account lock
// nor a state-changing job write). This is the same event-free bare-lease-write the harness fixture
// expireLease already relies on, and is proven by TestRescan_LeaseExtensionIsEventFree +
// TestRescan_LeaseExtensionVsOutcomeNoDeadlock.
//
// Because the rescan issues NO reclaim_loop transition, it needs no actor GUC and writes no
// job_events; the two bulk UPDATEs run in one short transaction. It is idempotent under repeated
// restarts (re-running simply re-extends leases + re-refreshes heartbeats from the new boot —
// bounded, event-free, correct; §1.4).
package dispatch

import (
	"context"
	"log"
	"time"
)

// StartupRescan runs the one-time §1.2 reconnect-reconciliation pass. It reads config fresh (so the
// window is the seeded startup_reconnect_window_sec, config-not-code), then in ONE transaction:
// extends every orphaned in-flight lease by the window (event-free) and refreshes non-disabled worker
// heartbeats to NOW(). It performs NO reclaim — the continuous loop owns that (§1.4). Returns the
// number of orphan jobs whose lease was extended (for the boot log / tests). A failure is returned
// to the caller, which logs it and proceeds to start the loops anyway: the continuous loop is the
// backstop even if the rescan could not run (the rescan only reshapes WHEN the detectors fire; it is
// not load-bearing for eventual recovery), so a transient DB blip at boot must not wedge startup.
func (d *Dispatcher) StartupRescan(ctx context.Context) (int, error) {
	cfg, err := LoadConfig(ctx, d.db)
	if err != nil {
		return 0, err
	}
	return d.startupRescanWithWindow(ctx, cfg.StartupReconnectWindowSec)
}

// startupRescanWithWindow is the body, parameterized by the window so tests can drive an explicit
// value without mutating global config. windowSec <= 0 is treated as "no extension" defensively (a
// mis-seeded/zero window still refreshes heartbeats and is harmless — the continuous loop reclaims
// on the existing leases), but the seeded default is 120 and config-load will reject an unsafe
// buffer/window pair (P7-11, Chunk 4), so a zero here is not a normal path.
func (d *Dispatcher) startupRescanWithWindow(ctx context.Context, windowSec int) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx, err := d.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	// (2) Extend orphaned in-flight leases by the reconnect window. EVENT-FREE: only lease_expires_at/
	// updated_at change; state is untouched, so no job_events row and no account FK lock (see file
	// header). lease stays NON-NULL, so jobs_lease_valid_check holds for {assigned,sending}. Served by
	// jobs_reclaim_idx (state, lease_expires_at). If windowSec <= 0 we skip the extension (defensive).
	var extended int64
	if windowSec > 0 {
		tag, err := tx.Exec(ctx,
			`UPDATE send_managed.jobs
			    SET lease_expires_at = NOW() + make_interval(secs => $1),
			        updated_at = NOW()
			  WHERE state IN ('assigned','sending')`, windowSec)
		if err != nil {
			return 0, err
		}
		extended = tag.RowsAffected()
	}

	// (3) Refresh worker heartbeats so Detector A does not instantly mark a reconnectable worker stale — the
	// Control died, not the workers; they are presumed live until they miss a heartbeat measured FROM BOOT.
	// 'disabled' workers are an admin verb (intentionally offline) and are left as-is. This is a plain
	// non-state column on send_managed.boxes (no trigger), so it is likewise event-free.
	if _, err := tx.Exec(ctx,
		`UPDATE send_managed.boxes SET last_heartbeat_at = NOW() WHERE status <> 'disabled'`); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(extended), nil
}

// RunStartupRescan is the main.go entry point: run the one-time rescan and log its outcome, never
// failing the boot on a transient error (the continuous loop is the backstop, §1.4). Called BEFORE
// the dispatch/reclaim runners start so there is no window in which both the rescan and Detector B
// act on the same job (§1.4 sequencing).
func (d *Dispatcher) RunStartupRescan(ctx context.Context) {
	n, err := d.StartupRescan(ctx)
	if err != nil {
		// Phase 7 §2 (P7-3): if the failure is transport unavailability (the DB went down between the
		// boot ping and here), trip the shared outage gate so the loops start already PAUSED rather
		// than each discovering the outage with a full failing tick. Boot still proceeds — the Control
		// stays alive on bounded backoff and never crash-loops.
		d.health.Note(err)
		log.Printf("startup rescan: FAILED (%v) — proceeding; the continuous reclaim loop is the backstop", err)
		return
	}
	d.health.Note(nil)
	log.Printf("startup rescan: extended %d orphaned in-flight lease(s) + refreshed worker heartbeats (reconnect window opened; reclaim delegated to the continuous loop)", n)
}
