package dispatch

import (
	"context"
	"log"
	"time"
)

// defaultTickIntervalOnLoadErr is used ONLY when a config read fails transiently (e.g. a brief DB
// blip) so the loop can retry rather than spin or die; it is never a substitute for the seeded
// dispatch_tick_interval_ms (config-not-code) — the very next successful Tick re-reads it fresh.
const defaultTickIntervalOnLoadErr = 1 * time.Second

// notLeaderRetryInterval is how long a loop waits before re-checking leadership after finding it
// does not hold the singleton lock (G11-OD-1 / ADR-0011). Same SAFETY-BOUNDS exemption as
// dbhealth.go's outage backoff and singleton.go's keeper ticker: it is a guard-rail cadence on a
// paused loop, not a send-behavior dial an operator or campaign would tune. The tunable that DOES
// exist here is brain_singleton_reacquire_interval_sec (seeded, send_managed_0023) — how often the keeper
// re-attempts the LOCK. This constant only governs how often a paused loop re-reads an atomic bool,
// so making it a config key would seed a row nobody has a reason to change.
const notLeaderRetryInterval = 1 * time.Second

// Runner drives the Phase-4 polling dispatch loop (§1.2): a single Control ticks every
// dispatch_tick_interval_ms, re-reading config each time (Phase 2 OD-14 — no redeploy needed for a
// config change to take effect). Kick is an OPTIONAL responsiveness nudge — e.g. `run` creating a
// job, or a worker coming online — that runs an EXTRA tick immediately instead of waiting out the
// remainder of the interval; the polling timer is the primary, always-on driver (locked: polling
// loop, not event-driven).
type Runner struct {
	d    *Dispatcher
	kick chan struct{}
}

// NewRunner builds a Runner with a small buffered kick channel (coalescing: extra kicks while one
// is pending are dropped, since the loop re-ticks on each wake anyway).
func NewRunner(d *Dispatcher) *Runner {
	return &Runner{d: d, kick: make(chan struct{}, 1)}
}

// Kick nudges the runner to tick immediately rather than wait for the timer. Non-blocking
// (coalesces). Safe from any goroutine (the platform API's run handler, the worker-online callback).
func (r *Runner) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run loops until ctx is cancelled: tick, then sleep for the FRESHLY-READ dispatch_tick_interval_ms
// (or wake early on a Kick). It ticks once immediately at startup (in case work is already queued
// from a prior run) before entering the wait loop.
func (r *Runner) Run(ctx context.Context) {
	interval := r.tick(ctx)
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.kick:
			timer.Stop()
			interval = r.tick(ctx)
		case <-timer.C:
			interval = r.tick(ctx)
		}
	}
}

// tick runs one Dispatcher.Tick and returns how long to wait before the next one, derived from the
// freshly-read config (dispatch_tick_interval_ms) so a config change takes effect on the very next
// wait, no redeploy (§0, Phase 2 OD-14). Recovers a panic from ANY single tick so an unexpected
// failure degrades to "skip this tick, retry on the next one" rather than crashing the whole
// singleton Control — consistent with the kill-switch philosophy (CLAUDE.md Rule 7) that dispatch
// failures halt cleanly with zero job loss, never take the process down (a claimed-but-stuck job
// simply waits out its lease and is reclaimed by Phase 6; nothing here is unrecoverable).
func (r *Runner) tick(ctx context.Context) (interval time.Duration) {
	interval = defaultTickIntervalOnLoadErr
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("dispatch: tick PANIC recovered: %v — skipping this tick, will retry", rec)
		}
	}()
	// Phase 7 §2.2 (P7-3): the outage gate. While the DB is unreachable this loop is PAUSED — it
	// does not claim (queued→assigned) and does not push, so no new work enters flight and nothing
	// is written. We return the gate's bounded backoff, so a long outage re-probes on an
	// exponentially-backed-off cadence and the Control stays alive (never a crash-loop).
	if ok, wait := r.d.health.Ready(ctx); !ok {
		return wait
	}
	// G11-OD-1 / ADR-0011 (option B): the LEADERSHIP gate. A Control that has lost the singleton
	// advisory lock claims nothing and pushes nothing — Rule 7 is "one dispatcher or none", and
	// before this gate existed a lockless Control kept dispatching for 56 minutes while a second Control
	// held the lock. Checked AFTER the outage gate deliberately: losing the lock is usually caused by
	// a DB restart, so the outage gate is the one that should be logging and setting the cadence.
	if !r.d.IsLeader() {
		r.d.logNotLeader("dispatch")
		return notLeaderRetryInterval
	}
	cfg, pushed, err := r.d.Tick(ctx)
	if err != nil {
		// Distinguish transport unavailability (halt the loop cleanly) from an application error
		// (log and retry next tick, exactly as before). Note() clears the halt on a nil error, so a
		// successful tick is also what ends an outage.
		if r.d.health.Note(err) {
			return r.d.health.RetryIn()
		}
		// level=error matches the admin/platform surfaces' logging convention so ONE
		// `grep 'level=error'` finds every genuine fault, dispatch-side included (the runbook's
		// incident-triage advice depends on that being true; before Phase 10's R4 gate these loop
		// errors carried no level= at all and were invisible to exactly that grep).
		log.Printf("dispatch: tick error: %v level=error", err)
		return defaultTickIntervalOnLoadErr
	}
	r.d.health.Note(nil)
	if pushed > 0 {
		log.Printf("dispatch: tick pushed %d job(s)", pushed)
	}
	return time.Duration(cfg.DispatchTickIntervalMS) * time.Millisecond
}

// ReclaimRunner drives the Phase-6 reclaim loop (§2.1): a separate single-Control polling loop that
// ticks every reclaim_tick_interval_ms, re-reading config each time (no redeploy for a change to
// take effect). It has no Kick — reclaim is a periodic sweep, not event-driven; a stranded job's
// worst-case detection latency is one tick, bounded well below both reclaim windows by the seeded
// 15s default. It is a THIRD loop alongside the dispatch Runner and the in-tick pacing, all in the
// one singleton Control (Rule 7), so the reclaim/dispatch race-safety argument (§2.1) holds.
type ReclaimRunner struct {
	d *Dispatcher
}

func NewReclaimRunner(d *Dispatcher) *ReclaimRunner {
	return &ReclaimRunner{d: d}
}

// Run loops until ctx is cancelled: reclaim-tick, then sleep for the freshly-read
// reclaim_tick_interval_ms. It ticks once immediately at startup (a Control that just came up may
// have jobs stranded on workers that died while it was down — the CONTINUOUS loop catches them on
// its first pass). The one-time startup RESCAN (Phase 7 §1, dispatch.StartupRescan) runs in main.go
// BEFORE this runner starts: it extends orphan leases + refreshes heartbeats so this loop does NOT
// reclaim-all on its first tick, then delegates all actual reclaim back to this loop's §2.4 path
// (P6-8: the rescan and the loop share the one reclaim transition — there is no second reclaim path).
func (r *ReclaimRunner) Run(ctx context.Context) {
	interval := r.tick(ctx)
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			interval = r.tick(ctx)
		}
	}
}

// tick runs one Dispatcher.ReclaimTick and returns the wait before the next, derived from the
// freshly-read reclaim_tick_interval_ms. Recovers a panic from any single tick so a reclaim failure
// degrades to "skip this tick, retry next" rather than crashing the singleton Control (Rule 7) —
// identical discipline to the dispatch tick.
func (r *ReclaimRunner) tick(ctx context.Context) (interval time.Duration) {
	interval = defaultTickIntervalOnLoadErr
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("reclaim: tick PANIC recovered: %v — skipping this tick, will retry", rec)
		}
	}()
	// Phase 7 §2.2 (P7-3): the SAME outage gate the dispatch loop uses (shared via the Dispatcher),
	// so one detection pauses both loops. A paused reclaim loop is load-bearing for the
	// invent-nothing guarantee: it must not act on stale in-memory assumptions, and (structurally) it
	// cannot write the §2.4 transition without the DB anyway.
	if ok, wait := r.d.health.Ready(ctx); !ok {
		return wait
	}
	// ADR-0011 leadership gate. Load-bearing for reclaim specifically: reclaim REASSIGNS in-flight
	// jobs (sending→queued), so a non-leader running it would yank jobs out from under the worker a
	// LEADER Control is currently pushing to — the double-dispatch this gate exists to prevent.
	if !r.d.IsLeader() {
		r.d.logNotLeader("reclaim")
		return notLeaderRetryInterval
	}
	cfg, reclaimed, err := r.d.ReclaimTick(ctx)
	if err != nil {
		if r.d.health.Note(err) {
			return r.d.health.RetryIn()
		}
		log.Printf("reclaim: tick error: %v level=error", err) // level=error: see the dispatch loop
		return defaultTickIntervalOnLoadErr
	}
	r.d.health.Note(nil)
	if reclaimed > 0 {
		log.Printf("reclaim: tick reclaimed %d job(s)", reclaimed)
	}
	return time.Duration(cfg.ReclaimTickIntervalMS) * time.Millisecond
}
