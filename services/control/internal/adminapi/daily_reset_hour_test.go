package adminapi

// daily_reset_hour_test.go — Config Consumption Sweep closeout (2026-07-27): daily_reset_hour was
// seeded and write-time-validated since send_managed_0010 but never read by the health endpoint's own
// daily-reset-boundary queries (queryWaitingForCapacity/queryAccountCounts/queryPoolHeadroom) —
// found during the systematic 53-key consumption sweep, alongside the identical gap in
// internal/dispatch (fixed there too; see dispatch/daily_reset_hour_test.go for the full
// construction rationale this test mirrors). Real Postgres, real HTTP against the actual
// production Server — no mocks.

import (
	"testing"
	"time"
)

func todayInTZForHourAdmin(hour int) string {
	return time.Now().UTC().Add(-time.Duration(hour) * time.Hour).Format("2006-01-02")
}

// TestDailyResetHour_ShiftsHealthAccountCounts — GET /v1/admin/health's accounts.at_daily_limit
// (queryAccountCounts) flips for an IDENTICAL account row when only daily_reset_hour changes —
// proving the health endpoint's own boundary queries read the live dial, not a hardcoded midnight.
func TestDailyResetHour_ShiftsHealthAccountCounts(t *testing.T) {
	if time.Now().UTC().Hour() == 23 {
		t.Skip("wall-clock UTC hour is 23 — the hour=0 vs hour=23 boundaries coincide this minute; skip rather than flake")
	}
	e := newTestEnv(t, "resethourhealth")
	pool := e.seedHealthPool(t, "reset-hour-pool")
	acct := e.seedHealthAccount(t, pool, false, false, false, 10, 10) // unpaused, at its daily limit

	dayAtHour23 := todayInTZForHourAdmin(23)
	if _, err := e.admin.Exec(bgCtx(),
		`UPDATE send_managed.accounts SET daily_reset_day = $2::date WHERE id = $1`, acct, dayAtHour23); err != nil {
		t.Fatalf("pin daily_reset_day: %v", err)
	}

	// hour=23: daily_reset_day == today_in_tz(23) -> current cycle -> counted as at_daily_limit.
	e.setHealthConfigInt(t, "daily_reset_hour", 23)
	resp23 := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	accounts23, ok := resp23.body["accounts"].(map[string]any)
	if !ok {
		t.Fatalf("response missing accounts object: %s", resp23.raw)
	}
	if got := intField(t, accounts23, "at_daily_limit"); got != 1 {
		t.Fatalf("at_daily_limit=%d at hour=23, want 1 (current cycle, at limit): %+v", got, accounts23)
	}

	// hour=0: today_in_tz(0) is later than today_in_tz(23) (guaranteed by the guard above), so the
	// pinned daily_reset_day is now STALE relative to it -> no longer "the current cycle" ->
	// at_daily_limit must drop to 0 for this account, proving the dial reached the query.
	e.setHealthConfigInt(t, "daily_reset_hour", 0)
	resp0 := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	accounts0, ok := resp0.body["accounts"].(map[string]any)
	if !ok {
		t.Fatalf("response missing accounts object: %s", resp0.raw)
	}
	if got := intField(t, accounts0, "at_daily_limit"); got != 0 {
		t.Fatalf("at_daily_limit=%d at hour=0, want 0 (stale cycle from hour=0's perspective) — daily_reset_hour is not reaching queryAccountCounts: %+v", got, accounts0)
	}
}
