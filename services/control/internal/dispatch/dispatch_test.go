package dispatch

import (
	"context"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// TestDispatch_HappyPath — the dispatcher claims a queued job, selects the eligible account,
// decrypts the credential, builds §8 headers, and pushes ONE push_job carrying the DECRYPTED
// secret. Asserts the job moved queued→assigned→sending (3 events after genesis) with the worker
// assigned, the lease set, and the push_job content correct. (The accept→sent half is
// TestOutcome_* below; the full wire is the live-stack suite.)
func TestDispatch_HappyPath(t *testing.T) {
	e := newDispatchEnv(t, "happy")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "the-real-secret-A1")
	job := e.seedQueuedJob(t, "alice@example.com")

	pushed, err := e.d.TryDispatch(ctx)
	if err != nil {
		t.Fatalf("TryDispatch: %v", err)
	}
	if pushed != job {
		t.Fatalf("pushed=%q, want %q", pushed, job)
	}

	// State: sending, lease set (NOT null), worker assigned; genesis+assigned+sending = 3 events.
	state, hasPMID, leaseNull, completed, _ := e.jobState(t, job)
	if state != "sending" {
		t.Fatalf("state=%s, want sending", state)
	}
	if hasPMID {
		t.Fatalf("provider_message_id set before any accept — must be null in sending")
	}
	if leaseNull {
		t.Fatalf("lease is null in sending — CHECK requires it set")
	}
	if completed {
		t.Fatalf("completed_at set in sending — must be null")
	}
	if n := e.eventCount(t, job); n != 3 {
		t.Fatalf("event count=%d, want 3 (genesis+assigned+sending)", n)
	}

	// push_job carries the DECRYPTED secret + the §8 headers the worker will send.
	pj, ok := e.px.lastPush()
	if !ok {
		t.Fatal("no push recorded")
	}
	if pj.SMTP.Secret != "the-real-secret-A1" {
		t.Fatalf("push secret mismatch: got %q", pj.SMTP.Secret)
	}
	if pj.SMTP.Security != workerproto.SecurityStartTLS {
		t.Fatalf("security=%q, want starttls (port 587)", pj.SMTP.Security)
	}
	if pj.Send.Headers["To"] != "alice@example.com" {
		t.Fatalf("To header=%q", pj.Send.Headers["To"])
	}
	if pj.Send.Headers["Message-ID"] == "" {
		t.Fatalf("Message-ID header missing (idempotency fallback source)")
	}
	if pj.Send.Headers["Reply-To"] != "reply@ourdomain.com" {
		t.Fatalf("Reply-To header=%q", pj.Send.Headers["Reply-To"])
	}
}

// TestOutcome_AcceptedPersistsAndIdempotent — an accepted report drives sending→sent with a
// persisted provider_message_id + one send_log, clears the lease, sets completed_at, increments
// the account counter. A SECOND accepted report for the same job is a clean no-op (§5.3): no
// second send_log, provider_message_id unchanged. This is the T4 idempotency gate at the handler.
func TestOutcome_AcceptedPersistsAndIdempotent(t *testing.T) {
	e := newDispatchEnv(t, "accept")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "b@example.com")
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	rep := workerproto.Report{Type: workerproto.TypeReport, JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PROVIDER-XYZ-1", SMTPResponse: "250 2.0.0 Ok: queued as PROVIDER-XYZ-1"}
	e.oh.Handle(e.px.workerID, rep)

	state, hasPMID, leaseNull, completed, _ := e.jobState(t, job)
	if state != "sent" || !hasPMID || !leaseNull || !completed {
		t.Fatalf("after accept: state=%s hasPMID=%v leaseNull=%v completed=%v, want sent/true/true/true", state, hasPMID, leaseNull, completed)
	}
	if n := e.sendLogCount(t, job); n != 1 {
		t.Fatalf("send_log count=%d, want 1", n)
	}
	var dailyCount int
	if err := e.admin.QueryRow(ctx, `SELECT daily_sent_count FROM send_managed.accounts WHERE pool_id=$1`, pool).Scan(&dailyCount); err != nil {
		t.Fatalf("daily count: %v", err)
	}
	if dailyCount != 1 {
		t.Fatalf("daily_sent_count=%d, want 1", dailyCount)
	}

	// Idempotency: a SECOND accepted report is a clean no-op — no second send_log, id unchanged.
	e.oh.Handle(e.px.workerID, rep)
	if n := e.sendLogCount(t, job); n != 1 {
		t.Fatalf("send_log count=%d after duplicate accept, want 1 (idempotent no-op)", n)
	}
	var pmid string
	if err := e.admin.QueryRow(ctx, `SELECT provider_message_id FROM send_managed.jobs WHERE id=$1`, job).Scan(&pmid); err != nil {
		t.Fatalf("pmid: %v", err)
	}
	if pmid != "PROVIDER-XYZ-1" {
		t.Fatalf("provider_message_id changed to %q on duplicate accept", pmid)
	}
}

// TestOutcome_NeverInventsSent (T3) — a report is NEVER allowed to mark a job sent unless it is
// genuinely in-flight and carries a provider_message_id. Two negative controls:
//
//	(a) an accepted report with an EMPTY provider_message_id must NOT produce a sent job (it
//	    requeues per §4.3); the job never becomes sent-with-null-id.
//	(b) a report for a job that is NOT sending (still queued) is ignored — no state change.
//
// Positive control: a well-formed accepted report DOES reach sent (covered above).
func TestOutcome_NeverInventsSent(t *testing.T) {
	e := newDispatchEnv(t, "noinvent")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	// (a) accepted-with-empty-id on an in-flight job → NOT sent (requeued), never null-id sent.
	jobA := e.seedQueuedJob(t, "a@example.com")
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch A: %v", err)
	}
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: jobA, Outcome: workerproto.OutcomeAccepted, ProviderMessageID: ""})
	state, hasPMID, _, _, _ := e.jobState(t, jobA)
	if state == "sent" || hasPMID {
		t.Fatalf("accepted-with-empty-id produced sent/pmid (state=%s hasPMID=%v) — invented an outcome", state, hasPMID)
	}

	// (b) a report for a job that is still queued (never pushed) is ignored — no transition.
	jobB := e.seedQueuedJob(t, "bb@example.com")
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: jobB, Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "SHOULD-BE-IGNORED"})
	stateB, hasB, _, _, _ := e.jobState(t, jobB)
	if stateB != "queued" || hasB {
		t.Fatalf("report on a queued job changed it (state=%s hasPMID=%v) — must be ignored", stateB, hasB)
	}
}

// TestOutcome_ErrorRequeues (Phase 5) — a connect_refused error report classifies as `timeout`
// (§1.1 row 6) and drives sending→queued (the one locked failure transition), clears the lease,
// increments attempt_count, sets last_error_class='timeout' (Phase 5 replaces the Phase-3
// 'unknown' stub), applies NO account penalty (timeout is transient — §2.1), backs off
// next_attempt_at into the future (§6.4), writes one send_log, and does NOT auto re-dispatch.
// Anti-vacuity: the job was genuinely sending first.
func TestOutcome_ErrorRequeues(t *testing.T) {
	e := newDispatchEnv(t, "requeue")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "c@example.com")
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Precondition: genuinely sending.
	if state, _, _, _, _ := e.jobState(t, job); state != "sending" {
		t.Fatalf("precondition: state=%s, want sending", state)
	}

	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
		ErrorReason: workerproto.ReasonConnectRefused, SMTPResponse: ""})

	state, hasPMID, leaseNull, completed, attempt := e.jobState(t, job)
	if state != "queued" {
		t.Fatalf("state=%s after error, want queued", state)
	}
	if !leaseNull {
		t.Fatalf("lease not cleared on requeue — CHECK would abort")
	}
	if hasPMID || completed {
		t.Fatalf("requeue set pmid/completed (hasPMID=%v completed=%v)", hasPMID, completed)
	}
	if attempt != 1 {
		t.Fatalf("attempt_count=%d after one failure, want 1", attempt)
	}
	// Phase 5 taxonomy: connect_refused → timeout (§1.1 row 6), not the Phase-3 'unknown' stub.
	var ec *string
	if err := e.admin.QueryRow(ctx, `SELECT last_error_class::text FROM send_managed.jobs WHERE id=$1`, job).Scan(&ec); err != nil {
		t.Fatalf("error class: %v", err)
	}
	if ec == nil || *ec != "timeout" {
		t.Fatalf("last_error_class=%v, want timeout", ec)
	}
	// Backoff: timeout requeues with next_attempt_at in the FUTURE (§6.4), not NOW().
	var future bool
	if err := e.admin.QueryRow(ctx, `SELECT next_attempt_at > NOW() FROM send_managed.jobs WHERE id=$1`, job).Scan(&future); err != nil {
		t.Fatalf("next_attempt_at: %v", err)
	}
	if !future {
		t.Fatalf("next_attempt_at not backed off into the future for a timeout requeue (§6.4)")
	}
	// No account penalty for timeout: not paused/quarantined; NO account_events row written.
	var paused, quarantined bool
	if err := e.admin.QueryRow(ctx, `SELECT paused, quarantined FROM send_managed.accounts WHERE id=$1`, acct).Scan(&paused, &quarantined); err != nil {
		t.Fatalf("account flags: %v", err)
	}
	if paused || quarantined {
		t.Fatalf("timeout penalized the account (paused=%v quarantined=%v) — transient must not (§2.1)", paused, quarantined)
	}
	var aev int
	if err := e.admin.QueryRow(ctx, `SELECT count(*) FROM send_managed.account_events WHERE account_id=$1`, acct).Scan(&aev); err != nil {
		t.Fatalf("account_events: %v", err)
	}
	if aev != 0 {
		t.Fatalf("account_events count=%d for a no-penalty timeout, want 0", aev)
	}
	if n := e.sendLogCount(t, job); n != 1 {
		t.Fatalf("send_log count=%d after error, want 1 (the attempt is logged)", n)
	}
	// One sending→queued event exists with actor 'box'.
	var evActor string
	if err := e.admin.QueryRow(ctx,
		`SELECT actor::text FROM send_managed.job_events WHERE job_id=$1 AND from_state='sending' AND to_state='queued'`, job).Scan(&evActor); err != nil {
		t.Fatalf("requeue event: %v", err)
	}
	if evActor != "box" {
		t.Fatalf("requeue event actor=%s, want worker", evActor)
	}
}

// TestDispatch_HeaderInjectionRequeued — a job whose subject carries CR/LF (an SMTP header/body
// injection attempt) is NOT pushed: the dispatcher requeues it (lease cleared) and the worker never
// receives a push_job for it. Anti-vacuity: a clean job in the same env DOES push.
func TestDispatch_HeaderInjectionRequeued(t *testing.T) {
	e := newDispatchEnv(t, "crlf")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	// A poisoned job: subject contains CRLF + an injected Bcc header.
	job := e.seedQueuedJobSubject(t, "inj@example.com", "Hello\r\nBcc: victim@evil.com")
	pushed, err := e.d.TryDispatch(ctx)
	if err != nil {
		t.Fatalf("TryDispatch: %v", err)
	}
	if pushed == job {
		t.Fatalf("poisoned job was pushed — header-injection guard failed")
	}
	if e.px.pushCount() != 0 {
		t.Fatalf("push count=%d, want 0 (nothing pushed for a poisoned job)", e.px.pushCount())
	}
	// The job is requeued to a legal state (queued, lease cleared).
	state, _, leaseNull, _, attempt := e.jobState(t, job)
	if state != "queued" || !leaseNull {
		t.Fatalf("poisoned job state=%s leaseNull=%v, want queued/true (requeued)", state, leaseNull)
	}
	if attempt != 1 {
		t.Fatalf("attempt_count=%d, want 1", attempt)
	}

	// Positive control: a CLEAN job DOES push (the guard didn't wedge dispatch). Remove the
	// poisoned job first so it isn't re-claimed ahead of the clean one (it is older; the claim
	// orders by created_at). Deleting it is fine — this only proves the guard doesn't over-reject.
	if _, err := e.admin.Exec(ctx, `DELETE FROM send_managed.jobs WHERE id=$1`, job); err != nil {
		t.Fatalf("cleanup poisoned job: %v", err)
	}
	clean := e.seedQueuedJob(t, "clean@example.com")
	if got, _ := e.d.TryDispatch(ctx); got != clean {
		t.Fatalf("clean job not pushed (got %q, want %q) — guard over-rejects", got, clean)
	}
}

// TestOutcome_BouncedTerminal — a bounced report drives sending→bounced (terminal), clears the
// lease, sets completed_at.
func TestOutcome_BouncedTerminal(t *testing.T) {
	e := newDispatchEnv(t, "bounce")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "d@example.com")
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeBounced,
		SMTPResponse: "550 5.1.1 No such user"})
	state, _, leaseNull, completed, _ := e.jobState(t, job)
	if state != "bounced" || !leaseNull || !completed {
		t.Fatalf("after bounce: state=%s leaseNull=%v completed=%v, want bounced/true/true", state, leaseNull, completed)
	}
}
