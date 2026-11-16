package dispatch

import (
	"context"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// attempttoken_test.go — Phase 8 CHUNK 1 (P7-OD-6, RULED-ADOPT 2026-07-26): the Control's per-attempt
// identity, send_managed.jobs.attempt_token.
//
// THE HOLE THESE TESTS CLOSE. The worker's outcome buffer deliberately outlives its own attempt
// (box_outcome_buffer_ttl_sec 900s >> job_max_processing_time_sec 120s), so a job can be reclaimed and
// RE-DISPATCHED — plausibly to the SAME worker — while an older attempt's outcome is still buffered and
// being replayed. Phase 7's attribution guard (staleattr_test.go) can only decide the DIFFERENT-worker
// case. On a same-worker re-dispatch the reporting worker IS assigned_box_id and the job IS legitimately
// 'sending' again, so before this fix a stale replay was applied to the LIVE attempt: a stale ACCEPTED
// marks the job sent while attempt 2 is mid-flight (attempt 2's real delivery becomes an unrecorded
// DUPLICATE), and a stale FAILURE requeues the job out from under attempt 2 and penalizes the account
// it is using — with the requeue producing a genuine second send.
//
// Everything below drives the REAL dispatcher, the REAL OutcomeHandler, and real Postgres. Nothing is
// mocked. Each guard test carries a paired positive control (Rule 3): the LIVE attempt's report with
// the CURRENT token must still apply in full, or an unconditionally-rejecting Handle would pass too.

// jobAttemptToken reads a job's current attempt_token.
func (e *dispatchEnv) jobAttemptToken(t *testing.T, jobID string) int64 {
	t.Helper()
	var tok int64
	if err := e.admin.QueryRow(context.Background(),
		`SELECT attempt_token FROM send_managed.jobs WHERE id=$1`, jobID).Scan(&tok); err != nil {
		t.Fatalf("read attempt_token: %v", err)
	}
	return tok
}

// TestAttemptToken_MintedPerDispatchAndPushed — the mint half of the contract: a queued job starts at
// token 0 (no attempt yet), each dispatch increments it by exactly one, and the value the Control
// persisted is the value it put on the wire. Anti-vacuity: the token on push_job is compared against
// the DB row, so a push carrying a stale/zero/guessed token fails here.
func TestAttemptToken_MintedPerDispatchAndPushed(t *testing.T) {
	e := newDispatchEnv(t, "attoktmint")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	job := e.seedQueuedJob(t, "mint@example.com")
	if tok := e.jobAttemptToken(t, job); tok != 0 {
		t.Fatalf("a never-dispatched job has attempt_token=%d, want 0 (no attempt has begun)", tok)
	}

	e.dispatchToSending(t, job)
	if tok := e.jobAttemptToken(t, job); tok != 1 {
		t.Fatalf("after the first dispatch attempt_token=%d, want 1", tok)
	}
	pj, ok := e.px.lastPush()
	if !ok {
		t.Fatal("no push recorded")
	}
	if pj.JobID != job {
		t.Fatalf("last push is for job %s, want %s", pj.JobID, job)
	}
	if pj.AttemptToken != 1 {
		t.Fatalf("push_job.attempt_token=%d, want 1 — the worker was handed an identity the Control did not persist", pj.AttemptToken)
	}

	// Reclaim + re-dispatch: the SECOND attempt must mint token 2 and push it.
	e.expireLease(t, job)
	if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 1 {
		t.Fatalf("reclaim: n=%d err=%v, want 1/nil", n, err)
	}
	e.px.freeAllWorkers()
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	if tok := e.jobAttemptToken(t, job); tok != 2 {
		t.Fatalf("after the second dispatch attempt_token=%d, want 2 (monotonic per attempt)", tok)
	}
	pj2, _ := e.px.lastPush()
	if pj2.AttemptToken != 2 {
		t.Fatalf("second push_job.attempt_token=%d, want 2", pj2.AttemptToken)
	}
	// The two attempts of the SAME job carry DIFFERENT identities — the whole point.
	if pj.AttemptToken == pj2.AttemptToken {
		t.Fatalf("both attempts of job %s carry token %d — the token does not identify an attempt", job, pj.AttemptToken)
	}
}

// TestAttemptToken_MonotonicAndNonSaturating — the property that makes attempt_count unusable as an
// identity, asserted as a contrast. Over more attempts than max_attempts (3), attempt_count SATURATES
// (Phase 5: LEAST(attempt_count+1, max_attempts)) and therefore REPEATS across attempts, while
// attempt_token keeps climbing and never repeats. Anti-vacuity: the test asserts BOTH halves — that
// attempt_count genuinely did repeat (so the hazard is real, not hypothetical) and that attempt_token
// did not.
func TestAttemptToken_MonotonicAndNonSaturating(t *testing.T) {
	e := newDispatchEnv(t, "attoktmono")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	// Keep the account healthy across many classified failures so dispatch keeps succeeding — we are
	// measuring the counters, not scoring.
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET last_success_at=NOW() WHERE id=$1`, acct); err != nil {
		t.Fatalf("seed success: %v", err)
	}

	job := e.seedQueuedJob(t, "mono@example.com")
	// SWEEP (send_managed_0028, M2-G2): six failed attempts on one job reach the DEFAULT attempt cap
	// (job_max_failed_attempts=6) and the sixth would end it as failed/attempts_exhausted. This test measures
	// two COUNTERS (attempt_token vs the saturating attempt_count), not the cap, so the cap is raised out of
	// its way; attempts_cap_test.go asserts the cap itself at the seeded default.
	e.setConfigInt(t, "job_max_failed_attempts", 1000)
	const attempts = 6 // > max_attempts (3), so attempt_count must saturate
	var tokens []int64
	var counts []int
	for i := 0; i < attempts; i++ {
		if _, err := e.admin.Exec(ctx,
			`UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
			t.Fatalf("clear backoff: %v", err)
		}
		e.px.freeAllWorkers()
		if _, err := e.d.TryDispatch(ctx); err != nil {
			t.Fatalf("attempt %d dispatch: %v", i+1, err)
		}
		if st, _, _, _, _ := e.jobState(t, job); st != "sending" {
			t.Fatalf("attempt %d: state=%s, want sending", i+1, st)
		}
		tok := e.jobAttemptToken(t, job)
		// The LIVE token must apply — report the real failure with it and requeue for the next round.
		if !e.oh.Handle(e.jobWorker(t, job), workerproto.Report{
			JobID: job, AttemptToken: tok, Outcome: workerproto.OutcomeError,
			SMTPResponse: "451 4.3.0 temporary local error",
		}) {
			t.Fatalf("attempt %d: the live report was not resolved", i+1)
		}
		_, _, _, _, cnt := e.jobState(t, job)
		tokens = append(tokens, tok)
		counts = append(counts, cnt)
	}

	// attempt_token is strictly increasing and never repeats.
	for i := 1; i < len(tokens); i++ {
		if tokens[i] <= tokens[i-1] {
			t.Fatalf("attempt_token not strictly increasing: %v", tokens)
		}
	}
	if tokens[len(tokens)-1] != attempts {
		t.Fatalf("after %d attempts attempt_token=%d, want %d (non-saturating)", attempts, tokens[len(tokens)-1], attempts)
	}
	// ANTI-VACUITY / the contrast: attempt_count genuinely DID repeat, which is exactly why it cannot
	// be the identity. If this ever stops being true the comparison above is meaningless.
	repeated := false
	for i := 1; i < len(counts); i++ {
		if counts[i] == counts[i-1] {
			repeated = true
		}
	}
	if !repeated {
		t.Fatalf("attempt_count never repeated over %d attempts (%v) — the saturation premise this fix rests on is not being exercised, so the token/count contrast is vacuous", attempts, counts)
	}
	t.Logf("over %d attempts: attempt_token=%v (monotonic), attempt_count=%v (saturating)", attempts, tokens, counts)
}

// TestAttemptToken_StaleReplaySameWorkerRejected — THE headline Chunk-1 property, and the exact hole
// P7-OD-6 filed: a stale attempt-1 replay arriving from the SAME worker after the job was reclaimed and
// re-dispatched must be REJECTED — no state mutation, no account scoring, no send_logs row — while the
// LIVE attempt-2 report with the current token is accepted in full.
//
// The interleave is FORCED, not raced (Rule 10): attempt 1's report is captured but deliberately never
// delivered, the job is then reclaimed and re-dispatched to the same worker, and only THEN is attempt 1's
// buffered report handed to Handle. There is no timing luck — the ordering is written out.
func TestAttemptToken_StaleReplaySameWorkerRejected(t *testing.T) {
	e := newDispatchEnv(t, "attoktstale")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET last_success_at=NOW() WHERE id=$1`, acct); err != nil {
		t.Fatalf("seed success: %v", err)
	}

	job := e.seedQueuedJob(t, "sameworker@example.com")
	e.dispatchToSending(t, job)
	worker1 := e.jobWorker(t, job)
	token1 := e.jobAttemptToken(t, job)

	// Attempt 1 produced an outcome the Control never acked (DB down / stream drop). The worker buffers it.
	// We hold it here — it is delivered at the bottom, AFTER the re-dispatch, which is the whole point.
	staleFailure := workerproto.Report{
		Type: workerproto.TypeReport, JobID: job, AttemptToken: token1,
		Outcome: workerproto.OutcomeError, ErrorReason: workerproto.ReasonAuthFailed,
		SMTPResponse: "535 5.7.8 stale from attempt 1",
	}
	staleSuccess := workerproto.Report{
		Type: workerproto.TypeReport, JobID: job, AttemptToken: token1,
		Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "stale-attempt-1-pmid",
		SMTPResponse: "250 2.0.0 stale OK from attempt 1",
	}

	// The lease expires, the reclaim loop requeues, and the job is re-dispatched to the SAME worker (only
	// one worker is registered, so the re-dispatch necessarily lands there — that is the case Phase 7
	// could not decide).
	e.expireLease(t, job)
	if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 1 {
		t.Fatalf("reclaim: n=%d err=%v, want 1/nil", n, err)
	}
	e.px.freeAllWorkers()
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	worker2 := e.jobWorker(t, job)
	token2 := e.jobAttemptToken(t, job)

	// PRECONDITIONS that make this test the SAME-worker case (vacuous otherwise, in either direction).
	if worker2 != worker1 {
		t.Fatalf("re-dispatch landed on a DIFFERENT worker (%s -> %s) — that case is already covered by the Phase 7 attribution guard, so this test would prove nothing about per-attempt identity", worker1, worker2)
	}
	if token2 == token1 {
		t.Fatalf("the re-dispatch did NOT mint a new token (still %d) — the two attempts are indistinguishable and every assertion below is vacuous", token1)
	}
	if st, _, _, _, _ := e.jobState(t, job); st != "sending" {
		t.Fatalf("precondition: state=%s, want sending (attempt 2 is live — this is what makes the stale replay dangerous)", st)
	}

	// Snapshot everything the stale replay must not touch.
	var cfBefore int
	var scoreBefore float64
	if err := e.admin.QueryRow(ctx,
		`SELECT consecutive_failures, account_status_score FROM send_managed.accounts WHERE id=$1`, acct).
		Scan(&cfBefore, &scoreBefore); err != nil {
		t.Fatalf("read account before: %v", err)
	}
	_, _, _, _, attemptBefore := e.jobState(t, job)
	logsBefore := e.sendLogCount(t, job)
	eventsBefore := e.eventCount(t, job)

	// *** THE STALE REPLAY, both flavors, from the SAME worker that owns the live attempt. ***
	for _, rep := range []workerproto.Report{staleFailure, staleSuccess} {
		if !e.oh.Handle(worker1, rep) {
			t.Errorf("stale %s replay: resolved=false — the worker would replay a permanently-inapplicable outcome until its TTL", rep.Outcome)
		}
		st, hasPMID, _, _, attemptAfter := e.jobState(t, job)
		if st != "sending" {
			t.Fatalf("stale %s replay moved the job to %s — it was applied to the LIVE attempt", rep.Outcome, st)
		}
		if hasPMID {
			t.Fatalf("stale %s replay set provider_message_id — the job is marked sent while attempt 2 is still in flight (this is the DUPLICATE)", rep.Outcome)
		}
		if attemptAfter != attemptBefore {
			t.Errorf("stale %s replay changed attempt_count %d→%d", rep.Outcome, attemptBefore, attemptAfter)
		}
		if got := e.sendLogCount(t, job); got != logsBefore {
			t.Errorf("stale %s replay wrote a send_log (%d→%d) for an attempt that is still running", rep.Outcome, logsBefore, got)
		}
		if got := e.eventCount(t, job); got != eventsBefore {
			t.Errorf("stale %s replay wrote a job_event (%d→%d) — it mutated state", rep.Outcome, eventsBefore, got)
		}
		var cfAfter int
		var scoreAfter float64
		if err := e.admin.QueryRow(ctx,
			`SELECT consecutive_failures, account_status_score FROM send_managed.accounts WHERE id=$1`, acct).
			Scan(&cfAfter, &scoreAfter); err != nil {
			t.Fatalf("read account after: %v", err)
		}
		if cfAfter != cfBefore || scoreAfter != scoreBefore {
			t.Errorf("stale %s replay SCORED the account: consecutive_failures %d→%d, score %.3f→%.3f (a result this attempt never produced)",
				rep.Outcome, cfBefore, cfAfter, scoreBefore, scoreAfter)
		}
	}

	// POSITIVE CONTROL (anti-vacuity, Rule 3). The SAME worker, the SAME job, the SAME outcome shape — only
	// the token differs. Attempt 2's report must apply in FULL. Without this, a Handle that rejected
	// every report unconditionally would satisfy every assertion above.
	if !e.oh.Handle(worker1, workerproto.Report{
		Type: workerproto.TypeReport, JobID: job, AttemptToken: token2,
		Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "live-attempt-2-pmid",
		SMTPResponse: "250 2.0.0 real OK from attempt 2",
	}) {
		t.Fatal("positive control: attempt 2's own report was not resolved")
	}
	st, hasPMID, leaseNull, completedSet, _ := e.jobState(t, job)
	if st != "sent" || !hasPMID || !leaseNull || !completedSet {
		t.Fatalf("positive control: state=%s pmid=%v leaseNull=%v completed=%v, want sent/true/true/true — the guard is rejecting the LIVE attempt too (vacuous)",
			st, hasPMID, leaseNull, completedSet)
	}
	if got := e.sendLogCount(t, job); got != logsBefore+1 {
		t.Errorf("positive control: send_logs %d→%d, want +1 (the live attempt MUST be logged)", logsBefore, got)
	}
	// And the pmid stored is attempt 2's, not the stale one the rejected replay carried.
	var pmid string
	if err := e.admin.QueryRow(ctx, `SELECT provider_message_id FROM send_managed.jobs WHERE id=$1`, job).Scan(&pmid); err != nil {
		t.Fatalf("read pmid: %v", err)
	}
	if pmid != "live-attempt-2-pmid" {
		t.Fatalf("job recorded provider_message_id %q, want the LIVE attempt's — a stale replay won the race", pmid)
	}
}

// TestAttemptToken_AbsentTokenIsNotAMismatch — the compatibility half of the guard, asserted rather
// than assumed. The guard rejects ONLY a PROVABLE mismatch (both tokens non-zero and different), so:
//
//	(a) a report with NO token (a pre-0020 worker, or an attempt that was in flight across the migration)
//	    must still be applied — degrading to exactly Phase 7 behavior, never a false rejection; and
//	(b) a job whose stored token is 0 (dispatched before the migration) must still accept a report.
//
// Without this, the guard could silently discard every legitimate outcome from a worker that predates the
// wire field — a far worse failure than the hole it closes.
func TestAttemptToken_AbsentTokenIsNotAMismatch(t *testing.T) {
	e := newDispatchEnv(t, "attoktcompat")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	// (a) Live attempt, real token stored; the REPORT omits it (old worker).
	jobA := e.seedQueuedJob(t, "compat-a@example.com")
	e.dispatchToSending(t, jobA)
	if tok := e.jobAttemptToken(t, jobA); tok == 0 {
		t.Fatal("precondition: jobA has no stored token, so case (a) is not being exercised")
	}
	if !e.oh.Handle(e.jobWorker(t, jobA), workerproto.Report{
		Type: workerproto.TypeReport, JobID: jobA, /* AttemptToken deliberately omitted */
		Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "compat-a-pmid", SMTPResponse: "250 OK",
	}) {
		t.Fatal("a token-less report was not resolved")
	}
	if st, hasPMID, _, _, _ := e.jobState(t, jobA); st != "sent" || !hasPMID {
		t.Fatalf("token-less report: state=%s pmid=%v, want sent/true — the guard rejected a legitimate outcome from a worker that does not send the field", st, hasPMID)
	}

	// (b) Stored token forced to 0 (a job dispatched before send_managed_0020), report carries a token.
	jobB := e.seedQueuedJob(t, "compat-b@example.com")
	e.px.freeAllWorkers()
	e.dispatchToSending(t, jobB)
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET attempt_token=0 WHERE id=$1`, jobB); err != nil {
		t.Fatalf("force stored token 0: %v", err)
	}
	if !e.oh.Handle(e.jobWorker(t, jobB), workerproto.Report{
		Type: workerproto.TypeReport, JobID: jobB, AttemptToken: 7,
		Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "compat-b-pmid", SMTPResponse: "250 OK",
	}) {
		t.Fatal("a report against a token-less job row was not resolved")
	}
	if st, hasPMID, _, _, _ := e.jobState(t, jobB); st != "sent" || !hasPMID {
		t.Fatalf("token-less job row: state=%s pmid=%v, want sent/true — the guard rejected an outcome for an attempt that predates the column", st, hasPMID)
	}
}
