package dispatch

import (
	"context"
	"testing"
	"time"
)

// Pacing behavioral tests T1/T2 (§6). These drive the REAL dispatcher + REAL Postgres through real
// Tick() calls with a real per-lane delay set in config; the ONLY seam is the pacer's clock (§6
// explicitly permits "the clock for advancing past windows"), so the test advances simulated time
// past the 3–7s inter-send delay instead of sleeping minutes for 200 sends. The delay, the lane
// register, the DB claim/consume/handoff are all real.

// settleForPacing marks every in-flight job sent and frees the workers, so the next Tick sees fresh
// lane capacity (mirrors settleAllSending but named for the pacing tests' intent).
func (e *dispatchEnv) drainInFlight(t *testing.T) {
	t.Helper()
	e.settleAllSending(t)
}

// runPacedTicks drives `ticks` dispatch ticks, advancing the injected clock by `advance` before
// each, draining in-flight jobs each time, and returns how many jobs were pushed in total. With a
// non-zero send delay, the number pushed per tick is bounded by how many lanes have cooled down
// since the last send — i.e. the pacer, not capacity.
func (e *dispatchEnv) runPacedTicks(t *testing.T, clock *time.Time, advance time.Duration, ticks int) int {
	t.Helper()
	ctx := context.Background()
	total := 0
	for i := 0; i < ticks; i++ {
		*clock = clock.Add(advance)
		e.drainInFlight(t)
		_, pushed, err := e.d.Tick(ctx)
		if err != nil {
			t.Fatalf("paced tick %d: %v", i, err)
		}
		total += pushed
	}
	return total
}

// TestPacing_T2_ThroughputScalesWithLanes (T2 — the DEFINING Interpretation-A property) — the same
// workload with 2 lanes drains at ~2× the rate of 1 lane. A single-global-rate implementation would
// send at the same rate regardless of lane count; this refutes it. Real dispatcher + real DB; the
// clock is advanced by exactly the average delay per tick so each tick cools ~`lanes` lanes.
func TestPacing_T2_ThroughputScalesWithLanes(t *testing.T) {
	run := func(lanes int) int {
		e := newDispatchEnv(t, "pacet2l"+string(rune('0'+lanes)))
		ctx := context.Background()
		pool := e.seedPool(t)
		// Over-provision accounts + workers so NEITHER is the bottleneck — the delay must set the pace.
		acct := e.seedAccount(t, pool, "sec")
		e.setAccountDailyState(t, acct, 100000, 0, todayUTC(), false, nil)
		for i := 0; i < lanes+2; i++ {
			e.addWorker(t)
		}
		e.setConfigInt(t, "concurrency_per_user_default", lanes)
		// A real, fixed inter-send delay of 5s (min==max==5 → deterministic cadence for the assertion).
		e.setConfigInt(t, "send_delay_min_sec", 5)
		e.setConfigInt(t, "send_delay_max_sec", 5)
		// Plenty of jobs so demand outlasts the measurement window.
		for i := 0; i < 60; i++ {
			e.seedQueuedJob(t, "t2"+string(rune('a'+i%26))+string(rune('a'+i/26))+"@example.com")
		}
		// Inject a controllable clock into the dispatcher's pacer.
		clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
		e.d.pace.setClock(func() time.Time { return clock })
		_ = ctx
		// Advance 5s per tick (== the delay) over 10 ticks. Each tick, ~`lanes` lanes have cooled, so
		// ~`lanes` sends fire. Total ≈ lanes × ticks (bounded by the pacer, not capacity).
		return e.runPacedTicks(t, &clock, 5*time.Second, 10)
	}

	one := run(1)
	two := run(2)

	// 2 lanes must send meaningfully more than 1 lane over the same simulated window — the teeth
	// against a single global rate. Allow tolerance for the restart-burst first tick (both get an
	// unpaced first send per lane) and integer effects; assert a clear separation, not an exact 2×.
	if two <= one {
		t.Fatalf("T2 FAILED: 2 lanes pushed %d, 1 lane pushed %d — throughput did NOT scale with lanes (looks like a single global rate)", two, one)
	}
	// Sanity floor: with 5s advance × 10 ticks and a 5s delay, one lane fires ~once/tick (~10-11),
	// two lanes ~twice/tick (~20-22). Assert two is at least ~1.5× one (well clear of noise).
	if two*2 < one*3 {
		t.Fatalf("T2 weak: 2 lanes=%d, 1 lane=%d — expected roughly 2×, got <1.5× (Interpretation A not honored)", two, one)
	}
	t.Logf("T2: 1 lane pushed %d, 2 lanes pushed %d over 10 ticks (throughput scales with lanes)", one, two)
}

// TestPacing_T1_DelayBoundNotCapacityBound (T1 anti-vacuity core) — with accounts/workers
// over-provisioned, the send rate is set by the DELAY, not capacity: a non-zero delay throttles
// throughput below what capacity alone would allow, and dropping the delay to ~0 lets the SAME
// workload drain far faster. This proves the pace is delay-bound (§1.3), the T1 anti-vacuity claim.
func TestPacing_T1_DelayBoundNotCapacityBound(t *testing.T) {
	setup := func(suffix string, delaySec int) (*dispatchEnv, *time.Time) {
		e := newDispatchEnv(t, suffix)
		pool := e.seedPool(t)
		acct := e.seedAccount(t, pool, "sec")
		e.setAccountDailyState(t, acct, 100000, 0, todayUTC(), false, nil)
		for i := 0; i < 5; i++ {
			e.addWorker(t)
		}
		e.setConfigInt(t, "concurrency_per_user_default", 3)
		e.setConfigInt(t, "send_delay_min_sec", delaySec)
		e.setConfigInt(t, "send_delay_max_sec", delaySec)
		for i := 0; i < 60; i++ {
			e.seedQueuedJob(t, "t1"+string(rune('a'+i%26))+string(rune('a'+i/26))+"@example.com")
		}
		clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
		c := &clock
		e.d.pace.setClock(func() time.Time { return *c })
		return e, c
	}

	// Paced (5s delay): advance only 1s per tick — most ticks find no cooled lane, so few sends fire.
	ePaced, cPaced := setup("t1paced", 5)
	paced := ePaced.runPacedTicks(t, cPaced, 1*time.Second, 10)

	// Unpaced (~0 delay): same 1s/tick advance — lanes are always ready, so it drains capacity-fast.
	eFast, cFast := setup("t1fast", 0)
	fast := eFast.runPacedTicks(t, cFast, 1*time.Second, 10)

	if fast <= paced {
		t.Fatalf("T1 FAILED: unpaced run pushed %d, paced run pushed %d — the delay did NOT bind the rate (should be delay-bound, §1.3)", fast, paced)
	}
	t.Logf("T1: paced(5s, 1s/tick)=%d vs unpaced(~0)=%d — the delay, not capacity, set the pace", paced, fast)
}
