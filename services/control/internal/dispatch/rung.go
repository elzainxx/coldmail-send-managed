package dispatch

// rung.go — fix-plan item 12 / classification 13.1, module-comparison-report §7.4,
// canonical-module-contract.md §1.11.1. The GRADUATED dispatch kill-switch ladder.
//
// WHAT WAS WRONG. `dispatch_enabled` is a boolean: dispatch runs, or it stops. During a partial
// incident — one provider degrading, a suspected bad config, a shared pool misbehaving — the only
// lever available halted every tenant, and it halted only ONE of the several things an operator might
// mean by "stop." The audit's words: "every incident response is all-or-nothing for every tenant."
//
// AND `dispatch_enabled=false` DOES LESS THAN ITS NAME SUGGESTS. Verified against this tree before
// writing this: it gates exactly two sites (loop.go's Tick, dispatch.go's TryDispatch), both on the
// CLAIM path. Intake (`POST /v1/run`) still accepts and still creates queued jobs; reclaim, outcome
// ingestion and heartbeats all continue by design (its own log line says so). So the existing `false`
// is already, precisely, an ACCEPT-AND-DEFER rung — it was simply never named as one, and the two
// rungs above it did not exist.
//
// WHY NOT "COPY M1", AND WHY THE VOCABULARY IS SHARED ANYWAY. The fix plan is explicit: "M1's ladder
// declares four rungs and one is inert (item 4). M1 has the design without the wiring; the senders
// have wiring without the design." So what is adopted from M1 is the SPELLING of the rung the modules
// genuinely share (`serving`, `accept_and_defer` — a platform operator reading two modules' config
// should not have to learn two words for one state), and what is emphatically NOT adopted is M1's
// mistake of declaring a rung whose behaviour nothing implements. Every rung below is wired at a named
// site, and every rung differs from its neighbour in at least one DIRECTLY OBSERVABLE way:
//
//   rung               │ POST /v1/run │ claim→push │ reclaim requeue │ outcome ingest
//   ───────────────────┼──────────────┼────────────┼─────────────────┼───────────────
//   serving            │ accept       │ yes        │ yes             │ yes
//   accept_and_defer   │ accept       │ NO         │ yes             │ yes
//   refuse_new         │ 503 paused   │ no         │ yes             │ yes
//   stopped            │ 503 paused   │ no         │ NO              │ yes
//
// Each step down changes exactly one column, which is what makes the ladder graduated rather than
// decorative — and it is what TestDispatchRung_NoRungIsASynonymForItsNeighbour asserts, by driving
// all four rungs through the same fixture and requiring the observable triple to differ. A rung that
// behaved like its neighbour would reproduce the exact defect this item exists to remove, so that is
// a test and not a comment.
//
// THIS LADDER IS BYTE-IDENTICAL TO MODULE 3's, BY MECHANISM. The two modules share no code (separate
// repos, separate Go modules), so this is a deliberate second implementation of the same contract:
// `503 paused` means the same thing on both, which is what lets a platform client hold one switch
// instead of two. Within THIS repo the three package-local copies are held identical by
// TestDispatchRung_EveryCopyOfTheLadderMatchesThisOne; ACROSS repos nothing checks it, and saying so
// is the point — that direction is fix-plan item 14's job and is not silently covered here.
//
// OUTCOME INGESTION IS NEVER GATED, AT ANY RUNG. That column is deliberately constant. An outcome
// report is the record of a mail that has ALREADY LEFT — refusing it would not prevent a send, it
// would destroy the evidence of one, leaving a job stuck in `sending` with a delivered mail nobody
// can account for. CLAUDE.md Rule 5 (never invent an outcome) has a mirror image: never discard a
// real one. `stopped` is the deepest rung available and it still accepts outcomes.
//
// FREEZING RECLAIM AT `stopped` STRANDS NOTHING. Reclaim's predicate is a DB read
// (`state IN ('assigned','sending') AND lease_expires_at < NOW()`), not in-memory state, so a frozen
// sweep loses no information — the expired lease is still expired on the first tick after release,
// and the job is still exactly where it was. It is safe *because* claiming is already stopped at that
// rung: nothing takes a new lease while frozen, so the only in-flight rows belong to a pre-pause
// attempt. (M1's ADR-0015 §3 reasons identically about its own drainer; the agreement is not
// coincidence, it is the same invariant.)

import "log"

// The rung names. Config-table string enum, exactly like autoquarantine_mode's off|shadow|enforce
// (§1.11.1). ADDITIVE-ONLY: a rung is never renamed or repurposed, only appended — and per M1
// ADR-0015's standing obligation, a new rung must arrive WITH its distinct behaviour in the same
// change, which rungSeverity + the no-synonym test together enforce.
const (
	RungServing        = "serving"          // rung 0 — normal operation
	RungAcceptAndDefer = "accept_and_defer" // rung 1 — intake accepted, nothing claimed or pushed
	RungRefuseNew      = "refuse_new"       // rung 2 — intake refused; in-flight work still drains
	RungStopped        = "stopped"          // rung 3 — as rung 2, and the reclaim sweep is frozen too
)

// rungSeverity orders the ladder. This is the ONE place the ordering lives: the composition rule
// below, the predicates, and the tests all read it, so a rung cannot be added to the enum without
// being placed on the ladder (a rung absent from this map is unrecognized and fails safe).
var rungSeverity = map[string]int{
	RungServing:        0,
	RungAcceptAndDefer: 1,
	RungRefuseNew:      2,
	RungStopped:        3,
}

// rungFallback is where an unrecognized dispatch_mode lands. THIS IS A DECISION, not a default.
//
// The two obvious candidates are both wrong. Falling back to `serving` means a garbage value keeps
// sending mail under a policy nobody can read — and the operator who typed it believed they had
// stopped something. Falling back to `stopped` means one typo freezes every tenant's mail AND the
// reclaim sweep, turning a config slip into an outage; "fail safe" cannot mean "fail into the most
// destructive state available."
//
// So it lands on the FIRST degraded rung: no mail goes out under an unreadable policy, intake keeps
// accepting so no caller is refused and nothing is lost, and the state is resumable with zero loss
// the instant the value is corrected. It is strictly safer than serving and strictly less destructive
// than stopped, which is what "fail toward the safer rung" (§1.11.4) actually asks for.
const rungFallback = RungAcceptAndDefer

// resolveRung turns the raw dispatch_mode string and the legacy dispatch_enabled boolean into the
// ONE effective rung every gate consults.
//
// WHY TWO DIALS COMPOSE INSTEAD OF ONE REPLACING THE OTHER. `dispatch_enabled` is not dropped:
// module 3's ADR-0016 two-condition test permits removing a seeded row only when the key never had a
// genuine module consumer, and this one has several (both claim gates, the admin health snapshot's
// `dispatch_paused`, the campaign report's `dispatch_paused`, and the disaster-recovery procedures
// that instruct an operator to set it). That case "still needs its own ruling" — so this change does
// not take one.
//
// But two independent levers on one control is precisely how half a rung stays pulled, which M1's
// ADR-0015 §1 names as the shape to avoid ("separate 'stop the drainer' and 'front-door mode' levers
// is exactly how one half stays running"). The resolution is that they are NOT independent: the
// effective rung is the MORE SEVERE of the two, monotonically. `dispatch_enabled=false` is a
// SEVERITY FLOOR of accept_and_defer — which is exactly what it already did — so:
//
//   - every existing runbook, DR step and test that sets dispatch_enabled=false stays true to the
//     letter: claims stop, jobs stay queued, nothing is lost, reclaim/outcome/heartbeat continue;
//   - no combination can leave a pulled rung half-effective, because max() has no such state. An
//     operator who pulls either lever gets at least what that lever promises, always.
//
// Note what the floor does NOT do: it never LOWERS a rung. dispatch_enabled=true with
// dispatch_mode=stopped is stopped. Jamming the boolean true cannot un-pull a deeper rung, which is
// the failure a naive `enabled && mode` conjunction would have introduced.
//
// TestDispatchRung_EnabledFalseIsAFloorNotASecondLever drives the full 4x2 matrix rather than a
// sample, because the failure being excluded is a single unlucky pair.
func resolveRung(mode string, dispatchEnabled bool) string {
	if _, ok := rungSeverity[mode]; !ok {
		// level=error, not warn: unlike autoquarantine_mode's degrade-to-shadow (a mutator declining
		// to act), this one HALTS OUTBOUND MAIL, and an operator whose sends stopped because of a
		// typo needs to find that in the one `grep 'level=error'` the runbook's triage step uses.
		// The admin API validator rejects a bad value at write time, so reaching this line means the
		// row was written around that surface — worth an error either way.
		log.Printf("dispatch: dispatch_mode=%q unrecognized — falling back to %q (no mail is sent under an unreadable policy; intake keeps accepting, nothing is lost) level=error", mode, rungFallback)
		mode = rungFallback
	}
	if !dispatchEnabled && rungSeverity[mode] < rungSeverity[RungAcceptAndDefer] {
		return RungAcceptAndDefer
	}
	return mode
}

// EffectiveRung is the rung this Config resolves to — the single value every gate reads. Derived on
// every call from the freshly-loaded config, never cached: a stored or memoized rung would keep
// reporting a pause after it was released, which is the inert-control bug in the other direction
// (M1 ADR-0015 §4.2 makes the same choice about its own disclosure flag).
func (c Config) EffectiveRung() string {
	return resolveRung(c.DispatchMode, c.DispatchEnabled)
}

// ClaimsAllowed reports whether this rung may claim a queued job and push it to a worker. False at
// every rung except serving — this is the behaviour `dispatch_enabled=false` already had, now with
// three rungs sharing it and differing elsewhere.
func (c Config) ClaimsAllowed() bool {
	return c.EffectiveRung() == RungServing
}

// IntakeAllowed reports whether POST /v1/run may accept new work. False from refuse_new down: the
// platform is told 503 `paused` and retries, rather than accumulating a backlog this module has been
// told not to work through.
//
// /v1/quote is deliberately NOT gated by this. A quote creates no job and sends no mail; it answers
// "could this be sent." During a pause that question has a real answer and refusing it would make a
// paused module look broken to a caller that was only asking. The intake refusal is about WORK, not
// about reads.
func (c Config) IntakeAllowed() bool {
	return rungSeverity[c.EffectiveRung()] < rungSeverity[RungRefuseNew]
}

// ReclaimAllowed reports whether the reclaim sweep may requeue in-flight work. False only at
// stopped. This is the column that makes stopped observably distinct from refuse_new: with a dead
// worker holding an expired lease, refuse_new returns that job to `queued`, stopped leaves it
// `sending` until the rung is released.
func (c Config) ReclaimAllowed() bool {
	return rungSeverity[c.EffectiveRung()] < rungSeverity[RungStopped]
}
