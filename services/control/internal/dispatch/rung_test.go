package dispatch

// rung_test.go — fix-plan item 12. The graduated dispatch kill-switch ladder.
//
// WHAT THESE TESTS ARE FOR, AND WHY THE OBVIOUS TEST IS THE WEAK ONE.
//
// The defect item 12 exists to remove is that a control with one setting cannot express a partial
// incident. The defect item 12 could EASILY INTRODUCE is the one M1 shipped and the fix plan warns
// about in its own words: M1's ladder declares four rungs and one of them is inert — the design
// without the wiring. A rung that loads, validates, prints itself in a health snapshot and then
// behaves exactly like its neighbour is worse than a rung that does not exist, because every surface
// reports it as real.
//
// So the load-bearing test here is NOT "does accept_and_defer stop claiming." It is
// TestDispatchRung_NoRungIsASynonymForItsNeighbour, which drives all four rungs through the same
// fixture and requires the observable triple (claim / intake / reclaim) to be DISTINCT for each. That
// test fails the day a rung is added or gated without behaviour, which is exactly how M1's inert rung
// got in.
//
// Every test that asserts something DOESN'T happen carries an anti-vacuity control, because "nothing
// happened" is both the expected observation at a pause and what a broken fixture produces — which is
// how an inert rung hides in the first place.
//
// THE PRE-0025 SUITE IS NOT REPLACED. panicswitch_test.go still drives dispatch_enabled directly and
// still asserts the same halt/zero-loss/reclaim-continues properties it always did. Those tests are
// the compatibility guarantee this change makes in writing, so they were left exactly as they were
// rather than rewritten in the new vocabulary — a ported test proves the port, not the promise.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// setRung sets dispatch_mode in the live config table — the same row the admin PUT writes.
func (e *dispatchEnv) setRung(t *testing.T, rung string) {
	t.Helper()
	e.setConfigStr(t, "dispatch_mode", rung)
}

// rungBehaviour is what a rung actually DOES, observed through production code paths only.
type rungBehaviour struct {
	claimed bool // did one Tick claim a queued job and push it to a worker?
	intake  bool // may POST /v1/run accept new work? (the predicate the handler consults)
	reclaim bool // did one ReclaimTick requeue a job whose lease had expired?
}

// observeRung measures one rung on its OWN database clone.
//
// A fresh clone per rung is not fastidiousness, it is correctness: jobs claimed at an earlier rung stay
// in flight forever (no worker reports in this fixture), and in-flight jobs consume the per-user lane
// ceiling. Sharing one clone across four rungs would eventually make a HEALTHY rung claim nothing for
// want of a lane, and the test would then "prove" the ladder works while actually observing lane
// exhaustion — a vacuous pass of exactly the kind this pass keeps finding.
//
// Staging order matters too. The in-flight job whose lease the reclaim leg expires is staged at
// `serving`, through the real claim path, BEFORE the rung under test is applied. A hand-forged
// `sending` row would not prove the sweep's own predicate is what gates, and staging it after the rung
// was applied would leave nothing in flight at any paused rung — so the reclaim leg would read false
// everywhere and three rungs would look identical for the wrong reason.
func observeRung(t *testing.T, rung string) rungBehaviour {
	t.Helper()
	e := newDispatchEnv(t, "rung_"+strings.ReplaceAll(rung, "_", ""))
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.seedAccount(t, pool, "sec2")
	e.addWorker(t) // a second real worker row, so the two jobs below need not share one

	ctx := context.Background()

	// --- stage the reclaim subject at `serving`, through production code ---
	leaseJob := e.seedQueuedJob(t, "lease@example.com")
	if _, pushed, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("rung %s: staging tick: %v", rung, err)
	} else if pushed != 1 {
		t.Fatalf("rung %s: staging tick pushed %d job(s), want 1 — the fixture is not dispatching, so nothing below would mean anything", rung, pushed)
	}
	if st, _, _, _, _ := e.jobState(t, leaseJob); st == "queued" {
		t.Fatalf("rung %s: staging tick left the lease fixture queued — the reclaim leg would be vacuous", rung)
	}
	e.expireLease(t, leaseJob)

	// --- apply the rung under test ---
	e.setRung(t, rung)
	cfg := e.mustLoadConfig(t)
	if cfg.EffectiveRung() != rung {
		t.Fatalf("rung %s: EffectiveRung()=%q — the fixture is not testing what it claims", rung, cfg.EffectiveRung())
	}

	// (a) CLAIM: a fresh queued job, one real tick.
	e.px.freeAllWorkers()
	claimJob := e.seedQueuedJob(t, "claim@example.com")
	before := e.px.pushCount()
	if _, _, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("rung %s: tick: %v", rung, err)
	}
	claimed := e.px.pushCount() > before
	state, _, _, _, _ := e.jobState(t, claimJob)
	// Cross-check the push against the DB rather than trusting the pusher alone. A push without a state
	// transition (or the reverse) is a different bug, and conflating the two would let it pass.
	if claimed != (state != "queued") {
		t.Fatalf("rung %s: pusher says claimed=%t but the job is %q — push and state transition disagree", rung, claimed, state)
	}

	// (b) INTAKE: the predicate POST /v1/run consults.
	intake := cfg.IntakeAllowed()

	// (c) RECLAIM: one real ReclaimTick over the lease staged above.
	if _, _, err := e.d.ReclaimTick(ctx); err != nil {
		t.Fatalf("rung %s: reclaim tick: %v", rung, err)
	}
	lst, _, _, _, _ := e.jobState(t, leaseJob)
	reclaim := lst == "queued"

	b := rungBehaviour{claimed: claimed, intake: intake, reclaim: reclaim}
	t.Logf("rung=%-16s claimed=%-5t intake=%-5t reclaim=%t", rung, b.claimed, b.intake, b.reclaim)
	return b
}

// TestDispatchRung_NoRungIsASynonymForItsNeighbour is the load-bearing test of item 12.
//
// It asserts two things. First, that each rung does what it declares. Second — and this is the part
// that is a MECHANISM rather than a restatement — that no two rungs are behaviourally identical. The
// second assertion is what fails if a rung is declared, validated, seeded and reported while doing
// nothing, which is the defect this item exists to remove and the one it was most likely to reproduce.
func TestDispatchRung_NoRungIsASynonymForItsNeighbour(t *testing.T) {
	ladder := []string{RungServing, RungAcceptAndDefer, RungRefuseNew, RungStopped}

	got := map[string]rungBehaviour{}
	for _, rung := range ladder {
		got[rung] = observeRung(t, rung)
	}

	// The asserted ladder, written out literally rather than derived from rungSeverity — a change to the
	// ordering must not be able to re-derive the expectation to match itself.
	want := map[string]rungBehaviour{
		RungServing:        {claimed: true, intake: true, reclaim: true},
		RungAcceptAndDefer: {claimed: false, intake: true, reclaim: true},
		RungRefuseNew:      {claimed: false, intake: false, reclaim: true},
		RungStopped:        {claimed: false, intake: false, reclaim: false},
	}
	for _, rung := range ladder {
		if got[rung] != want[rung] {
			t.Errorf("rung %q behaved as %+v, want %+v — the ladder does not do what it declares", rung, got[rung], want[rung])
		}
	}

	// THE ANTI-INERT ASSERTION. Every pair must differ.
	for i := 0; i < len(ladder); i++ {
		for j := i + 1; j < len(ladder); j++ {
			if got[ladder[i]] == got[ladder[j]] {
				t.Errorf("rungs %q and %q are INDISTINGUISHABLE (both %+v) — a rung with no distinct behaviour is the exact defect item 12 forbids reproducing (M1's inert accept_and_defer)",
					ladder[i], ladder[j], got[ladder[i]])
			}
		}
	}
}

// TestDispatchRung_StoppedFreezesTheSweepWithoutStrandingAnything is the `stopped` rung's own
// guarantee. Freezing a sweep is only defensible if the work is still there afterwards, so this asserts
// the frozen job is untouched (same state, same attempt count, no reclaim provenance) AND that
// releasing the rung reclaims it on the very next tick.
//
// Three frozen ticks, not one: a first-tick-only latch would pass a single-tick test.
func TestDispatchRung_StoppedFreezesTheSweepWithoutStrandingAnything(t *testing.T) {
	e := newDispatchEnv(t, "rung_stopped_freeze")
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	ctx := context.Background()

	jobID := e.seedQueuedJob(t, "frozen@example.com")
	if _, pushed, err := e.d.Tick(ctx); err != nil || pushed != 1 {
		t.Fatalf("staging tick pushed=%d err=%v, want 1/nil", pushed, err)
	}
	stateBefore, _, _, _, attemptBefore := e.jobState(t, jobID)
	if stateBefore == "queued" {
		t.Fatalf("staging tick did not claim the job — this test would assert nothing")
	}
	e.expireLease(t, jobID)

	e.setRung(t, RungStopped)
	for i := 0; i < 3; i++ {
		_, n, err := e.d.ReclaimTick(ctx)
		if err != nil {
			t.Fatalf("frozen reclaim tick %d: %v", i, err)
		}
		if n != 0 {
			t.Fatalf("frozen reclaim tick %d reclaimed %d job(s), want 0", i, n)
		}
	}

	// P1: the job is exactly where it was — not requeued, not failed, not mutated.
	stateAfter, _, _, _, attemptAfter := e.jobState(t, jobID)
	if stateAfter != stateBefore {
		t.Errorf("frozen sweep changed job state %q -> %q; a frozen sweep must write NOTHING", stateBefore, stateAfter)
	}
	if attemptAfter != attemptBefore {
		t.Errorf("frozen sweep changed attempt_count %d -> %d", attemptBefore, attemptAfter)
	}
	// P2: no reclaim provenance (the sweep did not half-run).
	if ev := e.reclaimEvents(t, jobID); len(ev) != 0 {
		t.Errorf("frozen sweep wrote %d reclaim_loop job_events row(s): %v — it must write nothing at all", len(ev), ev)
	}

	// P3 / ANTI-VACUITY: release the rung and the SAME expired lease is reclaimed immediately. This is
	// what proves the freeze was a freeze and not a loss — and it is the control that fails if the
	// fixture never had a reclaimable job, in which case the zeros above would mean nothing.
	e.setRung(t, RungRefuseNew)
	_, n, err := e.d.ReclaimTick(ctx)
	if err != nil {
		t.Fatalf("released reclaim tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("after releasing `stopped`, reclaim recovered %d job(s), want 1 — either the freeze lost the job or the fixture was never reclaimable", n)
	}
	if st, _, leaseNull, _, _ := e.jobState(t, jobID); st != "queued" || !leaseNull {
		t.Errorf("after release the job is %q leaseNull=%t, want queued/true", st, leaseNull)
	}
}

// TestDispatchRung_EnabledFalseIsAFloorNotASecondLever drives the FULL 4x2 matrix of
// (dispatch_mode, dispatch_enabled).
//
// Why the whole matrix and not a sample: the failure being excluded is one unlucky pair in which a
// pulled lever is silently ineffective, and a sampled test is exactly how such a pair survives. The
// property is monotone — the effective rung is never LESS severe than either dial alone, and never more
// severe than both together — and every cell is checked against it.
//
// This also pins the backwards-compatibility promise send_managed_0025 makes in writing: every existing
// runbook, DR procedure and test that sets dispatch_enabled=false must still get a state in which
// nothing is claimed. Each `false` cell asserts that as behaviour, not as severity arithmetic.
func TestDispatchRung_EnabledFalseIsAFloorNotASecondLever(t *testing.T) {
	for _, mode := range []string{RungServing, RungAcceptAndDefer, RungRefuseNew, RungStopped} {
		for _, enabled := range []bool{true, false} {
			eff := resolveRung(mode, enabled)
			cfg := Config{DispatchMode: mode, DispatchEnabled: enabled}

			if rungSeverity[eff] < rungSeverity[mode] {
				t.Errorf("mode=%q enabled=%t resolved to %q, LESS severe than the mode — a pulled rung must never be softened by the other dial", mode, enabled, eff)
			}
			if !enabled && rungSeverity[eff] < rungSeverity[RungAcceptAndDefer] {
				t.Errorf("mode=%q enabled=false resolved to %q — must be at least %q (the promise every existing runbook depends on)", mode, eff, RungAcceptAndDefer)
			}
			if !enabled && cfg.ClaimsAllowed() {
				t.Errorf("mode=%q enabled=false still allows claims — dispatch_enabled=false has stopped working, which silently breaks three disaster-recovery procedures", mode)
			}
			// The boolean must not ADD severity beyond its floor: it may not turn refuse_new into stopped,
			// or neither dial could be reasoned about on its own.
			if want := maxSeverity(rungSeverity[mode], rungSeverity[RungAcceptAndDefer]); !enabled && rungSeverity[eff] > want {
				t.Errorf("mode=%q enabled=false resolved to %q — MORE severe than either dial alone; the composition must be a maximum, not an escalation", mode, eff)
			}
			// Whatever the dials are, the resolved value must BE a rung — never a raw dial value or "".
			if _, ok := rungSeverity[eff]; !ok {
				t.Errorf("mode=%q enabled=%t resolved to %q, which is not a rung at all", mode, enabled, eff)
			}
		}
	}

	// ANTI-VACUITY: the matrix above is inequalities, which a degenerate resolveRung returning one
	// constant severe rung would satisfy for the `false` half. Pin the cells that must NOT move.
	if got := resolveRung(RungServing, true); got != RungServing {
		t.Errorf("resolveRung(serving,true)=%q, want serving — the composition is over-severe and would pause a healthy system", got)
	}
	if got := resolveRung(RungStopped, true); got != RungStopped {
		t.Errorf("resolveRung(stopped,true)=%q, want stopped — dispatch_enabled=true must not soften a pulled rung", got)
	}
	// And the boolean floor must be REACHABLE from the gentlest mode, or "floor" is a word with no
	// behaviour behind it.
	if got := resolveRung(RungServing, false); got != RungAcceptAndDefer {
		t.Errorf("resolveRung(serving,false)=%q, want %q — the legacy boolean no longer pauses anything", got, RungAcceptAndDefer)
	}
}

func maxSeverity(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestDispatchRung_EnabledFalseStillStopsClaimsThroughTheRealLoop is the behavioural half of the
// compatibility promise above. The 4x2 matrix reasons about resolveRung; what three DR procedures
// actually depend on is that writing dispatch_enabled=false to the config table stops the live loop.
// Proven through a real Tick against real Postgres, with dispatch_mode left at its seeded `serving`.
func TestDispatchRung_EnabledFalseStillStopsClaimsThroughTheRealLoop(t *testing.T) {
	e := newDispatchEnv(t, "rung_legacybool")
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	ctx := context.Background()

	e.setConfigBool(t, "dispatch_enabled", false)
	if cfg := e.mustLoadConfig(t); cfg.DispatchMode != RungServing {
		t.Fatalf("precondition: dispatch_mode=%q, want the seeded %q — this test is about the BOOLEAN acting alone", cfg.DispatchMode, RungServing)
	}

	job := e.seedQueuedJob(t, "legacy@example.com")
	if _, pushed, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	} else if pushed != 0 {
		t.Fatalf("dispatch_enabled=false still pushed %d job(s) — the legacy kill switch is broken", pushed)
	}
	if st, _, _, _, _ := e.jobState(t, job); st != "queued" {
		t.Errorf("job is %q, want queued", st)
	}

	// ANTI-VACUITY: flip the boolean back and the same job drains, so the zero above was the switch and
	// not an undispatchable fixture.
	e.setConfigBool(t, "dispatch_enabled", true)
	if _, pushed, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("release tick: %v", err)
	} else if pushed != 1 {
		t.Fatalf("after re-enabling, the tick pushed %d job(s), want 1 — the fixture was never dispatchable", pushed)
	}
}

// TestDispatchRung_UnrecognizedModeFailsTowardTheSaferRungAndNotThePunitiveOne pins the fail-safe
// DIRECTION, a decision with a wrong answer on each side of it.
//
// The fallback must be accept_and_defer specifically: not serving (which would keep sending mail under
// a policy nobody can read, while the operator believed they had stopped something) and not stopped
// (which would let one typo freeze every tenant's mail AND the reclaim sweep — "fail safe" cannot mean
// "fail into the most destructive available state").
func TestDispatchRung_UnrecognizedModeFailsTowardTheSaferRungAndNotThePunitiveOne(t *testing.T) {
	for _, bad := range []string{"", " ", "srving", "accept-and-defer", "SERVING", "off", "shadow", "enforce", "true", "stopped ", "paused"} {
		cfg := Config{DispatchMode: bad, DispatchEnabled: true}
		eff := cfg.EffectiveRung()
		if eff != RungAcceptAndDefer {
			t.Errorf("dispatch_mode=%q resolved to %q, want %q", bad, eff, RungAcceptAndDefer)
		}
		if cfg.ClaimsAllowed() {
			t.Errorf("dispatch_mode=%q still allows claims — a value nobody can read must not keep sending mail", bad)
		}
		// The other half of the decision, and the one a reflexive "fail closed" gets wrong: intake keeps
		// accepting and the sweep keeps running, so a typo costs nothing that cannot be undone.
		if !cfg.IntakeAllowed() {
			t.Errorf("dispatch_mode=%q refuses intake — a typo must not refuse callers' work; accept_and_defer accepts, which is why it is the fallback", bad)
		}
		if !cfg.ReclaimAllowed() {
			t.Errorf("dispatch_mode=%q freezes the reclaim sweep — a typo must not be more destructive than the deepest rung an operator can deliberately choose", bad)
		}
	}

	// `stopped ` (trailing space) and `SERVING` are in that list for a reason: they are the near misses.
	// Neither may be "helpfully" normalised into the rung it resembles — an operator who believes they
	// typed a rung and silently got a different one is back in M1's looks-normal-while-ignored failure,
	// this time with their own typo as the cause.
	if got := resolveRung("stopped ", true); got == RungStopped {
		t.Error("a trailing-space `stopped ` was normalised to the stopped rung — a near miss must fall back, not be guessed at")
	}
}

// TestDispatchRung_UnrecognizedModeIsObeyedByTheRealLoopToo. The fallback above is a unit property, and
// a unit property about a value nobody validates is exactly the shape that passes while the live path
// does something else. So: write garbage into the config table and drive the real loop.
//
// This also proves the loader does not REJECT the row. dispatch_mode is read raw and unvalidated on
// purpose (config.go), so that resolveRung is the single place a value is dispositioned; had LoadConfig
// hard-errored instead, one typo would take the Control down and every rung would be moot.
func TestDispatchRung_UnrecognizedModeIsObeyedByTheRealLoopToo(t *testing.T) {
	e := newDispatchEnv(t, "rung_garbage")
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	ctx := context.Background()

	// Written straight to the table. The admin PUT validator would refuse this value, which is its job;
	// this test covers the value arriving by any other route — a hand-run SQL fix, a restore, a bad seed.
	e.setRung(t, "srving")
	cfg, err := LoadConfig(ctx, e.pool)
	if err != nil {
		t.Fatalf("LoadConfig hard-failed on an unrecognized dispatch_mode: %v — one typo must not take the Control down; the value is dispositioned by resolveRung, not by the loader", err)
	}
	if cfg.EffectiveRung() != RungAcceptAndDefer {
		t.Fatalf("live config resolved %q to %q, want %q", cfg.DispatchMode, cfg.EffectiveRung(), RungAcceptAndDefer)
	}

	job := e.seedQueuedJob(t, "garbage@example.com")
	if _, pushed, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	} else if pushed != 0 {
		t.Errorf("an unreadable dispatch_mode still pushed %d job(s) — mail is going out under a policy nobody can read", pushed)
	}
	if st, _, _, _, _ := e.jobState(t, job); st != "queued" {
		t.Errorf("job is %q, want queued (untouched)", st)
	}

	// ANTI-VACUITY: a good value drains the same job.
	e.setRung(t, RungServing)
	if _, pushed, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("release tick: %v", err)
	} else if pushed != 1 {
		t.Fatalf("after correcting the value the tick pushed %d job(s), want 1 — the fixture was never dispatchable", pushed)
	}
}

// TestDispatchRung_OutcomeIngestionIsNeverGatedAtAnyRung. The one column of the ladder that is
// deliberately CONSTANT, asserted as a property rather than left as a comment.
//
// An outcome report is the record of a mail that has already left. Gating it would not prevent a send —
// it would destroy the evidence of one, leaving a job stuck in `sending` and a delivered mail nobody
// can account for. CLAUDE.md Rule 5 forbids inventing an outcome; discarding a real one is the same
// wound in the other direction.
//
// SOURCE-LEVEL, because the property is "no rung gate was ever added here", which no amount of
// black-box probing can establish: a behavioural test passes on the day someone adds a gate for a rung
// the test does not enumerate.
func TestDispatchRung_OutcomeIngestionIsNeverGatedAtAnyRung(t *testing.T) {
	raw, err := os.ReadFile("outcome.go")
	if err != nil {
		t.Fatalf("read outcome.go: %v", err)
	}
	src := string(raw)

	for _, forbidden := range []string{"ClaimsAllowed", "IntakeAllowed", "ReclaimAllowed", "EffectiveRung", "DispatchMode", "DispatchEnabled"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("outcome.go references %q — outcome ingestion must NOT be gated on any rung. An outcome is the record of a mail that already left; refusing it destroys evidence rather than preventing a send. If such a gate is genuinely wanted it needs its own ruling, not a quiet reference here.", forbidden)
		}
	}

	// ANTI-VACUITY: prove the file read IS the outcome handler, so a rename or a move turns this red
	// instead of leaving it trivially satisfied by a missing or wrong file.
	for _, needle := range []string{"func (h *OutcomeHandler)", "send_managed.jobs"} {
		if !strings.Contains(src, needle) {
			t.Fatalf("outcome.go does not contain %q — this scan is reading the wrong file and proves nothing", needle)
		}
	}
}

// TestDispatchRung_OutcomeIngestionSucceedsAtTheDeepestRung is the behavioural companion to the scan
// above: at `stopped`, with the legacy boolean also pulled, a worker's report is still applied in full.
// The scan proves no gate was added; this proves the path actually works with every lever down.
func TestDispatchRung_OutcomeIngestionSucceedsAtTheDeepestRung(t *testing.T) {
	e := newDispatchEnv(t, "rung_outcome_stopped")
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	ctx := context.Background()

	job := e.seedQueuedJob(t, "outcome@example.com")
	if _, pushed, err := e.d.Tick(ctx); err != nil || pushed != 1 {
		t.Fatalf("staging tick pushed=%d err=%v, want 1/nil", pushed, err)
	}
	if st, _, _, _, _ := e.jobState(t, job); st != "sending" {
		t.Fatalf("staged job is %q, want sending", st)
	}

	// Every lever to its deepest setting, then report.
	e.setRung(t, RungStopped)
	e.setConfigBool(t, "dispatch_enabled", false)

	if resolved := e.oh.Handle(e.jobWorker(t, job), workerproto.Report{
		Type: workerproto.TypeReport, JobID: job, AttemptToken: e.jobAttemptToken(t, job),
		Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "pmid-rung-stopped",
		SMTPResponse: "250 2.0.0 OK",
	}); !resolved {
		t.Fatal("the outcome handler did not resolve a report at rung=stopped — the mail already left, and its record has just been dropped")
	}
	st, hasPMID, _, completedSet, _ := e.jobState(t, job)
	if st != "sent" || !hasPMID || !completedSet {
		t.Errorf("after reporting at rung=stopped: state=%q hasPMID=%t completed=%t, want sent/true/true", st, hasPMID, completedSet)
	}
	if n := e.sendLogCount(t, job); n != 1 {
		t.Errorf("send_logs rows=%d, want 1 — the send's own record must survive every rung", n)
	}
}

// TestDispatchRung_TickIsANoOpAtEveryPausedRung — the "nothing happened" guarantee with the control
// that makes it mean something. At each claim-stopped rung: three ticks, no state change, no job_events
// row, no push; then release, and the SAME job drains. Three ticks because a first-tick-only latch
// would pass one.
func TestDispatchRung_TickIsANoOpAtEveryPausedRung(t *testing.T) {
	for _, rung := range []string{RungAcceptAndDefer, RungRefuseNew, RungStopped} {
		t.Run(rung, func(t *testing.T) {
			e := newDispatchEnv(t, "rung_noop_"+strings.ReplaceAll(rung, "_", ""))
			pool := e.seedPool(t)
			e.seedAccount(t, pool, "sec")
			ctx := context.Background()

			jobID := e.seedQueuedJob(t, "noop@example.com")
			eventsBefore := e.eventCount(t, jobID)

			e.setRung(t, rung)
			for i := 0; i < 3; i++ {
				_, pushed, err := e.d.Tick(ctx)
				if err != nil {
					t.Fatalf("tick %d: %v", i, err)
				}
				if pushed != 0 {
					t.Fatalf("tick %d pushed %d job(s) at rung %q, want 0", i, pushed, rung)
				}
			}
			if st, _, _, _, _ := e.jobState(t, jobID); st != "queued" {
				t.Errorf("job is %q after three paused ticks, want queued — a paused rung must leave queued work exactly queued", st)
			}
			if n := e.eventCount(t, jobID); n != eventsBefore {
				t.Errorf("paused ticks wrote %d job_events row(s) (was %d) — a paused tick must write NOTHING", n-eventsBefore, eventsBefore)
			}
			if n := e.px.pushCount(); n != 0 {
				t.Errorf("paused ticks pushed %d frame(s), want 0", n)
			}

			// ANTI-VACUITY: release and the same job drains. Without this, a fixture with no eligible
			// account or no idle worker would produce identical zeros and the test would pass on nothing.
			e.setRung(t, RungServing)
			if _, pushed, err := e.d.Tick(ctx); err != nil {
				t.Fatalf("release tick: %v", err)
			} else if pushed != 1 {
				t.Fatalf("after release the tick pushed %d job(s), want 1 — the fixture was never dispatchable, so the zeros above proved nothing", pushed)
			}
		})
	}
}

// TestDispatchRung_ConfigIsRereadEveryTickSoAPullTakesEffectWithoutRestart. The ladder is only an
// incident-response tool if pulling it works DURING an incident. M1's ADR-0015 records the opposite as
// a stated limitation of its own ladder (config read at boot, no reload, a restart required), so the
// senders' hot-read property is worth an assertion rather than an assumption — in both directions:
// pulling AND releasing, on one Dispatcher, one pool, no restart.
func TestDispatchRung_ConfigIsRereadEveryTickSoAPullTakesEffectWithoutRestart(t *testing.T) {
	e := newDispatchEnv(t, "rung_hotreload")
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.seedAccount(t, pool, "sec2")
	e.addWorker(t)
	ctx := context.Background()

	first := e.seedQueuedJob(t, "hot1@example.com")
	if _, pushed, err := e.d.Tick(ctx); err != nil || pushed != 1 {
		t.Fatalf("first tick pushed=%d err=%v, want 1/nil", pushed, err)
	}
	if st, _, _, _, _ := e.jobState(t, first); st == "queued" {
		t.Fatalf("first job still queued — the fixture is not dispatching")
	}

	// Pull the rung with NO restart, NO new Dispatcher, NO reconnect. The very next tick must obey.
	e.setRung(t, RungRefuseNew)
	e.px.freeAllWorkers()
	second := e.seedQueuedJob(t, "hot2@example.com")
	if _, pushed, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick after pull: %v", err)
	} else if pushed != 0 {
		t.Errorf("the tick immediately after pulling the rung pushed %d job(s) — a kill switch that needs a restart is not an incident-response tool", pushed)
	}
	if st, _, _, _, _ := e.jobState(t, second); st != "queued" {
		t.Errorf("second job is %q, want queued", st)
	}

	// Release, same Dispatcher, same pool.
	e.setRung(t, RungServing)
	e.px.freeAllWorkers()
	if _, pushed, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick after release: %v", err)
	} else if pushed != 1 {
		t.Errorf("the tick immediately after releasing pushed %d job(s), want 1 — a rung that cannot be RELEASED without a restart is worse than one that cannot be pulled", pushed)
	}
}

// --- the no-drift mechanism across all three copies of the ladder -----------------------------

// ladderCopies are the files outside this package that duplicate the rung names and their ordering,
// because platformapi and adminapi do not import internal/dispatch (structural isolation, OD-7).
var ladderCopies = []string{
	"../platformapi/dispatch_rung.go",
	"../adminapi/dispatch_rung.go",
}

// TestDispatchRung_EveryCopyOfTheLadderMatchesThisOne is the mechanism that replaces "remember to
// update all three."
//
// Three copies of one ladder is a drift hazard, and a drifted copy is invisible: every package
// compiles, every suite passes, and the halves of one control disagree only in production. So this
// parses the rung names and the severity ordering out of each copy's SOURCE and fails if any differs
// from this package's.
//
// AND IT FAILS IF A FOURTH COPY APPEARS THAT IT DOES NOT KNOW ABOUT. That second half is deliberate. An
// earlier test in this same alignment pass was written to read ONE file while claiming to guard a whole
// package, and a mutation that moved the code to a new file sailed straight through it. A check whose
// coverage is narrower than the property it claims to guard is this pass's recurring failure, so the
// discovery here is mechanical rather than hand-listed: every non-test .go file under internal/ that
// declares the severity table must be enumerated above.
func TestDispatchRung_EveryCopyOfTheLadderMatchesThisOne(t *testing.T) {
	// The authoritative table is read from this package's own SOURCE, not from its runtime map: a
	// Go-value comparison would compare this package's compiled map against the others' text, and so
	// would not notice a constant renamed in rung.go itself.
	wantNames, wantOrder := parseLadder(t, "rung.go")
	if len(wantNames) != 4 || len(wantOrder) != 4 {
		t.Fatalf("parsed %d rung constant(s) and %d severity entr(ies) from rung.go, want 4 and 4: %v / %v", len(wantNames), len(wantOrder), wantNames, wantOrder)
	}
	t.Logf("authoritative ladder: %v / %v", wantNames, wantOrder)

	for _, copyPath := range ladderCopies {
		names, order := parseLadder(t, copyPath)
		if !equalStrings(names, wantNames) {
			t.Errorf("%s declares rung names %v, rung.go declares %v — the ladder has DRIFTED; one surface would answer for a rung the other does not have", copyPath, names, wantNames)
		}
		if !equalStrings(order, wantOrder) {
			t.Errorf("%s orders severity %v, rung.go orders %v — a drifted ORDERING is worse than a drifted name: both compile, and \"refuse_new and below\" silently means two different things on two surfaces", copyPath, order, wantOrder)
		}
	}

	// The coverage half: discover every copy and require it to be enumerated.
	found := discoverLadderCopies(t)
	for _, f := range found {
		if f == "rung.go" {
			continue
		}
		known := false
		for _, c := range ladderCopies {
			if c == f {
				known = true
				break
			}
		}
		if !known {
			t.Errorf("%s declares the rung ladder but is NOT in ladderCopies — this test would not have compared it, so the ladder could drift there unnoticed. Add it to ladderCopies; coverage narrower than the property is the whole failure this assertion exists to catch.", f)
		}
	}
	if len(found) < 3 {
		t.Fatalf("discovery found only %d ladder declaration(s) (%v) — expected at least 3 (dispatch, platformapi, adminapi). The scan is looking in the wrong place and proves nothing.", len(found), found)
	}
}

var (
	rungConstRe    = regexp.MustCompile(`(?m)^\s*[Rr]ung\w+\s*=\s*"([a-z_]+)"`)
	rungSeverityRe = regexp.MustCompile(`(?m)^\s*[Rr]ung(\w+):\s*(\d+),`)
)

// parseLadder extracts the rung slugs (in declaration order) and the severity table (as "name=n"
// strings, in declaration order) from a Go source file.
func parseLadder(t *testing.T, path string) (names, order []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(raw)
	for _, m := range rungConstRe.FindAllStringSubmatch(src, -1) {
		names = append(names, m[1])
	}
	for _, m := range rungSeverityRe.FindAllStringSubmatch(src, -1) {
		order = append(order, strings.ToLower(m[1])+"="+m[2])
	}
	// A pattern that has stopped matching would make every comparison against this file vacuously equal
	// — the drift check would go quietly green exactly when it stopped working.
	if len(names) == 0 || len(order) == 0 {
		t.Fatalf("%s: parsed %d rung constant(s) and %d severity entr(ies) — the patterns no longer match this file's shape, so every comparison against it would be vacuous", path, len(names), len(order))
	}
	return names, order
}

// discoverLadderCopies walks internal/ and returns every non-test .go file that declares the rung
// severity table, as a path relative to this package (so it is directly comparable with ladderCopies).
// This is what makes the enumeration self-checking rather than a hand-maintained list.
func discoverLadderCopies(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.Walk("..", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if rungSeverityRe.Match(raw) {
			rel := filepath.ToSlash(path)
			// Normalise this package's own file to the bare name parseLadder is called with.
			rel = strings.TrimPrefix(rel, "../dispatch/")
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDispatchRung_LogLineNamesTheRungNotJustThePause. M1's D2 damage was that everything LOOKED normal
// while the config was ignored, so a diagnostic reporting "paused" without saying WHICH rung is pulled
// reproduces that ambiguity in the operator's only window into the system. There are now three rungs at
// which claiming stops, and they differ in what else is still running.
//
// Source-level for the same reason as the outcome scan, plus one of its own: the diagnostics are
// throttled to one line per interval per process, so capturing all three from behaviour would be
// timing-dependent — a flaky test, which is a worse guard than none.
//
// SCOPED TO EACH FUNCTION'S OWN BODY, not to the whole file. The first version of this test scanned
// dispatch.go entire, and a mutation that pointed the frozen-sweep diagnostic at the DISPATCH throttle
// timestamp passed it — because the field declaration and its explanatory comment elsewhere in the file
// still contained the identifier the test was looking for. A file-wide scan for a name proves the name
// exists SOMEWHERE, which is not the property; "the check covers less than it claims to guard" is the
// exact failure this alignment pass keeps finding, and it was found here by mutation rather than by
// reading. So each assertion now reads the body of the function that must satisfy it.
func TestDispatchRung_LogLineNamesTheRungNotJustThePause(t *testing.T) {
	raw, err := os.ReadFile("dispatch.go")
	if err != nil {
		t.Fatalf("read dispatch.go: %v", err)
	}
	src := string(raw)

	paused := funcBodySource(t, src, "logDispatchPausedByOperator")
	frozen := funcBodySource(t, src, "logReclaimFrozenByOperator")

	// The paused diagnostic must distinguish the three claim-stopped rungs.
	for _, needle := range []string{"RungStopped", "RungRefuseNew", "rung=%s"} {
		if !strings.Contains(paused, needle) {
			t.Errorf("logDispatchPausedByOperator does not reference %q — an operator reading it cannot tell WHICH rung is pulled, which is the ambiguity that made M1's D2 expensive", needle)
		}
	}
	// It must report BOTH dials, or an operator who pulled only one cannot tell why they are here.
	if !strings.Contains(paused, "dispatch_mode=%q") || !strings.Contains(paused, "dispatch_enabled=%t") {
		t.Error("logDispatchPausedByOperator must name both dials (dispatch_mode and dispatch_enabled) — with two inputs composing, reporting one is how an operator concludes the other is ignored")
	}
	// The frozen-sweep line is the one fact no other line carries.
	if !strings.Contains(frozen, "reclaim_frozen_by_operator") {
		t.Error("logReclaimFrozenByOperator does not emit reclaim_frozen_by_operator — the frozen sweep is the one thing about `stopped` an operator cannot infer from any other log line")
	}
	if !strings.Contains(frozen, "rung=%s") {
		t.Error("logReclaimFrozenByOperator does not name the rung either — same D2 ambiguity, on the line that matters most")
	}
	// And it must throttle on its OWN timestamp, in BOTH the guard and the stamp, and must not touch the
	// dispatch one: the dispatch loop ticks far more often and would swallow this line for the whole
	// interval. Asserted as "uses its own AND does not use the other" because either half alone passes a
	// mutation that keeps one reference and switches the other.
	for _, needle := range []string{"time.Since(d.lastReclaimFrozenLog)", "d.lastReclaimFrozenLog = time.Now()"} {
		if !strings.Contains(frozen, needle) {
			t.Errorf("logReclaimFrozenByOperator does not %q — a throttle it does not own is a throttle another loop controls", needle)
		}
	}
	if strings.Contains(frozen, "lastDispatchPausedLog") {
		t.Error("logReclaimFrozenByOperator references lastDispatchPausedLog — sharing the dispatch throttle lets a chatty dispatch tick suppress the frozen-sweep line for a minute, hiding the deepest rung's most surprising consequence during the incident it matters in")
	}
	if strings.Contains(paused, "lastReclaimFrozenLog") {
		t.Error("logDispatchPausedByOperator references lastReclaimFrozenLog — the same collision in the other direction")
	}
}

// funcBodySource returns the source text of the named method's body, so a scan asserts something about
// the function that must satisfy the property rather than about the file that happens to contain it.
func funcBodySource(t *testing.T, src, name string) string {
	t.Helper()
	marker := ") " + name + "("
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("no func %s in dispatch.go — this test's subject moved or was renamed; fix the test, do not delete the assertion", name)
	}
	open := strings.Index(src[i:], "{")
	if open < 0 {
		t.Fatalf("func %s has no body", name)
	}
	rest := src[i+open:]
	// Methods are top-level, so the body ends at the first "\n}" in column zero.
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("cannot find the end of func %s", name)
	}
	return rest[:end]
}
