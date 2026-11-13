package dispatch

// atlimit_recovery_test.go — the TWO at-limit account shapes recover DIFFERENTLY today.
//
// WHY THIS EXISTS. Both shapes read "at daily limit" to an operator's eye and both self-clear at the
// daily reset, so the runbook treated them as one case and told the operator to raise `daily_limit`
// to recover capacity. That advice works on only ONE of them:
//
//	shape A — daily_sent_count reached daily_limit, paused = FALSE
//	          → the current-day eligibility branch is `daily_sent_count < daily_limit`, so raising
//	            the limit re-opens the account IMMEDIATELY.
//	shape B — the send that hit the cap also PAUSED the row (pause_reason='daily_limit')
//	          → the same branch also requires `paused = FALSE`, which it is not, so raising the limit
//	            has NO EFFECT today no matter how much headroom it creates. And POST
//	            /accounts/{id}/unpause refuses it (422 not_manual_pause, Phase 2 §837) because a
//	            self-clearing pause is not an operator's to lift. Only the daily reset clears it.
//
// Shape B is deliberate and locked (Docs/history/phase-1-data-model.md decisions #17 and #19: "lowering
// can make an account immediately ineligible until reset — expected admin behavior"). So this test
// does NOT assert a fix; it PINS the asymmetry, because the runbook now documents it as the reason an
// operator must add an account rather than wait or hand-edit `paused`. Found walking Stage 8 of
// Docs/install-and-deploy.md#part-7--developing-and-testing-locally: raising a real paused account's limit from 5 to 500 (495
// units of headroom) left it ineligible, and /unpause returned 422.
//
// ANTI-VACUITY: the two shapes are asserted to DIFFER from each other after the identical limit
// raise. A regression that made both eligible, or both ineligible, fails — a test that only checked
// shape B in isolation would pass just as happily if raising a limit had stopped working entirely.

import (
	"context"
	"testing"
)

// TestAtLimit_RaisingDailyLimitRecoversOnlyTheUnpausedShape — the asymmetry above, both directions,
// through the REAL accountConsume path (not a hand-written predicate).
func TestAtLimit_RaisingDailyLimitRecoversOnlyTheUnpausedShape(t *testing.T) {
	e := newDispatchEnv(t, "atlimitrecov")

	// Each shape gets its OWN pool, so "which account got consumed" is unambiguous — with both in one
	// pool, consuming the eligible one tells us nothing about whether the other was merely outranked
	// by the id-ASC tiebreak (P4-OD-1) rather than genuinely ineligible.
	poolA := e.seedSharedPool(t, "atlimit-unpaused")
	poolB := e.seedSharedPool(t, "atlimit-paused")

	// Shape A: count == limit, NOT paused.
	acctA := e.seedAccount(t, poolA, "shapeA")
	e.setAccountDailyState(t, acctA, 5, 5, todayUTC(), false, nil)
	// Shape B: count == limit AND paused with reason daily_limit — what the capping send leaves behind.
	acctB := e.seedAccount(t, poolB, "shapeB")
	e.setAccountDailyState(t, acctB, 5, 5, todayUTC(), true, strPtr("daily_limit"))

	// PRECONDITION: at the same limit, BOTH are ineligible. Without this the post-raise result could
	// be explained by shape A having been eligible all along.
	for _, c := range []struct {
		name string
		pool string
	}{{"shape A (not paused)", poolA}, {"shape B (paused)", poolB}} {
		if _, ok, err := e.d.accountConsume(context.Background(), c.pool, defaultAccountOrderBy, nil, "UTC", 0, 900); err != nil {
			t.Fatalf("precondition consume %s: %v", c.name, err)
		} else if ok {
			t.Fatalf("precondition: %s was consumable while at its limit — both shapes must start ineligible", c.name)
		}
	}

	// THE OPERATOR ACTION under test, applied IDENTICALLY to both: raise daily_limit 5 → 500,
	// leaving 495 units of headroom. Only daily_limit changes; paused/count/reset_day are untouched,
	// exactly as PATCH /v1/admin/accounts/{id} {"daily_limit":500} would leave them.
	for _, id := range []string{acctA, acctB} {
		if _, err := e.admin.Exec(context.Background(),
			`UPDATE send_managed.accounts SET daily_limit = 500 WHERE id = $1`, id); err != nil {
			t.Fatalf("raise daily_limit: %v", err)
		}
	}

	gotA, okA, err := e.d.accountConsume(context.Background(), poolA, defaultAccountOrderBy, nil, "UTC", 0, 900)
	if err != nil {
		t.Fatalf("consume shape A after raise: %v", err)
	}
	if !okA {
		t.Fatal("shape A (at-limit, NOT paused) was still ineligible after raising daily_limit — more headroom is all the current-day branch needs; the runbook's 'raise daily_limit' advice depends on this")
	}
	if gotA.id != acctA {
		t.Fatalf("consumed %s from pool A, want %s", gotA.id, acctA)
	}

	_, okB, err := e.d.accountConsume(context.Background(), poolB, defaultAccountOrderBy, nil, "UTC", 0, 900)
	if err != nil {
		t.Fatalf("consume shape B after raise: %v", err)
	}
	if okB {
		t.Fatal("shape B (paused: daily_limit) became eligible from a limit raise alone — the current-day branch requires paused=FALSE, and Phase 1 decision #19 locks that only the daily reset clears this pause. If this is now intended it is a locked-spec change (Rule 2), not a test to relax")
	}

	// THE LOAD-BEARING ASSERTION: the identical action produced DIFFERENT outcomes. This is the claim
	// the runbook makes; a regression collapsing the two shapes fails here even if it picks either one.
	if okA == okB {
		t.Fatalf("both shapes returned eligible=%v after the same limit raise — the runbook documents them as recovering differently; one of the two branches has changed", okA)
	}

	// Shape B's ONLY same-day recovery is the daily reset. Prove the account is otherwise healthy (so
	// its ineligibility above really was the pause, not something else about the row) by aging
	// daily_reset_day and consuming it — the lazy self-heal in the same UPDATE.
	if _, err := e.admin.Exec(context.Background(),
		`UPDATE send_managed.accounts SET daily_reset_day = $2::date WHERE id = $1`, acctB, yesterdayUTC()); err != nil {
		t.Fatalf("age reset day: %v", err)
	}
	gotB, okB2, err := e.d.accountConsume(context.Background(), poolB, defaultAccountOrderBy, nil, "UTC", 0, 900)
	if err != nil {
		t.Fatalf("consume shape B after reset: %v", err)
	}
	if !okB2 || gotB.id != acctB {
		t.Fatalf("shape B was not admitted by the daily reset (ok=%v id=%s) — the reset is documented as its only recovery path, so it must work", okB2, gotB.id)
	}
	var paused bool
	var reason *string
	if err := e.admin.QueryRow(context.Background(),
		`SELECT paused, pause_reason::text FROM send_managed.accounts WHERE id=$1`, acctB).Scan(&paused, &reason); err != nil {
		t.Fatalf("read back shape B: %v", err)
	}
	if paused || reason != nil {
		t.Fatalf("shape B after the reset: paused=%v reason=%v, want FALSE/NULL (the reset clears a daily_limit pause)", paused, reason)
	}
}
