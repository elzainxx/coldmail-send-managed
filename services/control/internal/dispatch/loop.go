// loop.go — the Phase-4 polling dispatch loop (§1.2): one tick snapshots ground truth, computes
// per-user lanes, applies the pre-claim pool-eligibility gate (§1.2/§6, P4-5), visits active
// campaigns in fairness-cursor order (§5), claims per active user (§1.1), and turns each claimed
// job into a push_job handoff. This is the PRODUCTION driver; TryDispatch (dispatch.go) is the
// Phase-3 single-shot primitive kept for its own behavioral tests.
package dispatch

import (
	"context"
	"log"
	"sort"
	"time"
)

// claimForUserQuery is the LOCKED per-user claim (§1.1, CLAIM_FOR_USER), backed by the additive
// jobs_claim_by_user_idx (migration send_managed_0015). It keeps the Phase 1 draft 02 shape exactly —
// FOR UPDATE SKIP LOCKED, ORDER BY next_attempt_at, created_at, the same lease — generalized with
// a user_id predicate and a batch LIMIT. It sets ONLY state/lease/updated_at (no account, no worker)
// — worker/account are resolved per job afterward (§1.3), so a job can legally sit `assigned` with
// both assigned_account_id and assigned_box_id NULL between TXN 1 and TXN 2.
const claimForUserQuery = `
WITH claimed AS (
    SELECT id FROM send_managed.jobs
    WHERE state = 'queued' AND next_attempt_at <= NOW() AND user_id = $1
      AND cancel_requested_at IS NULL
    ORDER BY next_attempt_at ASC, created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
UPDATE send_managed.jobs
SET state = 'assigned',
    lease_expires_at = NOW() + make_interval(secs => $3),
    updated_at = NOW()
WHERE id IN (SELECT id FROM claimed)
RETURNING id, user_id, campaign_id, recipient_email, rendered_subject, rendered_body, reply_to`

// ringSlot is one active user's turn this tick: how many lanes they may claim.
type ringSlot struct {
	userID string
	grant  int
}

// Tick runs one full dispatch cycle (§1.2). It re-reads config fresh (Phase 2 OD-14 — a config
// change takes effect on the very next tick, no redeploy). Returns the Config it read (so the
// caller can time the next wait) and the number of jobs successfully pushed this tick.
func (d *Dispatcher) Tick(ctx context.Context) (Config, int, error) {
	cfg, err := LoadConfig(ctx, d.db)
	if err != nil {
		return Config{}, 0, err
	}

	// Phase 7 §4.1 step 1 (P7-5): STOP-CLAIM. Once SIGTERM has put us in the drain, this tick claims
	// nothing and pushes nothing — no queued→assigned transition, so no new job enters flight behind a
	// draining Control. Checked ONCE here at the top rather than per-job, so a tick already past this
	// point finishes the handoffs it had initiated ("lane workers finish only the handoffs already
	// initiated"). Nothing is written on this path: a draining tick is a pure no-op.
	if d.draining.Load() {
		return cfg, 0, nil
	}

	// Phase 9.5 B1 (§1.2), GRADUATED by fix-plan item 12: the dispatch kill-switch. Checked at the
	// very top, before any claim/route/push — this tick performs NO claim, no route, no push, and
	// writes NOTHING. Queued jobs stay exactly queued (no state change, no job_events row); the
	// reclaim loop, the outcome handler, heartbeats, and config reads all continue independently (they
	// are driven by their own loops/handlers, not gated here — and at `stopped` the reclaim loop gates
	// itself, in its own tick, rather than being reached into from here). The loop keeps ticking at its
	// normal interval — it does not sleep-forever or exit — so it resumes the instant the rung is
	// raised. Throttled so a long pause produces one log line per interval, never a flood.
	//
	// ClaimsAllowed(), not `cfg.DispatchEnabled`: the boolean is now one of TWO dials and reading it
	// directly here would honour only half the control. The predicate composes both (rung.go), and
	// `dispatch_enabled=false` still resolves to accept_and_defer, so this gate's behaviour for that
	// input is byte-for-byte what it was.
	if !cfg.ClaimsAllowed() {
		d.logDispatchPausedByOperator(cfg)
		return cfg, 0, nil
	}

	// Phase A: snapshot ground truth (two cheap reads; §1.2 A1/A2).
	inFlight, err := d.snapshotInFlight(ctx)
	if err != nil {
		return cfg, 0, err
	}
	activeUsers, err := d.snapshotActiveUsers(ctx)
	if err != nil {
		return cfg, 0, err
	}

	// Phase B: available lanes (the ceiling) + the pre-claim pool-eligibility gate (§1.2/§6, P4-5).
	// lanes[U] = max(0, ceiling - in_flight[U]); a user already at ceiling is skipped (frees no
	// churn). A user whose resolved pool chain has ZERO eligible accounts — including an empty
	// chain from an unset/invalid default_shared_pool_id, §4.3 — is likewise skipped: their jobs
	// stay queued, untouched, no claim/requeue churn, no job_events flooding under sustained
	// exhaustion (the rare-race requeue in resolveAndPush remains the only backstop).
	// A per-user gate-check error must not abort the whole tick either (same no-starvation
	// reasoning as the claim loop below): log and skip that ONE user for this tick, leave every
	// other active user's gate evaluation unaffected.
	var ring []ringSlot
	for _, u := range activeUsers {
		lanes := cfg.ConcurrencyPerUserDefault - inFlight[u]
		if lanes <= 0 {
			continue
		}
		// Phase 5 §5.1 wait-vs-fail: evaluate the user's resolved chain once. CLAIM (≥1 eligible)
		// adds them to the ring; WAIT (empty/unresolvable chain OR ≥1 self-recovering account) leaves
		// their jobs untouched in queued (no churn, exactly Phase 4's P4-5 gate); FAIL (a non-empty
		// chain whose every account is terminal_blocked) fails their ready jobs to terminal `failed`.
		verdict, err := d.evaluateChain(ctx, u, cfg)
		if err != nil {
			// Phase 7 §2.2 (P7-3): a TRANSPORT failure aborts the whole tick and surfaces to the
			// caller's outage gate, which pauses the loop. Skipping per-user is only correct for an
			// application error (that user's chain is unreadable); with the DB unreachable no other
			// user can be served either, so there is no starvation cost to aborting — and continuing
			// would hammer a dead socket once per active user, per tick.
			if IsOutage(err) {
				return cfg, 0, err
			}
			log.Printf("dispatch: evaluateChain(%s) failed: %v — skipping this user for this tick", u, err)
			continue
		}
		switch verdict {
		case verdictClaim:
			// Phase 6 pacing (§1): withhold the portion of this user's grant whose lanes are still
			// cooling down. Admit returns min(lanes, paced-ready lanes) — a `min`, so it can only
			// FURTHER delay a send, never raise the grant above Phase 4's DB-derived ceiling
			// (`lanes`). A user with a positive lane count but no paced-ready slot this tick gets
			// grant 0: their jobs stay queued, untouched (claimed on a later tick once a timer
			// elapses) — the tick never blocks. Slots are stamped only on an ACTUAL push (Phase D).
			paced := d.pace.Admit(u, cfg.ConcurrencyPerUserDefault, lanes)
			if paced <= 0 {
				continue
			}
			ring = append(ring, ringSlot{userID: u, grant: paced})
		case verdictFail:
			if err := d.failExhaustedJobs(ctx, u); err != nil {
				log.Printf("dispatch: failExhaustedJobs(%s) failed: %v", u, err)
			}
		default: // verdictWait — leave the user's jobs queued, untouched (no claim, no write, no churn)
		}
	}
	sort.Slice(ring, func(i, j int) bool { return ring[i].userID < ring[j].userID })

	// Phase C: rotate the ring to start at the first member after the fairness cursor (§5.1),
	// wrapping to the smallest if the cursor is past every member or unset/stale (a departed
	// campaign's id, e.g.). Every member is still visited this tick regardless of rotation —
	// no-starvation is structural (§5.2); the cursor only changes interleaving order across ticks.
	ring = rotateRing(ring, d.fairnessCursor())

	// Phase C+D: claim per active user (in rotated ring order), then resolve+push each claimed job.
	// A per-user claim error must NOT abort the whole tick — every OTHER ring member must still be
	// visited this tick (§5.2's no-starvation guarantee is "every ring member is visited every
	// tick"; a transient failure on one user's claim would otherwise starve everyone ordered after
	// them in the ring, unboundedly if the condition persists). Log and move to the next slot. The
	// fairness cursor advances AFTER the claim attempt (§1.2's own pseudocode: "batch =
	// CLAIM_FOR_USER(U, ...); rr_cursor = U"), not before — so a user is only recorded as
	// "serviced" once they were actually attempted.
	pushed := 0
	for _, slot := range ring {
		jobs, err := d.claimForUser(ctx, slot.userID, slot.grant, cfg.JobMaxProcessingTimeSec)
		d.setFairnessCursor(slot.userID)
		if err != nil {
			// Same §2.2 split as the gate check above: a transport failure aborts the tick (the loop
			// pauses; the claim transaction rolled back, so NOTHING was claimed — no job moved to
			// assigned, nothing to reconcile on resume). An application error still only skips this one
			// user, preserving §5.2's no-starvation reasoning.
			if IsOutage(err) {
				return cfg, pushed, err
			}
			log.Printf("dispatch: claimForUser(%s) failed: %v — skipping this user for this tick, ring continues", slot.userID, err)
			continue
		}
		for _, job := range jobs {
			// Worker selection is orthogonal to routing (§1.3): workers are interchangeable, and Phase
			// 3's one-in-flight-per-worker constraint means a tick that claims more jobs than there
			// are idle workers must requeue the overflow cleanly rather than block.
			workerID := d.px.AnyConnectedWorker()
			if workerID == "" {
				d.requeue(ctx, job.id, "no_worker_available")
				continue
			}
			ok, perr := d.resolveAndPush(ctx, job, workerID, cfg)
			if perr != nil {
				continue // resolveAndPush already logs; the job was requeued or left claimable
			}
			if ok {
				// A send actually fired: cost this user one lane's cooldown (a fresh uniform delay
				// re-drawn per send, §1.1). Stamped ONLY on a real push, so a claim that returned
				// fewer jobs than granted, or a requeued/failed handoff, never burns a lane's timer.
				d.pace.Stamp(slot.userID, cfg.ConcurrencyPerUserDefault, cfg.SendDelayMinSec, cfg.SendDelayMaxSec)
				pushed++
			}
		}
	}
	return cfg, pushed, nil
}

// (The P4-5 pre-claim gate's "has ≥1 eligible account?" check is now subsumed by evaluateChain
// (exhaustion.go), which extends it from claim/skip to claim/WAIT/FAIL — Phase 5 §5. The old
// standalone poolChainHasEligible was removed so there is exactly ONE chain-classification path and
// no risk of two predicates drifting.)

// fairnessCursor/setFairnessCursor guard Dispatcher.fairCursor (§5.1) under its own mutex —
// separate from rrMu (the account-cursor lock), since they protect independent state and are read
// from different call sites.
func (d *Dispatcher) fairnessCursor() string {
	d.fairMu.Lock()
	defer d.fairMu.Unlock()
	return d.fairCursor
}

func (d *Dispatcher) setFairnessCursor(userID string) {
	d.fairMu.Lock()
	d.fairCursor = userID
	d.fairMu.Unlock()
}

// rotateRing reorders ring (already sorted ascending by userID) to start at the first member
// with userID > cursor, wrapping to the beginning if none qualifies (cursor is unset, stale, or
// past every current member — §5.1's "resolves to the next id after the one we last served").
// Every element is kept; only the starting point changes.
func rotateRing(ring []ringSlot, cursor string) []ringSlot {
	if len(ring) == 0 {
		return ring
	}
	start := 0
	for i, slot := range ring {
		if slot.userID > cursor {
			start = i
			break
		}
		if i == len(ring)-1 {
			start = 0 // no member exceeds the cursor — wrap to the smallest
		}
	}
	return append(append([]ringSlot{}, ring[start:]...), ring[:start]...)
}

// snapshotInFlight is Phase A1 (§1.2): per-user count of jobs currently assigned/sending, served
// by the existing jobs_user_id_state_idx (user_id, state) — no new index needed.
func (d *Dispatcher) snapshotInFlight(ctx context.Context) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := d.db.Query(ctx,
		`SELECT user_id, COUNT(*) FROM send_managed.jobs WHERE state IN ('assigned','sending') GROUP BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var u string
		var n int
		if err := rows.Scan(&u, &n); err != nil {
			return nil, err
		}
		out[u] = n
	}
	return out, rows.Err()
}

// snapshotActiveUsers is Phase A2 (§1.2): the distinct set of users with claimable work right now,
// served by the existing jobs_claim_idx (next_attempt_at, created_at) WHERE state='queued'.
func (d *Dispatcher) snapshotActiveUsers(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := d.db.Query(ctx,
		`SELECT DISTINCT user_id FROM send_managed.jobs
		  WHERE state='queued' AND next_attempt_at <= NOW() AND cancel_requested_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// claimForUser runs CLAIM_FOR_USER (§1.1) in one transaction with the actor GUC set, claiming up
// to `grant` queued jobs for userID. grant<=0 is a no-op (defensive; the caller already filters
// this, but a negative/zero grant must never claim unboundedly).
func (d *Dispatcher) claimForUser(ctx context.Context, userID string, grant, jobMaxProcessingTimeSec int) ([]claimedJob, error) {
	if grant <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor', 'dispatcher', true)"); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, claimForUserQuery, userID, grant, jobMaxProcessingTimeSec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []claimedJob
	for rows.Next() {
		var j claimedJob
		if err := rows.Scan(&j.id, &j.userID, &j.campaignID, &j.recipient, &j.subject, &j.body, &j.replyTo); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return jobs, nil
}
