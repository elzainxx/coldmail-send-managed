package dispatch

// mutator_mode_test.go — fix-plan item 13 (send_managed_0026, ADR-0014): every autonomous mutator is
// observable before it acts, and a NEW autonomous mutator cannot ship enforcing on day one.
//
// Named TestMutatorMode_* rather than TestShadowMode_* because shadowmode_test.go already owns that
// prefix for Phase 9.5's autoquarantine ladder — the dial this item generalizes. Two files answering to
// one prefix is how a `-run` filter silently stops covering half of what its name suggests.
//
// Real Postgres throughout for the behavioural half (Rule 3): the two newly-gated writers are an
// accounts/account_events write and a send_logs DELETE, and neither property survives being mocked —
// "shadow wrote no row" is only meaningful against a table that would have received one.
//
// NOTE ON THE HARNESS DEFAULT. newDispatchEnv pins autopause_mode=enforce and retention_mode=enforce for
// the same reason it already pins autoquarantine_mode=enforce: the Phase-5-era suites assert the real
// enforcing mechanics and would otherwise observe a logged decision instead of the flag. Every test below
// therefore sets its own mode explicitly, and the assertion that the MIGRATION seeds `shadow` lives where
// no harness override can mask it — on default_value here (Rule 1 forbids mutating it), and on the seeded
// `value` in tools/send-managed-migrate/runner_integration_test.go, which runs the raw chain with no
// harness at all.

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// --- the ladder itself -------------------------------------------------------------------------

// TestMutatorMode_NoModeIsASynonymForItsNeighbour is the non-degeneracy pin. Three modes are only worth
// three names if each does something the others do not, so both predicates are tabulated across the
// whole ladder and the result is required to be a genuine staircase — 2/3 decide, 1/3 writes.
//
// A ladder whose predicates split 0/3 or 3/3 would compile, pass every behavioural test that exercises
// only one mode, and quietly mean "off" or "enforce" in three spellings. rung.go carries the same pin
// for the same reason.
func TestMutatorMode_NoModeIsASynonymForItsNeighbour(t *testing.T) {
	type want struct{ decides, writes bool }
	table := map[MutatorMode]want{
		ModeOff:     {decides: false, writes: false},
		ModeShadow:  {decides: true, writes: false},
		ModeEnforce: {decides: true, writes: true},
	}
	if len(table) != len(mutatorActivity) {
		t.Fatalf("the table covers %d mode(s) but mutatorActivity has %d — a mode was added to the ladder and this test did not learn about it", len(table), len(mutatorActivity))
	}

	decidingModes, writingModes, observingModes := 0, 0, 0
	for mode, w := range table {
		// autopause_enabled=true so the CEILING is not what is under test here.
		cfg := Config{AutopauseMode: mode, AutopauseEnabled: true, RetentionMode: mode}
		if got := cfg.AutopauseDecides(); got != w.decides {
			t.Errorf("autopause_mode=%s: AutopauseDecides()=%v, want %v", mode, got, w.decides)
		}
		if got := cfg.AutopauseWrites(); got != w.writes {
			t.Errorf("autopause_mode=%s: AutopauseWrites()=%v, want %v", mode, got, w.writes)
		}
		if got := cfg.RetentionCounts(); got != w.decides {
			t.Errorf("retention_mode=%s: RetentionCounts()=%v, want %v", mode, got, w.decides)
		}
		if got := cfg.RetentionDeletes(); got != w.writes {
			t.Errorf("retention_mode=%s: RetentionDeletes()=%v, want %v", mode, got, w.writes)
		}
		if w.decides {
			decidingModes++
		}
		if w.writes {
			writingModes++
		}
		if w.decides && !w.writes {
			observingModes++
			if mode != ModeShadow {
				t.Errorf("mode %q decides without writing — that is shadow's role, and two modes sharing it means one of them is dead weight", mode)
			}
		}
		t.Logf("mode=%-8s decides=%-5v writes=%v", mode, w.decides, w.writes)
	}
	// The staircase itself. Without these counts, a mutation making every mode write would satisfy a
	// table someone had edited to match it.
	if decidingModes != 2 || writingModes != 1 {
		t.Errorf("%d mode(s) decide and %d write(s) — want exactly 2 and 1. A ladder where those counts collapse is one mode wearing three names, which is the whole failure a shadow default exists to prevent", decidingModes, writingModes)
	}
	// And the middle rung must be genuinely in the middle: exactly one mode decides without writing.
	// With none there is no observation mode at all, whatever the names say.
	if observingModes != 1 {
		t.Errorf("%d mode(s) decide-without-writing, want exactly 1 (shadow)", observingModes)
	}
}

// TestMutatorMode_UnrecognizedModeFallsBackToShadowNotEnforceAndNotOff. Both wrong answers are wrong in
// their own direction, so both are asserted rather than only "it does not enforce":
//   - enforce would let a typo silently gain the power to act (and, for retention, to DELETE)
//   - off would silently disable a control an operator believes is running, which is M1's D2 finding
//
// Case variants ("SHADOW", "Enforce") are required to fall back deliberately: the config row holds the
// operator's exact string and the admin validator accepts only the three lowercase spellings, so
// accepting case variants here would mean the two layers disagree about what is valid.
func TestMutatorMode_UnrecognizedModeFallsBackToShadowNotEnforceAndNotOff(t *testing.T) {
	for _, raw := range []string{"", "SHADOW", "Enforce", "shadow ", "enfroce", "on", "true", "null", "{}"} {
		cfg := Config{AutopauseMode: raw, AutopauseEnabled: true, RetentionMode: raw}

		if got := cfg.EffectiveAutopauseMode(); got != ModeShadow {
			t.Errorf("autopause_mode=%q resolved to %q, want shadow", raw, got)
		}
		if got := cfg.EffectiveRetentionMode(); got != ModeShadow {
			t.Errorf("retention_mode=%q resolved to %q, want shadow", raw, got)
		}
		// Spelled out as the two separate failures they are, so a regression names its own direction.
		if cfg.AutopauseWrites() {
			t.Errorf("autopause_mode=%q would WRITE — a typo must never gain the power to pause an account", raw)
		}
		if cfg.RetentionDeletes() {
			t.Errorf("retention_mode=%q would DELETE — a typo must never gain the power to drop rows", raw)
		}
		if !cfg.AutopauseDecides() {
			t.Errorf("autopause_mode=%q evaluates nothing — falling back to `off` silently disables a control the operator believes is running (M1's D2)", raw)
		}
		if !cfg.RetentionCounts() {
			t.Errorf("retention_mode=%q measures nothing — the same silent-disable defect on the sweep", raw)
		}
	}
}

// TestMutatorMode_EnabledFalseIsACeilingNotASecondLever drives the FULL 3x2 matrix of
// (autopause_mode x autopause_enabled).
//
// The property: the boolean can only ever REDUCE what the system does. So enabled=false caps the mode at
// `off`, and enabled=true never RAISES one — shadow stays shadow rather than being promoted to enforce.
//
// This is the OPPOSITE composition direction from rung.go's FLOOR, and the matrix is what proves the
// direction is deliberate rather than a copy-paste slip: on a severity scale "do less" is a MORE severe
// rung, on an activity scale "do less" is a LESS active mode. Both are the same rule.
func TestMutatorMode_EnabledFalseIsACeilingNotASecondLever(t *testing.T) {
	cases := []struct {
		mode    MutatorMode
		enabled bool
		want    MutatorMode
	}{
		{ModeOff, true, ModeOff},
		{ModeShadow, true, ModeShadow}, // NOT promoted to enforce by a true boolean
		{ModeEnforce, true, ModeEnforce},
		{ModeOff, false, ModeOff},
		{ModeShadow, false, ModeOff},  // capped
		{ModeEnforce, false, ModeOff}, // capped — the load-bearing row
	}
	for _, c := range cases {
		cfg := Config{AutopauseMode: c.mode, AutopauseEnabled: c.enabled}
		if got := cfg.EffectiveAutopauseMode(); got != c.want {
			t.Errorf("autopause_mode=%s autopause_enabled=%v -> %s, want %s", c.mode, c.enabled, got, c.want)
		}
	}
	// The two rows that matter most, as their own assertions, so a regression reads as the operator
	// complaint it would generate rather than as a table mismatch.
	capped := Config{AutopauseMode: ModeEnforce, AutopauseEnabled: false}
	if capped.AutopauseWrites() || capped.AutopauseDecides() {
		t.Error("autopause_enabled=false with autopause_mode=enforce still acts — an operator who disabled auto-pause got an auto-pause, which is exactly the half-honoured control item 12 fixed on the dispatch side")
	}
	if got := (Config{AutopauseMode: ModeShadow, AutopauseEnabled: true}).EffectiveAutopauseMode(); got == ModeEnforce {
		t.Error("autopause_enabled=true PROMOTED shadow to enforce — the boolean must never raise a mode an operator left in observation")
	}
	// retention_mode joins no boolean, so it must be indifferent to this one. Asserted, because
	// "retention has no boolean" is a claim about the code, not a fact about the struct.
	//
	// BOTH retention predicates, not just the write one. Found by break-verification (B47): a version
	// asserting only RetentionDeletes stayed green while RetentionCounts was cross-wired to
	// autopause_enabled — which would silently stop the shadow sweep from REPORTING for any operator who
	// had disabled auto-pause, leaving them watching a log line that never comes while send_logs grows.
	// The observation half of a shadow ladder is exactly as load-bearing as the write half.
	for _, enabled := range []bool{true, false} {
		for _, mode := range []MutatorMode{ModeShadow, ModeEnforce} {
			cfg := Config{RetentionMode: mode, AutopauseEnabled: enabled}
			if !cfg.RetentionCounts() {
				t.Errorf("retention_mode=%s stopped MEASURING because autopause_enabled=%v — the two dials are unrelated and must not be cross-wired; a shadow sweep that reports nothing is invisible", mode, enabled)
			}
			if got := cfg.EffectiveRetentionMode(); got != mode {
				t.Errorf("retention_mode=%s resolved to %s under autopause_enabled=%v — autopause's boolean must not reach this ladder at all", mode, got, enabled)
			}
		}
		if cfg := (Config{RetentionMode: ModeEnforce, AutopauseEnabled: enabled}); !cfg.RetentionDeletes() {
			t.Errorf("retention_mode=enforce stopped deleting because autopause_enabled=%v — the two dials are unrelated and must not be cross-wired", enabled)
		}
	}
}

// TestMutatorMode_MigrationSeedsShadowNotEnforce. The whole safety claim of item 13 is that a freshly
// migrated deployment mutates nothing until an operator promotes the dial, so the SEEDED value is the
// property — not just the Go ladder's behaviour once a value is in hand.
//
// Asserted on default_value, which Rule 1 forbids any test (and the harness) from mutating, so this
// cannot be masked by newDispatchEnv's enforce override the way `value` can. The seeded `value` itself is
// asserted in tools/send-managed-migrate/runner_integration_test.go, against the raw migration chain
// with no harness in the picture at all.
func TestMutatorMode_MigrationSeedsShadowNotEnforce(t *testing.T) {
	e := newDispatchEnv(t, "mode_seed")
	for _, key := range []string{"autopause_mode", "retention_mode", "autoquarantine_mode"} {
		var def, valueType string
		if err := e.admin.QueryRow(context.Background(),
			`SELECT default_value->>'v', value_type FROM send_managed.config WHERE key=$1`, key).Scan(&def, &valueType); err != nil {
			t.Fatalf("read %s: %v (an ABSENT row is a hard boot failure, not a default — see config.go)", key, err)
		}
		if def != ModeShadow {
			t.Errorf("%s default_value=%q, want %q — a mutator dial seeded anywhere but shadow ships enforcing on day one, which is the risk item 13 exists to remove", key, def, ModeShadow)
		}
		if valueType != "string" {
			t.Errorf("%s value_type=%q, want string", key, valueType)
		}
	}
}

// --- auto-pause: real behaviour on real Postgres -----------------------------------------------

// throttleOutcome drives ONE real throttle outcome through the real OutcomeHandler against a fresh job,
// which is the only way the auto-pause decision is reached.
func (e *dispatchEnv) throttleOutcome(t *testing.T, recipient string) {
	t.Helper()
	e.px.freeAllWorkers()
	job := e.seedQueuedJob(t, recipient)
	e.dispatchToSending(t, job)
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
		SMTPResponse: "450 4.7.1 rate limit exceeded, slow down"})
}

// countAccountEventReasons counts this account's actor='system' events whose reason carries the prefix.
func (e *dispatchEnv) countAccountEventReasons(t *testing.T, acctID, prefix string) int {
	t.Helper()
	n := 0
	for _, ev := range e.systemAccountEvents(t, acctID) {
		if strings.HasPrefix(ev[1], prefix) {
			n++
		}
	}
	return n
}

// TestMutatorMode_AutopauseShadowRecordsTheDecisionAndDoesNotPause is the point of the whole item: at
// shadow a throttle outcome leaves the account sending, and leaves evidence.
//
// Both halves are asserted. "Did not pause" alone would also pass for a mode that evaluated nothing
// (that is `off`), and an observation dial that observes nothing is indistinguishable from an off
// switch — precisely the state an operator would misread as "no account is tripping the threshold".
func TestMutatorMode_AutopauseShadowRecordsTheDecisionAndDoesNotPause(t *testing.T) {
	e := newDispatchEnv(t, "autopause_shadow")
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec-shadowpause")

	e.setConfigStr(t, "autopause_mode", ModeShadow)
	e.throttleOutcome(t, "shadowpause@example.com")

	paused, quarantined, reason := e.accountFlagsNow(t, acct)
	if paused {
		t.Errorf("account is paused at autopause_mode=shadow (reason=%v) — shadow decides but must not act", reason)
	}
	if quarantined {
		t.Error("a throttle outcome quarantined the account — not this policy's job under any mode")
	}
	if n := e.countAccountEventReasons(t, acct, "shadow_would_pause:"); n != 1 {
		t.Errorf("%d shadow_would_pause observation row(s), want 1 — a shadow mode that records nothing is an off switch wearing another name, and an operator would read the silence as \"nothing is tripping\"", n)
	}
	// The row must be a self-evident no-op, not a state change an audit reader might replay.
	var sameFlags bool
	if err := e.admin.QueryRow(context.Background(),
		`SELECT from_flags = to_flags FROM send_managed.account_events
		  WHERE account_id=$1 AND reason LIKE 'shadow_would_pause:%' ORDER BY created_at DESC LIMIT 1`,
		acct).Scan(&sameFlags); err != nil {
		t.Fatalf("read observation row: %v", err)
	}
	if !sameFlags {
		t.Error("the observation row has from_flags != to_flags — a shadow marker must be from==to, or an audit reader cannot tell it from a real pause")
	}
}

// TestMutatorMode_AutopauseEnforceStillPausesExactlyAsBefore is the paired positive control, and it is
// what makes the test above mean something: without it, deleting the pause code entirely would pass.
func TestMutatorMode_AutopauseEnforceStillPausesExactlyAsBefore(t *testing.T) {
	e := newDispatchEnv(t, "autopause_enforce")
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec-enforcepause")

	e.setConfigStr(t, "autopause_mode", ModeEnforce)
	e.throttleOutcome(t, "enforcepause@example.com")

	paused, _, reason := e.accountFlagsNow(t, acct)
	if !paused {
		t.Error("account is NOT paused at autopause_mode=enforce — the pre-0026 behaviour must survive the ladder")
	}
	if reason == nil || *reason != "throttle" {
		t.Errorf("pause_reason=%v, want \"throttle\"", reason)
	}
	// At enforce the decision is recorded as a REAL state change, not also as a shadow marker: emitting
	// both would double-count every pause in the audit trail.
	if n := e.countAccountEventReasons(t, acct, "shadow_would_pause:"); n != 0 {
		t.Errorf("%d shadow_would_pause row(s) at enforce, want 0 — the shadow marker belongs to the mode that does NOT act", n)
	}
	realPause := false
	for _, ev := range e.systemAccountEvents(t, acct) {
		if ev[0] == "pause" && ev[1] == "throttle" {
			realPause = true
		}
	}
	if !realPause {
		t.Errorf("no real (pause, throttle) event at enforce: %v", e.systemAccountEvents(t, acct))
	}
}

// TestMutatorMode_AutopauseOffEvaluatesNothing separates `off` from `shadow` BEHAVIOURALLY. Without it
// the two differ only in a log line, and a mutation collapsing shadow into off would go unnoticed.
func TestMutatorMode_AutopauseOffEvaluatesNothing(t *testing.T) {
	e := newDispatchEnv(t, "autopause_off")
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec-offpause")

	e.setConfigStr(t, "autopause_mode", ModeOff)
	e.throttleOutcome(t, "offpause@example.com")

	if paused, _, _ := e.accountFlagsNow(t, acct); paused {
		t.Error("account paused at autopause_mode=off")
	}
	if n := e.countAccountEventReasons(t, acct, "shadow_would_pause:"); n != 0 {
		t.Errorf("%d observation row(s) at off, want 0 — off means no decision at all, which is what distinguishes it from shadow", n)
	}
}

// TestMutatorMode_AutoUnpauseIsNotGatedByTheAutopauseDial. The reversal half must keep working at every
// mode, including `off`. Shadowing or disabling a reversal leaves real pauses in place forever, which
// manufactures the stuck-account incident the observation period exists to avoid — strictly worse than
// the enforcing behaviour the dial was added to make safe.
func TestMutatorMode_AutoUnpauseIsNotGatedByTheAutopauseDial(t *testing.T) {
	for _, mode := range []MutatorMode{ModeOff, ModeShadow, ModeEnforce} {
		t.Run(mode, func(t *testing.T) {
			e := newDispatchEnv(t, "autounpause_"+mode)
			pool := e.seedPool(t)
			acct := e.seedAccount(t, pool, "sec-unpause")

			// A real, ALREADY-EXPIRED throttle pause set directly (statemachine_test.go's T3 recipe): the
			// subject is the RELEASE path, and routing through the pause path would make this test depend
			// on the very dial it is proving the release is independent of.
			if _, err := e.admin.Exec(context.Background(),
				`UPDATE send_managed.accounts
				    SET paused = TRUE, pause_reason = 'throttle', throttle_paused_at = NOW() - INTERVAL '1 hour'
				  WHERE id = $1`, acct); err != nil {
				t.Fatalf("seed expired throttle pause: %v", err)
			}
			e.setConfigStr(t, "autopause_mode", mode)

			// The self-clear happens inside the consume/eligibility CTE, so it needs a real claim.
			job := e.seedQueuedJob(t, "unpause-"+mode+"@example.com")
			e.dispatchToSending(t, job)

			paused, _, reason := e.accountFlagsNow(t, acct)
			if paused {
				t.Errorf("autopause_mode=%s left an EXPIRED throttle pause in place (reason=%v) — the dial gates the pause, never the unpause; blocking a reversal is how an observation period becomes an outage", mode, reason)
			}
			// And the reversal is AUDITED at every mode: a silent unpause is as unreviewable as a silent
			// pause, and a shadow marker must never stand in for the real unpause event.
			evs := e.systemAccountEvents(t, acct)
			unpauses := 0
			for _, ev := range evs {
				if ev[0] == "unpause" {
					unpauses++
				}
			}
			if unpauses != 1 {
				t.Errorf("autopause_mode=%s produced %d unpause event(s), want 1: %v", mode, unpauses, evs)
			}
		})
	}
}

// --- retention: real behaviour on real Postgres ------------------------------------------------

// seedExpiredSendLogs creates n send_logs rows whose TTL is already in the past, each on its own job.
func (e *dispatchEnv) seedExpiredSendLogs(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		job := e.seedQueuedJob(t, "retention"+strconv.Itoa(i)+"@example.com")
		e.seedSendLog(t, job, -24*time.Hour)
	}
}

// TestMutatorMode_RetentionShadowCountsAndDeletesNothing — the mode that must not drop a row.
func TestMutatorMode_RetentionShadowCountsAndDeletesNothing(t *testing.T) {
	e := newDispatchEnv(t, "retention_shadow")
	e.seedPool(t)
	e.seedExpiredSendLogs(t, 7)
	before := e.sendLogTotal(t)
	if before != 7 {
		t.Fatalf("fixture seeded %d send_logs row(s), want 7 — nothing below proves anything without them", before)
	}

	e.setConfigStr(t, "retention_mode", ModeShadow)
	deleted, batches, err := e.d.sweepExpiredSendLogs(context.Background(), e.mustLoadConfig(t))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if deleted != 0 || batches != 0 {
		t.Errorf("sweep reported %d deletion(s) in %d batch(es) at retention_mode=shadow, want 0/0", deleted, batches)
	}
	if after := e.sendLogTotal(t); after != before {
		t.Errorf("send_logs went %d -> %d at shadow — a deleted row cannot be restored by flipping the dial back, which is why this mutator most needed a shadow rung", before, after)
	}
}

// TestMutatorMode_RetentionEnforceStillDeletes is the paired positive control. Without it, a mutation
// breaking the DELETE entirely would look exactly like a correctly-shadowing sweep.
func TestMutatorMode_RetentionEnforceStillDeletes(t *testing.T) {
	e := newDispatchEnv(t, "retention_enforce")
	e.seedPool(t)
	e.seedExpiredSendLogs(t, 7)

	e.setConfigStr(t, "retention_mode", ModeEnforce)
	deleted, _, err := e.d.sweepExpiredSendLogs(context.Background(), e.mustLoadConfig(t))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if deleted != 7 {
		t.Errorf("sweep deleted %d row(s) at retention_mode=enforce, want 7 — the pre-0026 behaviour must survive the ladder", deleted)
	}
	if n := e.sendLogTotal(t); n != 0 {
		t.Errorf("%d expired send_logs row(s) survived enforce, want 0", n)
	}
}

// TestMutatorMode_RetentionOffSweepsNothing separates `off` from `shadow` on the sweep.
func TestMutatorMode_RetentionOffSweepsNothing(t *testing.T) {
	e := newDispatchEnv(t, "retention_off")
	e.seedPool(t)
	e.seedExpiredSendLogs(t, 4)
	before := e.sendLogTotal(t)

	e.setConfigStr(t, "retention_mode", ModeOff)
	deleted, batches, err := e.d.sweepExpiredSendLogs(context.Background(), e.mustLoadConfig(t))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if deleted != 0 || batches != 0 {
		t.Errorf("off issued %d deletion(s) in %d batch(es), want 0/0", deleted, batches)
	}
	if after := e.sendLogTotal(t); after != before {
		t.Errorf("send_logs went %d -> %d at off", before, after)
	}
}

// TestMutatorMode_RetentionShadowReportsTheBacklogAndOffDoesNot. The auto-pause dial distinguishes
// shadow from off in a TABLE (an observation row), so a behavioural assertion reaches it. The sweep
// cannot: at shadow and at off it deletes nothing and returns 0/0 identically, and the ONLY difference
// is the log line carrying the count. So the log line is the observation, and an assertion on the
// returned zeros is not enough — a sweep that silently skipped at shadow would pass every other test
// here while telling an operator nothing about an unbounded table.
//
// Both directions, because each failure is its own defect:
//   - shadow that does not report is an off switch wearing another name, and an operator watching the
//     log would read the silence as "there is nothing to delete" — the exact misreading that leads to
//     promoting a dial they never actually observed.
//   - off that DOES report is a control claiming to be measuring while the operator has switched it off.
//
// The COUNT itself is asserted, not just the marker: a line that reported 0 (or the batch size, which is
// what reading it off retention_delete_batch_size would produce) is a number nobody can size a retention
// window against, and the seeded batch is 1000 while this fixture is 5 — so a batch-derived count could
// not masquerade as a correct one.
func TestMutatorMode_RetentionShadowReportsTheBacklogAndOffDoesNot(t *testing.T) {
	e := newDispatchEnv(t, "retention_report")
	e.seedPool(t)
	e.seedExpiredSendLogs(t, 5)
	ctx := context.Background()

	const marker = "retention_shadow_would_delete"

	e.setConfigStr(t, "retention_mode", ModeShadow)
	shadowLogs := captureLogs(t)
	if _, _, err := e.d.sweepExpiredSendLogs(ctx, e.mustLoadConfig(t)); err != nil {
		t.Fatalf("shadow sweep: %v", err)
	}
	got := shadowLogs.String()
	if !strings.Contains(got, marker) {
		t.Errorf("shadow sweep logged no %q line — at shadow the log line IS the observation, and a silent shadow is indistinguishable from off. Logged: %q", marker, got)
	}
	// The real backlog, not zero and not the batch size (seeded 1000).
	if !strings.Contains(got, "5 expired send_logs row(s)") {
		t.Errorf("the shadow line does not report the true backlog of 5: %q — a count that is 0, or capped at retention_delete_batch_size, is useless for sizing a retention window (ADR-0014 decision 7)", got)
	}
	// And it must say the cost out loud, since the whole hazard of the seeded default is that it is easy
	// to leave in place.
	if !strings.Contains(got, "GROWS WITHOUT BOUND") {
		t.Errorf("the shadow line does not state that send_logs grows without bound: %q — the standing cost of the seeded default has to be visible on every tick, not just in a doc", got)
	}

	e.setConfigStr(t, "retention_mode", ModeOff)
	offLogs := captureLogs(t)
	if _, _, err := e.d.sweepExpiredSendLogs(ctx, e.mustLoadConfig(t)); err != nil {
		t.Fatalf("off sweep: %v", err)
	}
	if n := offLogs.count(marker); n != 0 {
		t.Errorf("off logged the shadow observation line %d time(s), want 0 — off means no sweep and no count; a control that keeps measuring after being switched off is reporting on work it is not doing", n)
	}
}

// TestMutatorMode_RetentionNeverTouchesJobsOrJobEventsAtAnyMode. §3.2's permanence guarantee, asserted
// against the SOURCE of the sweep rather than only its behaviour: a mode dial that could be promoted
// into deleting an outcome record would make the ladder a liability rather than a safety net, and the
// behavioural version can only prove it for the rows a test happened to seed.
//
// Scoped to the FUNCTION body, not the file: a file-wide scan for these table names would be satisfied
// by any comment or unrelated query that mentions them, which is exactly the defect break 22 found in
// rung_test.go — a source scan narrower than the property it claims to guard.
func TestMutatorMode_RetentionNeverTouchesJobsOrJobEventsAtAnyMode(t *testing.T) {
	raw, err := os.ReadFile("retention.go")
	if err != nil {
		t.Fatalf("read retention.go: %v", err)
	}
	body := funcBodySource(t, string(raw), "sweepExpiredSendLogs")
	for _, forbidden := range []string{"send_managed.jobs", "send_managed.job_events", "send_managed.accounts"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("sweepExpiredSendLogs names %s — the sweep touches send_logs ONLY (§3.2: outcomes are permanent by construction). A retention dial that can reach a jobs row is not a safety control", forbidden)
		}
	}
	if !strings.Contains(body, "send_managed.send_logs") {
		t.Error("sweepExpiredSendLogs does not name send_managed.send_logs at all — this scan is reading the wrong function and proves nothing")
	}
}

// --- the discovery mechanism: a NEW mutator cannot ship undialled ------------------------------

// autonomousWriteExclusions are the send_managed.* write sites that deliberately have NO mutator-mode
// dial, each with its reason. This map is the ONLY way a write site passes the discovery test below
// without a dial, so adding to it is a deliberate act that shows up in a diff — which is the whole
// difference between an audit and a claim that one happened.
var autonomousWriteExclusions = map[string]string{
	"account_score.go": "score/consecutive_failures is DERIVED STATE, not an action: it is the observation " +
		"the quarantine and pause decisions then read. Shadowing it would mean storing a stale number and " +
		"making the dialled mutators act on a lie, which previews nothing.",
	"eligibility.go": "auto-unpause is the REVERSAL half of auto-pause. Gating a reversal leaves real " +
		"pauses in place forever, manufacturing the stuck-account incident an observation period exists to " +
		"avoid. Asserted at every mode by TestMutatorMode_AutoUnpauseIsNotGatedByTheAutopauseDial.",
	"dispatch.go": "claim/lease/push writes are governed by dispatch_mode's four-rung ladder " +
		"(send_managed_0025, fix-plan item 12) plus dispatch_enabled — already dialled, on the ladder that " +
		"suits a kill-switch rather than a mutator.",
	"loop.go":    "the claim UPDATE; same ladder as dispatch.go (dispatch_mode).",
	"reclaim.go": "reclaim/lease-expiry writes are gated by dispatch_mode; the `stopped` rung freezes the sweep.",
	"rescan.go": "startup rescan re-adopts in-flight work after a restart. A RECOVERY path, not a policy " +
		"decision: it writes what the previous process already decided, and shadowing it would strand live " +
		"jobs on every deploy. Gated by dispatch_mode like the rest of the claim path.",
	"outcome.go": "records what a worker REPORTED — never autonomous. The Control is transcribing an " +
		"external event, and item 12 established that outcome ingestion is never gated at any rung because " +
		"refusing it destroys evidence rather than preventing an action.",
	"exhaustion.go": "requeues a job whose pool had no eligible account. A mechanical consequence of the " +
		"claim path (the job returns to the state it came from), gated by dispatch_mode.",
}

// dialledAutonomousWrites are the write sites that DO have a mutator-mode dial, and the dial each reads.
// Enumerated rather than inferred, so a site LOSING its gate fails here instead of passing silently.
var dialledAutonomousWrites = map[string][]string{
	"account_effect.go": {"AutoquarantineMode", "AutopauseDecides", "AutopauseWrites"},
	"retention.go":      {"RetentionCounts", "RetentionDeletes"},
}

var autonomousWriteRe = regexp.MustCompile(`(?i)(UPDATE|DELETE FROM)\s+send_managed\.`)

// TestMutatorMode_EveryAutonomousMutatorIsDialledOrExplicitlyExcluded is the MECHANISM that replaces
// "we audited everything, honest".
//
// It discovers every file in this package that issues a write against send_managed.* and requires each
// to be either (a) enumerated as dialled, with its dial actually referenced in the file, or (b)
// enumerated as excluded, with a reason. A new autonomous mutator therefore cannot ship enforcing on day
// one: its file appears in neither map and this test goes red naming it.
//
// WHY A DISCOVERY SCAN AND NOT A CHECKLIST IN A DOC. This alignment pass has repeatedly found the same
// defect — a warning comment, a prose audit, or a hand-maintained list standing in for a control — and
// item 13's own risk statement is "a new autonomous behaviour ships enforcing on day one, with no
// observation period". A list nobody is forced to update is that risk with extra steps.
func TestMutatorMode_EveryAutonomousMutatorIsDialledOrExplicitlyExcluded(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	writers := []string{}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !autonomousWriteRe.Match(raw) {
			continue
		}
		writers = append(writers, name)

		dials, isDialled := dialledAutonomousWrites[name]
		reason, isExcluded := autonomousWriteExclusions[name]

		switch {
		case isDialled && isExcluded:
			t.Errorf("%s is listed as BOTH dialled and excluded — one entry is stale, and the ambiguity means neither is trustworthy", name)
		case isDialled:
			// The dial must actually be REFERENCED. Enumerating a file as dialled while its gate has been
			// removed is the claimed-but-not-wired defect, and this half is what catches it.
			src := string(raw)
			for _, dial := range dials {
				if !strings.Contains(src, dial) {
					t.Errorf("%s is enumerated as gated by %s but does not reference it — the dial was removed or renamed and that write is now unconditional", name, dial)
				}
			}
		case isExcluded:
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s is excluded with an empty reason — an exclusion without a reason is a silent carve-out", name)
			}
		default:
			t.Errorf("%s writes to send_managed.* but is in neither dialledAutonomousWrites nor autonomousWriteExclusions. Fix-plan item 13: every autonomous mutator gets a mode dial defaulting to shadow, or an explicit recorded reason why it does not. This test is that mechanism — do not add the file to the exclusion map without a real reason, and do not delete this assertion.", name)
		}
	}

	// Anti-vacuity: the scan must actually be finding things. A regex that matched nothing (wrong
	// directory, reformatted SQL) would pass in perfect silence — the exact "coverage narrower than the
	// property" shape break 22 found elsewhere in this pass.
	if len(writers) < 6 {
		t.Fatalf("discovery found only %d write site(s) (%v) — expected at least 6 in this package. The scan is looking in the wrong place or the pattern no longer matches, and it proves nothing", len(writers), writers)
	}
	t.Logf("send_managed.* write sites discovered: %v", writers)

	// Every ENUMERATED file must still exist: a stale entry is how an exclusion outlives the code it
	// excused and starts silently covering something else.
	for name := range autonomousWriteExclusions {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("autonomousWriteExclusions names %s, which does not exist — a stale exclusion excuses nothing today and something else tomorrow", name)
		}
	}
	for name := range dialledAutonomousWrites {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("dialledAutonomousWrites names %s, which does not exist", name)
		}
	}
}

// TestMutatorMode_ModeIsRereadEveryTickSoPromotionTakesEffectWithoutRestart. A safety dial an operator
// cannot promote without a redeploy is not usable at the moment they need it — and promoting
// retention_mode is a REQUIRED go-live step, so it happens on a live system by definition. Both
// directions, one Dispatcher, no restart.
func TestMutatorMode_ModeIsRereadEveryTickSoPromotionTakesEffectWithoutRestart(t *testing.T) {
	e := newDispatchEnv(t, "retention_hotreload")
	e.seedPool(t)
	e.seedExpiredSendLogs(t, 3)
	ctx := context.Background()

	e.setConfigStr(t, "retention_mode", ModeShadow)
	if _, deleted, err := e.d.RetentionTick(ctx); err != nil {
		t.Fatalf("shadow tick: %v", err)
	} else if deleted != 0 {
		t.Fatalf("shadow tick deleted %d, want 0", deleted)
	}

	// Promote with NO restart and NO new Dispatcher. The very next tick must act.
	e.setConfigStr(t, "retention_mode", ModeEnforce)
	if _, deleted, err := e.d.RetentionTick(ctx); err != nil {
		t.Fatalf("enforce tick: %v", err)
	} else if deleted != 3 {
		t.Errorf("the tick immediately after promoting deleted %d row(s), want 3 — a safety dial that needs a restart to promote is one an operator cannot use during the go-live window it exists for", deleted)
	}

	// And demote: an operator who sees the sweep dropping more than expected must be able to stop it
	// mid-incident. Asserted because a one-way dial is only half a control.
	e.seedExpiredSendLogs(t, 3)
	e.setConfigStr(t, "retention_mode", ModeShadow)
	if _, deleted, err := e.d.RetentionTick(ctx); err != nil {
		t.Fatalf("demote tick: %v", err)
	} else if deleted != 0 {
		t.Errorf("the tick immediately after demoting deleted %d row(s), want 0 — a dial that cannot be pulled back is worse than one that cannot be promoted", deleted)
	}
}
