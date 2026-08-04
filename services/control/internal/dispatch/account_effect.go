// account_effect.go — Phase 5 Chunks 2/3/4: the per-outcome account effect (§2/§3/§4). Given a
// penalizing failure's error_class and the account it landed on, this applies — in the SAME
// transaction as the job requeue (outcome.go) — the score recompute (§3, Chunk 3) and the flag
// change (§4, Chunk 4), writing send_managed.account_events(actor='system') on any flag change (§4.2/§2).
//
// THE DECISION SEAMS (Chunk 4 structural requirement): auto-quarantine and auto-pause each go
// through ONE explicit predicate — quarantineDecision() and pauseDecision(). Every automatic
// quarantine/pause flows through exactly that one place, which is exactly what let Phase 9.5's
// autoquarantine_mode (§2.1) / autopause_enabled (§2.2) kill-switches WRAP the decision below by
// editing only these two functions (and applyAccountEffect's single call site each) — no surgical
// re-plumbing across the outcome handler, honoring the seam Phase 5 deliberately left
// (Docs/history/open-decisions.md P5-OD-1).
package dispatch

import (
	"context"
	"encoding/json"
	"log"
	"strconv"

	"github.com/coldmail-send-managed/control/internal/workerproto"
	"github.com/jackc/pgx/v5"
)

// accountFlagState is the account's flag snapshot before/after an effect, matching the JSONB shape
// Phase 2 writes to account_events.from_flags/to_flags ({paused, pause_reason, quarantined,
// disabled}) so the automatic (actor='system') audit rows are byte-compatible with the manual
// (actor='admin') ones (§2, §4.2, T8).
type accountFlagState struct {
	Paused      bool    `json:"paused"`
	PauseReason *string `json:"pause_reason"`
	Quarantined bool    `json:"quarantined"`
	Disabled    bool    `json:"disabled"`
}

// applyAccountEffect applies the §2.1 account effect for a penalizing failure to one account,
// inside the caller's transaction (actor GUC already 'box'). It reads the current flags, recomputes
// the score (Chunk 3), decides the flag change via the two decision seams (Chunk 4) — each now
// gated by its Phase 9.5 B2 control (autoquarantine_mode's shadow ladder / autopause_enabled's
// kill-switch) — writes the flag + score changes, and appends ONE account_events(system) row iff a
// flag changed (PLUS, in shadow mode, a separate would-quarantine observation row that changes no
// flag). bounce_permanent NEVER reaches here (its no-penalty carve-out is handled by applyBounced,
// §3.3).
func (h *OutcomeHandler) applyAccountEffect(ctx context.Context, tx pgx.Tx, accountID, class string, rep workerproto.Report, cfg Config) error {
	_ = rep // rep is available for future signal extraction; the effect is driven by class + state
	before, uid, err := h.readAccountEffectState(ctx, tx, accountID)
	if err != nil {
		return err
	}

	// (Chunk 3 seam) Score recompute + consecutive_failures. Implemented in account_score.go; a
	// penalizing failure increments consecutive_failures, sets last_failure_at, and recomputes the
	// stored score. This returns the new score so the quarantine decision (Chunk 4) can use it.
	newScore, err := h.recomputeScoreOnFailure(ctx, tx, accountID, cfg)
	if err != nil {
		return err
	}

	after := before

	// (Chunk 4 seam #1, wrapped by Phase 9.5 B2 §2.1) auto-quarantine — the SINGLE place that
	// answers "should I quarantine?", now gated by autoquarantine_mode's three-way ladder:
	//   enforce — current Phase 5 behavior: the decision sets the flag (event written below).
	//   shadow  — decide, but do NOT set the flag; log a SEPARATE would-quarantine observation row
	//             using only locked account_events columns (§2.1's schema-neutral encoding).
	//   off     — no decision at all (quarantineDecision is never even evaluated), no row.
	// An unrecognized mode value (should not occur — the admin write path validates it) defensively
	// falls back to "shadow", the documented launch-safety default, never to "enforce" — a bad
	// value must never silently gain the ability to act.
	mode := cfg.AutoquarantineMode
	switch mode {
	case "off", "shadow", "enforce":
		// recognized — proceed below.
	default:
		log.Printf("dispatch: autoquarantine_mode=%q unrecognized — defaulting to shadow (safe default, never enforce)", mode)
		mode = "shadow"
	}
	switch mode {
	case "off":
		// No decision, no row (§2.1 "off" branch).
	case "shadow":
		if quarantineDecision(class, newScore, cfg.QuarantineThreshold, before) {
			if err := h.writeShadowQuarantineEvent(ctx, tx, accountID, uid, newScore, cfg.QuarantineThreshold, quarantineTrigger(class), before); err != nil {
				return err
			}
			// Deliberately do NOT set after.Quarantined — shadow decides but does not act; sending
			// continues on this account exactly as before this outcome.
		}
	default: // "enforce"
		if quarantineDecision(class, newScore, cfg.QuarantineThreshold, before) {
			after.Quarantined = true
		}
	}

	// (Chunk 4 seam #2, wrapped by Phase 9.5 B2 §2.2) auto-pause — the SINGLE place that answers
	// "should I pause (throttle)?", now gated by the autopause_enabled kill-switch. false = the
	// decision is never evaluated at all (no ladder, no shadow row — auto-pause is self-reversing,
	// §2.2, so it gets a simple on/off, not the quarantine ladder's observation path).
	if cfg.AutopauseEnabled && pauseDecision(class, before) {
		after.Paused = true
		reason := "throttle"
		after.PauseReason = &reason
	}

	// Write the flag change + stamp throttle_paused_at when a NEW throttle pause was set (§4.3 —
	// the precise write-independent clock). No flag change → no write, no event (idempotent).
	if after != before {
		newThrottlePause := after.Paused && after.PauseReason != nil && *after.PauseReason == "throttle" &&
			!(before.Paused && before.PauseReason != nil && *before.PauseReason == "throttle")
		if _, err := tx.Exec(ctx,
			`UPDATE send_managed.accounts
			   SET quarantined = $2,
			       paused = $3,
			       pause_reason = $4,
			       throttle_paused_at = CASE WHEN $5 THEN NOW() ELSE throttle_paused_at END,
			       updated_at = NOW()
			 WHERE id = $1`,
			accountID, after.Quarantined, after.Paused, after.PauseReason, newThrottlePause); err != nil {
			return err
		}
		if err := h.writeSystemAccountEvent(ctx, tx, accountID, uid, class, before, after, newScore); err != nil {
			return err
		}
	}
	return nil
}

// quarantineTrigger maps a penalizing failure's class to the §2.1 shadow-marker trigger label
// (score|auth_failure) — the same two triggers quarantineDecision itself distinguishes.
func quarantineTrigger(class string) string {
	if class == classAuthFailure {
		return "auth_failure"
	}
	return "score"
}

// quarantineDecision is the SINGLE auto-quarantine predicate (Chunk 4, §4.2). It answers
// "should this account be quarantined as a result of this outcome?" from the class + the freshly
// recomputed score + the threshold + the current flags. Two triggers, both manual-recovery-only:
//
//	(a) IMMEDIATE on auth_failure, regardless of score (§4.2a — a credential fault fails every send);
//	(b) score < quarantine_threshold after the recompute (§4.2b / §3.4).
//
// Idempotent: an already-quarantined account returns false (no second event, §4.2 idempotence).
// This is the seam Phase 9.5 will wrap with autoquarantine_mode (off|shadow|enforce) — NOT here.
func quarantineDecision(class string, newScore, threshold int, before accountFlagState) bool {
	if before.Quarantined {
		return false // already quarantined — no-op, no duplicate event
	}
	if class == classAuthFailure {
		return true // (a) immediate, regardless of score
	}
	return newScore < threshold // (b) score-below-threshold
}

// pauseDecision is the SINGLE auto-pause predicate (Chunk 4, §4.3). It answers "should this
// account be throttle-paused as a result of this outcome?": a throttle outcome pauses (reason
// 'throttle') iff the account is not ALREADY paused (independence + idempotence — a manual or
// daily_limit pause is never overwritten, and re-pausing an already-throttle-paused account is a
// no-op). Phase 9.5 will wrap this with autopause_enabled — NOT here.
func pauseDecision(class string, before accountFlagState) bool {
	if class != classThrottle {
		return false
	}
	return !before.Paused // don't overwrite an existing pause of any reason (independence, §4.1)
}

// readAccountEffectState loads the account's current flags + user_id (for the audit row's tenant).
//
// FOR UPDATE is LOAD-BEARING, not incidental (R4 finding): applyAccountEffect reads `before` here,
// then later writes back whole-column literals (quarantined=$2, paused=$3, ...) computed in Go from
// that snapshot. An account can serve MULTIPLE concurrent in-flight jobs (per-user lanes, no
// per-account in-flight lock), whose worker reports are processed in INDEPENDENT transactions. Without
// a row lock, two concurrent outcomes both read before={quarantined:false}; if one commits
// quarantined=TRUE and the other then writes its stale quarantined=FALSE literal, the account is
// silently un-quarantined — violating the LOCKED "quarantine is manual-recovery-only" invariant
// (§4.2). Because the write is a client-side literal (not a SQL expression like `quarantined OR …`),
// READ COMMITTED alone does NOT save us. FOR UPDATE serializes concurrent outcome transactions on
// the account row: the second SELECT blocks until the first commits, then re-reads the fresh
// post-commit flags (EvalPlanQual), so `before` is never stale. Lock order is consistent
// (job row first in applyClassifiedFailure, then this account row), so no deadlock.
func (h *OutcomeHandler) readAccountEffectState(ctx context.Context, tx pgx.Tx, accountID string) (accountFlagState, string, error) {
	var f accountFlagState
	var uid string
	err := tx.QueryRow(ctx,
		`SELECT paused, pause_reason, quarantined, disabled, user_id FROM send_managed.accounts WHERE id=$1 FOR UPDATE`, accountID).
		Scan(&f.Paused, &f.PauseReason, &f.Quarantined, &f.Disabled, &uid)
	return f, uid, err
}

// writeSystemAccountEvent appends ONE account_events row with actor='system' (§2, §4.2, T8),
// action derived from the flag delta ('quarantine' or 'pause' — the two automatic actions Phase 5
// introduces), from_flags/to_flags as the before/after snapshots (byte-compatible with Phase 2's
// admin rows), and an INTERNAL reason recording the trigger (auth_failure / score_below_threshold
// / throttle) + the score in metadata-style text — never surfaced to the platform (two-vocabulary).
func (h *OutcomeHandler) writeSystemAccountEvent(ctx context.Context, tx pgx.Tx, accountID, userID, class string, before, after accountFlagState, newScore int) error {
	action, reason := deriveAccountAction(class, before, after, newScore)
	if action == "" {
		return nil // no auditable transition (defensive; caller only calls on a flag change)
	}
	fromJSON, err := json.Marshal(before)
	if err != nil {
		return err
	}
	toJSON, err := json.Marshal(after)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO send_managed.account_events
		   (account_id, user_id, actor, action, from_flags, to_flags, reason, request_id)
		 VALUES ($1, $2, 'system', $3, $4, $5, $6, NULL)`,
		accountID, userID, action, fromJSON, toJSON, reason)
	return err
}

// writeShadowQuarantineEvent appends the Phase 9.5 §2.1 shadow observation row: the Control would
// have quarantined this account, but autoquarantine_mode=shadow means it decided WITHOUT acting.
// Schema-neutral encoding (§2.1's own justification, restated): NO new column, NO new account_events
// `action` value — the row uses only the locked send_managed_0014 columns.
//
//	action = 'updated'        (an existing CHECK-allowed value; a system OBSERVATION, not a real
//	                           state change)
//	actor  = 'system'
//	from_flags == to_flags    (both the account's CURRENT, unchanged flags — the equality is
//	                           itself the signal that nothing was actually mutated)
//	reason = "shadow_would_quarantine: score=<X> threshold=<T> trigger=<score|auth_failure>"
//	         (the exact structured marker string §2.1 specifies, used verbatim by §3's
//	         GET /v1/admin/health shadow.recent surface and by direct-query review)
//
// Never called when the account is ALREADY quarantined — quarantineDecision's own idempotence
// (`before.Quarantined` short-circuits to false) means this function is only reached on a genuine
// fresh would-quarantine decision, so shadow mode does not spam a duplicate row on every subsequent
// penalizing outcome against an account already flagged (conceptually) for quarantine.
func (h *OutcomeHandler) writeShadowQuarantineEvent(ctx context.Context, tx pgx.Tx, accountID, userID string, score, threshold int, trigger string, current accountFlagState) error {
	flagsJSON, err := json.Marshal(current)
	if err != nil {
		return err
	}
	reason := shadowWouldQuarantineReason(score, threshold, trigger)
	_, err = tx.Exec(ctx,
		`INSERT INTO send_managed.account_events
		   (account_id, user_id, actor, action, from_flags, to_flags, reason, request_id)
		 VALUES ($1, $2, 'system', 'updated', $3, $3, $4, NULL)`,
		accountID, userID, flagsJSON, reason)
	return err
}

// shadowWouldQuarantineReason renders the §2.1 structured marker string verbatim.
func shadowWouldQuarantineReason(score, threshold int, trigger string) string {
	return "shadow_would_quarantine: score=" + strconv.Itoa(score) + " threshold=" + strconv.Itoa(threshold) + " trigger=" + trigger
}

// deriveAccountAction maps a flag delta to the audit action + internal reason (§4.2). Quarantine
// takes precedence in the reason if both changed in one outcome (an auth_failure that also crossed
// the score threshold is still recorded as a quarantine; a single outcome never both quarantines
// and pauses in Phase 5 since auth_failure and throttle are distinct classes).
func deriveAccountAction(class string, before, after accountFlagState, newScore int) (action, reason string) {
	if !before.Quarantined && after.Quarantined {
		if class == classAuthFailure {
			return "quarantine", "auth_failure"
		}
		return "quarantine", "score_below_threshold"
	}
	if !before.Paused && after.Paused {
		return "pause", "throttle"
	}
	return "", ""
}
