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
// is later than hour=23's -> stale-reset admits it) isolates the dial as the only variable.
//
// WHY THE ZONE IS NOT "UTC" (FIXED 2026-11-17; the same defect module 3 fixed as FW-M3-23). In UTC
// the two dates differ only while the wall-clock hour-of-day is below 23: at 23:xx, `now - 23h` is
// still today, so hour=23 and hour=0 name the SAME date and the property is unobservable. These tests
// used to t.Skip for that hour, which made the whole suite unable to go green for one hour of every
// day (the harness rightly counts a skip as a shortfall: a test that cannot run FAILS, it never
// skips). Nothing in the property is about UTC: `daily_reset_timezone` is itself a dial, and the
// formula takes it as a parameter. So the tests run in the fixed-offset zone in which it is 12:xx
// local RIGHT NOW (midDayZoneAt) — whatever the UTC hour is, hour=23 and hour=0 then name two
// different dates, and the local clock is 11-12 hours from any midnight, which also removes the
// (milliseconds-wide) race the UTC construction had at 22:59:59.
// TestDailyResetHour_BoundaryIsObservableAtEveryUTCHour proves that for all 24 hours without a
// database, and carries the control that the UTC construction really does coincide at 23:xx.
import (
	"context"
	"fmt"
	"testing"
	"time"
)

// midDayZoneAt returns the fixed-offset zone in which the instant `now` reads 12:xx local time, as the
// IANA name Postgres resolves and as the equivalent Go location. UTC+off is `Etc/GMT-off` — the IANA
// Etc zones carry the INVERTED sign — and off is 12 - (UTC hour), so it is always in [-11, +12]: every
// such name exists in Postgres's own zone set (no dependency on the host's tz database for the DB side)
// and the Go side is a plain fixed zone (no dependency on one for the test side either).
func midDayZoneAt(now time.Time) (string, *time.Location) {
	off := 12 - now.UTC().Hour()
	name := "UTC"
	switch {
	case off > 0:
		name = fmt.Sprintf("Etc/GMT-%d", off)
	case off < 0:
		name = fmt.Sprintf("Etc/GMT+%d", -off)
	}
	return name, time.FixedZone(name, off*3600)
}

// dateAtHourIn computes the SAME boundary the production SQL computes —
// ((NOW() - INTERVAL '1 hour' * hour) AT TIME ZONE tz)::date — for the zone loc.
func dateAtHourIn(now time.Time, loc *time.Location, hour int) string {
	return now.In(loc).Add(-time.Duration(hour) * time.Hour).Format("2006-01-02")
}

// requireZoneAgrees is the anti-vacuity premise of the live tests: Postgres and Go must agree on
// what `zone` means RIGHT NOW (an inverted Etc sign would shift one side by 2*off hours and could
// still pass the assertions below for the wrong reason), and the two boundary dates must differ.
func (e *dispatchEnv) requireZoneAgrees(t *testing.T, zone string, loc *time.Location, now time.Time) {
	t.Helper()
	var dbDate string
	if err := e.admin.QueryRow(context.Background(),
		`SELECT to_char((NOW() AT TIME ZONE $1)::date, 'YYYY-MM-DD')`, zone).Scan(&dbDate); err != nil {
		t.Fatalf("read today in %s from Postgres: %v", zone, err)
	}
	if goDate := dateAtHourIn(now, loc, 0); dbDate != goDate {
		t.Fatalf("Postgres says today in %s is %s, the test computed %s — the zone name and the Go location disagree", zone, dbDate, goDate)
	}
	if d0, d23 := dateAtHourIn(now, loc, 0), dateAtHourIn(now, loc, 23); d0 == d23 {
		t.Fatalf("test setup is vacuous: in %s the boundary dates for hour=0 and hour=23 are both %s", zone, d0)
	}
}

// TestDailyResetHour_BoundaryIsObservableAtEveryUTCHour is the property that REPLACES the old
// t.Skip: for every UTC hour of the day (first and last minute of each) the zone the live tests use
// reads 12:xx local, is a name Postgres accepts in the sign convention it uses, and puts hour=0 and
// hour=23 on two different dates. Pure — no database, never skips. THE CONTROL is the construction
// the skip existed for: in plain UTC the two dates coincide at 23:xx (and only then), so this test
// can fail, and would, if midDayZoneAt were ever replaced by UTC.
func TestDailyResetHour_BoundaryIsObservableAtEveryUTCHour(t *testing.T) {
	for h := 0; h < 24; h++ {
		for _, minute := range []int{0, 59} {
			now := time.Date(2026, 3, 14, h, minute, 30, 0, time.UTC)
			name, loc := midDayZoneAt(now)
			if got := now.In(loc).Hour(); got != 12 {
				t.Errorf("UTC %02d:%02d: zone %s reads local hour %d, want 12", h, minute, name, got)
			}
			if d0, d23 := dateAtHourIn(now, loc, 0), dateAtHourIn(now, loc, 23); d0 == d23 {
				t.Errorf("UTC %02d:%02d: in %s hour=0 and hour=23 are both %s — the property is unobservable at this hour", h, minute, name, d0)
			}
			// The Etc sign is inverted (Etc/GMT-5 is UTC+5): the Go side's offset must agree with the name.
			_, off := now.In(loc).Zone()
			var wantName string
			switch hours := off / 3600; {
			case hours > 0:
				wantName = fmt.Sprintf("Etc/GMT-%d", hours)
			case hours < 0:
				wantName = fmt.Sprintf("Etc/GMT+%d", -hours)
			default:
				wantName = "UTC"
			}
			if name != wantName {
				t.Errorf("UTC %02d:%02d: zone name %q does not match its own offset %+ds (want %q)", h, minute, name, off, wantName)
			}
		}
	}

	// Control: the OLD construction (plain UTC) coincides at 23:30 and distinguishes at 22:30. If this
	// pair ever stops holding, the premise of the whole fix is gone.
	late := time.Date(2026, 3, 14, 23, 30, 0, 0, time.UTC)
	if dateAtHourIn(late, time.UTC, 0) != dateAtHourIn(late, time.UTC, 23) {
		t.Fatal("control: in UTC at 23:30 the hour=0 and hour=23 dates should coincide (that is the window the old t.Skip papered over)")
	}
	early := time.Date(2026, 3, 14, 22, 30, 0, 0, time.UTC)
	if dateAtHourIn(early, time.UTC, 0) == dateAtHourIn(early, time.UTC, 23) {
		t.Fatal("control premise: in UTC at 22:30 the hour=0 and hour=23 dates should differ")
	}
}

// TestDailyResetHour_ShiftsTheEligibilityBoundary — poolHasEligible flips for an IDENTICAL account
// row when only daily_reset_hour changes.
func TestDailyResetHour_ShiftsTheEligibilityBoundary(t *testing.T) {
	e := newDispatchEnv(t, "resethour")
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	now := time.Now()
	zone, loc := midDayZoneAt(now)
	e.requireZoneAgrees(t, zone, loc, now)

	dayAtHour23 := dateAtHourIn(now, loc, 23)
	e.setAccountDailyState(t, acct, 10, 10, dayAtHour23, false, nil)

	// hour=23: daily_reset_day == today_in_tz(23) -> current cycle, at-limit -> INELIGIBLE.
	has23, err := e.d.poolHasEligible(context.Background(), pool, zone, 23, 900)
	if err != nil {
		t.Fatalf("poolHasEligible(hour=23): %v", err)
	}
	if has23 {
		t.Fatalf("account reported eligible at hour=23 despite being at-limit for that hour's current cycle (daily_reset_day=%s, zone=%s)", dayAtHour23, zone)
	}

	// hour=0: today_in_tz(0) is today's plain local date, later than today_in_tz(23) (the local clock
	// reads 12:xx, so 23 hours back is yesterday) -> daily_reset_day is now STALE (< today_in_tz) ->
	// stale-reset branch admits an unpaused account -> ELIGIBLE.
	has0, err := e.d.poolHasEligible(context.Background(), pool, zone, 0, 900)
	if err != nil {
		t.Fatalf("poolHasEligible(hour=0): %v", err)
	}
	if !has0 {
		t.Fatalf("account still reported ineligible at hour=0 (daily_reset_day=%s, today_in_tz(0)=%s, zone=%s) — daily_reset_hour is not genuinely shifting the boundary", dayAtHour23, dateAtHourIn(now, loc, 0), zone)
	}

	// Anti-vacuity: re-checking hour=23 again is still ineligible (poolHasEligible is read-only and
	// idempotent — the earlier flip was caused by the hour PARAMETER, not a side effect of the call).
	has23Again, err := e.d.poolHasEligible(context.Background(), pool, zone, 23, 900)
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
	e := newDispatchEnv(t, "resethourconsume")
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	now := time.Now()
	zone, loc := midDayZoneAt(now)
	e.requireZoneAgrees(t, zone, loc, now)

	dayAtHour23 := dateAtHourIn(now, loc, 23)
	e.setAccountDailyState(t, acct, 10, 10, dayAtHour23, false, nil)

	// Ineligible at hour=23 (current cycle, at-limit) — the consume must find nothing.
	_, ok23, err := e.d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, zone, 23, 900)
	if err != nil {
		t.Fatalf("accountConsume(hour=23): %v", err)
	}
	if ok23 {
		t.Fatal("accountConsume succeeded at hour=23 despite the account being at-limit for that cycle")
	}

	// Eligible (and genuinely consumable) at hour=0, where the same row reads as stale.
	consumed, ok0, err := e.d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, zone, 0, 900)
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
