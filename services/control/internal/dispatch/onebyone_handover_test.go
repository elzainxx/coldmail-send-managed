package dispatch

// onebyone_handover_test.go — `one_by_one` does NOT exhaust the leading account before moving on.
//
// WHY THIS EXISTS. The operator runbook's "levers that actually exist in V1" table described account
// rotation as "the default order prefers the highest-scoring account and exhausts it before moving
// on; round_robin spreads instead." Two things were wrong with that sentence, and Stage 8 of
// Docs/install-and-deploy.md#part-7--developing-and-testing-locally surfaced both:
//
//	(1) round_robin IS the default (send_managed_0004_create_pools.up.sql's column DEFAULT and
//	    adminapi's POST /v1/admin/pools default), so "the default order" named the wrong strategy.
//	(2) "exhausts it before moving on" is not what one_by_one does when the accounts have DIFFERENT
//	    daily_limits. defaultAccountOrderBy is `account_status_score DESC, a.id ASC`, and the score
//	    (account_score.go §3.2) subtracts w_prox * clamp01(daily_sent_count / daily_limit). So every
//	    success on the leader COSTS IT SCORE in proportion to how much of its OWN limit it has
//	    consumed. A small-limit leader sheds score faster than a large-limit peer and loses the
//	    `score DESC` lead long BEFORE it reaches its limit — handover happens mid-way, not at
//	    exhaustion.
//
// The CODE is right: proximity-aware ordering is the intended §3.2 model (steering away from an
// account approaching its cap is better than slamming into it). Only the docs were wrong. So this
// test PINS the mechanism the corrected runbook now describes, rather than asserting a fix.
//
// ANTI-VACUITY (Rule 10). Two things make this test able to fail:
//   - the EQUAL-limit control in the same test: with identical limits, dailyProx moves identically on
//     both accounts, the tiebreak `a.id ASC` holds, and one_by_one IS fully sticky. If ordering ever
//     stopped consulting the score at all, the unequal case would go sticky too and the first
//     assertion fails; if ordering became a spread, the equal-limit control fails. A regression
//     cannot satisfy both halves.
//   - the handover is asserted to happen with the leader STILL ELIGIBLE (under its own limit and not
//     paused), which is the precise claim: it moved on WITHOUT being exhausted. A test that only
//     counted "both accounts got used" would pass on a plain round-robin.

import (
	"context"
	"testing"
)

// TestOneByOne_HandsOverBeforeExhaustion_WithEqualLimitControl — the two halves above.
func TestOneByOne_HandsOverBeforeExhaustion_WithEqualLimitControl(t *testing.T) {
	// --- HALF 1: UNEQUAL limits → the small-limit leader hands over while still eligible. ---
	t.Run("unequal_limits_hand_over_before_exhaustion", func(t *testing.T) {
		e := newDispatchEnv(t, "obohandover")
		pool := e.seedPoolWithStrategy(t, "one_by_one")
		cfg := e.mustLoadConfig(t)

		a1 := e.seedAccount(t, pool, "s1")
		a2 := e.seedAccount(t, pool, "s2")

		// The SMALL limit must land on the account that will actually LEAD, or this test proves nothing:
		// both start tied at score 100, so `defaultAccountOrderBy`'s `a.id ASC` tiebreak decides the
		// leader, and seedAccount's ids are random UUIDs. Putting the small limit on the LOSER of that
		// tiebreak makes the run trivially sticky (the large-limit leader sheds almost no proximity
		// penalty) — which is a real behaviour, but not the one under test, and it would make this test
		// flaky at ~50%. So resolve the tiebreak FIRST and give the small limit to the winner.
		leader, peer := a1, a2
		if a2 < a1 {
			leader, peer = a2, a1
		}
		e.setAccountDailyState(t, leader, 10, 0, todayUTC(), false, nil)
		e.setAccountDailyState(t, peer, 1000, 0, todayUTC(), false, nil)

		// WARM-UP both accounts to a real recomputed score before measuring. Without this, the peer
		// still carries the column-default 100 while never having succeeded, and the handover could be
		// attributed to that stale default rather than to w_prox. One recompute apiece stamps
		// last_success_at on both (zeroing the w_recency term for both) and stores the §3.2 score from
		// their actual rows — at 0 sent that is 100 for both, so they enter the loop genuinely TIED and
		// the only thing that can separate them afterwards is proximity to their own limits.
		e.recomputeSuccess(t, leader, cfg)
		e.recomputeSuccess(t, peer, cfg)

		// Every consume is followed by the REAL success-side score recompute
		// (recomputeScoreOnSuccess), so the score driving the next consume is the one production would
		// have written — not a hand-set number.
		picks := make([]string, 0, 10)
		for i := 0; i < 10; i++ {
			got, ok, err := e.d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, "UTC", 0, 900)
			if err != nil {
				t.Fatalf("consume %d: %v", i, err)
			}
			if !ok {
				t.Fatalf("consume %d found no eligible account — both accounts have headroom (limits 10 and 1000), so the pool cannot be exhausted here", i)
			}
			picks = append(picks, got.id)
			e.recomputeSuccess(t, got.id, cfg)
		}

		// The tiebreak prediction must actually hold, else the rest of the assertions are about the
		// wrong account. This is a genuine check, not a formality: it fails loudly if the ORDER BY ever
		// stops resolving score ties by `a.id ASC` (P4-OD-1).
		if picks[0] != leader {
			t.Fatalf("first pick was %s, want the lower-id account %s — defaultAccountOrderBy resolves a score tie with `a.id ASC` (P4-OD-1), so the lower id must lead while both sit at 100", picks[0], leader)
		}

		// THE CLAIM: the leader did NOT take all 10. It handed over.
		leaderRun := 0
		for _, p := range picks {
			if p != leader {
				break
			}
			leaderRun++
		}
		if leaderRun == len(picks) {
			t.Fatalf("the leading account took all %d sends — one_by_one is documented as handing over EARLY when limits differ, because account_score.go's w_prox term penalizes daily_sent_count/daily_limit. If ordering no longer consults the score, the equal-limit control below is now the only thing distinguishing one_by_one from round_robin", len(picks))
		}
		if !containsID(picks, peer) {
			t.Fatalf("the peer account was never selected across %d sends (picks=%v) — handover means the OTHER account actually ran", len(picks), picks)
		}

		// THE PRECISE CLAIM — "before exhaustion": at the moment of handover the leader was STILL
		// ELIGIBLE. Its own daily_limit was 10 and it made only leaderRun sends, so it had headroom;
		// prove it was not paused/quarantined/disabled either, so "still eligible" is not an inference.
		var limit, sent int
		var paused, quarantined, disabled bool
		if err := e.admin.QueryRow(context.Background(),
			`SELECT daily_limit, daily_sent_count, paused, quarantined, disabled
			   FROM send_managed.accounts WHERE id = $1`, leader).
			Scan(&limit, &sent, &paused, &quarantined, &disabled); err != nil {
			t.Fatalf("read leader state: %v", err)
		}
		if sent >= limit {
			t.Fatalf("the leader reached its daily_limit (%d/%d) — then this test proved exhaust-then-move-on, not the early handover it claims. Lower the send count or raise the limit so the handover is provably pre-exhaustion", sent, limit)
		}
		if paused || quarantined || disabled {
			t.Fatalf("the leader was removed from eligibility another way (paused=%v quarantined=%v disabled=%v) — the handover must be caused by the score, not by a flag", paused, quarantined, disabled)
		}
		t.Logf("unequal limits: leader ran %d consecutive of %d sends, then handed over at %d/%d of its own limit (picks=%v)",
			leaderRun, len(picks), sent, limit, picks)
	})

	// --- HALF 2 (the control): EQUAL limits → one_by_one IS fully sticky. ---
	// This is what makes half 1 meaningful. If the score term were removed, or ordering became a
	// spread, this half fails — so the two halves cannot both be satisfied by a regression.
	t.Run("equal_limits_are_fully_sticky", func(t *testing.T) {
		e := newDispatchEnv(t, "obosticky")
		pool := e.seedPoolWithStrategy(t, "one_by_one")
		cfg := e.mustLoadConfig(t)

		a1 := e.seedAccount(t, pool, "s1")
		a2 := e.seedAccount(t, pool, "s2")
		// IDENTICAL limits: dailyProx moves in lockstep, so the scores stay equal and `a.id ASC`
		// keeps the same leader for the whole run.
		e.setAccountDailyState(t, a1, 1000, 0, todayUTC(), false, nil)
		e.setAccountDailyState(t, a2, 1000, 0, todayUTC(), false, nil)

		picks := make([]string, 0, 10)
		for i := 0; i < 10; i++ {
			got, ok, err := e.d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, "UTC", 0, 900)
			if err != nil {
				t.Fatalf("consume %d: %v", i, err)
			}
			if !ok {
				t.Fatalf("consume %d found no eligible account — both accounts have 1000 headroom", i)
			}
			picks = append(picks, got.id)
			e.recomputeSuccess(t, got.id, cfg)
		}

		for i, p := range picks {
			if p != picks[0] {
				t.Fatalf("one_by_one switched accounts at send %d (picks=%v) with IDENTICAL daily_limits — with equal limits the w_prox penalty moves identically on both rows, so score DESC stays tied and the a.id ASC tiebreak (P4-OD-1) must hold the same leader. A spread here means one_by_one has become round_robin", i, picks)
			}
		}
		t.Logf("equal limits: fully sticky, all 10 sends on %s", picks[0])
	})
}

// recomputeSuccess applies the REAL success-side account bookkeeping + score recompute
// (OutcomeHandler.recomputeScoreOnSuccess) in its own transaction, so a test loop's next consume
// orders on a production-written score rather than a hand-set one.
func (e *dispatchEnv) recomputeSuccess(t *testing.T, acctID string, cfg Config) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin score tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := e.oh.recomputeScoreOnSuccess(ctx, tx, acctID, cfg); err != nil {
		t.Fatalf("recomputeScoreOnSuccess: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit score tx: %v", err)
	}
}

func containsID(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
