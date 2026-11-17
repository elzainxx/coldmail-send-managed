package adminapi

// daily_reset_hour_test.go — Config Consumption Sweep closeout (2026-07-27): daily_reset_hour was
// seeded and write-time-validated since send_managed_0010 but never read by the health endpoint's own
// daily-reset-boundary queries (queryWaitingForCapacity/queryAccountCounts/queryPoolHeadroom) —
// found during the systematic 53-key consumption sweep, alongside the identical gap in
// internal/dispatch (fixed there too; see dispatch/daily_reset_hour_test.go for the full
// construction rationale this test mirrors, including WHY the zone is not "UTC": the test used to
// t.Skip during UTC hour 23, when hour=0 and hour=23 name the same UTC date, and so made the suite
// unable to go green for one hour of every day. It now sets `daily_reset_timezone` to the
// fixed-offset zone in which it is 12:xx local right now, where the two boundary dates differ at
// EVERY hour). Real Postgres, real HTTP against the actual production Server — no mocks.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// midDayZoneAt returns the fixed-offset zone in which the instant `now` reads 12:xx local time, as the
// IANA name Postgres resolves and as the equivalent Go location. UTC+off is `Etc/GMT-off` — the IANA
// Etc zones carry the INVERTED sign — and off is 12 - (UTC hour), so it is always in [-11, +12]. Duplicated
// from dispatch/daily_reset_hour_test.go (test helpers cannot be shared across packages).
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

// requireZoneAgrees is the anti-vacuity premise: Postgres and Go must agree on what `zone` means
// RIGHT NOW (an inverted Etc sign would shift one side by 2*off hours and could still pass the
// assertions for the wrong reason), and the two boundary dates must differ.
func (e *testEnv) requireZoneAgrees(t *testing.T, zone string, loc *time.Location, now time.Time) {
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

// TestDailyResetHour_BoundaryIsObservableAtEveryUTCHour is the property that REPLACES the old t.Skip:
// at every UTC hour (first and last minute of each) the zone the live test uses reads 12:xx local,
// carries a name that matches its own offset (the Etc sign is inverted), and puts hour=0 and hour=23
// on two different dates. Pure — no database, never skips. THE CONTROL: in plain UTC the two dates
// coincide at 23:30 (and differ at 22:30), which is the window the skip existed for — so this test can
// fail, and would, if midDayZoneAt were ever replaced by UTC.
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
	late := time.Date(2026, 3, 14, 23, 30, 0, 0, time.UTC)
	if dateAtHourIn(late, time.UTC, 0) != dateAtHourIn(late, time.UTC, 23) {
		t.Fatal("control: in UTC at 23:30 the hour=0 and hour=23 dates should coincide (the window the old t.Skip papered over)")
	}
	early := time.Date(2026, 3, 14, 22, 30, 0, 0, time.UTC)
	if dateAtHourIn(early, time.UTC, 0) == dateAtHourIn(early, time.UTC, 23) {
		t.Fatal("control premise: in UTC at 22:30 the hour=0 and hour=23 dates should differ")
	}
}

// TestDailyResetHour_ShiftsHealthAccountCounts — GET /v1/admin/health's accounts.at_daily_limit
// (queryAccountCounts) flips for an IDENTICAL account row when only daily_reset_hour changes —
// proving the health endpoint's own boundary queries read the live dial, not a hardcoded midnight.
func TestDailyResetHour_ShiftsHealthAccountCounts(t *testing.T) {
	e := newTestEnv(t, "resethourhealth")
	pool := e.seedHealthPool(t, "reset-hour-pool")
	acct := e.seedHealthAccount(t, pool, false, false, false, 10, 10) // unpaused, at its daily limit

	now := time.Now()
	zone, loc := midDayZoneAt(now)
	e.requireZoneAgrees(t, zone, loc, now)
	e.setHealthConfigStr(t, "daily_reset_timezone", zone)

	dayAtHour23 := dateAtHourIn(now, loc, 23)
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
		t.Fatalf("at_daily_limit=%d at hour=23, want 1 (current cycle, at limit; zone=%s daily_reset_day=%s): %+v", got, zone, dayAtHour23, accounts23)
	}

	// hour=0: today_in_tz(0) is later than today_in_tz(23) (the local clock reads 12:xx, so 23 hours
	// back is yesterday), so the pinned daily_reset_day is now STALE relative to it -> no longer "the
	// current cycle" -> at_daily_limit must drop to 0 for this account, proving the dial reached the query.
	e.setHealthConfigInt(t, "daily_reset_hour", 0)
	resp0 := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	accounts0, ok := resp0.body["accounts"].(map[string]any)
	if !ok {
		t.Fatalf("response missing accounts object: %s", resp0.raw)
	}
	if got := intField(t, accounts0, "at_daily_limit"); got != 0 {
		t.Fatalf("at_daily_limit=%d at hour=0, want 0 (stale cycle from hour=0's perspective) — daily_reset_hour is not reaching queryAccountCounts: %+v", got, accounts0)
	}

	// Paired control (the dial unchanged => the boundary does not move): back at hour=23 the SAME row
	// is counted again, so the drop above was caused by the hour and by nothing else.
	e.setHealthConfigInt(t, "daily_reset_hour", 23)
	respBack := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	accountsBack, ok := respBack.body["accounts"].(map[string]any)
	if !ok {
		t.Fatalf("response missing accounts object: %s", respBack.raw)
	}
	if got := intField(t, accountsBack, "at_daily_limit"); got != 1 {
		t.Fatalf("at_daily_limit=%d after returning to hour=23, want 1 — the earlier drop was not caused by the dial alone: %+v", got, accountsBack)
	}
}
