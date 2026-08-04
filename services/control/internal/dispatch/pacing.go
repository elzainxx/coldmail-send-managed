// pacing.go — Phase 6 §1: the per-lane random inter-send delay (Interpretation A, P6-1).
//
// The delay lives HERE, in the per-user lane layer, and NEVER in the dispatch tick: a blocking
// sleep inside the single-Control tick would stall the entire module (§1.1). Instead each of a
// user's lanes carries a soft, in-memory `next_send_at` timestamp; the tick only READS these
// (a timestamp comparison, never a sleep) and withholds a lane's grant until its timer elapses.
// Throughput therefore scales with lane count (throughput ≈ lanes ÷ avg-delay, §1.3) — this is
// explicitly NOT a single global send rate.
//
// This is soft state, single-Control, no schema — the identical accepted restart-skew tradeoff as
// Phase 4's fairness/account cursors (§1.1): on Control restart the slots re-initialize
// immediately-available (a bounded, one-time unpaced burst of at most `ceiling` sends per active
// user), after which pacing resumes. No correctness invariant depends on the pacing timer — the
// daily cap and idempotency are DB-enforced and pacing-independent.
//
// SUBORDINATION TO THE DB IN-FLIGHT COUNT (§1.1, load-bearing). The pacer decides only WHEN a
// grant may fire, never HOW MANY are in flight. The caller passes the Phase-4 DB-derived grant
// (`max(0, ceiling − in_flight[U])`); Admit returns `min(dbGrant, paced-ready slots)`. Because the
// result is a `min`, the register can only ever *further delay* a send — it can never cause the
// Phase-4 concurrency ceiling to be exceeded. If the in-memory register and the DB count ever
// disagree (e.g. after a restart), the DB count wins.
package dispatch

import (
	"math/rand/v2"
	"sync"
	"time"
)

// pacer holds each active user's per-lane `next_send_at` register (§1.1). A user's slice has up to
// `ceiling` entries; entry i is the earliest instant lane i may fire its next send. now/drawDelay
// are injectable so the pacing logic can be unit-tested deterministically (the permitted clock
// seam, §6); production uses wall-clock + a uniform random draw.
type pacer struct {
	mu    sync.Mutex
	slots map[string][]time.Time // userID -> per-lane next_send_at

	now       func() time.Time                       // wall clock (seam for tests)
	drawDelay func(minSec, maxSec int) time.Duration // uniform draw in [minSec,maxSec] seconds (seam for tests)
}

// newPacer builds a pacer with the production clock and a uniform random delay draw.
func newPacer() *pacer {
	return &pacer{
		slots:     make(map[string][]time.Time),
		now:       time.Now,
		drawDelay: uniformDelay,
	}
}

// setClock replaces the pacer's clock (the permitted timing seam, §6). Used by the T1/T2 pacing
// behavioral tests to advance time deterministically past the real inter-send delay window instead
// of sleeping 3–7s per send. Production never calls this — newPacer wires the wall clock.
func (p *pacer) setClock(now func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.now = now
}

// uniformDelay draws a per-send delay uniformly from [minSec, maxSec] seconds (re-drawn per send,
// which also de-synchronizes lanes so they do not align into bursts — §1.1). A degenerate range
// (max ≤ min, e.g. both 0 for the T1 anti-vacuity "delay ~0" run) collapses to exactly minSec.
// math/rand/v2 is concurrency-safe and needs no seeding.
func uniformDelay(minSec, maxSec int) time.Duration {
	if minSec < 0 {
		minSec = 0
	}
	if maxSec <= minSec {
		return time.Duration(minSec) * time.Second
	}
	span := float64(maxSec - minSec)
	secs := float64(minSec) + rand.Float64()*span
	return time.Duration(secs * float64(time.Second))
}

// Admit returns how many sends user U may fire THIS tick: min(dbGrant, paced-ready lanes). It
// normalizes the user's register to `ceiling` slots first (new lanes initialize
// immediately-available at the zero time; a shrunk ceiling drops the most-cooled-down extras),
// then counts slots whose timer has elapsed. It never mutates timers — Stamp does that, only when
// a send actually fires — so a claim that returns fewer jobs than admitted, or a push that fails,
// simply leaves those lanes ready for the next tick.
//
// dbGrant is authoritative for the ceiling (Phase 4 §6): Admit can only return ≤ dbGrant, so
// pacing never lets in-flight exceed the concurrency ceiling.
func (p *pacer) Admit(userID string, ceiling, dbGrant int) int {
	if dbGrant <= 0 || ceiling <= 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	slots := p.normalize(userID, ceiling)
	ready := 0
	for _, t := range slots {
		if !t.After(now) { // t <= now → this lane's timer has elapsed
			ready++
		}
	}
	if ready < dbGrant {
		return ready
	}
	return dbGrant
}

// Stamp records that a send just fired for user U: it sets the earliest-ready lane's timer to
// now + a fresh uniform delay (§1.1 — "the slot's next_send_at is set to NOW() +
// random_uniform(...)"). Called once per ACTUAL dispatch (a successful handoff), so unfired or
// failed grants never consume a lane's cooldown. If no lane is currently ready (defensive — Admit
// should have gated this), it advances the earliest lane anyway so a send always costs a cooldown.
func (p *pacer) Stamp(userID string, ceiling, minSec, maxSec int) {
	if ceiling <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	slots := p.normalize(userID, ceiling)
	// Pick the lane with the smallest next_send_at (the one most likely already ready). Advancing
	// the earliest keeps the register fair across lanes and de-synchronized (fresh draw each time).
	earliest := 0
	for i := 1; i < len(slots); i++ {
		if slots[i].Before(slots[earliest]) {
			earliest = i
		}
	}
	slots[earliest] = now.Add(p.drawDelay(minSec, maxSec))
	p.slots[userID] = slots
}

// normalize returns user U's register resized to exactly `ceiling` slots (caller holds p.mu). New
// slots are the zero time (before any wall clock → immediately ready, matching the restart-burst
// and fresh-lane "first send fires immediately" contract, §1.1). Extra slots (a lowered ceiling)
// are dropped from the front (the most-cooled-down), keeping the longest-cooling timers.
func (p *pacer) normalize(userID string, ceiling int) []time.Time {
	slots := p.slots[userID]
	if len(slots) == ceiling {
		return slots
	}
	if len(slots) < ceiling {
		for len(slots) < ceiling {
			slots = append(slots, time.Time{}) // zero time → ready now
		}
	} else {
		slots = slots[len(slots)-ceiling:]
	}
	p.slots[userID] = slots
	return slots
}
