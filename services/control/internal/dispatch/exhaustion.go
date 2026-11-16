// exhaustion.go — Phase 5 Chunk 5: the terminal-`failed`-on-pool-exhaustion boundary (§5). This is
// the wait-vs-fail decision Phase 4's pre-claim gate (P4-5) explicitly deferred. It EXTENDS that
// gate from two outcomes (claim / leave-queued) to three (claim / WAIT / FAIL), evaluated per user
// once per tick, read-mostly, and rate-safe (a fail UPDATE moves jobs to terminal so they leave the
// queued working set; a WAIT produces zero writes — no churn, mirroring P4-5's anti-churn property).
package dispatch

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// chainVerdict is the §5.1 per-user decision, evaluated top-down, first match wins.
type chainVerdict int

const (
	verdictWait  chainVerdict = iota // leave jobs queued, untouched — capacity may return automatically
	verdictClaim                     // ≥1 eligible account — claim normally (Phase 4 path)
	verdictFail                      // resolved non-empty chain, ALL terminal_blocked — fail the ready jobs
)

// evaluateChain classifies a user's resolved pool chain into the §5.1 verdict. Order (first match):
//  1. ∃ eligible account            → CLAIM (not exhausted).
//  2. chain is EMPTY/unresolvable    → WAIT (§5.1 P5-4b: a config gap — e.g. default_shared_pool_id
//     unset — is NOT a pool exhaustion; ordered BEFORE the fail branch so it can never fall through).
//  3. ∃ self_recovering account      → WAIT (a reset/throttle timer will re-open it automatically).
//  4. else (non-empty, all terminal_blocked) → FAIL.
//
// The eligibility test reuses poolHasEligible (the IDENTICAL predicate the consume uses), so "is
// there an eligible account" can never drift from the real selection logic. self_recovering vs
// terminal_blocked is a cheap follow-up classification of the same accounts.
func (d *Dispatcher) evaluateChain(ctx context.Context, userID string, cfg Config) (chainVerdict, error) {
	pools, err := d.resolvePoolChain(ctx, userID, cfg)
	if err != nil {
		return verdictWait, err
	}
	// (2) Empty/unresolvable chain → WAIT. Checked first-among-non-eligible so a config gap NEVER
	// fails a job (§5.1 P5-4b; preserves Phase 4 §4.3 shared_pool_not_configured). resolvePoolChain
	// already emitted the throttled diagnostic when the chain is empty.
	if len(pools) == 0 {
		return verdictWait, nil
	}

	// (1) Any eligible account across the chain → CLAIM.
	for _, poolID := range pools {
		ok, err := d.poolHasEligible(ctx, poolID, cfg.DailyResetTimezone, cfg.DailyResetHour, cfg.ThrottlePauseSeconds)
		if err != nil {
			return verdictWait, err
		}
		if ok {
			return verdictClaim, nil
		}
	}

	// No eligible account anywhere in a non-empty chain. Classify: is ANY account self-recovering?
	// (3) self_recovering → WAIT; (4) else all terminal_blocked → FAIL.
	selfRecovering, err := d.chainHasSelfRecovering(ctx, pools, cfg)
	if err != nil {
		return verdictWait, err
	}
	if selfRecovering {
		return verdictWait, nil
	}
	return verdictFail, nil
}

// chainHasSelfRecovering reports whether ANY account across the chain's pools is self_recovering
// (§5.1): NOT eligible now, but an AUTOMATIC process will make it eligible —
//   - paused with pause_reason='daily_limit'   (clears at the daily reset), OR
//   - at the daily limit on the CURRENT cycle  (clears at the daily reset), OR
//   - paused with pause_reason='throttle' whose window has NOT yet expired (clears at the throttle
//     timer — an EXPIRED throttle pause is already `eligible` via P5-13, so it never reaches here).
//
// An account that is quarantined / disabled / manually-paused is terminal_blocked (manual recovery
// only) and does NOT count as self_recovering. A stale daily_limit-paused / at-limit account also
// self-recovers (the lazy reset re-opens it) — but if its reset day is already past it would be
// eligible (handled by the CLAIM branch), so here we scope to the current cycle.
func (d *Dispatcher) chainHasSelfRecovering(ctx context.Context, pools []string, cfg Config) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// One query across the whole chain (pools = the resolved id list). An account is self_recovering
	// iff it is not quarantined/disabled AND (daily_limit-paused OR at-limit-this-cycle OR
	// throttle-paused-still-in-window). Manual pauses and quarantine/disable are excluded → they are
	// terminal_blocked. today_in_tz uses the config daily-reset tz to match the eligibility notion.
	const q = `
WITH today_calc AS (SELECT ((NOW() - INTERVAL '1 hour' * $4) AT TIME ZONE $2)::date AS today_in_tz)
SELECT EXISTS (
  SELECT 1
  FROM send_managed.accounts a, today_calc
  WHERE a.pool_id = ANY($1)
    AND a.quarantined = FALSE
    AND a.disabled = FALSE
    AND (
          -- daily_limit pause (clears at reset)
          (a.paused = TRUE AND a.pause_reason = 'daily_limit')
          OR
          -- at the daily limit on the current cycle (clears at reset)
          (a.daily_reset_day = today_calc.today_in_tz AND a.daily_sent_count >= a.daily_limit)
          OR
          -- throttle pause still WITHIN its window (clears at the throttle timer; an EXPIRED one is
          -- already eligible via P5-13 and handled by the CLAIM branch, not here)
          (a.paused = TRUE AND a.pause_reason = 'throttle'
             AND NOW() < a.throttle_paused_at + INTERVAL '1 second' * $3)
        )
)`
	var exists bool
	if err := d.db.QueryRow(ctx, q, pools, cfg.DailyResetTimezone, cfg.ThrottlePauseSeconds, cfg.DailyResetHour).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// failExhaustedJobs transitions a user's READY queued jobs (next_attempt_at <= NOW()) to terminal
// `failed` (§5.1 FAIL branch). queued→failed (actor='dispatcher'), completed_at=NOW(), lease stays
// NULL (the job was queued). last_error_class is left as the job's most recent real class (NOT
// overwritten with a made-up value — pool_exhausted is NOT an error_class enum value, §5.2 P5-14);
// the internal reason is recorded in last_error_message + job_events.metadata. The platform report
// shows only state:"failed" (two-vocabulary). Only READY jobs are failed — a job whose
// next_attempt_at is still in the future (mid-backoff) is not yet due, so it is left to a later tick.
//
// Since send_managed_0028 this is one of TWO writers of `failed`, and the report must be able to tell them
// apart without parsing text: this one records terminal_reason='pool_exhausted' (the other — the attempt
// cap in outcome.go/reclaim.go — records 'attempts_exhausted'). A queued job that has had NO failed SMTP
// attempt can only reach `failed` here, and a job that DID exhaust its attempts is already terminal and
// is never in this statement's working set (state='queued'). The cancel flag is respected for free: a
// job cancelled before the claim is `cancelled`, not `queued`, so it is not matched here either.
//
// The rows are locked in id order (a locking sub-select) before they are updated. This statement is a
// multi-row UPDATE over a user's whole ready queue, and POST /v1/cancel runs the same shape over a
// campaign's queued items; two multi-row writers locking an overlapping set in different heap orders is
// the classic 40P01, so both take their row locks in the same (id) order. It changes nothing about WHICH
// rows are failed.
func (d *Dispatcher) failExhaustedJobs(ctx context.Context, userID string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return d.inActorTx(ctx, "dispatcher", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE send_managed.jobs
			   SET state='failed',
			       completed_at=NOW(),
			       terminal_reason='pool_exhausted',
			       last_error_message='pool_exhausted: all eligible accounts terminal_blocked (§5.1)',
			       updated_at=NOW()
			 WHERE id IN (SELECT id FROM send_managed.jobs
			               WHERE user_id=$1 AND state='queued' AND next_attempt_at <= NOW()
			                 AND cancel_requested_at IS NULL
			               ORDER BY id FOR UPDATE)`, userID)
		return err
	})
}
