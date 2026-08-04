package dispatch

// daily_reset_hour_test.go — Config Consumption Sweep closeout (2026-07-27): daily_reset_hour was
// seeded and write-time-validated since send_managed_0010 but never read by any daily-reset-boundary
// computation (eligibility/consume, pool exhaustion, account scoring, the admin health endpoint) —
// found during the systematic 53-key consumption sweep. Wired into all of them (§11 of
// Docs/history/phase-1-data-model.md: "resetting at daily_reset_hour"). This test proves the wiring is
// genuinely live, not just that it compiles: an account's eligibility flips when daily_reset_hour
// changes, for an IDENTICAL daily_reset_day/daily_sent_count/daily_limit row — the only variable
// is the config dial. Real Postgres, real Dispatcher — no mocks.
//
// CONSTRUCTION. The production formula is today_in_tz = ((NOW() - h hours) AT TZ tz)::date — a
// LARGER h shifts the boundary EARLIER in wall-clock time (the calendar flips to "today" later in
// the day). Pinning daily_reset_day to today_in_tz(hour=23) and checking at hour=23 (current
// cycle, at-limit -> ineligible) vs hour=0 (that same date is now STALE, since hour=0's today_in_tz
// is >= hour=23's whenever the current UTC hour-of-day is < 23 -> stale-reset admits it) isolates
// the dial as the only variable. The construction only holds when the wall-clock UTC hour-of-day
// is < 23 (skipped otherwise, rather than flaking in that ~1-hour daily window).
import (
	"context"
	"testing"
	"time"
)

// todayInTZForHour computes the SAME boundary the production SQL computes
// (((NOW() - INTERVAL '1 hour' * hour) AT TIME ZONE 'UTC')::date).
func todayInTZForHour(hour int) string {
	return time.Now().UTC().Add(-time.Duration(hour) * time.Hour).Format("2006-01-02")
}

// skipNearUTCHourBoundary skips the test in the ~1-hour daily window where the hour=23-vs-hour=0
// construction below cannot distinguish the two boundaries (see the file header).
func skipNearUTCHourBoundary(t *testing.T) {
	t.Helper()
	if time.Now().UTC().Hour() == 23 {
		t.Skip("wall-clock UTC hour is 23 — the hour=0 vs hour=23 boundaries coincide this minute; skip rather than flake")
	}
}

// TestDailyResetHour_ShiftsTheEligibilityBoundary — poolHasEligible flips for an IDENTICAL account
// row when only daily_reset_hour changes.
func TestDailyResetHour_ShiftsTheEligibilityBoundary(t *testing.T) {
	skipNearUTCHourBoundary(t)
	e := newDispatchEnv(t, "resethour")
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")

	dayAtHour23 := todayInTZForHour(23)
	e.setAccountDailyState(t, acct, 10, 10, dayAtHour23, false, nil)

	// hour=23: daily_reset_day == today_in_tz(23) -> current cycle, at-limit -> INELIGIBLE.
	has23, err := e.d.poolHasEligible(context.Background(), pool, "UTC", 23, 900)
	if err != nil {
		t.Fatalf("poolHasEligible(hour=23): %v", err)
	}
	if has23 {
		t.Fatalf("account reported eligible at hour=23 despite being at-limit for that hour's current cycle (daily_reset_day=%s)", dayAtHour23)
	}

	// hour=0: today_in_tz(0) is today's plain UTC date, later than today_in_tz(23) whenever the
	// current UTC hour-of-day is < 23 (guaranteed by skipNearUTCHourBoundary) -> daily_reset_day is
	// now STALE (< today_in_tz) -> stale-reset branch admits an unpaused account -> ELIGIBLE.
	has0, err := e.d.poolHasEligible(context.Background(), pool, "UTC", 0, 900)
	if err != nil {
		t.Fatalf("poolHasEligible(hour=0): %v", err)
	}
	if !has0 {
		t.Fatalf("account still reported ineligible at hour=0 (daily_reset_day=%s, today_in_tz(0)=%s) — daily_reset_hour is not genuinely shifting the boundary", dayAtHour23, todayInTZForHour(0))
	}

	// Anti-vacuity: re-checking hour=23 again is still ineligible (poolHasEligible is read-only and
	// idempotent — the earlier flip was caused by the hour PARAMETER, not a side effect of the call).
	has23Again, err := e.d.poolHasEligible(context.Background(), pool, "UTC", 23, 900)
	if err != nil {
		t.Fatalf("poolHasEligible(hour=23, recheck): %v", err)
	}
	if has23Again {
		t.Fatal("account became eligible at hour=23 on the SECOND check — poolHasEligible must be read-only and idempotent")
	}
}

// TestDailyResetHour_ReachesAccountConsumeAndScoreRecompute — the SAME dial reaches the two other
// call sites this closeout wired (accountConsume's atomic consume, and the lazy-reset counter
// zeroing), not just the read-only poolHasEligible predicate. Uses the identical construction.
func TestDailyResetHour_ReachesAccountConsumeAndScoreRecompute(t *testing.T) {
	skipNearUTCHourBoundary(t)
	e := newDispatchEnv(t, "resethourconsume")
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")

	dayAtHour23 := todayInTZForHour(23)
	e.setAccountDailyState(t, acct, 10, 10, dayAtHour23, false, nil)

	// Ineligible at hour=23 (current cycle, at-limit) — the consume must find nothing.
	_, ok23, err := e.d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, "UTC", 23, 900)
	if err != nil {
		t.Fatalf("accountConsume(hour=23): %v", err)
	}
	if ok23 {
		t.Fatal("accountConsume succeeded at hour=23 despite the account being at-limit for that cycle")
	}

	// Eligible (and genuinely consumable) at hour=0, where the same row reads as stale.
	consumed, ok0, err := e.d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, "UTC", 0, 900)
	if err != nil {
		t.Fatalf("accountConsume(hour=0): %v", err)
	}
	if !ok0 || consumed.id != acct {
		t.Fatalf("accountConsume did not admit the account at hour=0 (ok=%v, got=%+v) — daily_reset_hour is not reaching the atomic consume", ok0, consumed)
	}
	// The consume must have reset the counter (stale-cycle branch): daily_sent_count is now 1, not 11.
	if got := e.accountDailySentCount(t, acct); got != 1 {
		t.Fatalf("daily_sent_count after the stale-reset consume = %d, want 1 (the lazy reset should have zeroed then incremented)", got)
	}
}
