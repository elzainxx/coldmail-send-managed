package dispatch

// cancel_test.go — M2-G3: POST /v1/cancel against the REAL dispatcher, outcome handler and reclaim loop.
//
// The API's own shape (validation, tenant scoping, idempotency, the report counts) is proven in
// internal/platformapi/cancel_test.go. What can ONLY be proven here is what cancel does to work that is
// moving: that nothing new is claimed or pushed after a cancel, that an item already in flight still
// finishes and reports exactly once, that an in-flight item which FAILS after the cancel is not retried, and
// that the three interleavings the design depends on cannot leave an item both cancelled and sent, or
// queued after a cancel. Each of those interleavings is FORCED — one side is held open on its own
// connection inside a transaction and the other is observed BLOCKED in pg_stat_activity before the holder
// is released — never raced and hoped for (CLAUDE.md Rule 10).

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// jobItemOf returns the job_item_id of a job (the key the campaign report's items carry).
func (e *dispatchEnv) jobItemOf(t *testing.T, jobID string) string {
	t.Helper()
	var id string
	if err := e.admin.QueryRow(context.Background(),
		`SELECT job_item_id::text FROM send_managed.jobs WHERE id=$1`, jobID).Scan(&id); err != nil {
		t.Fatalf("job_item_id of %s: %v", jobID, err)
	}
	return id
}

// waitForBlockedBackend proves CONTENTION: it returns only once a backend whose statement contains
// queryFragment is genuinely waiting on a lock, scoped to this clone (datname) AND to that statement
// (query ILIKE) per CLAUDE.md Rule 10 — a datname-only filter could match an unrelated backend. It fails the
// test if no such backend appears, because an interleave that never contended proves nothing.
func (e *dispatchEnv) waitForBlockedBackend(t *testing.T, queryFragment string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := e.admin.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND pid <> pg_backend_pid()
			    AND state = 'active' AND wait_event_type = 'Lock'
			    AND query ILIKE '%' || $1 || '%'`, queryFragment).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity probe: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("no backend running a statement containing %q ever blocked on a lock within 20s — the interleave was NOT forced, so this test proves nothing", queryFragment)
}

// cancelOutcome carries a cancel call's result back from the goroutine that made it.
type cancelOutcome struct {
	resp platformResp
	err  error
}

// cancelAsync fires POST /v1/cancel on its own goroutine and returns the channel its result arrives on.
func (c *platformClient) cancelAsync(campaignID, userID string) <-chan cancelOutcome {
	ch := make(chan cancelOutcome, 1)
	go func() {
		r, err := c.doErr("POST", "/v1/cancel", map[string]any{"campaign_id": campaignID, "user_id": userID})
		ch <- cancelOutcome{resp: r, err: err}
	}()
	return ch
}

// awaitCancel waits (bounded) for an async cancel to return and requires a 200.
func awaitCancel(t *testing.T, ch <-chan cancelOutcome) platformResp {
	t.Helper()
	select {
	case out := <-ch:
		if out.err != nil {
			t.Fatalf("cancel call failed: %v", out.err)
		}
		if out.resp.status != 200 {
			t.Fatalf("cancel: status=%d, want 200; body=%s", out.resp.status, out.resp.raw)
		}
		return out.resp
	case <-time.After(30 * time.Second):
		t.Fatal("cancel did not return within 30s after the blocking transaction was released — a lock was never released or a statement is wedged")
	}
	return platformResp{}
}

// pushedJobIDs returns every job id the recording pusher was asked to push, in order.
func (e *dispatchEnv) pushedJobIDs() []string {
	e.px.mu.Lock()
	defer e.px.mu.Unlock()
	out := make([]string, 0, len(e.px.pushes))
	for _, p := range e.px.pushes {
		out = append(out, p.JobID)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestCancel_NothingNewIsClaimedOrPushedAfterCancel — after a cancel, the live dispatcher claims NOTHING and
// pushes NOTHING for the cancelled campaign, however many ticks run; every item is `cancelled` with its two
// events and no send_logs row. PAIRED CONTROL: a second tenant's campaign, un-cancelled, in the SAME
// database and the SAME ticks, IS claimed and pushed — so the silence above is the cancel, not a dispatcher
// that was never going to move.
func TestCancel_NothingNewIsClaimedOrPushedAfterCancel(t *testing.T) {
	e := newDispatchEnv(t, "cancelnoclaim")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.addWorker(t) // two idle workers: one tick could push two jobs

	userA := e.seedUser(t, "cancel-a@tenant.example")
	userB := e.seedUser(t, "cancel-b@tenant.example")
	campA, jobsA := e.seedCampaignJobs(t, userA, 4)
	_, jobsB := e.seedCampaignJobs(t, userB, 2) // the control: never cancelled

	pc := e.newPlatformClient(t)
	res := pc.cancel(t, campA, userA)
	if res.num("cancelled") != 4 || res.num("in_flight") != 0 || res.num("already_terminal") != 0 {
		t.Fatalf("cancel of a fully-queued campaign of 4: %s, want cancelled=4 in_flight=0 already_terminal=0", res.raw)
	}

	totalPushed := 0
	for i := 0; i < 4; i++ {
		_, pushed, err := e.d.Tick(ctx)
		if err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		totalPushed += pushed
		e.px.freeAllWorkers()
	}
	if _, err := e.d.TryDispatch(ctx); err != nil { // the Phase-3 claim path is a second way to claim; it must honour the cancel too
		t.Fatalf("TryDispatch: %v", err)
	}

	pushed := e.pushedJobIDs()
	for _, j := range jobsA {
		if contains(pushed, j) {
			t.Fatalf("job %s of the CANCELLED campaign was pushed to a worker after the cancel", j)
		}
		c := e.jobColsOf(t, j)
		if c.State != "cancelled" || c.HasPMID || !c.CompletedSet || !c.LeaseNull || c.TerminalReason != "cancelled" {
			t.Fatalf("cancelled job %s = %+v, want state=cancelled, no provider_message_id, completed_at set, no lease, terminal_reason=cancelled", j, c)
		}
		if got := e.eventStates(t, j); len(got) != 2 || got[0] != "->queued" || got[1] != "queued>cancelled" {
			t.Fatalf("cancelled job %s event chain = %v, want [->queued queued>cancelled] — one event per transition and no claim", j, got)
		}
		if n := e.sendLogCount(t, j); n != 0 {
			t.Fatalf("cancelled job %s has %d send_logs rows — something sent it", j, n)
		}
	}

	// The control must have moved, or the assertions above are vacuous.
	if totalPushed == 0 || len(pushed) == 0 {
		t.Fatalf("control campaign was never pushed (pushed=%d, recorded=%v) — the dispatcher did not run, so 'nothing was claimed' proves nothing", totalPushed, pushed)
	}
	for _, j := range pushed {
		if !contains(jobsB, j) {
			t.Fatalf("the dispatcher pushed %s, which is not one of the control campaign's jobs %v", j, jobsB)
		}
	}
}

// TestCancel_InFlightItemsFinishAndAreReportedExactlyOnce — items already assigned/sending when the cancel
// lands are NOT touched in state: they finish (one sent, one bounced), each is reported exactly once, a
// REPLAYED report is a no-op, and neither ever acquires a cancelled event or a terminal_reason. The cancelled
// queued items alongside them stay cancelled, and the campaign reads as complete.
func TestCancel_InFlightItemsFinishAndAreReportedExactlyOnce(t *testing.T) {
	e := newDispatchEnv(t, "cancelinflight")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.addWorker(t)

	user := e.seedUser(t, "cancel-inflight@tenant.example")
	camp, jobs := e.seedCampaignJobs(t, user, 5)

	// Two items genuinely in flight through the real claim/handoff path.
	e.dispatchToSending(t, jobs[0])
	e.dispatchToSending(t, jobs[1])
	tok0 := e.jobAttemptToken(t, jobs[0])
	tok1 := e.jobAttemptToken(t, jobs[1])

	pc := e.newPlatformClient(t)
	res := pc.cancel(t, camp, user)
	if res.num("cancelled") != 3 || res.num("in_flight") != 2 || res.num("already_terminal") != 0 {
		t.Fatalf("cancel with 2 in flight and 3 queued: %s, want cancelled=3 in_flight=2 already_terminal=0", res.raw)
	}
	// In flight means UNTOUCHED in state: still sending, lease intact, flagged (so a failure would not retry).
	for _, j := range jobs[:2] {
		c := e.jobColsOf(t, j)
		if c.State != "sending" || c.LeaseNull || !c.CancelRequestedSet || c.TerminalReason != "" {
			t.Fatalf("in-flight job %s after cancel = %+v, want state=sending, lease kept, cancel flag set, no terminal_reason", j, c)
		}
	}

	// Item 0 is delivered; the same report is then REPLAYED (a worker retry / buffered duplicate).
	acc := workerproto.Report{JobID: jobs[0], AttemptToken: tok0, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-CANCEL-0", SMTPResponse: "250 2.0.0 Ok"}
	if !e.oh.Handle(e.jobWorker(t, jobs[0]), acc) {
		t.Fatal("the in-flight item's accepted report was not resolved")
	}
	if !e.oh.Handle(e.jobWorker(t, jobs[0]), acc) {
		t.Fatal("the replayed accepted report was not acked as a no-op")
	}
	// Item 1 is a hard bounce.
	if !e.oh.Handle(e.jobWorker(t, jobs[1]), workerproto.Report{JobID: jobs[1], AttemptToken: tok1,
		Outcome: workerproto.OutcomeBounced, SMTPResponse: "550 5.1.1 No such user"}) {
		t.Fatal("the in-flight item's bounce report was not resolved")
	}

	c0 := e.jobColsOf(t, jobs[0])
	if c0.State != "sent" || !c0.HasPMID || c0.TerminalReason != "" {
		t.Fatalf("delivered in-flight job = %+v, want sent with a provider_message_id and NO terminal_reason (it was not cancelled)", c0)
	}
	if got := e.eventStates(t, jobs[0]); len(got) != 4 || got[3] != "sending>sent" {
		t.Fatalf("delivered job event chain = %v, want exactly [->queued queued>assigned assigned>sending sending>sent] — one 'sent', no 'cancelled', and the replay wrote nothing", got)
	}
	if n := e.sendLogCount(t, jobs[0]); n != 1 {
		t.Fatalf("delivered job has %d send_logs rows after a REPLAYED report, want exactly 1 (reported exactly once)", n)
	}
	c1 := e.jobColsOf(t, jobs[1])
	if c1.State != "bounced" || c1.TerminalReason != "" {
		t.Fatalf("bounced in-flight job = %+v, want bounced with no terminal_reason", c1)
	}
	if got := e.eventStates(t, jobs[1]); got[len(got)-1] != "sending>bounced" || contains(got, "sending>cancelled") {
		t.Fatalf("bounced job event chain = %v, want it to END sending>bounced with no cancelled transition", got)
	}

	// The campaign now reads complete: 1 sent + 1 bounced + 3 cancelled, nothing in flight, and each item
	// says why.
	rep := pc.campaignReport(t, camp, user)
	tot := rep.totals(t)
	if tot["sent"] != 1.0 || tot["bounced"] != 1.0 || tot["cancelled"] != 3.0 || tot["in_flight"] != 0.0 || tot["total"] != 5.0 {
		t.Fatalf("campaign totals = %v, want total=5 sent=1 bounced=1 cancelled=3 in_flight=0", tot)
	}
	prog, _ := rep.body["progress"].(map[string]any)
	if prog["terminal"] != 5.0 || prog["is_complete"] != true {
		t.Fatalf("progress = %v, want terminal=5 is_complete=true (cancelled is terminal)", prog)
	}
	reasons := map[string]string{} // job_item_id -> reason ("" for null)
	for _, it := range rep.items(t) {
		r, _ := it["reason"].(string)
		reasons[it["job_item_id"].(string)] = r
		if _, present := it["reason"]; !present {
			t.Fatalf("item %v has no `reason` key — it must be present (null) on every item", it)
		}
	}
	if reasons[e.jobItemOf(t, jobs[0])] != "" {
		t.Fatalf("the SENT item's reason = %q, want null", reasons[e.jobItemOf(t, jobs[0])])
	}
	if reasons[e.jobItemOf(t, jobs[1])] != "bounce_permanent" {
		t.Fatalf("the BOUNCED item's reason = %q, want bounce_permanent", reasons[e.jobItemOf(t, jobs[1])])
	}
	for _, j := range jobs[2:] {
		if reasons[e.jobItemOf(t, j)] != "cancelled" {
			t.Fatalf("a cancelled item's reason = %q, want cancelled", reasons[e.jobItemOf(t, j)])
		}
	}

	// And the dispatcher still moves nothing for this campaign.
	before := len(e.pushedJobIDs())
	for i := 0; i < 3; i++ {
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if after := len(e.pushedJobIDs()); after != before {
		t.Fatalf("pushes grew %d -> %d after the cancel settled — something was claimed", before, after)
	}
}

// TestCancel_RaceWithClaim_NeverLeavesAnItemBothCancelledAndSent — THE forced interleave the ruling asks
// for. The dispatcher's claim (the PRODUCTION claimForUserQuery, run on its own connection as send_managed_app)
// is frozen holding the row lock on the oldest item, UNCOMMITTED. A cancel is then fired and OBSERVED BLOCKED in
// pg_stat_activity behind that lock. The claim is released; the cancel completes. The item the claim won is
// in flight and finishes as sent — it is NOT cancelled — and every item the claim did not win is cancelled
// and never sent. No item has both a `cancelled` and a `sent` event.
func TestCancel_RaceWithClaim_NeverLeavesAnItemBothCancelledAndSent(t *testing.T) {
	e := newDispatchEnv(t, "cancelraceclaim")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	user := e.seedUser(t, "cancel-race@tenant.example")
	camp, jobs := e.seedCampaignJobs(t, user, 4)
	pc := e.newPlatformClient(t)

	// The claim, frozen mid-flight on its own connection.
	claimConn := connect(t, e.moduleDSN)
	claimTx, err := claimConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin claim tx: %v", err)
	}
	defer claimTx.Rollback(ctx)
	if _, err := claimTx.Exec(ctx, "SELECT set_config('send_managed.actor','dispatcher',true)"); err != nil {
		t.Fatalf("set actor: %v", err)
	}
	rows, err := claimTx.Query(ctx, claimForUserQuery, user, 1, 120)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	var won claimedJob
	n := 0
	for rows.Next() {
		if err := rows.Scan(&won.id, &won.userID, &won.campaignID, &won.recipient, &won.subject, &won.body, &won.text, &won.replyTo); err != nil {
			t.Fatalf("scan claimed: %v", err)
		}
		n++
	}
	rows.Close()
	if n != 1 || !contains(jobs, won.id) {
		t.Fatalf("the held claim won %d rows (%q), want exactly 1 of the campaign's jobs", n, won.id)
	}

	// The cancel, fired while that claim holds the row.
	done := pc.cancelAsync(camp, user)
	e.waitForBlockedBackend(t, "cancel_requested_at") // PROOF OF CONTENTION: the cancel is waiting on the claim's row lock
	select {
	case out := <-done:
		t.Fatalf("cancel returned (%+v) while the claim still held the row lock — it did not wait, so the interleave was not what we think", out)
	default:
	}

	if err := claimTx.Commit(ctx); err != nil {
		t.Fatalf("commit claim: %v", err)
	}
	res := awaitCancel(t, done)

	// The claim won exactly one row; the cancel took the other three.
	if res.num("cancelled") != 3 || res.num("in_flight") != 1 || res.num("already_terminal") != 0 {
		t.Fatalf("cancel after losing one row to the claim: %s, want cancelled=3 in_flight=1 already_terminal=0", res.raw)
	}
	wc := e.jobColsOf(t, won.id)
	if wc.State != "assigned" || !wc.CancelRequestedSet || wc.TerminalReason != "" {
		t.Fatalf("the claimed item = %+v, want assigned (in flight, NOT cancelled) with the cancel flag set", wc)
	}

	// It finishes through the real handoff and report path: pushed once, sent once.
	cfg := e.mustLoadConfig(t)
	pushed, err := e.d.resolveAndPush(ctx, won, e.px.workerID, cfg)
	if err != nil || !pushed {
		t.Fatalf("resolveAndPush of the claimed item: pushed=%v err=%v", pushed, err)
	}
	tok := e.jobAttemptToken(t, won.id)
	if !e.oh.Handle(e.px.workerID, workerproto.Report{JobID: won.id, AttemptToken: tok, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-RACE-CLAIM", SMTPResponse: "250 ok"}) {
		t.Fatal("the claimed item's accepted report was not resolved")
	}

	// THE INVARIANT, per item: none is both cancelled and sent, and exactly the claimed one is sent.
	sent := 0
	for _, j := range jobs {
		chain := e.eventStates(t, j)
		wasCancelled := contains(chain, "queued>cancelled")
		wasSent := contains(chain, "sending>sent")
		if wasCancelled && wasSent {
			t.Fatalf("job %s has BOTH a cancelled and a sent event: %v", j, chain)
		}
		c := e.jobColsOf(t, j)
		if j == won.id {
			if c.State != "sent" || wasCancelled {
				t.Fatalf("the claimed item = %+v chain=%v, want sent and never cancelled", c, chain)
			}
			sent++
			continue
		}
		if c.State != "cancelled" || c.HasPMID || e.sendLogCount(t, j) != 0 {
			t.Fatalf("an item the claim did NOT win = %+v, want cancelled with nothing sent", c)
		}
	}
	if sent != 1 {
		t.Fatalf("%d items sent, want exactly the one the claim won", sent)
	}
	if got := e.pushedJobIDs(); len(got) != 1 || got[0] != won.id {
		t.Fatalf("pushes = %v, want exactly the claimed item", got)
	}
}

// TestCancel_InFlightRequeueRacingCancel_NeverLeavesAQueuedItem — the OTHER interleave: an in-flight item's
// attempt FAILS and is requeued (its UPDATE held open, uncommitted, on its own connection) while the
// cancel runs. The cancel is observed BLOCKED on that row; when the requeue commits the row is `queued`, and
// the cancel — whose first step waited for it and re-read it — flags it and cancels it. The item must end
// `cancelled`, never `queued`: a queued item after a cancel is a claimable item after a cancel.
func TestCancel_InFlightRequeueRacingCancel_NeverLeavesAQueuedItem(t *testing.T) {
	e := newDispatchEnv(t, "cancelracerequeue")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	user := e.seedUser(t, "cancel-requeue@tenant.example")
	camp, jobs := e.seedCampaignJobs(t, user, 3)
	e.dispatchToSending(t, jobs[0])
	inflight := jobs[0]
	pc := e.newPlatformClient(t)

	// The requeue half of an outcome handler, frozen: sending -> queued, uncommitted.
	rqConn := connect(t, e.moduleDSN)
	rq, err := rqConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin requeue tx: %v", err)
	}
	defer rq.Rollback(ctx)
	if _, err := rq.Exec(ctx, "SELECT set_config('send_managed.actor','box',true)"); err != nil {
		t.Fatalf("set actor: %v", err)
	}
	if tag, err := rq.Exec(ctx,
		`UPDATE send_managed.jobs SET state='queued', lease_expires_at=NULL, last_error_class='transport_4xx',
		        last_error_message='451 4.3.0 temporary', next_attempt_at=NOW()
		  WHERE id=$1 AND state='sending'`, inflight); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("requeue update: tag=%v err=%v", tag, err)
	}

	done := pc.cancelAsync(camp, user)
	e.waitForBlockedBackend(t, "cancel_requested_at")
	select {
	case out := <-done:
		t.Fatalf("cancel returned (%+v) while the requeue still held the row — it did not wait", out)
	default:
	}
	if err := rq.Commit(ctx); err != nil {
		t.Fatalf("commit requeue: %v", err)
	}
	res := awaitCancel(t, done)

	if res.num("cancelled") != 3 || res.num("in_flight") != 0 {
		t.Fatalf("cancel: %s, want all 3 cancelled (the requeued one included) and none in flight", res.raw)
	}
	c := e.jobColsOf(t, inflight)
	if c.State != "cancelled" || c.TerminalReason != "cancelled" || !c.CompletedSet {
		t.Fatalf("the requeued in-flight item = %+v, want cancelled — a queued item after a cancel could be claimed and sent", c)
	}
	if got := e.eventStates(t, inflight); got[len(got)-2] != "sending>queued" || got[len(got)-1] != "queued>cancelled" {
		t.Fatalf("event chain = %v, want it to end [sending>queued queued>cancelled]", got)
	}
	// Nothing is left claimable.
	before := len(e.pushedJobIDs())
	for i := 0; i < 3; i++ {
		e.px.freeAllWorkers()
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if after := len(e.pushedJobIDs()); after != before {
		t.Fatalf("pushes grew %d -> %d — a cancelled campaign's item was claimed", before, after)
	}
}

// TestCancel_InFlightFailureAfterCancelIsNotRetried — an item in flight when the cancel lands whose attempt
// then FAILS (a real classified failure through the real outcome handler) ends `cancelled`, not `queued`: the
// failed attempt is still recorded (counter, class, send_logs row) and reported truthfully, but the retry the
// platform asked us not to make is not made. PAIRED CONTROL: the identical failure on an item of a campaign
// that was NOT cancelled is requeued as always — so the difference is the cancel, not the failure path.
func TestCancel_InFlightFailureAfterCancelIsNotRetried(t *testing.T) {
	e := newDispatchEnv(t, "cancelfailnoretry")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.addWorker(t)
	user := e.seedUser(t, "cancel-fail@tenant.example")
	camp, jobs := e.seedCampaignJobs(t, user, 3)
	_, ctrlJobs := e.seedCampaignJobs(t, user, 1)

	// One item of the campaign goes in flight; the campaign is cancelled around it; THEN the control item
	// (a different campaign, never cancelled) goes in flight too. (The order is not cosmetic: the claim takes
	// the OLDEST queued item, so the control item can only be dispatched once the cancel has taken the
	// campaign's remaining queued items out of its way.)
	e.dispatchToSending(t, jobs[0])
	tokJ := e.jobAttemptToken(t, jobs[0])

	pc := e.newPlatformClient(t)
	if res := pc.cancel(t, camp, user); res.num("cancelled") != 2 || res.num("in_flight") != 1 {
		t.Fatalf("cancel: %s, want cancelled=2 in_flight=1", res.raw)
	}
	e.dispatchToSending(t, ctrlJobs[0])
	tokC := e.jobAttemptToken(t, ctrlJobs[0])

	failure := func(job string, tok int64) workerproto.Report {
		return workerproto.Report{JobID: job, AttemptToken: tok, Outcome: workerproto.OutcomeError,
			SMTPResponse: "451 4.3.0 temporary local error"}
	}
	if !e.oh.Handle(e.jobWorker(t, jobs[0]), failure(jobs[0], tokJ)) {
		t.Fatal("the cancelled campaign's failure report was not resolved")
	}
	if !e.oh.Handle(e.jobWorker(t, ctrlJobs[0]), failure(ctrlJobs[0], tokC)) {
		t.Fatal("the control campaign's failure report was not resolved")
	}

	j := e.jobColsOf(t, jobs[0])
	if j.State != "cancelled" || j.FailedAttemptCount != 1 || j.LastErrorClass != "transport_4xx" || j.TerminalReason != "cancelled" {
		t.Fatalf("failed in-flight item of a cancelled campaign = %+v, want cancelled with the failure still recorded (count=1, class=transport_4xx)", j)
	}
	if n := e.sendLogCount(t, jobs[0]); n != 1 {
		t.Fatalf("the failed attempt wrote %d send_logs rows, want 1 — a cancel must not hide that an attempt happened", n)
	}
	if got := e.eventStates(t, jobs[0]); got[len(got)-1] != "sending>cancelled" {
		t.Fatalf("event chain = %v, want it to end sending>cancelled", got)
	}
	// The report says WHY: cancelled, with the class of the attempt that failed.
	rep := pc.jobReport(t, jobs[0], user)
	if r, _ := rep.strOrNil("reason"); r != "cancelled" {
		t.Fatalf("report reason = %q, want cancelled: %s", r, rep.raw)
	}
	if ec, _ := rep.strOrNil("error_class"); ec != "transport_4xx" {
		t.Fatalf("report error_class = %q, want transport_4xx: %s", ec, rep.raw)
	}

	c := e.jobColsOf(t, ctrlJobs[0])
	if c.State != "queued" || c.FailedAttemptCount != 1 {
		t.Fatalf("CONTROL: the same failure on an un-cancelled campaign = %+v, want queued (retry) with count=1", c)
	}

	// Nothing of the cancelled campaign is ever claimed again.
	cancelledPushes := func() int {
		n := 0
		for _, id := range e.pushedJobIDs() {
			if contains(jobs, id) {
				n++
			}
		}
		return n
	}
	before := cancelledPushes()
	for i := 0; i < 3; i++ {
		e.px.freeAllWorkers()
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if after := cancelledPushes(); after != before {
		t.Fatalf("the cancelled campaign's jobs were pushed %d -> %d times after the cancel", before, after)
	}
}

// TestCancel_ReclaimOfAnInFlightItemOfACancelledCampaignCancelsIt — the reclaim loop is a SECOND writer that
// returns an in-flight job to the queue (a lease that expired, a dead worker). For a cancelled campaign it
// must not: the reclaimed item ends `cancelled`. The reclaim still counts as a failed attempt (it was
// SENDING), which is true either way. PAIRED CONTROL: the same reclaim on an un-cancelled campaign requeues.
func TestCancel_ReclaimOfAnInFlightItemOfACancelledCampaignCancelsIt(t *testing.T) {
	e := newDispatchEnv(t, "cancelreclaim")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.addWorker(t)
	user := e.seedUser(t, "cancel-reclaim@tenant.example")
	camp, jobs := e.seedCampaignJobs(t, user, 2)
	_, ctrl := e.seedCampaignJobs(t, user, 1)

	// Same ordering note as the failure test: the control item can only be claimed after the cancel has
	// cleared the campaign's other queued item out of the (oldest-first) claim's way.
	e.dispatchToSending(t, jobs[0])
	pc := e.newPlatformClient(t)
	pc.cancel(t, camp, user)
	e.dispatchToSending(t, ctrl[0])

	e.expireLease(t, jobs[0])
	e.expireLease(t, ctrl[0])
	if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 2 {
		t.Fatalf("ReclaimTick: n=%d err=%v, want both expired leases reclaimed", n, err)
	}
	if c := e.jobColsOf(t, jobs[0]); c.State != "cancelled" || c.FailedAttemptCount != 1 || c.TerminalReason != "cancelled" || !c.LeaseNull {
		t.Fatalf("reclaimed item of a cancelled campaign = %+v, want cancelled (count=1, lease cleared)", c)
	}
	if got := e.eventStates(t, jobs[0]); got[len(got)-1] != "sending>cancelled" {
		t.Fatalf("event chain = %v, want it to end sending>cancelled", got)
	}
	if c := e.jobColsOf(t, ctrl[0]); c.State != "queued" {
		t.Fatalf("CONTROL: the same reclaim on an un-cancelled campaign = %+v, want queued", c)
	}
}
