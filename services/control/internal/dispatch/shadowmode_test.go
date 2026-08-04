package dispatch

// shadowmode_test.go — Phase 9.5 Chunk 2 (B2, §2). Real Postgres, real Handle() calls — no mocks
// (CLAUDE.md Rule 3). Proves the O2/O3 behavioral contracts: autoquarantine_mode's shadow ladder
// genuinely decides-without-acting in shadow and acts identically to pre-9.5 Phase 5 in enforce,
// with a paired positive control on the SAME condition; autopause_enabled and variation_enabled
// are independent kill-switches that disable only their own behavior.

import (
	"context"
	"strings"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// TestShadowMode_LogsWithoutActing_ThenEnforceActs (O2) — driving the SAME auth_failure condition
// under autoquarantine_mode=shadow logs a would-quarantine account_events row (action='updated',
// actor='system', from_flags==to_flags, reason starting "shadow_would_quarantine:") and leaves
// quarantined=false — the account keeps sending. Promoting to enforce and repeating the identical
// condition on a FRESH account DOES set quarantined=true with the real 'quarantine' event — the
// paired positive control proving shadow's non-action isn't just a broken test, but the mode gate.
func TestShadowMode_LogsWithoutActing_ThenEnforceActs(t *testing.T) {
	e := newDispatchEnv(t, "shadowauth")
	ctx := context.Background()
	pool := e.seedPool(t)
	acctShadow := e.seedAccount(t, pool, "sec-shadow")
	jobShadow := e.seedQueuedJob(t, "shadow@example.com")

	e.setConfigStr(t, "autoquarantine_mode", "shadow")
	e.dispatchToSending(t, jobShadow)
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: jobShadow, Outcome: workerproto.OutcomeError,
		SMTPResponse: "535 5.7.8 Authentication credentials invalid"})

	// The account must NOT be quarantined: shadow decides, does not act.
	_, quarantined, _ := e.accountFlagsNow(t, acctShadow)
	if quarantined {
		t.Fatalf("account WAS quarantined under autoquarantine_mode=shadow — shadow must decide without acting (§2.1)")
	}
	// Exactly one shadow observation row: action='updated', from_flags==to_flags, reason marker.
	var action, reason string
	var fromFlags, toFlags []byte
	if err := e.admin.QueryRow(ctx,
		`SELECT action, reason, from_flags::text, to_flags::text FROM send_managed.account_events
		 WHERE account_id=$1 AND actor='system' ORDER BY created_at DESC LIMIT 1`, acctShadow).
		Scan(&action, &reason, &fromFlags, &toFlags); err != nil {
		t.Fatalf("read shadow event: %v", err)
	}
	if action != "updated" {
		t.Fatalf("shadow event action=%q, want updated (an existing CHECK-allowed value, §2.1 schema-neutral encoding)", action)
	}
	if string(fromFlags) != string(toFlags) {
		t.Fatalf("shadow event from_flags(%s) != to_flags(%s) — the equality IS the signal that nothing was mutated (§2.1)", fromFlags, toFlags)
	}
	const marker = "shadow_would_quarantine:"
	if len(reason) < len(marker) || reason[:len(marker)] != marker {
		t.Fatalf("shadow event reason=%q, want it to start with %q (§2.1's structured marker)", reason, marker)
	}
	if !containsAll(reason, "score=", "threshold=", "trigger=auth_failure") {
		t.Fatalf("shadow event reason=%q, missing expected structured fields", reason)
	}

	// Job still requeued normally (the JOB policy is unaffected by the account-effect mode) and the
	// account remains ELIGIBLE — sending continues (positive control that shadow doesn't silently
	// disable the account some other way).
	state, _, leaseNull, _, _ := e.jobState(t, jobShadow)
	if state != "queued" || !leaseNull {
		t.Fatalf("job state=%s leaseNull=%v, want queued/true (job policy unaffected by account-effect mode)", state, leaseNull)
	}
	e.px.freeAllWorkers()                                                   // the single test worker is still marked in-flight from the first dispatch
	e.setAccountDailyState(t, acctShadow, 100, 0, todayUTC(), false, nil) // ensure still eligible (defensive re-arm)
	// auth_failure requeues with a ZERO backoff (next_attempt_at=NOW()), so jobShadow itself would
	// win the very next claim (it's older) — delete it so job2 is unambiguously the one dispatched,
	// isolating "does the account still send" from "which job the claim happens to pick".
	if _, err := e.admin.Exec(ctx, `DELETE FROM send_managed.jobs WHERE id=$1`, jobShadow); err != nil {
		t.Fatalf("cleanup jobShadow: %v", err)
	}
	job2 := e.seedQueuedJob(t, "shadow2@example.com")
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch after shadow decision: %v", err)
	}
	if state2, _, _, _, _ := e.jobState(t, job2); state2 != "sending" {
		t.Fatalf("a non-quarantined shadow account did not keep sending (state=%s) — shadow must not silently block sends", state2)
	}
	e.px.freeAllWorkers()

	// PAIRED POSITIVE CONTROL: promote to enforce and drive the IDENTICAL condition on a fresh
	// account. This time quarantined=true and a REAL 'quarantine' event is written.
	e.setConfigStr(t, "autoquarantine_mode", "enforce")
	acctEnforce := e.seedAccount(t, pool, "sec-enforce")
	jobEnforce := e.seedQueuedJob(t, "enforce@example.com")
	e.dispatchToSending(t, jobEnforce)
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: jobEnforce, Outcome: workerproto.OutcomeError,
		SMTPResponse: "535 5.7.8 Authentication credentials invalid"})
	_, quarantinedNow, _ := e.accountFlagsNow(t, acctEnforce)
	if !quarantinedNow {
		t.Fatalf("account NOT quarantined under autoquarantine_mode=enforce on the identical condition — the mode gate itself is broken, not just shadow's non-action")
	}
	evs := e.systemAccountEvents(t, acctEnforce)
	found := false
	for _, ev := range evs {
		if ev[0] == "quarantine" && ev[1] == "auth_failure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no real quarantine event under enforce: %v", evs)
	}
}

// TestShadowMode_OffDisablesDecisionEntirely — autoquarantine_mode=off means quarantineDecision is
// never even evaluated: no flag change AND no shadow observation row, on the identical
// score-crossing condition that quarantines under enforce (statemachine_test.go's own recipe).
func TestShadowMode_OffDisablesDecisionEntirely(t *testing.T) {
	e := newDispatchEnv(t, "quaroff")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec-off")
	job := e.seedQueuedJob(t, "off@example.com")
	e.setConfigStr(t, "autoquarantine_mode", "off")

	// Drive the same 4-transient-failure recipe that crosses quarantine_threshold under enforce.
	for i := 0; i < 5; i++ {
		if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
			t.Fatalf("reset next: %v", err)
		}
		e.dispatchToSending(t, job)
		e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
			SMTPResponse: "451 4.3.0 temporary local error"})
		e.px.freeAllWorkers()
	}

	score, _, _ := e.accountScoreState(t, acct)
	if score >= 30 {
		t.Fatalf("score=%d never dropped below the default threshold (30) — test setup broken, can't prove off suppressed anything", score)
	}
	_, quarantined, _ := e.accountFlagsNow(t, acct)
	if quarantined {
		t.Fatalf("account quarantined despite autoquarantine_mode=off — off must disable the decision entirely")
	}
	for _, ev := range e.systemAccountEvents(t, acct) {
		if ev[0] == "quarantine" || (ev[0] == "updated" && containsAll(ev[1], "shadow_would_quarantine")) {
			t.Fatalf("found a quarantine/shadow event under mode=off: %v — off means NO decision, NO row", ev)
		}
	}
}

// TestKillSwitch_AutopauseDisablesOnlyAutopause (O3) — autopause_enabled=false means a throttle
// outcome does NOT pause the account, while auto-quarantine (a DIFFERENT condition, unaffected by
// this switch) still fires normally — proving the kill-switch is scoped to its own subsystem.
func TestKillSwitch_AutopauseDisablesOnlyAutopause(t *testing.T) {
	e := newDispatchEnv(t, "autopauseoff")
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec-throttle")
	job := e.seedQueuedJob(t, "throttle@example.com")

	e.setConfigBool(t, "autopause_enabled", false)
	e.dispatchToSending(t, job)
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
		SMTPResponse: "450 4.7.1 rate limit exceeded, slow down"})

	paused, quarantined, reason := e.accountFlagsNow(t, acct)
	if paused {
		t.Fatalf("account paused despite autopause_enabled=false (reason=%v) — the kill-switch must disable auto-pause entirely", reason)
	}
	if quarantined {
		t.Fatalf("a throttle outcome quarantined the account — that's not this policy's job under any switch")
	}

	// Positive control: the SAME throttle condition on a fresh account WITH the switch on DOES pause.
	e.px.freeAllWorkers() // the single test worker is still marked in-flight from the first dispatch
	e.setConfigBool(t, "autopause_enabled", true)
	acct2 := e.seedAccount(t, pool, "sec-throttle2")
	job2 := e.seedQueuedJob(t, "throttle2@example.com")
	e.dispatchToSending(t, job2)
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job2, Outcome: workerproto.OutcomeError,
		SMTPResponse: "450 4.7.1 rate limit exceeded, slow down"})
	paused2, _, reason2 := e.accountFlagsNow(t, acct2)
	if !paused2 || reason2 == nil || *reason2 != "throttle" {
		t.Fatalf("account2 paused=%v reason=%v with autopause_enabled=true, want true/throttle — the switch gate itself is broken", paused2, reason2)
	}
}

// TestKillSwitch_VariationDisablesEntireLayer (O3) — variation_enabled=false bypasses the WHOLE
// Phase 8 variation layer regardless of individual toggles: the push_job.variation snapshot must
// be nil/absent (the worker's own identity-function default), not merely "all fields off" — proving
// the master switch acts at the snapshot level, not by silently flipping each toggle.
func TestKillSwitch_VariationDisablesEntireLayer(t *testing.T) {
	e := newDispatchEnv(t, "variationoff")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.seedQueuedJob(t, "varoff@example.com")

	// Turn ON an individual toggle so a naive "each toggle already defaults off" implementation
	// would be caught if variation_enabled didn't actually suppress it.
	e.setConfigBool(t, "variation_html_minify", true)
	e.setConfigBool(t, "variation_enabled", false)

	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	pj, ok := e.px.lastPush()
	if !ok {
		t.Fatal("no push recorded")
	}
	if pj.Variation != nil {
		t.Fatalf("push_job.variation=%+v with variation_enabled=false, want nil (baseline Phase 3 assembly, byte-identical — §2.3)", pj.Variation)
	}

	// Positive control: the SAME individual toggle, with the master switch back on, DOES appear.
	e.px.freeAllWorkers() // the single test worker is still marked in-flight from the first dispatch
	e.setConfigBool(t, "variation_enabled", true)
	e.seedQueuedJob(t, "varon@example.com")
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch after re-enable: %v", err)
	}
	pj2, ok := e.px.lastPush()
	if !ok {
		t.Fatal("no push recorded (2nd)")
	}
	if pj2.Variation == nil || !pj2.Variation.HTMLMinify {
		t.Fatalf("push_job.variation=%+v with variation_enabled=true, want HTMLMinify=true (the toggle takes effect again once the master switch is back on)", pj2.Variation)
	}
}

// containsAll reports whether s contains every one of subs as a substring (small test helper — no
// need to repeat strings.Contains at call sites with multiple checks).
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
