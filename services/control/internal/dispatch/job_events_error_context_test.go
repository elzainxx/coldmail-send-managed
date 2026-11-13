package dispatch

// job_events_error_context_test.go — job_events.error_class/error_message appear ONLY on error
// transitions, which is what send_managed_0008's own column comment always claimed and what send_managed_0022
// finally made true.
//
// WHY THIS EXISTS. Found walking Stage 9 of Docs/install-and-deploy.md#part-7--developing-and-testing-locally against the live
// docker stack. send_managed_0008 declares, verbatim, `-- Error context (nullable, only for error
// transitions)` over job_events.error_class / error_message — then both of its triggers copied
// `NEW.last_error_class, NEW.last_error_message` into EVERY row unconditionally. The job row's
// last_error_* columns are STICKY BY DESIGN (the name says "last"; no success path clears them —
// applyAccepted sets state/provider_message_id/completed_at and nothing else), so once a job had
// failed ONCE, every later event it emitted carried that stale class. Live trace of ONE requeue:
//
//	seq  actor       transition            error_class  error_message
//	755  system      (new)    -> queued    -            -
//	759  dispatcher  queued   -> assigned  -            -
//	763  dispatcher  assigned -> queued    unknown      no_eligible_account   <- the ONE real error
//	768  dispatcher  queued   -> assigned  unknown      no_eligible_account
//	769  dispatcher  assigned -> sending   unknown      no_eligible_account
//	770  worker         sending  -> sent      unknown      no_eligible_account   <- a SUCCESS, tagged
//
// The operator-facing consequence, not merely a tidiness one: adminapi's queryErrorsInWindow is
// `COUNT(*) WHERE error_class IS NOT NULL GROUP BY error_class`, so GET /v1/admin/health reported
// errors_last_window {"unknown": 4} for that single failure — 4x incident sizing on the number the
// runbook's "Sends are failing" triage reads first. And as an audit record it is simply false:
// `sending -> sent` did not fail with error_class=unknown (CLAUDE.md Rule 5, "never invent an
// outcome"). The row-level fact on send_managed.jobs is fine and is deliberately NOT changed; only the
// append-only point-in-time event is corrected.
//
// ANTI-VACUITY (Rule 10). A test that only asserted "the success event has no error_class" would
// pass just as happily if the triggers had stopped recording error context ALTOGETHER — which would
// be a worse bug (silent failures). So this test is a PAIRED positive/negative control over the SAME
// job's SAME event chain:
//   - NEGATIVE: the non-error transitions (genesis, queued->assigned, assigned->sending,
//     sending->sent) carry NO error context — the defect.
//   - POSITIVE: the requeue that actually failed (sending->queued) STILL carries its real class and
//     message, unchanged by the fix.
//
// Both must hold. Blanking the columns entirely fails the positive half; reverting send_managed_0022 fails
// the negative half. Neither half is satisfiable by a regression that trips the other.

import (
	"context"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// jobEventChain returns every job_events row for a job as (from_state, to_state, actor, error_class,
// error_message), in seq order, with NULLs rendered as "". The FULL chain — not a filtered subset —
// because the defect was invisible in any single row: each row looked plausible on its own and only
// the chain showed one failure smeared across four events.
func (e *dispatchEnv) jobEventChain(t *testing.T, jobID string) [][5]string {
	t.Helper()
	rows, err := e.admin.Query(context.Background(),
		`SELECT COALESCE(from_state::text,''), to_state::text, actor::text,
		        COALESCE(error_class::text,''), COALESCE(error_message,'')
		   FROM send_managed.job_events WHERE job_id=$1 ORDER BY seq`, jobID)
	if err != nil {
		t.Fatalf("read job_events chain: %v", err)
	}
	defer rows.Close()
	var out [][5]string
	for rows.Next() {
		var fs, ts, ac, ec, em string
		if err := rows.Scan(&fs, &ts, &ac, &ec, &em); err != nil {
			t.Fatalf("scan job_events row: %v", err)
		}
		out = append(out, [5]string{fs, ts, ac, ec, em})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate job_events chain: %v", err)
	}
	return out
}

// jobLastErrorClass reads the job ROW's sticky last_error_class, "" when NULL. This is the
// row-level value send_managed_0022 deliberately leaves alone — read separately from the event chain so
// the test can assert the two behave DIFFERENTLY (row retains, event does not).
func (e *dispatchEnv) jobLastErrorClass(t *testing.T, jobID string) string {
	t.Helper()
	var class *string
	if err := e.admin.QueryRow(context.Background(),
		`SELECT last_error_class::text FROM send_managed.jobs WHERE id=$1`, jobID).Scan(&class); err != nil {
		t.Fatalf("read jobs.last_error_class: %v", err)
	}
	if class == nil {
		return ""
	}
	return *class
}

// TestJobEvents_ErrorContextOnlyOnErrorTransitions drives the REAL production chain — Dispatcher
// TryDispatch + OutcomeHandler.Handle — through fail-then-succeed on ONE job, which is exactly the
// shape that produced the live 4x inflation, and asserts both controls over the resulting chain.
func TestJobEvents_ErrorContextOnlyOnErrorTransitions(t *testing.T) {
	e := newDispatchEnv(t, "evterrctx")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "errctx@example.com")

	// --- Attempt 1: dispatch, then a REAL worker-reported transport_4xx failure. ---
	// A bare 451 is classified transport_4xx (classify.go row 5) and requeues the job
	// (applyClassifiedFailure), stamping send_managed.jobs.last_error_class — the sticky row-level value
	// that the old trigger then smeared onto every subsequent event.
	//
	// The wording matters and is not arbitrary: classify.go's rateText keywords ("try again later",
	// "rate", "too many", …) promote a 4xx to `throttle`, which PAUSES the account and would block
	// the re-dispatch attempt 2 depends on. This message deliberately contains none of them, so the
	// class is the no-account-penalty transport_4xx and the account stays eligible.
	e.dispatchToSending(t, job)
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
		SMTPResponse: "451 4.3.0 Temporary local error in processing"})

	if state, _, _, _, _ := e.jobState(t, job); state != "queued" {
		t.Fatalf("after the 451 the job is %s, want queued — this test needs a REAL requeue to have happened, else there is no sticky last_error_class to smear and both halves below are vacuous", state)
	}

	// PRECONDITION, and the whole premise of the defect: the row-level last_error_class is STICKY and
	// stays set. If a future change cleared it on requeue or on success, the smear could not happen and
	// this test would silently stop testing anything — so assert the premise rather than assume it.
	rowClass := e.jobLastErrorClass(t, job)
	if rowClass != "transport_4xx" {
		t.Fatalf("send_managed.jobs.last_error_class=%q, want transport_4xx — the requeue must record the real class on the ROW (that part is correct and unchanged by send_managed_0022). A different class here means classify.go routed this SMTP text elsewhere (e.g. rateText promoted it to throttle, which also pauses the account) and the rest of this test would be measuring a different path", rowClass)
	}

	// --- Attempt 2: re-dispatch the SAME job and let it SUCCEED. ---
	// The 451 backed next_attempt_at into the future (§6.4); clear it so the next tick claims the job.
	// This is scheduling only — it touches neither state nor an FK key, so per CLAUDE.md Rule 9(a)/(b)
	// it emits no event and takes no account lock.
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	e.px.freeAllWorkers()
	e.dispatchToSending(t, job)
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-errctx", SMTPResponse: "250 2.0.0 OK"})

	if state, _, _, _, _ := e.jobState(t, job); state != "sent" {
		t.Fatalf("job is %s, want sent — the success half of the chain is what carried the false error_class=unknown live, so the job MUST actually reach sent for this test to prove anything", state)
	}

	// The row-level value REMAINS transport_4xx across the success. This is the sticky-by-design
	// behavior send_managed_0022 deliberately did NOT change: "last error this job saw" is a running fact
	// about a mutable row and is useful history. The fix is scoped to the append-only event.
	if after := e.jobLastErrorClass(t, job); after != "transport_4xx" {
		t.Fatalf("send_managed.jobs.last_error_class=%q after the success, want transport_4xx retained — send_managed_0022 changes the TRIGGER, not the row. If clearing this is now intended it is a separate locked-contract change (Rule 2), not a test to relax", after)
	}

	chain := e.jobEventChain(t, job)

	// The chain must have the fail-then-succeed shape, or the two controls below are inspecting rows
	// that do not exist. Expected: genesis, queued->assigned, assigned->sending, sending->queued
	// (the 451), queued->assigned, assigned->sending, sending->sent.
	if len(chain) != 7 {
		t.Fatalf("job_events chain has %d rows, want 7 (genesis + 3 forward + requeue + 2 forward + sent); chain=%v", len(chain), chain)
	}

	// --- NEGATIVE CONTROL: every NON-error transition carries NO error context. ---
	// Includes the row that made this a real bug: sending -> sent.
	var sawSuccess, sawRequeue bool
	for i, ev := range chain {
		from, to, actor, class, msg := ev[0], ev[1], ev[2], ev[3], ev[4]
		isError := to == "failed" || to == "bounced" || (to == "queued" && from != "")
		if isError {
			sawRequeue = true
			continue
		}
		if to == "sent" {
			sawSuccess = true
		}
		if class != "" || msg != "" {
			t.Fatalf("job_events[%d] %s->%s (actor=%s) is NOT an error transition but carries error_class=%q error_message=%q. send_managed_0008 declares these columns 'only for error transitions'; send_managed_0022's trigger gate is what enforces it. This is the live defect: a sticky send_managed.jobs.last_error_* smeared onto an unrelated later transition — and for %s->%s it asserts a failure that did not happen (Rule 5). Full chain=%v",
				i, from, to, actor, class, msg, from, to, chain)
		}
	}
	if !sawSuccess {
		t.Fatalf("the chain contains no ->sent transition, so the negative control never examined the SUCCESS row — the single most important row here. chain=%v", chain)
	}

	// --- POSITIVE CONTROL: the one genuine error transition STILL records its real class + message. ---
	// This is what stops the fix from degenerating into "blank the columns and the test goes green":
	// suppressing error context everywhere would hide real failures from errors_last_window, which is
	// strictly worse than the defect it replaced.
	if !sawRequeue {
		t.Fatalf("the chain contains no error transition at all, so the positive control has nothing to check — the 451 must have produced a sending->queued requeue event. chain=%v", chain)
	}
	errorEvents := 0
	for i, ev := range chain {
		from, to, class, msg := ev[0], ev[1], ev[3], ev[4]
		if !(to == "failed" || to == "bounced" || (to == "queued" && from != "")) {
			continue
		}
		errorEvents++
		if class != "transport_4xx" {
			t.Fatalf("job_events[%d] %s->%s is a genuine error transition but error_class=%q, want transport_4xx — send_managed_0022 NARROWS which rows carry error context; it must never stop a real failure from being recorded, or errors_last_window under-reports and real incidents go invisible. chain=%v",
				i, from, to, class, chain)
		}
		if msg == "" {
			t.Fatalf("job_events[%d] %s->%s records error_class=%s but an EMPTY error_message — the class and the message are gated together, so both must survive on a real error. chain=%v", i, from, to, class, chain)
		}
	}

	// EXACTLY ONE error event for exactly one failure. This is the assertion that would have caught the
	// live 4x directly: pre-send_managed_0022 this count was 4 for the same single 451.
	if errorEvents != 1 {
		t.Fatalf("the chain carries %d error-tagged events for ONE failure, want exactly 1 — this is the health-metric inflation itself (queryErrorsInWindow counts each tagged row, so N here becomes N in errors_last_window). chain=%v", errorEvents, chain)
	}

	t.Logf("chain (7 rows, 1 error-tagged): %v", chain)
}
