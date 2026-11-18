package dispatch

// attempt_count_meaning_test.go — Phase 3 close-out B4: WHAT `attempt_count` COUNTS, pinned.
//
// `attempt_count` is the integer the platform sees on GET /v1/report (single job and campaign items). It is NOT the
// number of SMTP attempts. The documented meaning (Docs/api-contract.md, "What `attempt_count` counts") is exactly
// this, and every line of it is asserted below against the real dispatcher, outcome handler and reclaim loop:
//
//	It starts at 0 and a send that succeeds at the first try reports 0.
//	It goes up by ONE, saturating at max_attempts (3: it never reads above 3), each time
//	  (a) a worker reports a classified FAILURE of an attempt (the job is requeued, or ends failed/cancelled);
//	  (b) the reclaim loop takes back a job whose lease expired or whose worker went silent — assigned OR sending;
//	  (c) the dispatcher requeues a job that NEVER REACHED SMTP: no_eligible_account, no_worker_available,
//	      push_failed, or the header-injection guard.
//	It does NOT change on: a claim, assigned -> sending, a successful send, a bounce, a cancel, a worker reconnect
//	  re-delivering an outcome the Control already applied (a duplicate or stale report is a no-op), or pool
//	  exhaustion.
//	The real counter of failed SMTP attempts is failed_attempt_count (not exposed): (a) and (b)-while-sending move
//	  it; (c) and (b)-while-assigned never do.
//
// The paired control of every "+1" rule is the "unchanged" rule next to it, and the saturation test shows the
// ceiling: a counter that stopped counting, counted twice, or counted a claim fails one of them.

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// ac reads (attempt_count, failed_attempt_count, state) of a job.
func (e *dispatchEnv) ac(t *testing.T, job string) (attempt, failed int, state string) {
	t.Helper()
	if err := e.admin.QueryRow(context.Background(),
		`SELECT attempt_count, failed_attempt_count, state::text FROM send_managed.jobs WHERE id=$1`, job).Scan(&attempt, &failed, &state); err != nil {
		t.Fatalf("read attempt counters: %v", err)
	}
	return attempt, failed, state
}

func (e *dispatchEnv) requireAC(t *testing.T, what, job string, wantAttempt, wantFailed int, wantState string) {
	t.Helper()
	a, f, s := e.ac(t, job)
	if a != wantAttempt || f != wantFailed || s != wantState {
		t.Fatalf("%s: attempt_count=%d failed_attempt_count=%d state=%s, want %d / %d / %s", what, a, f, s, wantAttempt, wantFailed, wantState)
	}
}

// acEnv: one env, one pool, accounts with a recent success (so repeated failures never quarantine the account and
// end the test's ability to dispatch — the property under test is the COUNTER), the attempt cap lifted out of the way.
func acEnv(t *testing.T, suffix string) *dispatchEnv {
	t.Helper()
	e := newDispatchEnv(t, suffix)
	pool := e.seedPool(t)
	for i := 0; i < 3; i++ {
		acct := e.seedAccount(t, pool, "sec")
		if _, err := e.admin.Exec(context.Background(), `UPDATE send_managed.accounts SET last_success_at = NOW() WHERE id=$1`, acct); err != nil {
			t.Fatalf("seed last_success_at: %v", err)
		}
	}
	e.setConfigInt(t, "job_max_failed_attempts", 1000)
	return e
}

func timeoutReport(job string, token int64) workerproto.Report {
	return workerproto.Report{Type: workerproto.TypeReport, JobID: job, AttemptToken: token, Outcome: workerproto.OutcomeError,
		ErrorReason: workerproto.ReasonConnectRefused}
}

// A first-try send, a bounce: attempt_count stays 0 through claim, assigned -> sending and the terminal report.
func TestAttemptCount_ClaimSendingSuccessAndBounceDoNotCount(t *testing.T) {
	e := acEnv(t, "acok")

	ok := e.seedQueuedJob(t, "ac-ok@example.com")
	e.requireAC(t, "queued", ok, 0, 0, "queued")
	e.dispatchToSending(t, ok) // claim (queued -> assigned) and assigned -> sending
	e.requireAC(t, "claimed and sending", ok, 0, 0, "sending")
	e.oh.Handle(e.px.workerID, workerproto.Report{Type: workerproto.TypeReport, JobID: ok, Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "PM-AC-1", SMTPResponse: "250 ok"})
	e.requireAC(t, "a send that succeeded at the first try", ok, 0, 0, "sent")
	e.px.freeAllWorkers()

	bounced := e.seedQueuedJob(t, "ac-bounce@example.com")
	e.dispatchToSending(t, bounced)
	e.oh.Handle(e.px.workerID, workerproto.Report{Type: workerproto.TypeReport, JobID: bounced, Outcome: workerproto.OutcomeBounced, SMTPResponse: "550 no such user"})
	e.requireAC(t, "a bounce", bounced, 0, 0, "bounced")
}

// (a) A worker-reported failure counts one each time, saturates at 3, and the real counter keeps going.
func TestAttemptCount_WorkerReportedFailureCountsOne_SaturatesAtThree(t *testing.T) {
	e := acEnv(t, "acfail")
	job := e.seedQueuedJob(t, "ac-fail@example.com")
	wantAttempt := []int{1, 2, 3, 3, 3}
	for i, want := range wantAttempt {
		if _, err := e.admin.Exec(context.Background(), `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
			t.Fatalf("reset next_attempt_at: %v", err)
		}
		e.dispatchToSending(t, job)
		e.requireAC(t, "dispatched again, before the report", job, min(i, 3), i, "sending") // the claim itself added nothing
		e.oh.Handle(e.px.workerID, timeoutReport(job, 0))
		e.px.freeAllWorkers()
		e.requireAC(t, "after a worker-reported failure", job, want, i+1, "queued")
	}
}

// (a) paired control, the replay: the SAME report delivered again (a worker reconnect re-sends an outcome the
// Control may already have applied) is a no-op — it does not count a second time.
func TestAttemptCount_AReplayedOrStaleReportDoesNotCount(t *testing.T) {
	e := acEnv(t, "acreplay")
	job := e.seedQueuedJob(t, "ac-replay@example.com")
	e.dispatchToSending(t, job)
	rep := timeoutReport(job, 0)
	e.oh.Handle(e.px.workerID, rep)
	e.requireAC(t, "the failure applied once", job, 1, 1, "queued")
	e.oh.Handle(e.px.workerID, rep) // the reconnect re-delivery
	e.oh.Handle(e.px.workerID, rep)
	e.requireAC(t, "the same report delivered twice more (the job is not sending any more)", job, 1, 1, "queued")

	// a stale report from a superseded attempt, delivered while the job is in flight again
	e.px.freeAllWorkers()
	if _, err := e.admin.Exec(context.Background(), `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
		t.Fatalf("reset next_attempt_at: %v", err)
	}
	e.dispatchToSending(t, job)
	var token int64
	if err := e.admin.QueryRow(context.Background(), `SELECT attempt_token FROM send_managed.jobs WHERE id=$1`, job).Scan(&token); err != nil {
		t.Fatalf("read attempt_token: %v", err)
	}
	e.oh.Handle(e.px.workerID, timeoutReport(job, token-1)) // the first attempt's outcome, replayed late
	e.requireAC(t, "a stale-token report while the second attempt is in flight", job, 1, 1, "sending")
	// control: the CURRENT attempt's report does count
	e.oh.Handle(e.px.workerID, timeoutReport(job, token))
	e.requireAC(t, "the current attempt's failure", job, 2, 2, "queued")
}

// (c) A dispatcher-side requeue that never reached SMTP counts one, and does NOT touch the real counter.
func TestAttemptCount_DispatcherSideRequeuesCountButNeverReachedSMTP(t *testing.T) {
	ctx := context.Background()

	t.Run("push_failed", func(t *testing.T) {
		e := acEnv(t, "acpushfail")
		job := e.seedQueuedJob(t, "ac-push@example.com")
		e.px.failPush = true
		if _, err := e.d.TryDispatch(ctx); err != nil {
			t.Fatalf("TryDispatch: %v", err)
		}
		if e.px.pushCount() != 0 {
			t.Fatalf("a push that failed was recorded")
		}
		e.requireAC(t, "push_failed", job, 1, 0, "queued")
	})

	t.Run("header_injection", func(t *testing.T) {
		e := acEnv(t, "achdr")
		job := e.seedQueuedJobSubject(t, "ac-hdr@example.com", "Hello\r\nBcc: x@example.com")
		if _, err := e.d.TryDispatch(ctx); err != nil {
			t.Fatalf("TryDispatch: %v", err)
		}
		e.requireAC(t, "header_injection", job, 1, 0, "queued")
	})

	t.Run("no_eligible_account", func(t *testing.T) {
		e := acEnv(t, "acnoacct")
		job := e.seedQueuedJob(t, "ac-noacct@example.com")
		claimed, ok, err := e.d.claimOne(ctx, e.px.workerID, 120)
		if err != nil || !ok || claimed.id != job {
			t.Fatalf("claimOne: %v ok=%v id=%s", err, ok, claimed.id)
		}
		e.requireAC(t, "claimed", job, 0, 0, "assigned")
		e.d.requeueNoAccount(ctx, job)
		e.requireAC(t, "no_eligible_account", job, 1, 0, "queued")
	})

	t.Run("no_worker_available", func(t *testing.T) {
		e := acEnv(t, "acnoworker")
		job := e.seedQueuedJob(t, "ac-noworker@example.com")
		saved := e.px.workerID
		e.px.workerID = "" // no connected worker: a tick claims the job and requeues it
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		e.px.workerID = saved
		e.requireAC(t, "no_worker_available", job, 1, 0, "queued")
	})
}

// (b) A reclaim counts one — for a job that was SENDING it is also a failed SMTP attempt, for one that was only
// ASSIGNED (claimed, never pushed) it is not.
func TestAttemptCount_ReclaimCountsOne_FailedAttemptOnlyWhileSending(t *testing.T) {
	e := acEnv(t, "acreclaim")
	ctx := context.Background()

	sending := e.seedQueuedJob(t, "ac-rec-sending@example.com")
	e.dispatchToSending(t, sending)
	e.px.freeAllWorkers()
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET lease_expires_at = NOW() - interval '5 seconds' WHERE id=$1`, sending); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	assigned := e.seedQueuedJob(t, "ac-rec-assigned@example.com")
	claimed, ok, err := e.d.claimOne(ctx, e.px.workerID, 120)
	if err != nil || !ok || claimed.id != assigned {
		t.Fatalf("claimOne: %v ok=%v id=%s", err, ok, claimed.id)
	}
	e.requireAC(t, "claimed, never pushed", assigned, 0, 0, "assigned")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET lease_expires_at = NOW() - interval '5 seconds' WHERE id=$1`, assigned); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	// keep the worker alive so only the lease-expiry detector fires
	e.setWorkerHeartbeat(t, e.px.workerID, 0)

	if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 2 {
		t.Fatalf("ReclaimTick: reclaimed %d err=%v, want 2", n, err)
	}
	e.requireAC(t, "reclaimed while SENDING (a failed SMTP attempt)", sending, 1, 1, "queued")
	e.requireAC(t, "reclaimed while only ASSIGNED (never reached a worker)", assigned, 1, 0, "queued")
}

// Pool exhaustion fails a job without counting an attempt: nothing was attempted.
func TestAttemptCount_PoolExhaustionDoesNotCount(t *testing.T) {
	e := acEnv(t, "acexhaust")
	job := e.seedQueuedJob(t, "ac-exhaust@example.com")
	// quarantine every account: the pool's whole chain is terminal-blocked
	if _, err := e.admin.Exec(context.Background(), `UPDATE send_managed.accounts SET quarantined = true`); err != nil {
		t.Fatalf("quarantine accounts: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, err := e.d.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if _, _, s := e.ac(t, job); s == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	e.requireAC(t, "failed by pool exhaustion", job, 0, 0, "failed")
}
