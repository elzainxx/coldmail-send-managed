package dispatch

import (
	"context"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// dispatchToSending claims + pushes the one queued job and asserts it reached sending, returning
// the account it was assigned. Shared setup for the per-class policy tests below.
func (e *dispatchEnv) dispatchToSending(t *testing.T, job string) {
	t.Helper()
	if _, err := e.d.TryDispatch(context.Background()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if state, _, _, _, _ := e.jobState(t, job); state != "sending" {
		t.Fatalf("precondition: state=%s, want sending", state)
	}
}

// accountFlagsNow reads an account's live flags (helper for the policy assertions).
func (e *dispatchEnv) accountFlagsNow(t *testing.T, acctID string) (paused, quarantined bool, pauseReason *string) {
	t.Helper()
	if err := e.admin.QueryRow(context.Background(),
		`SELECT paused, quarantined, pause_reason FROM send_managed.accounts WHERE id=$1`, acctID).
		Scan(&paused, &quarantined, &pauseReason); err != nil {
		t.Fatalf("read flags: %v", err)
	}
	return paused, quarantined, pauseReason
}

// systemAccountEvents returns the (action, reason) of every actor='system' account_events row for
// an account, in order — the audit cross-check the spec requires (§2, T8): a flag the code wrote
// is verified against the ledger, never only against the flag itself.
func (e *dispatchEnv) systemAccountEvents(t *testing.T, acctID string) [][2]string {
	t.Helper()
	rows, err := e.admin.Query(context.Background(),
		`SELECT action, COALESCE(reason,'') FROM send_managed.account_events
		 WHERE account_id=$1 AND actor='system' ORDER BY created_at`, acctID)
	if err != nil {
		t.Fatalf("read system events: %v", err)
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var a, r string
		if err := rows.Scan(&a, &r); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		out = append(out, [2]string{a, r})
	}
	return out
}

// TestPolicy_AuthFailureQuarantines — a 535 auth_failure requeues the job AND quarantines the
// account IMMEDIATELY (§4.2a), writing exactly one account_events(system, quarantine, auth_failure).
// Positive/negative controls: the account IS quarantined (positive), and it is NOT disabled and its
// pause flag is untouched (independence, §4.1 — quarantine never touches paused/disabled).
func TestPolicy_AuthFailureQuarantines(t *testing.T) {
	e := newDispatchEnv(t, "authfail")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "a@example.com")
	e.dispatchToSending(t, job)

	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
		SMTPResponse: "535 5.7.8 Authentication credentials invalid"})

	// Job requeued (§2.1), lease cleared, class recorded as auth_failure.
	state, _, leaseNull, _, _ := e.jobState(t, job)
	if state != "queued" || !leaseNull {
		t.Fatalf("job state=%s leaseNull=%v, want queued/true", state, leaseNull)
	}
	var ec *string
	if err := e.admin.QueryRow(ctx, `SELECT last_error_class::text FROM send_managed.jobs WHERE id=$1`, job).Scan(&ec); err != nil {
		t.Fatalf("class: %v", err)
	}
	if ec == nil || *ec != "auth_failure" {
		t.Fatalf("last_error_class=%v, want auth_failure", ec)
	}

	// Account quarantined (positive); paused/disabled untouched (independence).
	paused, quarantined, _ := e.accountFlagsNow(t, acct)
	if !quarantined {
		t.Fatalf("account not quarantined after auth_failure (§4.2a)")
	}
	if paused {
		t.Fatalf("auth_failure also paused the account — quarantine must not touch paused (§4.1)")
	}
	var disabled bool
	if err := e.admin.QueryRow(ctx, `SELECT disabled FROM send_managed.accounts WHERE id=$1`, acct).Scan(&disabled); err != nil {
		t.Fatalf("disabled: %v", err)
	}
	if disabled {
		t.Fatalf("auth_failure disabled the account — Phase 5 never auto-disables (§4.1)")
	}

	// Exactly one system quarantine event, reason=auth_failure (audit cross-check, §2/T8).
	ev := e.systemAccountEvents(t, acct)
	if len(ev) != 1 || ev[0][0] != "quarantine" || ev[0][1] != "auth_failure" {
		t.Fatalf("system account_events=%v, want [[quarantine auth_failure]]", ev)
	}
}

// TestPolicy_ThrottlePauses — a 421 rate-limit throttle requeues the job AND pauses the account
// with reason='throttle', stamps throttle_paused_at, and writes one account_events(system, pause,
// throttle). Negative control: the account is NOT quarantined (throttle pauses, never quarantines).
func TestPolicy_ThrottlePauses(t *testing.T) {
	e := newDispatchEnv(t, "throttle")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "b@example.com")
	e.dispatchToSending(t, job)

	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
		SMTPResponse: "421 4.7.0 Too many messages, rate limited"})

	// Job requeued, class=throttle, next_attempt_at ≈ NOW()+throttle_pause_seconds (future).
	state, _, leaseNull, _, _ := e.jobState(t, job)
	if state != "queued" || !leaseNull {
		t.Fatalf("job state=%s leaseNull=%v, want queued/true", state, leaseNull)
	}
	var ec *string
	if err := e.admin.QueryRow(ctx, `SELECT last_error_class::text FROM send_managed.jobs WHERE id=$1`, job).Scan(&ec); err != nil {
		t.Fatalf("class: %v", err)
	}
	if ec == nil || *ec != "throttle" {
		t.Fatalf("last_error_class=%v, want throttle", ec)
	}

	// Account paused (reason throttle), throttle_paused_at stamped, NOT quarantined.
	paused, quarantined, reason := e.accountFlagsNow(t, acct)
	if !paused || reason == nil || *reason != "throttle" {
		t.Fatalf("account paused=%v reason=%v, want paused/throttle", paused, reason)
	}
	if quarantined {
		t.Fatalf("throttle quarantined the account — throttle only pauses (§4.1/§4.3)")
	}
	var stamped bool
	if err := e.admin.QueryRow(ctx, `SELECT throttle_paused_at IS NOT NULL FROM send_managed.accounts WHERE id=$1`, acct).Scan(&stamped); err != nil {
		t.Fatalf("throttle_paused_at: %v", err)
	}
	if !stamped {
		t.Fatalf("throttle pause did not stamp throttle_paused_at (§4.3 clock)")
	}

	ev := e.systemAccountEvents(t, acct)
	if len(ev) != 1 || ev[0][0] != "pause" || ev[0][1] != "throttle" {
		t.Fatalf("system account_events=%v, want [[pause throttle]]", ev)
	}
}

// TestPolicy_AttemptCountSaturates — the retry-model reconciliation (§6.2): a job requeued MORE
// than max_attempts times has attempt_count rise to max_attempts and then STAY there, with NO
// 23514 CHECK violation ever, and the job is NEVER failed by reaching the cap (it stays queued).
// This is the teeth of "job failure is decoupled from attempt_count; only pool exhaustion fails."
func TestPolicy_AttemptCountSaturates(t *testing.T) {
	e := newDispatchEnv(t, "saturate")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	// Give the account a RECENT success so the recency penalty stays ~0 and its score floors at
	// ~40 (100 - w_fail) even when consecutive_failures saturates — keeping it ABOVE the
	// quarantine_threshold (30). This isolates the property under test (attempt_count saturation)
	// from auto-quarantine (§4.2b), which is exercised separately in the Chunk 4 tests. Without a
	// recent success the never-succeeded recency penalty would (correctly) push the score < 30 and
	// quarantine the account, ending the job's ability to reach 'sending'.
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET last_success_at = NOW() WHERE id=$1`, acct); err != nil {
		t.Fatalf("seed last_success_at: %v", err)
	}
	job := e.seedQueuedJob(t, "c@example.com")

	// max_attempts defaults to 3 (locked). Drive 6 transient (timeout) requeues — well past the cap.
	for i := 0; i < 6; i++ {
		// Reset next_attempt_at so the claim can re-pick it despite backoff, and re-dispatch.
		if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
			t.Fatalf("reset next_attempt_at: %v", err)
		}
		e.dispatchToSending(t, job)
		e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
			ErrorReason: workerproto.ReasonConnectRefused, SMTPResponse: ""})
		e.px.freeAllWorkers() // the report frees the worker for the next claim (mirrors real worker report handling)
		state, _, _, _, attempt := e.jobState(t, job)
		if state != "queued" {
			t.Fatalf("after requeue %d: state=%s, want queued (never failed by attempt cap, §6.2)", i+1, state)
		}
		if attempt > 3 {
			t.Fatalf("attempt_count=%d exceeded max_attempts=3 — CHECK should have saturated (§6.2)", attempt)
		}
	}
	// Final: saturated at exactly 3, still queued, never failed.
	state, _, _, _, attempt := e.jobState(t, job)
	if state != "queued" || attempt != 3 {
		t.Fatalf("final state=%s attempt=%d, want queued/3 (saturated, not failed)", state, attempt)
	}
}
