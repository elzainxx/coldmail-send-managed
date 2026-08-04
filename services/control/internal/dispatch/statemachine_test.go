package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// TestStateMachine_ScoreCrossingAutoQuarantines (T5) — driving consecutive transient failures on a
// never-succeeded account crosses the score below quarantine_threshold and auto-quarantines it in
// the SAME outcome transaction (§4.2b), with one account_events(system, quarantine,
// score_below_threshold). Then: the score HOLDS while quarantined (no outcomes → no recompute,
// §3.3), and recovery does NOT happen by the score (§4.2 no-auto-unquarantine).
func TestStateMachine_ScoreCrossingAutoQuarantines(t *testing.T) {
	e := newDispatchEnv(t, "scorequar")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "q@example.com")

	// A never-succeeded account (last_success_at NULL → recency penalty 1.0 = -25) crossing the
	// failure ramp will fall below 30. With default weights: 100 - 60*min(1,n/5) - 25(recency).
	// n=1 → 100-12-25=63; n=2 → 51; n=3 → 39; n=4 → 27 < 30 → quarantine at the 4th failure.
	quarantinedAt := 0
	for i := 1; i <= 5; i++ {
		if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
			t.Fatalf("reset next: %v", err)
		}
		// Once quarantined the account is ineligible, so the job can't reach sending anymore — stop.
		var q bool
		if err := e.admin.QueryRow(ctx, `SELECT quarantined FROM send_managed.accounts WHERE id=$1`, acct).Scan(&q); err != nil {
			t.Fatalf("read quar: %v", err)
		}
		if q {
			quarantinedAt = i - 1
			break
		}
		e.dispatchToSending(t, job)
		e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
			SMTPResponse: "451 4.3.0 temporary local error"})
		e.px.freeAllWorkers()
	}
	if quarantinedAt == 0 {
		t.Fatalf("account never auto-quarantined across 5 transient failures (§4.2b)")
	}

	// Exactly one system quarantine event with reason=score_below_threshold (NOT auth_failure).
	ev := e.systemAccountEvents(t, acct)
	var quarEvents int
	for _, x := range ev {
		if x[0] == "quarantine" {
			quarEvents++
			if x[1] != "score_below_threshold" {
				t.Fatalf("quarantine reason=%q, want score_below_threshold", x[1])
			}
		}
	}
	if quarEvents != 1 {
		t.Fatalf("system quarantine events=%d, want exactly 1 (idempotent §4.2)", quarEvents)
	}

	// Score HOLDS while quarantined: snapshot, run more dispatch ticks, assert unchanged (§3.3).
	var scoreHeld int
	if err := e.admin.QueryRow(ctx, `SELECT account_status_score FROM send_managed.accounts WHERE id=$1`, acct).Scan(&scoreHeld); err != nil {
		t.Fatalf("held score: %v", err)
	}
	if scoreHeld >= 30 {
		t.Fatalf("quarantined at score %d, expected < 30", scoreHeld)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
			t.Fatalf("reset: %v", err)
		}
		if _, err := e.d.TryDispatch(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	var scoreAfter int
	var quarStill bool
	if err := e.admin.QueryRow(ctx, `SELECT account_status_score, quarantined FROM send_managed.accounts WHERE id=$1`, acct).Scan(&scoreAfter, &quarStill); err != nil {
		t.Fatalf("after ticks: %v", err)
	}
	if scoreAfter != scoreHeld {
		t.Fatalf("quarantined account's score drifted %d -> %d (must HOLD, §3.3)", scoreHeld, scoreAfter)
	}
	if !quarStill {
		t.Fatalf("account auto-recovered from quarantine (must be manual-only, §4.2)")
	}
}

// TestStateMachine_ThrottleAutoClears (T3) — a throttle-paused account auto-clears at the throttle
// timer (lazily, at the next consume) once NOW() >= throttle_paused_at + throttle_pause_seconds,
// un-pausing itself and writing account_events(system, unpause). Negative control: a MANUAL pause
// in the same pool is NEVER swept up by the throttle auto-clear.
func TestStateMachine_ThrottleAutoClears(t *testing.T) {
	e := newDispatchEnv(t, "throttleclear")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")

	// Put the account into a throttle pause whose window has ALREADY elapsed: set throttle_paused_at
	// far in the past (throttle_pause_seconds default 900s; -1h is well past). This is the exact
	// post-throttle state (paused, reason throttle, timestamp stamped) the clock seam lets us test.
	if _, err := e.admin.Exec(ctx,
		`UPDATE send_managed.accounts SET paused=TRUE, pause_reason='throttle', throttle_paused_at=NOW()-INTERVAL '1 hour' WHERE id=$1`, acct); err != nil {
		t.Fatalf("seed throttle pause: %v", err)
	}

	// A second account, MANUALLY paused, in the SAME pool — must NOT be swept by throttle clearing.
	manualAcct := e.seedAccount(t, pool, "sec2")
	if _, err := e.admin.Exec(ctx,
		`UPDATE send_managed.accounts SET paused=TRUE, pause_reason='manual' WHERE id=$1`, manualAcct); err != nil {
		t.Fatalf("seed manual pause: %v", err)
	}

	// A consume should pick the expired-throttle account (eligible via P5-13), self-clear its pause,
	// and leave the manual pause untouched.
	job := e.seedQueuedJob(t, "tc@example.com")
	e.dispatchToSending(t, job)

	// The expired-throttle account is now un-paused (self-heal) and it was the one used.
	var paused bool
	var reason *string
	var stamp *time.Time
	if err := e.admin.QueryRow(ctx, `SELECT paused, pause_reason, throttle_paused_at FROM send_managed.accounts WHERE id=$1`, acct).Scan(&paused, &reason, &stamp); err != nil {
		t.Fatalf("read throttle acct: %v", err)
	}
	if paused || reason != nil || stamp != nil {
		t.Fatalf("expired-throttle account not self-cleared: paused=%v reason=%v stamp=%v (§4.3)", paused, reason, stamp)
	}
	// account_events(system, unpause) written for the self-heal.
	ev := e.systemAccountEvents(t, acct)
	if len(ev) != 1 || ev[0][0] != "unpause" {
		t.Fatalf("expired-throttle system events=%v, want [[unpause ...]]", ev)
	}

	// Negative control: the MANUAL pause is untouched — still paused, reason manual, NO unpause event.
	var mPaused bool
	var mReason *string
	if err := e.admin.QueryRow(ctx, `SELECT paused, pause_reason FROM send_managed.accounts WHERE id=$1`, manualAcct).Scan(&mPaused, &mReason); err != nil {
		t.Fatalf("read manual acct: %v", err)
	}
	if !mPaused || mReason == nil || *mReason != "manual" {
		t.Fatalf("manual pause swept by throttle clear: paused=%v reason=%v (§4.3 reason-scoped)", mPaused, mReason)
	}
	if mev := e.systemAccountEvents(t, manualAcct); len(mev) != 0 {
		t.Fatalf("manual-paused account got system events %v, want none", mev)
	}
}

// TestStateMachine_InWindowThrottleNotEligible — a throttle pause STILL WITHIN its window is NOT
// admitted by the consume (the negative control for the auto-clear: it clears at the timer, not
// on any throttle pause). Uses a fresh stamp (NOW) so the 900s window is not elapsed.
func TestStateMachine_InWindowThrottleNotEligible(t *testing.T) {
	e := newDispatchEnv(t, "throttlelive")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	if _, err := e.admin.Exec(ctx,
		`UPDATE send_managed.accounts SET paused=TRUE, pause_reason='throttle', throttle_paused_at=NOW() WHERE id=$1`, acct); err != nil {
		t.Fatalf("seed live throttle: %v", err)
	}

	// The only account is throttle-paused in-window → the pool has no eligible account → the job
	// stays queued (the pre-claim gate leaves it untouched). TryDispatch is a clean no-op.
	job := e.seedQueuedJob(t, "lw@example.com")
	pushed, err := e.d.TryDispatch(ctx)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if pushed != "" {
		t.Fatalf("dispatched a job onto an in-window throttle-paused account (must be ineligible §4.3)")
	}
	if state, _, _, _, _ := e.jobState(t, job); state != "queued" {
		t.Fatalf("job state=%s, want queued (in-window throttle account not selectable)", state)
	}
	// The pause is untouched (not cleared early), no unpause event.
	var paused bool
	if err := e.admin.QueryRow(ctx, `SELECT paused FROM send_managed.accounts WHERE id=$1`, acct).Scan(&paused); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !paused {
		t.Fatalf("in-window throttle pause was cleared early (§4.3 window-scoped)")
	}
}
