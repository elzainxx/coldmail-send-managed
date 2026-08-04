// account_score.go — Phase 5 Chunk 3: the account status-score model (§3). The score is a
// deterministic, pure function of send-viability signals ONLY (consecutive_failures, daily
// proximity, time since last success — §3.1); it uses NO bounce data (§3.3 carve-out). It is a
// STORED SNAPSHOT recomputed at OUTCOME time on the one account that just had an outcome
// (never a table sweep), and it is INDEPENDENT of the paused/quarantined flags — the flags never
// reset or freeze it; a quarantined account gets no outcomes so it simply HOLDS its score (§3.3).
package dispatch

import (
	"context"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// scoreInputs are the three §3.1 send-viability signals, read from the account row (post-update on
// a failure/success recompute), plus the config weights. All are already-current values.
type scoreInputs struct {
	consecutiveFailures int
	dailySentCount      int
	dailyLimit          int
	dailyResetDay       time.Time
	lastSuccessAt       *time.Time
	nowInTZ             time.Time // today's date in the daily-reset tz (for the stale-cycle rule)
	now                 time.Time // wall clock (for time_since_success)
}

// computeScore is the §3.2 weighted-penalty formula: base 100 minus weighted penalties, clamped to
// [0,100] (the locked accounts_status_score_check domain). It is PURE — no randomness, no history
// beyond the three inputs — so the test oracle (T5) can assert equality against a hand-computed
// value. Monotone in each signal.
//
//	score = round(clamp(100 - w_fail*failPenalty - w_prox*dailyProx - w_recency*recencyPenalty, 0, 100))
//	  failPenalty(n)   = min(1, n / fail_saturation)                       # linear ramp, saturates
//	  dailyProx        = clamp(effective_daily_sent / daily_limit, 0, 1)   # stale cycle => 0
//	  recencyPenalty(t)= clamp((now - last_success) / recency_horizon, 0, 1)  # NULL => full horizon (1)
func computeScore(in scoreInputs, w ScoreWeights) int {
	// failure penalty: 0..1 ramp saturating at fail_saturation consecutive failures.
	failPenalty := 0.0
	if w.FailSaturation > 0 {
		failPenalty = math.Min(1.0, float64(in.consecutiveFailures)/w.FailSaturation)
	}

	// daily proximity: a STALE reset cycle counts as 0 sent (the lazy-reset staleness rule, §3.1) —
	// the stored counter belongs to a past day. Otherwise sent/limit, clamped to [0,1].
	dailyProx := 0.0
	if !in.dailyResetDay.Before(in.nowInTZ) && in.dailyLimit > 0 {
		dailyProx = clamp01(float64(in.dailySentCount) / float64(in.dailyLimit))
	}

	// recency penalty: (now - last_success)/recency_horizon, clamped [0,1]. A NULL last_success_at
	// (never succeeded) is treated as the full horizon age (penalty 1), per §3.1.
	recencyPenalty := 1.0
	if in.lastSuccessAt != nil && w.RecencyHorizonHours > 0 {
		ageHours := in.now.Sub(*in.lastSuccessAt).Hours()
		if ageHours < 0 {
			ageHours = 0
		}
		recencyPenalty = clamp01(ageHours / w.RecencyHorizonHours)
	}

	raw := 100.0 - w.WFail*failPenalty - w.WProx*dailyProx - w.WRecency*recencyPenalty
	if raw < 0 {
		raw = 0
	}
	if raw > 100 {
		raw = 100
	}
	return int(math.Round(raw))
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// recomputeScoreOnFailure applies a penalizing failure's account bookkeeping (§3.3): increment
// consecutive_failures, stamp last_failure_at=NOW(), then recompute + store the score from the
// UPDATED row + the config weights, and RETURN the new value (so the quarantine decision can
// compare it to the threshold in the same tx, §3.4/§4.2b). One statement does the bookkeeping and
// returns the signals; a second stores the computed score — both inside the caller's tx.
func (h *OutcomeHandler) recomputeScoreOnFailure(ctx context.Context, tx pgx.Tx, accountID string, cfg Config) (int, error) {
	return h.bumpAndRecompute(ctx, tx, accountID, false /*success*/, cfg)
}

// recomputeScoreOnSuccess applies a successful send's account bookkeeping (§3.3): reset
// consecutive_failures to 0, set last_success_at=NOW(), then recompute + store the score. Returns
// the new value (unused by the success path today, but symmetric with the failure entry point).
func (h *OutcomeHandler) recomputeScoreOnSuccess(ctx context.Context, tx pgx.Tx, accountID string, cfg Config) (int, error) {
	return h.bumpAndRecompute(ctx, tx, accountID, true /*success*/, cfg)
}

// bumpAndRecompute is the shared recompute core. success=true resets consecutive_failures to 0 and
// stamps last_success_at; success=false increments consecutive_failures and stamps last_failure_at.
// It then reads the updated signals, computes the §3.2 score, and stores it — all in the caller's
// transaction, on the ONE account (no sweep, §3.3). cfg is the caller's already-freshly-loaded
// Config (Phase 9.5 freshness fix — see outcome.go's OutcomeHandler doc comment); the daily-reset tz
// and score weights it carries match the eligibility predicate's notion of "today" for this report.
func (h *OutcomeHandler) bumpAndRecompute(ctx context.Context, tx pgx.Tx, accountID string, success bool, cfg Config) (int, error) {
	var setClause string
	if success {
		setClause = `consecutive_failures = 0, last_success_at = NOW(), updated_at = NOW()`
	} else {
		setClause = `consecutive_failures = consecutive_failures + 1, last_failure_at = NOW(), updated_at = NOW()`
	}

	var in scoreInputs
	// Bookkeeping UPDATE returns the post-update signals + today-in-tz (computed from the config tz)
	// so the score is computed from exactly the row state after this outcome.
	row := tx.QueryRow(ctx,
		`UPDATE send_managed.accounts
		    SET `+setClause+`
		  WHERE id = $1
		  RETURNING consecutive_failures, daily_sent_count, daily_limit, daily_reset_day,
		            last_success_at, ((NOW() - INTERVAL '1 hour' * $3) AT TIME ZONE $2)::date, NOW()`,
		accountID, cfg.DailyResetTimezone, cfg.DailyResetHour)
	if err := row.Scan(&in.consecutiveFailures, &in.dailySentCount, &in.dailyLimit,
		&in.dailyResetDay, &in.lastSuccessAt, &in.nowInTZ, &in.now); err != nil {
		return 0, err
	}

	score := computeScore(in, cfg.ScoreWeights)
	if _, err := tx.Exec(ctx,
		`UPDATE send_managed.accounts SET account_status_score = $2, updated_at = NOW() WHERE id = $1`,
		accountID, score); err != nil {
		return 0, err
	}
	return score, nil
}
