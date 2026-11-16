package dispatch

// report_reason_test.go — M2-G1: the platform can tell WHY an item is in the state it is in.
//
// Every item of GET /v1/report (single-job and campaign) carries `reason` and `error_class`. The mapping from
// stored facts to those two keys is a pure function proven table-by-table in internal/platformapi; what that
// cannot prove is that the facts the REAL paths write are the facts the mapping reads. So this test produces
// every published reason through the real dispatcher / outcome handler / reclaim / exhaustion / cancel code
// and reads each one back through the production platformapi.Server, in BOTH report shapes, asserting the
// two agree. It ends by comparing the set of reasons it actually produced with the set /v1/describe
// publishes, both directions: a published reason with no real path behind it, or a produced reason that was
// never published, fails here.
//
// It is one test with sequential phases on purpose: the phases share one database and the ORDER is part of
// the design (capacity must exist for the class-bearing phases and must then be taken away for the last
// two), and the closing set comparison only means something if every phase ran in the same test.

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// failedAttemptReport drives one real failed attempt with a caller-built worker report.
func (e *dispatchEnv) failedAttemptReport(t *testing.T, job string, mk func(token int64) workerproto.Report) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(),
		`UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
		t.Fatalf("make job claimable: %v", err)
	}
	e.px.freeAllWorkers()
	e.dispatchToSending(t, job)
	if !e.oh.Handle(e.jobWorker(t, job), mk(e.jobAttemptToken(t, job))) {
		t.Fatalf("the failure report for job %s was not resolved", job)
	}
}

func TestReportReason_EveryPublishedReasonThroughARealPath(t *testing.T) {
	e := newDispatchEnv(t, "reportreason")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "s1")
	e.seedAccount(t, pool, "s2")
	// Account health (quarantine/pause) and the attempt cap are separate mechanisms with their own suites;
	// taking them out of the way is what lets a class-bearing item stay QUEUED so its reason can be read.
	e.setConfigStr(t, "autoquarantine_mode", "off")
	e.setConfigStr(t, "autopause_mode", "off")
	e.setConfigInt(t, "job_max_failed_attempts", 1000)
	pc := e.newPlatformClient(t)

	seen := map[string]bool{}

	park := func(job string) {
		t.Helper()
		if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW()+INTERVAL '1 hour' WHERE id=$1`, job); err != nil {
			t.Fatalf("park job: %v", err)
		}
	}

	// check reads one job through BOTH report shapes and asserts the state, the reason and the error_class
	// ("" means JSON null) — and that the two shapes agree with each other.
	check := func(label, campaign, user, job, wantState, wantReason, wantClass string) {
		t.Helper()
		single := pc.jobReport(t, job, user)
		if !single.hasKey("reason") || !single.hasKey("error_class") {
			t.Fatalf("%s: the single-job report must ALWAYS carry both `reason` and `error_class` keys (null when empty): %s", label, single.raw)
		}
		state, _ := single.strOrNil("state")
		reason, _ := single.strOrNil("reason")
		class, _ := single.strOrNil("error_class")
		if state != wantState || reason != wantReason || class != wantClass {
			t.Fatalf("%s: single report = (state=%q reason=%q error_class=%q), want (%q, %q, %q): %s",
				label, state, reason, class, wantState, wantReason, wantClass, single.raw)
		}

		item := e.jobItemOf(t, job)
		var found map[string]any
		for _, it := range pc.campaignReport(t, campaign, user).items(t) {
			if it["job_item_id"] == item {
				found = it
			}
		}
		if found == nil {
			t.Fatalf("%s: job_item %s is not in campaign %s's report", label, item, campaign)
		}
		if _, ok := found["reason"]; !ok {
			t.Fatalf("%s: the campaign item has no `reason` key: %v", label, found)
		}
		if _, ok := found["error_class"]; !ok {
			t.Fatalf("%s: the campaign item has no `error_class` key: %v", label, found)
		}
		cReason, _ := found["reason"].(string)
		cClass, _ := found["error_class"].(string)
		if found["state"] != wantState || cReason != wantReason || cClass != wantClass {
			t.Fatalf("%s: campaign item = %v, want (state=%q reason=%q error_class=%q) — the two report shapes must agree",
				label, found, wantState, wantReason, wantClass)
		}
		if wantReason != "" {
			seen[wantReason] = true
		}
	}
	checkSys := func(label, job, wantState, wantReason, wantClass string) {
		t.Helper()
		check(label, systemTenant, systemTenant, job, wantState, wantReason, wantClass)
	}

	// ---- PHASE 1: capacity exists. -------------------------------------------------------------------

	// A job nobody has tried: no reason, no class — the keys are present and null.
	never := e.seedQueuedJob(t, "rr-never@example.com")
	checkSys("never tried", never, "queued", "", "")
	park(never)

	// The five failure classes, each through the real classifier and the real requeue.
	classes := []struct {
		label, class string
		report       func(tok int64) workerproto.Report
	}{
		{"transport_4xx", "transport_4xx", func(tok int64) workerproto.Report {
			return workerproto.Report{JobID: "", AttemptToken: tok, Outcome: workerproto.OutcomeError, SMTPResponse: "451 4.3.0 temporary local error"}
		}},
		{"auth_failure", "auth_failure", func(tok int64) workerproto.Report {
			return workerproto.Report{AttemptToken: tok, Outcome: workerproto.OutcomeError, SMTPResponse: "535 5.7.8 Username and Password not accepted"}
		}},
		{"throttle", "throttle", func(tok int64) workerproto.Report {
			return workerproto.Report{AttemptToken: tok, Outcome: workerproto.OutcomeError, SMTPResponse: "421 4.7.0 Too many messages, rate limit exceeded"}
		}},
		{"timeout", "timeout", func(tok int64) workerproto.Report {
			return workerproto.Report{AttemptToken: tok, Outcome: workerproto.OutcomeError, ErrorReason: workerproto.ReasonTimeout}
		}},
		{"unknown", "unknown", func(tok int64) workerproto.Report {
			return workerproto.Report{AttemptToken: tok, Outcome: workerproto.OutcomeError, SMTPResponse: "554 5.7.1 Transaction failed: policy rejection"}
		}},
	}
	var classQueuedJob string // kept for the live-condition phases below
	for _, c := range classes {
		job := e.seedQueuedJob(t, "rr-"+c.label+"@example.com")
		jobID := job
		e.failedAttemptReport(t, job, func(tok int64) workerproto.Report {
			r := c.report(tok)
			r.JobID = jobID
			return r
		})
		checkSys("queued after a "+c.label+" failure", job, "queued", c.class, c.class)
		if c.label == "transport_4xx" {
			classQueuedJob = job
		}
		park(job)
	}

	// wantMsg proves a scenario really went through the dispatcher path it names: the internal requeue
	// reason the production code recorded on the row is the string platformapi's mapping matches by value
	// (item_reason.go's dispatcherSideRequeueReason) — so a rename on either side fails HERE, loudly, instead
	// of silently turning "no_worker_available" into "unknown".
	wantMsg := func(job, prefix string) {
		t.Helper()
		var msg string
		if err := e.admin.QueryRow(ctx, `SELECT COALESCE(last_error_message,'') FROM send_managed.jobs WHERE id=$1`, job).Scan(&msg); err != nil {
			t.Fatalf("read last_error_message: %v", err)
		}
		if !strings.HasPrefix(msg, prefix) {
			t.Fatalf("job %s last_error_message = %q, want it to start with %q — the scenario did not exercise the path it claims", job, msg, prefix)
		}
	}

	// Dispatcher-side requeues: the item never reached SMTP, so these are not failures — each has its own
	// reason, and the class column carries 'unknown' (the stored value) while the reason says what happened.
	noAcct := e.seedQueuedJob(t, "rr-noacct@example.com")
	if claimed, err := e.d.claimForUser(ctx, systemTenant, 1, 120); err != nil || len(claimed) != 1 || claimed[0].id != noAcct {
		t.Fatalf("claim for the no_eligible_account scenario: %v err=%v, want exactly %s", claimed, err, noAcct)
	} else {
		e.d.requeueNoAccount(ctx, claimed[0].id)
	}
	wantMsg(noAcct, "no_eligible_account")
	checkSys("claim found no account", noAcct, "queued", "no_eligible_account", "unknown")
	park(noAcct)

	noWorker := e.seedQueuedJob(t, "rr-noworker@example.com")
	savedWorker := e.px.workerID
	e.px.workerID = ""
	if _, _, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick without a worker: %v", err)
	}
	e.px.workerID = savedWorker
	wantMsg(noWorker, "no_worker_available")
	checkSys("no worker was connected", noWorker, "queued", "waiting_for_capacity", "unknown")
	park(noWorker)

	pushFails := e.seedQueuedJob(t, "rr-pushfail@example.com")
	e.px.freeAllWorkers() // an earlier scenario left the one worker marked busy, which would route this to no_worker_available instead
	e.px.failPush = true
	if _, _, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick with a failing push: %v", err)
	}
	e.px.failPush = false
	wantMsg(pushFails, "push_failed")
	checkSys("the handoff failed", pushFails, "queued", "waiting_for_capacity", "unknown")
	park(pushFails)

	poisoned := e.seedQueuedJobSubject(t, "rr-poison@example.com", "Hello\r\nBcc: victim@evil.example")
	e.px.freeAllWorkers()
	if _, _, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick with a poisoned subject: %v", err)
	}
	wantMsg(poisoned, "header_injection:")
	checkSys("header-injection guard", poisoned, "queued", "unknown", "unknown")
	park(poisoned)

	// sent: the reason is null and so is the class, EVEN THOUGH the row still carries the earlier failure's
	// class — a delivered item has nothing the caller must act on.
	sentJob := e.seedQueuedJob(t, "rr-sent@example.com")
	e.failedAttempt(t, sentJob, "451 4.3.0 temporary local error")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, sentJob); err != nil {
		t.Fatalf("make claimable: %v", err)
	}
	e.px.freeAllWorkers()
	e.dispatchToSending(t, sentJob)
	if !e.oh.Handle(e.jobWorker(t, sentJob), workerproto.Report{JobID: sentJob, AttemptToken: e.jobAttemptToken(t, sentJob),
		Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "PMID-RR-SENT", SMTPResponse: "250 ok"}) {
		t.Fatal("the accepted report was not resolved")
	}
	if c := e.jobColsOf(t, sentJob); c.State != "sent" || c.LastErrorClass != "transport_4xx" {
		t.Fatalf("precondition: sent job = %+v, want sent AND still carrying last_error_class=transport_4xx (otherwise the null below proves nothing)", c)
	}
	checkSys("sent after a failed attempt", sentJob, "sent", "", "")

	// bounced.
	bounced := e.seedQueuedJob(t, "rr-bounce@example.com")
	e.px.freeAllWorkers()
	e.dispatchToSending(t, bounced)
	if !e.oh.Handle(e.jobWorker(t, bounced), workerproto.Report{JobID: bounced, AttemptToken: e.jobAttemptToken(t, bounced),
		Outcome: workerproto.OutcomeBounced, SMTPResponse: "550 5.1.1 The email account that you tried to reach does not exist"}) {
		t.Fatal("the bounce report was not resolved")
	}
	checkSys("hard bounce", bounced, "bounced", "bounce_permanent", "bounce_permanent")

	// failed / attempts_exhausted: the attempt cap, with the dial at 1 so one real failure ends the job.
	e.setConfigInt(t, "job_max_failed_attempts", 1)
	exhausted := e.seedQueuedJob(t, "rr-exhausted@example.com")
	e.failedAttempt(t, exhausted, "554 5.7.1 Transaction failed: policy rejection")
	checkSys("attempt cap reached", exhausted, "failed", "attempts_exhausted", "unknown")
	e.setConfigInt(t, "job_max_failed_attempts", 1000)

	// cancelled, in a campaign of its own.
	userC := e.seedUser(t, "rr-cancel@tenant.example")
	campC, jobsC := e.seedCampaignJobs(t, userC, 1)
	pc.cancel(t, campC, userC)
	check("cancelled by the platform", campC, userC, jobsC[0], "cancelled", "cancelled", "")

	// ---- PHASE 2: the module is paused. A live condition outranks the item's own history. -------------
	e.setRung(t, "accept_and_defer")
	checkSys("paused, item last failed transport_4xx", classQueuedJob, "queued", "dispatch_paused", "transport_4xx")
	checkSys("paused, item never tried", never, "queued", "dispatch_paused", "")
	e.setRung(t, "serving")
	checkSys("unpaused again", classQueuedJob, "queued", "transport_4xx", "transport_4xx")

	// ---- PHASE 3: capacity is taken away. ------------------------------------------------------------
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET quarantined=TRUE WHERE pool_id=$1`, pool); err != nil {
		t.Fatalf("quarantine the pool: %v", err)
	}
	checkSys("no eligible account right now, item last failed transport_4xx", classQueuedJob, "queued", "waiting_for_capacity", "transport_4xx")
	camp := pc.campaignReport(t, systemTenant, systemTenant)
	if w := camp.totals(t)["waiting_for_capacity"]; w == nil || w.(float64) < 1 {
		t.Fatalf("campaign totals.waiting_for_capacity = %v with no eligible account, want >= 1 (the item-level reason and the aggregate must agree): %s", w, camp.raw)
	}
	// ...and a READY job in a pool whose every account is terminal-blocked is failed by exhaustion.
	exhaustedPool := e.seedQueuedJob(t, "rr-poolex@example.com")
	if _, _, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick over an exhausted pool: %v", err)
	}
	checkSys("pool exhausted", exhaustedPool, "failed", "pool_exhausted", "")

	// ---- The closing set comparison, both directions. -----------------------------------------------
	desc := pc.do(t, "GET", "/v1/describe", nil)
	vocabs, _ := desc.body["vocabularies"].(map[string]any)
	ir, _ := vocabs["item_reason"].(map[string]any)
	vals, _ := ir["values"].([]any)
	published := map[string]bool{}
	for _, v := range vals {
		s, _ := v.(string)
		published[s] = true
	}
	if len(published) == 0 {
		t.Fatalf("describe publishes no item_reason vocabulary: %s", desc.raw)
	}
	var unproduced, unpublished []string
	for r := range published {
		if !seen[r] {
			unproduced = append(unproduced, r)
		}
	}
	for r := range seen {
		if !published[r] {
			unpublished = append(unpublished, r)
		}
	}
	sort.Strings(unproduced)
	sort.Strings(unpublished)
	if len(unproduced) > 0 {
		t.Fatalf("describe publishes item_reason values that NO real path in this test produced: %s — a reason nothing can emit is a reason a client writes dead handling for", strings.Join(unproduced, ", "))
	}
	if len(unpublished) > 0 {
		t.Fatalf("the real paths produced reasons describe does NOT publish: %s", strings.Join(unpublished, ", "))
	}
}
