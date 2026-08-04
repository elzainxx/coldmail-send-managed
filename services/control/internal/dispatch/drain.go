// drain.go — Phase 7 §4: graceful shutdown (P7-5). SIGTERM ⇒ STOP-CLAIM → DRAIN (bounded by
// shutdown_drain_timeout_sec) → LEAVE-IN-PLACE.
//
// THE LOCKED DECISION (P7-5, §4.2): anything still in-flight when the drain window closes is left
// EXACTLY as stored — `sending`/`assigned` with its lease — and is recovered by the next boot's §1
// rescan. The Control does NOT flip sending→queued on the way out. That is not laziness: a `sending`
// job may have already hit `250` at the provider, so an eager requeue would MANUFACTURE the duplicate
// the whole design exists to avoid (identical reasoning to Phase 6's rejection of eager reclaim, P6-5,
// and to §1.2's choice of reconnect-reconciliation over reclaim-all). The worker's buffered outcome
// (Chunk 2, §1.3) replays on the next boot and resolves it with no duplicate; if the worker is genuinely
// gone, the Phase 6 loop reclaims it via the ONE §2.4 transition.
//
// THE LEAVE-IN-PLACE PROPERTY IS STRUCTURAL, NOT POLICY: this file contains NO job write at all. There
// is no shutdown-time UPDATE to review — draining is a read-only wait plus a flag. So "shutdown never
// invents an outcome and never requeues a maybe-sent job" cannot be violated by a future edit here
// without adding a statement that visibly does not belong.
//
// WHAT DRAINING CHANGES, exactly two things:
//  1. STOP-CLAIM (§4.1 step 1): Tick/TryDispatch refuse to claim or push. A tick already past the
//     check finishes the handoffs it had initiated (the spec's "lane workers finish only the handoffs
//     already initiated") — the flag is read once at the top of a tick, never mid-push.
//  2. A bounded WAIT (§4.1 step 2): the caller keeps the mTLS listener and the outcome handler ALIVE
//     and simply waits for in-flight jobs to report and resolve themselves normally
//     (sent/bounced/queued-with-backoff) — the clean, correct terminal for each of those jobs. No new
//     work enters flight behind it, because of (1).
//
// SIGKILL reduces to the SAME recovery path: no drain happens, so everything in-flight is left in
// place — which is precisely the state a graceful shutdown leaves too, only larger. The next boot's
// rescan handles both identically (§4.3), which is why T4's anti-vacuity control asserts a kill and a
// drain converge on the same recovered state; the drain only reduces how much is left, never whether
// recovery works.
package dispatch

import (
	"context"
	"log"
	"time"
)

// drainPollInterval is how often the drain loop re-counts in-flight jobs. A SAFETY BOUND (the
// Docs/history/open-decisions.md Phase-4-closeout class — a shutdown-path poll granularity, like
// singleton.go's lock poll), NOT a tunable: the operator-facing dial is shutdown_drain_timeout_sec,
// which bounds the whole wait. Phase 7 §5 seeds exactly six keys and this is not one of them.
const drainPollInterval = 250 * time.Millisecond

// StartDraining enters the §4.1 draining state: from here on the dispatcher claims no new job and
// pushes nothing. Idempotent, safe from any goroutine (the signal handler), and DELIBERATELY
// one-way — a shutdown is not resumable, and offering a "stop draining" would invite a caller to
// un-drain a Control that is already closing its pool.
func (d *Dispatcher) StartDraining() {
	if d.draining.Swap(true) {
		return // already draining; a second SIGTERM must not restart the sequence
	}
	log.Printf("shutdown: DRAINING — dispatch stops claiming (no new queued→assigned) and stops pushing; the worker link and outcome handler stay up so in-flight sends can still REPORT and be persisted (§4.1)")
}

// Draining reports whether the dispatcher has entered the shutdown drain.
func (d *Dispatcher) Draining() bool { return d.draining.Load() }

// InFlightCount returns the number of jobs currently in {assigned,sending} — the §4.1 "in-flight"
// population the drain waits on. Read-only; served by jobs_user_id_state_idx.
func (d *Dispatcher) InFlightCount(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var n int
	err := d.db.QueryRow(ctx,
		`SELECT count(*) FROM send_managed.jobs WHERE state IN ('assigned','sending')`).Scan(&n)
	return n, err
}

// Drain performs §4.1 step 2 + step 3: it enters the draining state (stop-claim) and then WAITS,
// up to timeout, for in-flight jobs to resolve themselves as their workers report. It returns the
// number of jobs still in-flight when it stopped waiting — those are the ones LEFT IN PLACE for the
// next boot's rescan (§4.3). It returns as soon as the count reaches zero, so a clean deploy usually
// costs far less than the full timeout.
//
// Drain WRITES NOTHING. It does not requeue, does not clear a lease, does not touch job_events. The
// jobs it reports as remaining are, by construction, in exactly the state the last real report left
// them in.
//
// A counting error is NOT fatal to shutdown: it is logged and the drain ends, leaving everything in
// place — the recoverable state. Shutdown must never block on a sick DB (and with the DB unreachable
// there is nothing to drain: no report can be persisted, so waiting would only delay the exit while
// the §2 gate has already paused every loop).
func (d *Dispatcher) Drain(ctx context.Context, timeout time.Duration) int {
	d.StartDraining()

	start := time.Now()
	n, err := d.InFlightCount(ctx)
	if err != nil {
		log.Printf("shutdown: cannot count in-flight jobs (%v) — ending the drain immediately; everything in-flight is LEFT IN PLACE and recovered by the next boot's rescan (§4.3)", err)
		return -1
	}
	if n == 0 {
		log.Printf("shutdown: nothing in flight — drain complete in %s", time.Since(start).Round(time.Millisecond))
		return 0
	}
	log.Printf("shutdown: draining %d in-flight job(s), waiting up to %s for their workers to report (§4.1)", n, timeout)

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		wait := drainPollInterval
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			// The caller's context ended (a second signal, or a hard deadline above us). Stop waiting;
			// leaving jobs in place is always safe.
			log.Printf("shutdown: drain wait cut short by context (%v) — %d job(s) left in place", ctx.Err(), n)
			return n
		case <-time.After(wait):
		}
		n, err = d.InFlightCount(ctx)
		if err != nil {
			log.Printf("shutdown: in-flight re-count failed (%v) — ending the drain; remaining jobs are left in place", err)
			return -1
		}
		if n == 0 {
			log.Printf("shutdown: drain complete in %s — every in-flight job reported and resolved normally; NOTHING was requeued or invented",
				time.Since(start).Round(time.Millisecond))
			return 0
		}
	}

	// §4.1 step 3: the timeout won. LEAVE the remainder exactly as stored — do not flip sending→queued
	// (P7-5/§4.2: a sending job may already have hit 250; requeuing it would manufacture a duplicate).
	log.Printf("shutdown: drain timeout after %s with %d job(s) still in flight — LEAVING them exactly as stored (sending/assigned with lease; NOT requeued, NOT marked). The next boot's startup rescan extends their leases so buffered worker outcomes replay with no duplicate; genuinely-stuck ones reclaim via the continuous loop (§4.3)",
		time.Since(start).Round(time.Millisecond), n)
	return n
}
