package dispatch

import (
	"testing"
	"time"
)

// newTestPacer builds a pacer with a controllable clock and a FIXED delay, so the per-lane
// register logic is asserted deterministically (the permitted clock seam, §6). The full real-time
// throughput tests (T1/T2) drive the actual paced dispatcher against real Postgres in Chunk 5;
// this unit test pins the register math the whole mechanism rests on.
func newTestPacer(clock *time.Time, delay time.Duration) *pacer {
	return &pacer{
		slots:     make(map[string][]time.Time),
		now:       func() time.Time { return *clock },
		drawDelay: func(_, _ int) time.Duration { return delay },
	}
}

// TestPacer_SubordinateToDBGrant — Admit is min(dbGrant, paced-ready): it NEVER returns more than
// the DB-derived grant, even when every lane is ready. This is the load-bearing subordination
// (§1.1): the register can only further delay, never raise the ceiling.
func TestPacer_SubordinateToDBGrant(t *testing.T) {
	clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	p := newTestPacer(&clock, 5*time.Second)

	// ceiling 3, all lanes fresh (ready), but the DB only grants 1 (2 already in flight).
	if got := p.Admit("u", 3, 1); got != 1 {
		t.Fatalf("Admit(ceiling=3, dbGrant=1) = %d, want 1 (must not exceed the DB grant)", got)
	}
	// dbGrant 0 → 0 regardless of readiness (a user at ceiling gets nothing).
	if got := p.Admit("u", 3, 0); got != 0 {
		t.Fatalf("Admit(dbGrant=0) = %d, want 0", got)
	}
}

// TestPacer_WithholdsUntilCooldownElapses — after a send stamps a lane, that lane is withheld
// until its next_send_at passes; the OTHER lanes stay independently available. Advancing the
// clock past the delay makes the cooled lane ready again.
func TestPacer_WithholdsUntilCooldownElapses(t *testing.T) {
	clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	p := newTestPacer(&clock, 5*time.Second)

	// One lane. Fresh → 1 ready. Fire it (stamp).
	if got := p.Admit("u", 1, 1); got != 1 {
		t.Fatalf("fresh single lane Admit = %d, want 1", got)
	}
	p.Stamp("u", 1, 3, 7)

	// Immediately after: the single lane is cooling → 0 admitted even though the DB would grant 1.
	if got := p.Admit("u", 1, 1); got != 0 {
		t.Fatalf("cooling single lane Admit = %d, want 0 (delay must withhold)", got)
	}
	// Advance 4s (< 5s delay): still cooling.
	clock = clock.Add(4 * time.Second)
	if got := p.Admit("u", 1, 1); got != 0 {
		t.Fatalf("Admit at +4s = %d, want 0 (delay is 5s)", got)
	}
	// Advance past 5s total: ready again.
	clock = clock.Add(2 * time.Second)
	if got := p.Admit("u", 1, 1); got != 1 {
		t.Fatalf("Admit at +6s = %d, want 1 (cooldown elapsed)", got)
	}
}

// TestPacer_LanesIndependent — the defining Interpretation-A property (§1.3): with ceiling 2, one
// send cools ONE lane; the second lane remains ready, so throughput scales with lanes rather than
// serializing at a single global rate. This is the unit-level analogue of behavioral test T2.
func TestPacer_LanesIndependent(t *testing.T) {
	clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	p := newTestPacer(&clock, 5*time.Second)

	// Two lanes fresh, DB grants both → 2 admitted.
	if got := p.Admit("u", 2, 2); got != 2 {
		t.Fatalf("two fresh lanes Admit = %d, want 2", got)
	}
	// Fire ONE send: cools one lane, the other stays ready.
	p.Stamp("u", 2, 3, 7)
	if got := p.Admit("u", 2, 2); got != 1 {
		t.Fatalf("after one stamp, Admit = %d, want 1 (one lane still ready — lanes are independent)", got)
	}
	// Fire the second: now both cooling → 0.
	p.Stamp("u", 2, 3, 7)
	if got := p.Admit("u", 2, 2); got != 0 {
		t.Fatalf("after two stamps, Admit = %d, want 0 (both lanes cooling)", got)
	}
	// A single-global-rate impl would have withheld the second lane after the first send; this
	// asserts the opposite (independent timers).
}

// TestPacer_RestartBurstThenPaces — a fresh pacer (models Control restart) initializes every lane
// immediately-available: the first `ceiling` sends fire unpaced (the accepted one-time burst,
// §1.1), then pacing resumes. Also covers ceiling growth/shrink normalization.
func TestPacer_RestartBurstThenPaces(t *testing.T) {
	clock := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	p := newTestPacer(&clock, 5*time.Second)

	// Fresh register, ceiling 3: all 3 ready at once (the restart burst).
	if got := p.Admit("u", 3, 3); got != 3 {
		t.Fatalf("restart burst Admit = %d, want 3 (all lanes start ready)", got)
	}
	p.Stamp("u", 3, 3, 7)
	p.Stamp("u", 3, 3, 7)
	p.Stamp("u", 3, 3, 7)
	// All 3 now cooling.
	if got := p.Admit("u", 3, 3); got != 0 {
		t.Fatalf("after 3 stamps Admit = %d, want 0 (pacing resumed)", got)
	}
	// Shrink the ceiling to 1: normalization keeps the longest-cooling timer, still cooling → 0.
	if got := p.Admit("u", 1, 1); got != 0 {
		t.Fatalf("shrunk-ceiling Admit = %d, want 0", got)
	}
}

// TestUniformDelay_InRangeAndDegenerate — the production draw stays within [min,max]s, and a
// degenerate range (used by T1's "delay ~0" anti-vacuity run) collapses to exactly min.
func TestUniformDelay_InRangeAndDegenerate(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d := uniformDelay(3, 7)
		if d < 3*time.Second || d > 7*time.Second {
			t.Fatalf("uniformDelay(3,7) = %v, out of [3s,7s]", d)
		}
	}
	if d := uniformDelay(0, 0); d != 0 {
		t.Fatalf("uniformDelay(0,0) = %v, want 0 (degenerate range → min)", d)
	}
	if d := uniformDelay(2, 2); d != 2*time.Second {
		t.Fatalf("uniformDelay(2,2) = %v, want 2s", d)
	}
}
