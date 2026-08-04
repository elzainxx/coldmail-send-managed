package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// setConfigRaw writes a RAW jsonb value to a config key, bypassing the typed setters (and the admin
// PUT validator entirely). This is the whole point of the test below: it reproduces the ONE way a
// type-invalid value can actually reach send_managed.config on a live system — a direct SQL write or a bad
// migration — which config.go's validateConfigOrdering comment already names as the reason
// load-time validation exists as a backstop at all.
func (e *dispatchEnv) setConfigRaw(t *testing.T, key, rawJSON string) {
	t.Helper()
	tag, err := e.admin.Exec(context.Background(),
		`UPDATE send_managed.config SET value = $2::jsonb, updated_at = NOW() WHERE key = $1`, key, rawJSON)
	if err != nil {
		t.Fatalf("set raw config %s = %s: %v", key, rawJSON, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("set raw config %s: %d rows affected, want 1 (key missing?)", key, tag.RowsAffected())
	}
}

// TestConfigTypeFault_IsNotADBOutage (§2.1 classification; found 2026-07-28 walking Stage 6 of
// Docs/coldmail-send-managed-local-test-guide.md, "an invalid config type does not wedge the Control").
//
// A WRONG-TYPE config value and a DB outage are different faults with different correct operator
// responses, and the §2.1 gate must not confuse them. This is the sibling of
// TestConfigOrdering_IsNotADBOutage: that one covers a violated ORDERING invariant (a
// *configOrderingError); this one covers a value that does not parse as its key's declared type
// (`{"v":"thirty"}` on an integer key). Both are "the DB answered perfectly and the config it
// returned is unusable" — Rule 1 says reject, and §2.1 says a reject is NOT transport
// unavailability.
//
// The bug this locks down: IsOutage exempted *configOrderingError ALONE, so a type fault — a bare
// fmt.Errorf from readIntKey — fell through to IsOutage's default `return true` and was reported as
// `db outage DETECTED: ... the DB is unreachable` against a healthy DB, then cleared by the very
// next tick's SELECT 1, i.e. the 500ms DETECTED/RESOLVED flap. Observed live: 80 outage lines in
// seconds, burying the one line that names the bad key. The HALT was correct throughout (nothing
// claimed, no restart, self-healed on repair) — the DIAGNOSIS was wrong, which is a Rule-5 honesty
// defect: it points the operator at the database instead of at the config row.
//
// Real infra throughout: a real Postgres clone, a real malformed row, the REAL dispatch Runner
// goroutine, the real logger captured verbatim. Paired controls, per Rule 10:
//   - NEGATIVE (behavior under test): a type fault must NOT halt the gate and must log NO outage line;
//   - ANTI-VACUITY: the loop provably ticked (it logged the fault repeatedly) and provably claims the
//     job the instant the value is repaired — so "nothing was claimed" was the reject, not a dead loop;
//   - POSITIVE: a REAL transport error on the same gate DOES halt and DOES log DETECTED, so the
//     zero-count assertions are real observations rather than a logger that was never wired up.
func TestConfigTypeFault_IsNotADBOutage(t *testing.T) {
	e := newDispatchEnv(t, "cfgtypefault")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	queued := e.seedQueuedJob(t, "typefault@example.com")

	// A REAL type-invalid value on an integer key, written the way it would really arrive.
	e.setConfigRaw(t, "smtp_operation_timeout_sec", `{"v":"thirty"}`)
	_, cfgErr := LoadConfig(ctx, e.pool)
	if cfgErr == nil {
		t.Fatal("precondition: LoadConfig ACCEPTED a quoted string on an integer key — nothing to classify")
	}

	// (a) CLASSIFICATION. It is a config fault, and config faults are not transport unavailability.
	var fault configFault
	if !errors.As(cfgErr, &fault) {
		t.Fatalf("a config TYPE fault does not implement the configFault marker: %T (%v) — it will be "+
			"classified as a DB outage by IsOutage's default branch", cfgErr, cfgErr)
	}
	if IsOutage(cfgErr) {
		t.Fatalf("a config type fault was classified as a DB OUTAGE (%v) — the DB answered fine; calling "+
			"this an outage flaps the §2.2 backoff at its 500ms base forever and misdirects the operator "+
			"to the database instead of the config row", cfgErr)
	}
	// The message must still NAME THE KEY: that one line is the entire diagnosis an operator gets.
	if msg := cfgErr.Error(); !strings.Contains(msg, "smtp_operation_timeout_sec") {
		t.Fatalf("the config fault does not name the offending key: %q — an operator cannot act on it", msg)
	}
	h := NewDBHealth(e.pool)
	if h.Note(cfgErr) || h.Halted() {
		t.Fatal("the gate HALTED on a config type fault — all three loops would report a DB outage for a healthy DB")
	}
	if got := h.RetryIn(); got != 0 {
		t.Fatalf("gate advertises a %s outage backoff after a config type fault, want 0 (it is not halted)", got)
	}

	// (b) THE REAL LOOP, over ~3 ticks of the fault. Fast tick so several land in the window.
	e.setConfigInt(t, "dispatch_tick_interval_ms", 50)
	logs := captureLogs(t)
	runner := NewRunner(e.d)
	loopCtx, cancelLoop := context.WithCancel(ctx)
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); runner.Run(loopCtx) }()
	time.Sleep(2500 * time.Millisecond)

	stop := func() { cancelLoop(); <-loopDone }

	// The loop is alive and noticed the fault repeatedly — anti-vacuity for the log assertions below.
	select {
	case <-loopDone:
		cancelLoop()
		t.Fatal("the dispatch Runner EXITED on a config type fault — a bad config value must not kill the loop")
	default:
	}
	if n := logs.count("smtp_operation_timeout_sec"); n < 2 {
		stop()
		t.Fatalf("the loop logged the offending key %d time(s) over 2.5s, want >=2 — it is not actually "+
			"retrying against the malformed value, so this test would be vacuous.\nlog:\n%s", n, logs.String())
	}
	// THE POINT: no outage declared for a healthy DB, and therefore no DETECTED/RESOLVED flap.
	if n := logs.count("db outage DETECTED"); n != 0 {
		stop()
		t.Fatalf("the loop logged %d \"db outage DETECTED\" line(s) while the DB was HEALTHY and only a "+
			"config VALUE was malformed — a config fault must never be reported as transport "+
			"unavailability.\nlog:\n%s", n, logs.String())
	}
	if n := logs.count("db outage RESOLVED"); n != 0 {
		stop()
		t.Fatalf("the loop logged %d \"db outage RESOLVED\" line(s) — the DETECTED/RESOLVED flap that pins "+
			"the §2.2 backoff at its base and buries the one line naming the bad key.\nlog:\n%s", n, logs.String())
	}
	if e.d.Health().Halted() {
		stop()
		t.Fatal("the shared gate is HALTED because of a config type fault — dispatch, reclaim AND retention " +
			"would all be paused by a mislabeled outage")
	}
	// Nothing was written: a loop that cannot load a usable config claims nothing (§2 invent-nothing).
	if st, _, leaseNull, _, attempt := e.jobState(t, queued); st != "queued" || !leaseNull || attempt != 0 {
		stop()
		t.Fatalf("job is state=%s leaseNull=%v attempt=%d during a config type fault, want queued/true/0 — "+
			"nothing may be claimed on unusable values", st, leaseNull, attempt)
	}

	// (c) ANTI-VACUITY: repair the value and the SAME still-running loop claims the job immediately.
	e.setConfigInt(t, "smtp_operation_timeout_sec", 30)
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, _, _, _, _ := e.jobState(t, queued)
		if st == "assigned" || st == "sending" {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("job still %q after the config value was repaired — the non-claim above was not caused "+
				"by the reject (vacuous test)", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancelLoop()
	select {
	case <-loopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not exit after ctx cancel")
	}

	// (d) POSITIVE CONTROL for the log assertions: a REAL outage on a REAL severed connection DOES halt
	// and DOES log DETECTED — so the zero-counts above are observations, not an unwired logger.
	proxy, proxyDSN := startPGProxy(t, e.moduleDSN)
	ppool := proxiedPool(t, proxyDSN)
	proxy.cut()
	var one int
	transportErr := ppool.QueryRow(ctx, `SELECT 1`).Scan(&one)
	if transportErr == nil {
		t.Fatal("precondition: SELECT 1 succeeded on a severed connection")
	}
	before := logs.count("db outage DETECTED")
	h2 := NewDBHealth(ppool)
	if !h2.Note(transportErr) || !h2.Halted() {
		t.Fatal("control: the gate did not halt on a REAL transport error")
	}
	if got := logs.count("db outage DETECTED"); got != before+1 {
		t.Fatalf("control: a REAL outage logged %d DETECTED line(s) (was %d) — the capture is not observing "+
			"the logger, so the zero-count assertions above prove nothing", got, before)
	}
}

// TestConfigFaultMarker_CoversEveryTypedReader (§2.1, structural). The classification above is only
// as good as its COVERAGE: exempting the marker is worthless if a reader forgets to use it. This
// walks EVERY typed reader's fault path through a real malformed row and asserts each one produces a
// configFault that IsOutage declines — including the ordering error, so the two sibling classes are
// held to one rule in one place.
//
// Rule 10 negative control: `{"v":30}` on an integer key (a VALID value) must produce NO error at
// all, proving each case below fails because of the malformed value and not because LoadConfig
// happens to reject everything.
func TestConfigFaultMarker_CoversEveryTypedReader(t *testing.T) {
	e := newDispatchEnv(t, "cfgfaultmarker")
	ctx := context.Background()

	// One key per reader kind, with a value of the WRONG json type for that reader.
	cases := []struct{ key, bad, good, reader string }{
		{"smtp_operation_timeout_sec", `{"v":"thirty"}`, `{"v":30}`, "readIntKey"},
		{"dispatch_enabled", `{"v":"yes"}`, `{"v":true}`, "readBoolKey"},
		{"variation_zero_width_rate", `{"v":"half"}`, `{"v":0.5}`, "readFloatKey"},
		{"rollup_granularity", `{"v":7}`, `{"v":"day"}`, "readStringKey"},
		{"variation_preheader_templates", `{"v":"not-an-array"}`, `{"v":[]}`, "readStringArrayKey"},
		{"default_shared_pool_id", `{"v":42}`, `{"v":null}`, "readNullableStringKey"},
		// A missing "v" wrapper is the other real corruption shape (a hand-written UPDATE that forgot it).
		{"job_max_processing_time_sec", `{"seconds":120}`, `{"v":120}`, "readIntKey (absent \"v\")"},
	}
	for _, c := range cases {
		t.Run(c.reader+"/"+c.key, func(t *testing.T) {
			// NEGATIVE CONTROL FIRST: the good value loads clean, so a failure below is the bad value.
			e.setConfigRaw(t, c.key, c.good)
			if _, err := LoadConfig(ctx, e.pool); err != nil {
				t.Fatalf("control: LoadConfig rejected the VALID value %s for %s: %v — this subtest cannot "+
					"attribute a failure to the malformed value", c.good, c.key, err)
			}
			e.setConfigRaw(t, c.key, c.bad)
			_, err := LoadConfig(ctx, e.pool)
			if err == nil {
				t.Fatalf("LoadConfig ACCEPTED %s for %s (%s) — a malformed value would be used as if valid",
					c.bad, c.key, c.reader)
			}
			var fault configFault
			if !errors.As(err, &fault) {
				t.Fatalf("%s returned %T (%v), which does NOT implement configFault — IsOutage's default "+
					"branch will report it as `db outage DETECTED` against a healthy DB", c.reader, err, err)
			}
			if IsOutage(err) {
				t.Fatalf("%s's fault is classified as a DB outage: %v", c.reader, err)
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Fatalf("%s's fault does not name the key %q: %q", c.reader, c.key, err.Error())
			}
			// Restore so the next subtest starts from a loadable set.
			e.setConfigRaw(t, c.key, c.good)
		})
	}

	// The ordering error is the OTHER config-fault class: same marker, same classification. Asserted
	// here so both siblings are covered by one rule rather than two independently-remembered ones.
	e.setConfigInt(t, "startup_reconnect_window_sec", 901) // > box_outcome_buffer_ttl_sec (900)
	_, ordErr := LoadConfig(ctx, e.pool)
	if ordErr == nil {
		t.Fatal("precondition: LoadConfig accepted a violating ordering pair")
	}
	var fault configFault
	if !errors.As(ordErr, &fault) {
		t.Fatalf("*configOrderingError no longer implements configFault: %T — the ordering exemption in "+
			"IsOutage has regressed", ordErr)
	}
	if IsOutage(ordErr) {
		t.Fatalf("a config-ordering violation is classified as a DB outage: %v", ordErr)
	}
}
