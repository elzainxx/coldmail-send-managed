// dbhealth.go — Phase 7 §2: the Supabase-down gate ("the invent-nothing chunk", P7-3).
//
// THE GUARANTEE. When Postgres is unreachable the Control marks NOTHING sent/failed/bounced/queued.
// That guarantee is STRUCTURAL, not policy: every terminal/reassignment write in this module is a
// SQL statement (outcome.go's applyAccepted/applyBounced/applyClassifiedFailure, reclaim.go's
// reclaimJobLocked, loop.go's claim), so a DB that cannot be reached cannot be written to — there
// is no in-memory shadow ledger that could later be flushed as an invented outcome, and no code
// path derives a job's fate from anything but a committed worker report or the §2.4 reclaim
// transition. This file adds the two things that structural property does NOT give us for free:
//
//  1. PAUSE, so the loops stop *attempting* work (and stop hot-spinning against a dead socket)
//     rather than failing a full tick's worth of queries every interval. §2.2: the dispatch loop,
//     the reclaim loop, and (Chunk 4) the retention sweep all pause.
//  2. NO CRASH-LOOP. The Control catches the connectivity failure, logs it, and STAYS ALIVE on
//     bounded exponential backoff — it never exit-restarts on a failed query (§2.2), which would
//     thrash and re-trigger the §1 startup rescan on every restart.
//
// RESUME (§2.4) needs no reconciliation logic at all, and that is the point: because the Control
// invented nothing during the outage, there is no in-memory/DB divergence. Jobs are exactly where
// the DB left them (queued/assigned/sending); a successful health ping clears the halt and the
// loops resume against that stored state. Workers replay their retained outcomes (P7-4) and the
// outcome handler commits them idempotently and only then acks. Only genuinely-stuck jobs — whose
// worker never reports — reclaim via the normal §2.4 timeout path afterwards.
//
// DETECTION (§2.1) distinguishes TRANSPORT unavailability from an application error: a SQLSTATE
// the server itself reported means we reached the server, so it is application logic to be handled
// inline (a CHECK/UNIQUE violation must NOT halt the loops). See IsOutage.
package dispatch

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SAFETY BOUNDS (the Docs/history/open-decisions.md Phase-4-closeout class — infra liveness cadence, not
// operator-facing send behavior; the same category as singleton.go's 200ms lock poll / 5s keeper
// ticker and runner.go's defaultTickIntervalOnLoadErr). Deliberately NOT config keys: Phase 7 §5
// seeds exactly six, and an outage-retry cadence is a guard rail on a dead socket, not a dial a
// campaign or operator would ever tune. They bound how fast a halted Control re-probes a down DB.
const (
	outageBackoffBase = 500 * time.Millisecond // first re-probe delay after the halt trips
	outageBackoffMax  = 30 * time.Second       // cap — a long outage re-probes twice a minute, forever, alive
	outagePingTimeout = 2 * time.Second        // bound on the SELECT 1 liveness probe itself
)

// DBHealth is the shared outage gate consulted by every Control loop. One instance per Dispatcher;
// safe for concurrent use by the dispatch loop, the reclaim loop, and the retention sweep.
type DBHealth struct {
	db *pgxpool.Pool

	mu          sync.Mutex
	haltedSince time.Time // zero value => healthy
	failures    int       // consecutive outage observations (drives the backoff)
}

// NewDBHealth builds the gate over the Control's pool. It starts in the healthy state: the pool was
// already proven reachable at boot (db.Connect pings), and a false "healthy" is self-correcting —
// the first failed query trips the halt.
func NewDBHealth(db *pgxpool.Pool) *DBHealth { return &DBHealth{db: db} }

// Ready is the gate every loop tick calls FIRST. It returns (true, 0) when the DB is believed
// reachable — the caller proceeds normally. When halted it re-probes with ONE cheap `SELECT 1`:
//   - probe succeeds → the outage is over: clear the halt, log the resume, return (true, 0) so this
//     very tick runs against stored state (§2.4 step 1);
//   - probe fails → return (false, backoff) — the caller does NO work this tick (no claim, no push,
//     no reclaim, no sweep, no write of any kind) and waits out the bounded backoff.
//
// A halted tick therefore costs exactly one trivial probe instead of a whole tick's failing
// queries, which is what turns "the loops error every interval" into "the loops are paused".
func (h *DBHealth) Ready(ctx context.Context) (bool, time.Duration) {
	h.mu.Lock()
	halted := !h.haltedSince.IsZero()
	h.mu.Unlock()
	if !halted {
		return true, 0
	}

	pingCtx, cancel := context.WithTimeout(ctx, outagePingTimeout)
	defer cancel()
	var one int
	err := h.db.QueryRow(pingCtx, `SELECT 1`).Scan(&one)
	if err == nil {
		h.clear()
		return true, 0
	}
	// Still down (or down again). Count it and hold the halt.
	h.mu.Lock()
	h.failures++
	wait := backoffFor(h.failures)
	down := time.Since(h.haltedSince).Truncate(time.Second)
	h.mu.Unlock()
	log.Printf("db outage: still unreachable after %s (%d consecutive probes): %v — loops remain PAUSED, no state invented; re-probing in %s",
		down, h.failures, err, wait)
	return false, wait
}

// Note records the outcome of a real tick's DB work and returns whether it was an OUTAGE (as
// opposed to nil or an application error). nil clears any halt (a successful tick is the strongest
// possible liveness proof). An outage error trips/holds the halt. An application error (a real
// SQLSTATE from a reachable server) leaves the gate alone — it is handled inline by the caller and
// must never pause the loops.
func (h *DBHealth) Note(err error) (outage bool) {
	if err == nil {
		h.clear()
		return false
	}
	if !IsOutage(err) {
		return false
	}
	h.mu.Lock()
	first := h.haltedSince.IsZero()
	if first {
		h.haltedSince = time.Now()
	}
	h.failures++
	wait := backoffFor(h.failures)
	h.mu.Unlock()
	if first {
		// Internal detail goes to the log only (two-vocabulary, Rule 5) — never to a client.
		log.Printf("db outage DETECTED: %v — HALTING dispatch/reclaim/retention loops cleanly. No job will be marked sent/failed/bounced/queued while the DB is unreachable (§2.2); in-flight jobs stay exactly as stored. Re-probing in %s", err, wait)
	}
	return true
}

// RetryIn is the wait a caller should use after Note reported an outage (the same bounded backoff
// Ready hands out). Returns 0 when healthy, so a caller can fall back to its normal interval.
func (h *DBHealth) RetryIn() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.haltedSince.IsZero() {
		return 0
	}
	return backoffFor(h.failures)
}

// Halted reports whether the gate currently believes the DB is unreachable (loops paused).
func (h *DBHealth) Halted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.haltedSince.IsZero()
}

// clear ends a halt (or is a no-op when already healthy), logging the resume exactly once.
func (h *DBHealth) clear() {
	h.mu.Lock()
	if h.haltedSince.IsZero() {
		h.mu.Unlock()
		return
	}
	down := time.Since(h.haltedSince).Truncate(time.Second)
	h.haltedSince = time.Time{}
	h.failures = 0
	h.mu.Unlock()
	log.Printf("db outage RESOLVED after %s — RESUMING loops from stored state; workers replay any buffered outcomes and are acked only after commit (§2.4, no double-processing)", down)
}

// backoffFor is the bounded exponential ladder: base, 2×, 4× … capped at outageBackoffMax. n is the
// consecutive-failure count (1-based); anything ≤1 gets the base.
func backoffFor(n int) time.Duration {
	if n <= 1 {
		return outageBackoffBase
	}
	d := outageBackoffBase
	for i := 1; i < n; i++ {
		d *= 2
		if d >= outageBackoffMax {
			return outageBackoffMax
		}
	}
	return d
}

// IsOutage classifies an error as TRANSPORT unavailability (the DB cannot be reached ⇒ halt) rather
// than application logic (§2.1). The dividing line is whether the SERVER answered:
//
//   - A *pgconn.PgError means we reached Postgres and it reported a condition — a CHECK (23514), a
//     UNIQUE (23505), a deadlock (40P01), an FK (23503). Those are handled inline by the callers and
//     must NOT pause the loops. The exceptions are the SQLSTATE classes that mean the connection
//     itself is going away: class 08 (connection exception), 57P01/02/03 (admin/crash shutdown,
//     cannot connect now — exactly what a Supabase restart looks like), 53300/53400 (out of
//     connection slots).
//   - context.Canceled is shutdown, not an outage (Chunk 5 cancels the loops deliberately).
//   - pgx.ErrNoRows is a normal empty read.
//   - Anything else — dial failure, closed pool, acquire timeout, DeadlineExceeded on a blocked
//     acquire, a broken pipe mid-query — is transport unavailability. Defaulting UNKNOWN errors to
//     "outage" is the safe direction: pausing is always harmless (state stays as stored), whereas
//     continuing to hammer a dead socket is not.
func IsOutage(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	// A CONFIG FAULT is not transport unavailability: the DB answered perfectly — we read the set and
	// refused to act on a value we cannot use (Rule 1: reject, never clamp). It must NOT be classified
	// as an outage. If it were, every tick would (a) log a false "db outage DETECTED" naming a DB that
	// is healthy, and (b) have that halt immediately cleared by the next tick's successful SELECT 1
	// probe — which RESETS the failure counter, so the bounded exponential backoff §2.2 requires could
	// never grow and all three loops would flap at the 500ms base interval indefinitely, burying the one
	// log line an operator needs (the offending key) under noise about an outage that is not happening.
	// Returning false routes it to the caller's application-error branch instead: log and retry on the
	// normal cadence, with the loops still correctly refusing to act (LoadConfig returns a ZEROED Config
	// alongside the error, so nothing runs on bad values).
	//
	// This tests the configFault MARKER, not a list of concrete types. It checked *configOrderingError
	// alone until 2026-07-28, which covered the ordering invariants but NOT a wrong-TYPE value — so a
	// `{"v":"thirty"}` on an integer key (a direct SQL write or a bad migration) produced exactly the
	// flap described above. Classifying by marker means the next config fault someone adds is exempt by
	// construction rather than by remembering to edit this function. See config.go's configFault.
	var cfgFault configFault
	if errors.As(err, &cfgFault) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case strings.HasPrefix(pgErr.Code, "08"): // connection_exception family
			return true
		case pgErr.Code == "57P01", pgErr.Code == "57P02", pgErr.Code == "57P03":
			return true // admin_shutdown | crash_shutdown | cannot_connect_now
		case pgErr.Code == "53300", pgErr.Code == "53400":
			return true // too_many_connections | configuration_limit_exceeded
		}
		return false // a reachable server's own error: application logic, handled inline
	}
	return true
}
