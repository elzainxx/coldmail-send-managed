// Package singleton implements the single-active-Control guard (CLAUDE.md Rule 7).
//
// Mechanism: a SESSION-level Postgres advisory lock on a fixed key, held on a DEDICATED
// standalone connection (NOT pooled) for the Control's lifetime. Session-level so Postgres
// auto-releases it when the connection dies — SIGKILL / host loss leaves NO stale lock.
//
// Ownership model (why this shape): a pgx.Conn is NOT safe for concurrent use, and readyz
// is called from HTTP goroutines. Rather than share the lock connection, a SINGLE background
// "keeper" goroutine exclusively owns the connection: it holds the lock and periodically
// verifies it with a lightweight query. readyz reads an atomic flag the keeper maintains —
// it never touches the connection. This is what makes the lock stable under a healthcheck
// that polls readyz every few seconds indefinitely (an earlier version pinged the lock conn
// from readyz, which raced and dropped the lock).
package singleton

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// LockKey is the fixed, namespaced advisory-lock key for the Control singleton. Distinct from
// the migration runner's key (9_100_000_001) — see docs/advisory-locks.md.
const LockKey int64 = 9_200_000_001

// ErrNotAcquired means another Control holds the lock and the bounded wait elapsed. Callers
// treat this as a clean, expected exit — not a crash.
var ErrNotAcquired = errors.New("singleton: lock not acquired within timeout")

// Guard owns the dedicated lock connection via its keeper goroutine.
type Guard struct {
	conn    *pgx.Conn
	dsn     string // for re-acquisition after a mid-life loss (ADR-0011 option B)
	held    atomic.Bool
	stop    chan struct{}
	stopped chan struct{}

	// Re-acquire bookkeeping. Touched ONLY by the keeper goroutine (keep -> tryReacquire ->
	// logReacquireFailure), which is also the sole owner of conn, so these need no mutex — the
	// same single-owner discipline that makes conn safe.
	lastAttempt       time.Time
	reacquireFailures int

	// notHeldSince is when the CURRENT not-held span began, as UnixNano; 0 = the lock has never been
	// lost. Atomic rather than a plain time.Time because it is written by the keeper and read from
	// outside it (NotHeldFor, used by the behavioral test that pins the reported duration) — the
	// single-owner argument above covers the fields the keeper alone touches, and this one is no
	// longer in that set.
	notHeldSince atomic.Int64

	// reacquireEvery is the cadence at which the keeper re-attempts the lock after LOSING it.
	// Sourced from the seeded brain_singleton_reacquire_interval_sec (Rule 1 — config not code) and
	// installed by SetReacquireInterval before the keeper needs it. Zero => never re-acquire (the
	// Control stays paused until restarted), which is also the safe value if the config read failed.
	reacquireEvery atomic.Int64 // nanoseconds
}

// SetReacquireInterval installs the seeded re-acquire cadence (config-not-code). Safe to call at any
// time, and the keeper re-reads the atomic on every attempt — but note that the only production
// caller is main.go AT STARTUP, so in practice a change to brain_singleton_reacquire_interval_sec
// takes effect on the next Control start, NOT on the next tick. That is deliberate rather than an
// oversight: this key governs recovery from a state in which the DB may be unreachable, so making the
// cadence depend on a fresh DB read would leave it undefined in exactly the situation it exists for.
// Both singleton keys are read at startup only (brain_singleton_acquire_timeout_sec necessarily so —
// it bounds startup itself); config-reference.md and the runbook say so, because for this one the
// startup-only read is a surprise rather than a tautology. A non-positive duration disables
// re-acquisition.
func (g *Guard) SetReacquireInterval(d time.Duration) {
	if g == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	g.reacquireEvery.Store(int64(d))
}

// Acquire opens a DEDICATED standalone connection (via dsn) and tries
// pg_try_advisory_lock(LockKey), polling until it succeeds or `timeout` elapses. timeout==0
// => a single non-blocking attempt (fail-fast). On success it starts the keeper goroutine
// that exclusively owns the connection and holds the lock for the process lifetime.
func Acquire(ctx context.Context, dsn string, timeout time.Duration) (*Guard, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("singleton dedicated connect: %w", err)
	}

	deadline := time.Now().Add(timeout)
	const backoff = 200 * time.Millisecond
	for {
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", LockKey).Scan(&got); err != nil {
			_ = conn.Close(ctx)
			return nil, fmt.Errorf("pg_try_advisory_lock: %w", err)
		}
		if got {
			g := &Guard{conn: conn, dsn: dsn, stop: make(chan struct{}), stopped: make(chan struct{})}
			g.held.Store(true)
			go g.keep()
			return g, nil
		}
		if timeout == 0 || time.Now().After(deadline) {
			_ = conn.Close(ctx)
			return nil, ErrNotAcquired
		}
		select {
		case <-ctx.Done():
			_ = conn.Close(ctx)
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// keep is the SOLE owner of the lock connection. It verifies the lock periodically with a
// trivial query. If the connection ever dies, it marks held=false — and held=false is what PAUSES
// dispatch/reclaim/retention (ADR-0011 option B), so the process stops being a dispatcher the
// moment it stops being the lock holder. It then re-attempts acquisition on the seeded bounded
// cadence and resumes only once it genuinely holds the lock again.
//
// WHY RE-ACQUIRING IS SAFE HERE AND WAS NOT BEFORE (G11-OD-1, ruled 2026-07-29). This function used
// to mark not-held and RETURN, with the comment "re-acquiring would be a split-control risk". The risk
// was real but the diagnosis was inverted: the danger was never the re-acquisition, it was that
// nothing consumed held=false, so the process kept dispatching WITHOUT the lock — observed live for
// 56 minutes across a DB restart, while a second Control took the free lock and came fully up. With
// dispatch gated on held (main.go wires guard.Healthy into the loops' gate), the two states are:
//
//	held=false → dispatches nothing, so it cannot conflict with a successor no matter what the
//	             successor does. pg_try_advisory_lock is non-blocking, so an attempt while a
//	             successor holds the lock simply fails and changes nothing.
//	held=true  → it is the lock holder, by the same primitive that made it the holder at startup.
//
// The stale-in-memory-state objection (ADR-0011 option C) is answered by WHAT is gated rather than
// by refusing to re-acquire: every resumed loop re-reads config and re-queries the DB for its work
// (claims come from send_managed.jobs, reclaim from lease timestamps, the fairness cursor only biases an
// ordering), so nothing that decides a send survives the pause in memory. The one piece of state
// that does — worker registrations on the live mTLS streams — is not stale but AUTHORITATIVE: those
// streams either stayed connected (the workers are genuinely there) or dropped and re-registered.
func (g *Guard) keep() {
	defer close(g.stopped)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-ticker.C:
			if g.held.Load() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				var one int
				err := g.conn.QueryRow(ctx, "SELECT 1").Scan(&one)
				cancel()
				if err != nil {
					// held=false FIRST, then log: the flag is what stops dispatch, and ordering it
					// before the (slower, lock-taking) log call keeps the unsafe window as short as
					// the scheduler allows.
					g.held.Store(false)
					// Stamp the loss HERE — at the moment it is detected — not on the first failed
					// re-acquire attempt. The common case is that the very first attempt SUCCEEDS, so
					// stamping it in logReacquireFailure left notHeldSince at its zero value and the
					// RE-ACQUIRED line reported "after 2562047h47m16s not-held" (time.Since(zero),
					// ~292 years). Observed on the live stack; a duration an operator reads during an
					// incident has to be right, and a nonsense one undermines the whole line.
					g.notHeldSince.Store(time.Now().UnixNano())
					g.reacquireFailures = 0
					g.lastAttempt = time.Time{} // attempt immediately, don't wait out a stale interval
					log.Printf("singleton: lock connection lost (%v); marking not-held — dispatch/reclaim/retention are now PAUSED (no claim, no push, no reclaim, no sweep) and readyz reports not_ready; the outcome path stays UP so an in-flight send can still report. Re-attempting acquisition (ADR-0011 option B)", err)
				}
				continue
			}
			// Not held: try to get it back, at most once per seeded interval.
			g.tryReacquire()
		}
	}
}

// tryReacquire attempts to regain the lock after a mid-life loss. Called only from keep (so the
// connection stays exclusively owned by one goroutine). It re-dials when the old connection is
// unusable, then makes ONE non-blocking pg_try_advisory_lock attempt — never a blocking wait, so a
// successor holding the lock costs this Control nothing but a failed try.
//
// Rate-limited to the seeded brain_singleton_reacquire_interval_sec. A zero/unset interval disables
// re-acquisition entirely (the Control stays paused until restarted), which is the correct degradation:
// paused-and-honest beats resuming on a cadence nobody configured.
func (g *Guard) tryReacquire() {
	every := time.Duration(g.reacquireEvery.Load())
	if every <= 0 {
		return
	}
	if !g.lastAttempt.IsZero() && time.Since(g.lastAttempt) < every {
		return
	}
	g.lastAttempt = time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// The old connection is almost always dead (that is how we noticed). Replace it before trying,
	// otherwise every attempt fails on the corpse. Closing a dead conn is harmless.
	if g.conn != nil {
		var one int
		if err := g.conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			_ = g.conn.Close(ctx)
			g.conn = nil
		}
	}
	if g.conn == nil {
		conn, err := pgx.Connect(ctx, g.dsn)
		if err != nil {
			g.logReacquireFailure("dedicated reconnect failed: " + err.Error())
			return
		}
		g.conn = conn
	}

	var got bool
	if err := g.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", LockKey).Scan(&got); err != nil {
		_ = g.conn.Close(context.Background())
		g.conn = nil
		g.logReacquireFailure("pg_try_advisory_lock failed: " + err.Error())
		return
	}
	if !got {
		// Another Control holds it. This is a NORMAL, safe outcome — we dispatch nothing, so there is
		// no conflict; we simply stay paused and try again next interval.
		g.logReacquireFailure("lock is held by another Control — staying PAUSED (dispatching nothing, which is why this is safe)")
		return
	}
	g.held.Store(true)
	g.reacquireFailures = 0
	// Read the elapsed span BEFORE clearing the stamp, so the line reports the real pause duration.
	paused := g.NotHeldFor().Truncate(time.Second)
	g.notHeldSince.Store(0)
	log.Printf("singleton: lock RE-ACQUIRED (key=%d) after %s not-held — dispatch/reclaim/retention RESUME; every loop re-reads config and re-queries its work from the DB, so nothing that decides a send carried over the pause", LockKey, paused)
}

// logReacquireFailure logs the first failure and then throttles, so a Control that stays paused for
// hours produces a bounded trickle instead of one line per interval. Same shape as the dispatch
// loop's diagnostics (§4.3) and the DB-outage gate's.
func (g *Guard) logReacquireFailure(reason string) {
	g.reacquireFailures++
	// notHeldSince is stamped by keep() when the loss is DETECTED, not here — see the comment there.
	// It is always set by the time this runs, because a failure can only follow a detected loss.
	// 1st, then every 12th attempt (≈ once a minute at the seeded 5s cadence).
	if g.reacquireFailures == 1 || g.reacquireFailures%12 == 0 {
		log.Printf("singleton: re-acquire attempt %d failed (%s); not-held for %s — loops remain PAUSED, no job claimed/pushed/reclaimed, no state invented level=error",
			g.reacquireFailures, reason, g.NotHeldFor().Truncate(time.Second))
	}
}

// NotHeldFor returns how long the lock has been continuously NOT held, or 0 if it is held (or was
// never lost). This is the value the pause/re-acquire log lines report, exposed so a behavioral test
// can assert it rather than trusting a log string: the first version of this code stamped the loss
// only on a FAILED re-acquire attempt, so the common path (first attempt succeeds) printed
// time.Since(zero) — "after 2562047h47m16s not-held", ~292 years — and seven passing tests did not
// notice, because none of them looked at the duration.
func (g *Guard) NotHeldFor() time.Duration {
	if g == nil {
		return 0
	}
	ns := g.notHeldSince.Load()
	if ns == 0 {
		return 0
	}
	return time.Since(time.Unix(0, ns))
}

// Healthy reports whether the lock is still held — read from an atomic flag the keeper
// maintains. It NEVER touches the lock connection, so concurrent readyz calls are safe and
// cannot disturb the lock.
func (g *Guard) Healthy(ctx context.Context) bool {
	return g != nil && g.held.Load()
}

// Release stops the keeper, releases the advisory lock, and closes the connection. On
// crash/SIGKILL this is unnecessary — Postgres auto-releases the session lock.
func (g *Guard) Release(ctx context.Context) {
	if g == nil {
		return
	}
	close(g.stop)
	<-g.stopped // keeper has released exclusive ownership of the connection

	// Unlock ONLY if we actually hold it. After a mid-life loss (ADR-0011) this Guard may be in the
	// not-held state with a successor Control holding the lock; pg_advisory_unlock is session-scoped so
	// it could not release another session's lock even if called, but issuing it would log a spurious
	// "you don't own a lock of type ExclusiveLock" WARNING on shutdown and read as a bug during an
	// incident. The connection is closed either way.
	wasHeld := g.held.Swap(false)
	if g.conn != nil {
		if wasHeld {
			_, _ = g.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", LockKey)
		}
		_ = g.conn.Close(ctx)
		g.conn = nil
	}
}
