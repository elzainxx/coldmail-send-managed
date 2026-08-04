package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// rollup_timezone_test.go — P7-OD-7 (RULED: the rollup follows daily_reset_timezone).
//
// R4 found that `date_trunc('day', created_at)` on a TIMESTAMPTZ truncates in the SESSION timezone,
// which nothing in this codebase sets — so a reported "day" was defined by an implicit environment
// value while EVERY other day boundary in the module (daily-limit reset, account scoring, pool
// exhaustion) is pinned to the seeded daily_reset_timezone. The ruling: follow daily_reset_timezone.
// There is one authoritative definition of "a day" here and it governs capacity; a report whose day
// edges did not match the capacity-reset edges would show an operator PARTIAL days of capacity.
//
// Both tests below run against real Postgres through the real pool, with real config rows and (in the
// first test) the real dispatch + real outcome paths — the same infra every other dispatch test uses.

// eventBucketDates returns the summary buckets for one user as yyyy-mm-dd strings mapped to their
// total, so a test can assert WHICH DAY rows landed in without depending on bucket ordering.
// A day bucket comes back as a zone-less timestamp (date_trunc over `created_at AT TIME ZONE tz`),
// i.e. wall-clock in daily_reset_timezone — which is exactly what the assertion compares.
func (e *dispatchEnv) eventBucketDates(t *testing.T, cfg Config, userID string, from, to time.Time) map[string]int {
	t.Helper()
	buckets, err := e.d.SummarizeByPeriod(context.Background(), cfg, userID, from, to)
	if err != nil {
		t.Fatalf("SummarizeByPeriod: %v", err)
	}
	out := map[string]int{}
	for _, b := range buckets {
		out[b.Bucket.Format("2006-01-02")] += b.Total
	}
	return out
}

// seedEventAt appends ONE job_events row for an existing job at an EXPLICIT created_at instant.
// Inserted as admin because job_events is append-only for everyone (the BEFORE UPDATE reject trigger
// fires for the owner too), so a fixture cannot backdate a row after the fact — it must be born at
// the instant under test. INSERT is the only mutation the table permits, which is what this uses.
func (e *dispatchEnv) seedEventAt(t *testing.T, jobID string, at time.Time) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(),
		`INSERT INTO send_managed.job_events (job_id, user_id, from_state, to_state, actor, created_at)
		 SELECT j.id, j.user_id, 'sending', 'sent', 'box', $2 FROM send_managed.jobs j WHERE j.id = $1`,
		jobID, at); err != nil {
		t.Fatalf("seed job_event at %s: %v", at, err)
	}
}

// TestRollup_BucketsFollowDailyResetTimezone is the P7-OD-7 consistency proof: the day a transition
// is REPORTED in is the same day the DAILY-LIMIT RESET puts it in.
//
// It does not assert that against a recomputed formula — it asserts it against the real reset state
// the real consume path wrote (send_managed.accounts.daily_reset_day, stamped from
// `(NOW() AT TIME ZONE daily_reset_timezone)::date` by the eligibility consume). If the rollup and
// the reset ever disagree on where "now" falls, this fails.
//
// ANTI-VACUITY, and the reason for the odd timezone: the zone is chosen so that the local date RIGHT
// NOW differs from the UTC date. So "the rollup bucket == daily_reset_day" cannot be satisfied by a
// rollup that ignores the config and truncates in UTC (or in whatever the session inherited) — the
// test explicitly asserts the bucket is NOT the UTC date. Before the fix this test fails.
func TestRollup_BucketsFollowDailyResetTimezone(t *testing.T) {
	e := newDispatchEnv(t, "rolltz")
	ctx := context.Background()

	// Pick a zone whose CURRENT local date differs from UTC's, whatever hour the suite runs at:
	// UTC-11 differs before 11:00 UTC, UTC+14 differs from 10:00 UTC onward. One always applies.
	nowUTC := time.Now().UTC()
	zone := "Pacific/Kiritimati" // UTC+14
	if nowUTC.Hour() < 11 {
		zone = "Pacific/Midway" // UTC-11
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Skipf("tz database unavailable for %s: %v", zone, err)
	}
	utcDate := nowUTC.Format("2006-01-02")
	zoneDate := nowUTC.In(loc).Format("2006-01-02")
	if zoneDate == utcDate {
		t.Fatalf("test setup is vacuous: %s local date %s == UTC date %s (zone choice is wrong for hour %d)",
			zone, zoneDate, utcDate, nowUTC.Hour())
	}

	e.setConfigStr(t, "daily_reset_timezone", zone)

	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "s3cr3t")
	cfg := e.mustLoadConfig(t)
	if cfg.DailyResetTimezone != zone {
		t.Fatalf("config did not take: daily_reset_timezone=%q, want %q", cfg.DailyResetTimezone, zone)
	}

	// FIXTURE NOTE (not the property under test). send_managed.accounts.daily_reset_day defaults to
	// CURRENT_DATE — the SERVER's date — while the eligibility predicate compares it against
	// `(NOW() AT TIME ZONE daily_reset_timezone)::date`. For a zone WEST of the server the seeded value
	// is a day AHEAD of today-in-tz, and neither eligibility branch matches (`<` fails, `=` fails), so
	// nothing would dispatch and this test would fail on its own fixture rather than on the rollup.
	// Aging the row makes the intended path run: a STALE daily_reset_day is exactly what the §2.1
	// self-heal handles, and the consume re-stamps it to today-in-tz — which is the value asserted below.
	// (The default-vs-config-tz mismatch itself is flagged for the user, not fixed here.)
	if _, err := e.admin.Exec(ctx,
		`UPDATE send_managed.accounts SET daily_reset_day = DATE '2000-01-01' WHERE id=$1`, acct); err != nil {
		t.Fatalf("age daily_reset_day: %v", err)
	}

	// Drive the REAL paths: claim+consume (which stamps daily_reset_day from the config tz) and a real
	// terminal outcome (which writes the sending→sent transition this rollup will summarize).
	oh := NewOutcomeHandler(e.pool, cfg)
	job := e.seedQueuedJob(t, "rolltz@example.com")
	e.dispatchToSending(t, job)
	if !oh.Handle(e.jobWorker(t, job), workerproto.Report{
		Type: workerproto.TypeReport, JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "rolltz-pmid-1", SMTPResponse: "250 2.0.0 OK",
	}) {
		t.Fatal("outcome not acked — precondition failed")
	}

	// The reset boundary the CAPACITY accounting actually used, read from the row the consume wrote.
	var resetDay time.Time
	if err := e.admin.QueryRow(ctx,
		`SELECT daily_reset_day FROM send_managed.accounts WHERE id=$1`, acct).Scan(&resetDay); err != nil {
		t.Fatalf("read daily_reset_day: %v", err)
	}
	resetDate := resetDay.Format("2006-01-02")
	if resetDate != zoneDate {
		t.Fatalf("precondition: daily_reset_day=%s but %s local date is %s — the reset path is not using the config tz, so this test cannot judge the rollup",
			resetDate, zone, zoneDate)
	}

	var userID string
	if err := e.admin.QueryRow(ctx, `SELECT user_id FROM send_managed.jobs WHERE id=$1`, job).Scan(&userID); err != nil {
		t.Fatalf("read job user: %v", err)
	}
	// Window wide enough that no boundary can push a row out of range in either direction.
	got := e.eventBucketDates(t, cfg, userID, nowUTC.Add(-72*time.Hour), nowUTC.Add(72*time.Hour))

	// THE ASSERTION: every transition this job just produced is reported in the SAME day the
	// daily-limit reset counts it in.
	if got[resetDate] == 0 {
		t.Fatalf("no summary bucket for %s (daily_reset_day) — buckets are %v; the rollup and the daily reset disagree on which day this send happened in",
			resetDate, got)
	}
	// ...and NOT in the UTC/session day, which is what an unpinned date_trunc would have produced.
	if n, ok := got[utcDate]; ok {
		t.Fatalf("summary put %d transitions in the UTC day %s while daily_reset_timezone=%s says %s — the rollup is truncating in the session timezone, not the configured one (P7-OD-7)",
			n, utcDate, zone, resetDate)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one day bucket (%s), got %v", resetDate, got)
	}
}

// TestRollup_ChangingDailyResetTimezoneShiftsBuckets is the direct restatement of the shift R4
// demonstrated: the SAME underlying job_events row buckets into a DIFFERENT day when
// daily_reset_timezone changes — 02:30 UTC is the 25th in UTC and the 24th (21:30/22:30) in New York.
//
// Nothing is re-inserted between the two reads: one row, two configs, two buckets. That is what makes
// this a test of the truncation boundary rather than of the fixture.
func TestRollup_ChangingDailyResetTimezoneShiftsBuckets(t *testing.T) {
	e := newDispatchEnv(t, "rollshift")
	ctx := context.Background()
	e.seedAccount(t, e.seedPool(t), "s3cr3t")

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tz database unavailable: %v", err)
	}

	// A fixed instant in the small hours UTC — the window where the two zones disagree on the date.
	// Anchored to yesterday so the row is comfortably inside any query window and never in the future.
	y := time.Now().UTC().Add(-24 * time.Hour)
	instant := time.Date(y.Year(), y.Month(), y.Day(), 2, 30, 0, 0, time.UTC)
	wantUTCDay := instant.Format("2006-01-02")
	wantNYDay := instant.In(ny).Format("2006-01-02")
	if wantUTCDay == wantNYDay {
		t.Fatalf("setup is vacuous: %s is the same date in UTC and New York", instant)
	}

	job := e.seedQueuedJob(t, "rollshift@example.com")
	var userID string
	if err := e.admin.QueryRow(ctx, `SELECT user_id FROM send_managed.jobs WHERE id=$1`, job).Scan(&userID); err != nil {
		t.Fatalf("read job user: %v", err)
	}
	// Exactly ONE event at the boundary instant. The job's own genesis event sits at NOW(), outside
	// the narrow window queried below, so the assertions see only this row.
	e.seedEventAt(t, job, instant)
	from, to := instant.Add(-6*time.Hour), instant.Add(6*time.Hour)

	// Read 1: daily_reset_timezone = UTC (the seeded default).
	e.setConfigStr(t, "daily_reset_timezone", "UTC")
	utcBuckets := e.eventBucketDates(t, e.mustLoadConfig(t), userID, from, to)
	if utcBuckets[wantUTCDay] != 1 || len(utcBuckets) != 1 {
		t.Fatalf("under UTC the row bucketed as %v, want exactly {%s:1}", utcBuckets, wantUTCDay)
	}

	// Read 2: same row, daily_reset_timezone = America/New_York.
	e.setConfigStr(t, "daily_reset_timezone", "America/New_York")
	nyBuckets := e.eventBucketDates(t, e.mustLoadConfig(t), userID, from, to)
	if nyBuckets[wantNYDay] != 1 || len(nyBuckets) != 1 {
		t.Fatalf("under America/New_York the row bucketed as %v, want exactly {%s:1} — changing daily_reset_timezone did NOT shift the bucket, so the rollup is ignoring it",
			nyBuckets, wantNYDay)
	}
	// PAIRED CONTROL: the two reads must genuinely DIFFER. Without this, a rollup that returned the
	// same bucket for every config would pass whichever of the two assertions happened to match.
	if _, stillUTC := nyBuckets[wantUTCDay]; stillUTC {
		t.Fatalf("the New_York read still reports the UTC day %s (%v) — the boundary is not being pinned by config", wantUTCDay, nyBuckets)
	}
	// And the row count is conserved: the shift moves the row, it does not drop or duplicate it.
	if total(utcBuckets) != total(nyBuckets) {
		t.Fatalf("row totals differ across the two reads: UTC %v vs New_York %v", utcBuckets, nyBuckets)
	}
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
