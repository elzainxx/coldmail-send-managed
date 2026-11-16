package dispatch

// attempts_cap_test.go — M2-G2: the attempt cap (send_managed_0028, attempts_cap.go).
//
// Before this, ONE writer produced `failed` (pool exhaustion). Every other SMTP failure that was not a hard
// bounce returned the job to `queued` with unbounded retries, so a persistent 554 or a dead credential
// never became terminal. These tests drive REAL failed attempts through the real claim -> handoff ->
// outcome path (a recording pusher stands in only for the SMTP wire, exactly as every other dispatch test
// does) and assert, with the dial at its seeded default:
//
//   - the job is `failed`/`attempts_exhausted` EXACTLY at the cap — the 6th failed attempt, not the 5th —
//     with completed_at, an event row, and the counter asserted; the 5-failure control is still queued;
//   - a dial change applies to the NEXT failed attempt, in both directions (it is read fresh);
//   - dispatcher-side requeues that never reached SMTP (no_worker_available, push_failed,
//     no_eligible_account) are NEVER counted, however many there are — they mean "waiting";
//   - the age cap counts from the FIRST FAILED attempt, not from creation, and has a control below it;
//   - a reclaim of a SENDING job counts; a reclaim of an ASSIGNED job (never reached a worker) does not;
//   - pool exhaustion keeps its own reason, and a terminal job is never re-opened by a replayed report.
//
// autoquarantine_mode is set to `off` throughout: a run of failures on the one account would otherwise
// quarantine it (score below threshold, enforce mode) and the job could not be dispatched again, which
// would end the loop for a reason that has nothing to do with the cap. Account health is exercised by its
// own suites; here it is taken out of the picture on purpose.

import (
	"context"
	"strings"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// persistent554 is a policy rejection that classifies as `unknown` (a 554 is neither a recipient bounce nor
// an auth failure nor a 4xx): the classic "persistent 5xx" that used to retry forever.
const persistent554 = "554 5.7.1 Transaction failed: policy rejection"

// systemTenant is the user_id seedQueuedJob gives every job it creates (and its campaign_id too).
const systemTenant = "00000000-0000-0000-0000-000000000000"

// newCapEnv builds an env with one pool + one account and ONE queued job, account quarantine off.
func newCapEnv(t *testing.T, suffix string) (*dispatchEnv, string) {
	t.Helper()
	e := newDispatchEnv(t, suffix)
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.setConfigStr(t, "autoquarantine_mode", "off")
	e.setConfigStr(t, "autopause_mode", "off")
	return e, e.seedQueuedJob(t, suffix+"@example.com")
}

// failedAttempt drives ONE real failed SMTP attempt on job: make it claimable, claim and push it through the
// production path, and deliver the worker's failure report to the production outcome handler.
func (e *dispatchEnv) failedAttempt(t *testing.T, job, smtpResponse string) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(),
		`UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
		t.Fatalf("make job claimable: %v", err)
	}
	e.px.freeAllWorkers()
	e.dispatchToSending(t, job)
	tok := e.jobAttemptToken(t, job)
	if !e.oh.Handle(e.jobWorker(t, job), workerproto.Report{JobID: job, AttemptToken: tok,
		Outcome: workerproto.OutcomeError, SMTPResponse: smtpResponse}) {
		t.Fatalf("the failure report for job %s was not resolved", job)
	}
}

// TestAttemptCap_FailedExactlyAtTheCap — the headline. At the seeded default (6) five failed attempts leave
// the job queued with the counter at 5 (the control BELOW the cap); the SIXTH moves it to failed with
// reason attempts_exhausted, completed_at set, the lease cleared and a sending->failed event carrying the
// class — and the caller's report says why.
func TestAttemptCap_FailedExactlyAtTheCap(t *testing.T) {
	e, job := newCapEnv(t, "capexact")
	ctx := context.Background()

	cfg := e.mustLoadConfig(t)
	if cfg.JobMaxFailedAttempts != 6 || cfg.JobFailedMaxAgeSec != 86400 {
		t.Fatalf("seeded dials = (%d, %d), want the documented defaults (6, 86400) — this test asserts the cap AT its default",
			cfg.JobMaxFailedAttempts, cfg.JobFailedMaxAgeSec)
	}
	pc := e.newPlatformClient(t)

	for i := 1; i <= 5; i++ {
		e.failedAttempt(t, job, persistent554)
		c := e.jobColsOf(t, job)
		if c.State != "queued" || c.FailedAttemptCount != i || !c.FirstFailedSet || c.TerminalReason != "" || c.CompletedSet || c.CancelRequestedSet {
			t.Fatalf("after failed attempt %d of a cap of 6: %+v, want still queued with failed_attempt_count=%d, no terminal_reason, not completed", i, c, i)
		}
	}
	// Below the cap the report still says "queued", and says why (the class of the last failure).
	rep := pc.jobReport(t, job, systemTenant)
	if st, _ := rep.strOrNil("state"); st != "queued" {
		t.Fatalf("report after 5 failures: %s, want queued", rep.raw)
	}
	if r, _ := rep.strOrNil("reason"); r != "unknown" {
		t.Fatalf("report reason after 5 failures = %q, want unknown (the class of the last failed attempt): %s", r, rep.raw)
	}

	e.failedAttempt(t, job, persistent554) // the 6th

	c := e.jobColsOf(t, job)
	if c.State != "failed" || c.FailedAttemptCount != 6 || c.TerminalReason != "attempts_exhausted" ||
		!c.CompletedSet || !c.LeaseNull || c.LastErrorClass != "unknown" || c.HasPMID {
		t.Fatalf("after the 6th failed attempt: %+v, want failed/attempts_exhausted, count=6, completed_at set, lease cleared, class unknown, never sent", c)
	}
	chain := e.jobEventChain(t, job)
	last := chain[len(chain)-1]
	if last[0] != "sending" || last[1] != "failed" || last[2] != "box" || last[3] != "unknown" || !strings.Contains(last[4], "554") {
		t.Fatalf("last job_events row = %v, want [sending failed box unknown <message naming the 554>] — the terminal transition must have its event row", last)
	}
	if n := e.sendLogCount(t, job); n != 6 {
		t.Fatalf("send_logs rows = %d, want 6 (every failed attempt is logged)", n)
	}

	// What the caller sees: failed, why, and the class — and none of the SMTP text.
	rep = pc.jobReport(t, job, systemTenant)
	if st, _ := rep.strOrNil("state"); st != "failed" {
		t.Fatalf("report state = %q, want failed: %s", st, rep.raw)
	}
	if r, _ := rep.strOrNil("reason"); r != "attempts_exhausted" {
		t.Fatalf("report reason = %q, want attempts_exhausted: %s", r, rep.raw)
	}
	if ec, _ := rep.strOrNil("error_class"); ec != "unknown" {
		t.Fatalf("report error_class = %q, want unknown: %s", ec, rep.raw)
	}
	if !rep.hasKey("completed_at") {
		t.Fatalf("report of a failed job has no completed_at: %s", rep.raw)
	}
	if strings.Contains(rep.raw, "Transaction failed") || strings.Contains(strings.ToLower(rep.raw), "policy") {
		t.Fatalf("the report leaked SMTP text: %s", rep.raw)
	}
	camp := pc.campaignReport(t, systemTenant, systemTenant)
	if f := camp.totals(t)["failed"]; f != 1.0 {
		t.Fatalf("campaign totals.failed = %v, want 1: %s", f, camp.raw)
	}
	its := camp.items(t)
	if len(its) != 1 || its[0]["reason"] != "attempts_exhausted" || its[0]["error_class"] != "unknown" {
		t.Fatalf("campaign item = %v, want reason=attempts_exhausted error_class=unknown", its)
	}

	// TERMINAL STAYS TERMINAL: a replayed failure report finds no `sending` row and changes nothing — the
	// cap cannot fire twice, and cannot re-open or re-count a job somebody already resolved.
	tok := e.jobAttemptToken(t, job)
	if !e.oh.Handle(e.jobWorker(t, job), workerproto.Report{JobID: job, AttemptToken: tok,
		Outcome: workerproto.OutcomeError, SMTPResponse: persistent554}) {
		t.Fatal("the replayed failure report was not acked as a no-op")
	}
	if c2 := e.jobColsOf(t, job); c2.State != "failed" || c2.FailedAttemptCount != 6 {
		t.Fatalf("after a replayed report: %+v, want unchanged failed/count=6", c2)
	}
	if n := e.sendLogCount(t, job); n != 6 {
		t.Fatalf("send_logs rows = %d after the replay, want still 6", n)
	}
	before := e.px.pushCount()
	for i := 0; i < 3; i++ {
		e.px.freeAllWorkers()
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if after := e.px.pushCount(); after != before {
		t.Fatalf("pushes grew %d -> %d — a failed (attempts_exhausted) job was claimed again", before, after)
	}
}

// TestAttemptCap_DialIsReadFreshAndHonoured — job_max_failed_attempts is a real dial: set to 3 the job fails
// on its 3rd failed attempt, and a change BETWEEN attempts applies to the very next one in both directions
// (raised 3 -> 5 after two failures: the 3rd no longer ends it; lowered 5 -> 4 afterwards: the 4th does).
func TestAttemptCap_DialIsReadFreshAndHonoured(t *testing.T) {
	e, job := newCapEnv(t, "capdial")

	e.setConfigInt(t, "job_max_failed_attempts", 3)
	e.failedAttempt(t, job, persistent554)
	e.failedAttempt(t, job, persistent554)
	if c := e.jobColsOf(t, job); c.State != "queued" || c.FailedAttemptCount != 2 {
		t.Fatalf("after 2 failures with cap 3: %+v, want queued/2", c)
	}

	e.setConfigInt(t, "job_max_failed_attempts", 5) // raised BETWEEN attempts
	e.failedAttempt(t, job, persistent554)          // the 3rd — would have ended it at cap 3
	if c := e.jobColsOf(t, job); c.State != "queued" || c.FailedAttemptCount != 3 {
		t.Fatalf("after the 3rd failure with the cap raised to 5: %+v, want queued/3 (the dial is read fresh)", c)
	}

	e.setConfigInt(t, "job_max_failed_attempts", 4) // lowered BETWEEN attempts
	e.failedAttempt(t, job, persistent554)          // the 4th
	if c := e.jobColsOf(t, job); c.State != "failed" || c.FailedAttemptCount != 4 || c.TerminalReason != "attempts_exhausted" {
		t.Fatalf("after the 4th failure with the cap lowered to 4: %+v, want failed/attempts_exhausted at count 4", c)
	}
}

// TestAttemptCap_DispatcherSideRequeuesNeverCount — a job can be requeued dozens of times without ever
// reaching SMTP: no worker is connected, the push fails, the claim finds no account. Each of those is
// "waiting", not "failed", and none of them may count toward the cap — otherwise a worker outage would
// terminate every queued job. With the cap at 2, TEN such requeues leave the counters at zero and the job
// queued; only then do two REAL failures count and the second end the job.
func TestAttemptCap_DispatcherSideRequeuesNeverCount(t *testing.T) {
	e, job := newCapEnv(t, "capwaiting")
	ctx := context.Background()
	e.setConfigInt(t, "job_max_failed_attempts", 2)
	worker := e.px.workerID

	// (a) no_worker_available: the claim succeeds, there is no idle worker, the claim is requeued.
	e.px.workerID = ""
	for i := 0; i < 4; i++ {
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("tick (no worker) %d: %v", i, err)
		}
	}
	if c := e.jobColsOf(t, job); c.State != "queued" || c.FailedAttemptCount != 0 || c.FirstFailedSet {
		t.Fatalf("after 4 no_worker_available requeues: %+v, want queued with the failure counters untouched", c)
	}
	var msg string
	if err := e.admin.QueryRow(ctx, `SELECT COALESCE(last_error_message,'') FROM send_managed.jobs WHERE id=$1`, job).Scan(&msg); err != nil {
		t.Fatalf("read last_error_message: %v", err)
	}
	if msg != "no_worker_available" {
		t.Fatalf("last_error_message = %q, want no_worker_available — the churn above must be the requeue path this test claims", msg)
	}
	e.px.workerID = worker

	// (b) push_failed: the worker is there, the handoff fails.
	e.px.failPush = true
	for i := 0; i < 3; i++ {
		e.px.freeAllWorkers()
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("tick (push fails) %d: %v", i, err)
		}
	}
	e.px.failPush = false
	if c := e.jobColsOf(t, job); c.State != "queued" || c.FailedAttemptCount != 0 || c.FirstFailedSet {
		t.Fatalf("after 3 push_failed requeues: %+v, want queued with the failure counters untouched", c)
	}

	// (c) no_eligible_account: the production requeue the claim path uses when no account can be consumed.
	for i := 0; i < 3; i++ {
		claimed, err := e.d.claimForUser(ctx, systemTenant, 1, 120)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim %d: n=%d err=%v", i, len(claimed), err)
		}
		e.d.requeueNoAccount(ctx, claimed[0].id)
	}
	c := e.jobColsOf(t, job)
	if c.State != "queued" || c.FailedAttemptCount != 0 || c.FirstFailedSet {
		t.Fatalf("after 3 no_eligible_account requeues: %+v, want queued with the failure counters untouched", c)
	}
	var attemptCount int
	if err := e.admin.QueryRow(ctx, `SELECT attempt_count FROM send_managed.jobs WHERE id=$1`, job).Scan(&attemptCount); err != nil {
		t.Fatalf("read attempt_count: %v", err)
	}
	if attemptCount != 3 {
		t.Fatalf("attempt_count = %d, want 3 (saturated) — the ten requeues DID bump it, which is exactly why it cannot be the cap's counter", attemptCount)
	}

	// Only REAL failed attempts count: one is still below a cap of 2, the second ends the job.
	e.failedAttempt(t, job, persistent554)
	if c := e.jobColsOf(t, job); c.State != "queued" || c.FailedAttemptCount != 1 {
		t.Fatalf("after ONE real failure with cap 2: %+v, want queued/1", c)
	}
	e.failedAttempt(t, job, persistent554)
	if c := e.jobColsOf(t, job); c.State != "failed" || c.FailedAttemptCount != 2 || c.TerminalReason != "attempts_exhausted" {
		t.Fatalf("after the SECOND real failure with cap 2: %+v, want failed/attempts_exhausted at count 2", c)
	}
}

// TestAttemptCap_AgeCapCountsFromTheFirstFailedAttempt — job_failed_max_age_sec ends a job whose FIRST failed
// attempt is old enough even when the count is nowhere near the cap. The clock starts at the first FAILED
// attempt, not at creation: a job created three days ago that has never failed gets a fresh clock on its
// first failure. Controls: an hour is below the 24h default; a tighter dial moves the line.
func TestAttemptCap_AgeCapCountsFromTheFirstFailedAttempt(t *testing.T) {
	e, job := newCapEnv(t, "capage")
	ctx := context.Background()

	// A job created long ago that has NEVER failed: its first failure starts the clock, it does not end the job.
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET created_at = NOW() - INTERVAL '3 days' WHERE id=$1`, job); err != nil {
		t.Fatalf("backdate created_at: %v", err)
	}
	e.failedAttempt(t, job, persistent554)
	if c := e.jobColsOf(t, job); c.State != "queued" || c.FailedAttemptCount != 1 || !c.FirstFailedSet {
		t.Fatalf("first failure of a 3-day-old job: %+v, want queued/1 — the age cap counts from the first FAILED attempt, not from creation", c)
	}

	// CONTROL: first failure only an hour old (below the 24h default) -> another failure keeps it queued.
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET first_failed_at = NOW() - INTERVAL '1 hour' WHERE id=$1`, job); err != nil {
		t.Fatalf("backdate first_failed_at (1h): %v", err)
	}
	e.failedAttempt(t, job, persistent554)
	if c := e.jobColsOf(t, job); c.State != "queued" || c.FailedAttemptCount != 2 {
		t.Fatalf("a failure 1h after the first (24h cap): %+v, want queued/2", c)
	}

	// THE AGE CAP: first failure 25h old -> this failure ends the job at count 3 of 6.
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET first_failed_at = NOW() - INTERVAL '25 hours' WHERE id=$1`, job); err != nil {
		t.Fatalf("backdate first_failed_at (25h): %v", err)
	}
	e.failedAttempt(t, job, persistent554)
	c := e.jobColsOf(t, job)
	if c.State != "failed" || c.TerminalReason != "attempts_exhausted" || c.FailedAttemptCount != 3 || !c.CompletedSet {
		t.Fatalf("a failure 25h after the first: %+v, want failed/attempts_exhausted at count 3 (< the cap of 6) — the AGE cap, not the count", c)
	}

	// The dial moves the line: with job_failed_max_age_sec=3600 a job 2h past its first failure ends.
	e.setConfigInt(t, "job_failed_max_age_sec", 3600)
	job2 := e.seedQueuedJob(t, "capage2@example.com")
	e.failedAttempt(t, job2, persistent554)
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET first_failed_at = NOW() - INTERVAL '2 hours' WHERE id=$1`, job2); err != nil {
		t.Fatalf("backdate job2: %v", err)
	}
	e.failedAttempt(t, job2, persistent554)
	if c2 := e.jobColsOf(t, job2); c2.State != "failed" || c2.TerminalReason != "attempts_exhausted" || c2.FailedAttemptCount != 2 {
		t.Fatalf("with the age dial at 3600: %+v, want a job 2h past its first failure failed/attempts_exhausted", c2)
	}
}

// TestAttemptCap_ReclaimCountsOnlyWhatReachedAWorker — the reclaim loop's `timeout` is a real failed attempt
// when the job was SENDING (the worker had it and never reported), and is NOT when the job was only ASSIGNED
// (claimed, never pushed: a Control crash in the claim->push window). With the cap at 2: two reclaims of a
// SENDING job end it as failed/attempts_exhausted under the reclaim_loop actor; any number of reclaims of an
// ASSIGNED job leave the counters at zero and the job queued.
func TestAttemptCap_ReclaimCountsOnlyWhatReachedAWorker(t *testing.T) {
	e, sendingJob := newCapEnv(t, "capreclaim")
	ctx := context.Background()
	e.setConfigInt(t, "job_max_failed_attempts", 2)

	// SENDING: lease expires twice.
	for i := 1; i <= 2; i++ {
		if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, sendingJob); err != nil {
			t.Fatalf("make claimable: %v", err)
		}
		e.px.freeAllWorkers()
		e.dispatchToSending(t, sendingJob)
		e.expireLease(t, sendingJob)
		if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 1 {
			t.Fatalf("reclaim %d: n=%d err=%v, want 1", i, n, err)
		}
		c := e.jobColsOf(t, sendingJob)
		if i == 1 && (c.State != "queued" || c.FailedAttemptCount != 1 || !c.FirstFailedSet) {
			t.Fatalf("after the 1st reclaim of a SENDING job (cap 2): %+v, want queued/1", c)
		}
		if i == 2 && (c.State != "failed" || c.FailedAttemptCount != 2 || c.TerminalReason != "attempts_exhausted" || !c.CompletedSet || !c.LeaseNull) {
			t.Fatalf("after the 2nd reclaim of a SENDING job (cap 2): %+v, want failed/attempts_exhausted at count 2", c)
		}
	}
	chain := e.jobEventChain(t, sendingJob)
	last := chain[len(chain)-1]
	if last[0] != "sending" || last[1] != "failed" || last[2] != "reclaim_loop" || last[3] != "timeout" {
		t.Fatalf("last event = %v, want [sending failed reclaim_loop timeout]", last)
	}

	// ASSIGNED only: stranded after the claim, never pushed, reclaimed again and again.
	assignedJob := e.seedQueuedJob(t, "capreclaim-assigned@example.com")
	for i := 0; i < 4; i++ {
		e.adminExecAsActor(t, "dispatcher",
			`UPDATE send_managed.jobs SET state='assigned', lease_expires_at=NOW()-INTERVAL '1 second' WHERE id=$1 AND state='queued'`, assignedJob)
		if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 1 {
			t.Fatalf("reclaim of the assigned job, round %d: n=%d err=%v, want 1", i, n, err)
		}
	}
	c := e.jobColsOf(t, assignedJob)
	if c.State != "queued" || c.FailedAttemptCount != 0 || c.FirstFailedSet || c.TerminalReason != "" {
		t.Fatalf("after 4 reclaims of an ASSIGNED-only job (cap 2): %+v, want queued with the failure counters untouched — it never reached a worker", c)
	}
}

// TestAttemptCap_PoolExhaustionKeepsItsOwnReason — the other writer of `failed` still writes it, and the
// report tells the two apart: a job whose pool has no usable account is failed/pool_exhausted with NO failed
// attempt on it (it never reached SMTP), not attempts_exhausted.
func TestAttemptCap_PoolExhaustionKeepsItsOwnReason(t *testing.T) {
	e := newDispatchEnv(t, "cappoolex")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET quarantined=TRUE WHERE id=$1`, acct); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	job := e.seedQueuedJob(t, "poolex@example.com")

	if _, _, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	c := e.jobColsOf(t, job)
	if c.State != "failed" || c.TerminalReason != "pool_exhausted" || c.FailedAttemptCount != 0 || c.FirstFailedSet || !c.CompletedSet {
		t.Fatalf("pool-exhausted job = %+v, want failed/pool_exhausted with NO failed attempt recorded", c)
	}
	pc := e.newPlatformClient(t)
	rep := pc.jobReport(t, job, systemTenant)
	if r, _ := rep.strOrNil("reason"); r != "pool_exhausted" {
		t.Fatalf("report reason = %q, want pool_exhausted: %s", r, rep.raw)
	}
	if _, has := rep.strOrNil("error_class"); has {
		t.Fatalf("report error_class present on a job that never reached SMTP: %s", rep.raw)
	}
	if !rep.hasKey("error_class") {
		t.Fatalf("report has no error_class KEY (it must be present, null): %s", rep.raw)
	}
}

// TestAttemptCap_RejectsAnUnusableDial — a cap below 1 would fail every job on its first failed attempt.
// LoadConfig refuses it as a config fault (Rule 1: reject, never clamp), names the key, and does not
// mistake it for a database outage.
func TestAttemptCap_RejectsAnUnusableDial(t *testing.T) {
	e := newDispatchEnv(t, "capdialbad")
	ctx := context.Background()
	for _, tc := range []struct{ key, raw string }{
		{"job_max_failed_attempts", `{"v":0}`},
		{"job_max_failed_attempts", `{"v":-3}`},
		{"job_failed_max_age_sec", `{"v":0}`},
	} {
		e.setConfigRaw(t, tc.key, tc.raw)
		_, err := LoadConfig(ctx, e.pool)
		if err == nil {
			t.Fatalf("LoadConfig accepted %s = %s — a cap of 0 fails every job on its first failure", tc.key, tc.raw)
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Fatalf("the rejection %q does not name %s", err.Error(), tc.key)
		}
		if IsOutage(err) {
			t.Fatalf("%s = %s was classified as a DB outage: %v — it is a config fault", tc.key, tc.raw, err)
		}
		// restore, so the next case starts from a loadable config
		e.setConfigRaw(t, tc.key, map[string]string{"job_max_failed_attempts": `{"v":6}`, "job_failed_max_age_sec": `{"v":86400}`}[tc.key])
		if _, err := LoadConfig(ctx, e.pool); err != nil {
			t.Fatalf("restoring %s: LoadConfig still failing: %v", tc.key, err)
		}
	}
}
