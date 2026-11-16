package platformapi

// cancel_test.go — M2-G3: POST /v1/cancel, the API half. Real Postgres, real HTTP against the production
// Server. (What cancel does to WORK THAT IS MOVING — the claim race, an in-flight failure after the cancel,
// the reclaim — needs the real dispatcher and lives in internal/dispatch/cancel_test.go.)
//
// Every test here pairs the property with its control: a tenant-scoping assertion with the owning tenant
// succeeding in the same database, an idempotency assertion with a first call that DID change something,
// and so on — so a route that does nothing, or does everything, cannot pass.

import (
	"strings"
	"testing"
)

// cancelBody builds the request body.
func cancelBody(campaignID, userID string) map[string]any {
	return map[string]any{"campaign_id": campaignID, "user_id": userID}
}

// stateCounts returns campaign state -> count straight from the database (the ground truth the response is
// checked against).
func (e *testEnv) stateCounts(t *testing.T, campaignID string) map[string]int {
	t.Helper()
	rows, err := e.admin.Query(bgctx(),
		`SELECT state::text, count(*) FROM send_managed.jobs WHERE campaign_id=$1 GROUP BY state`, campaignID)
	if err != nil {
		t.Fatalf("state counts: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[s] = n
	}
	return out
}

// eventCountForCampaign counts every job_events row of every job of a campaign.
func (e *testEnv) eventCountForCampaign(t *testing.T, campaignID string) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(bgctx(),
		`SELECT count(*) FROM send_managed.job_events ev JOIN send_managed.jobs j ON j.id = ev.job_id WHERE j.campaign_id=$1`,
		campaignID).Scan(&n); err != nil {
		t.Fatalf("event count: %v", err)
	}
	return n
}

// TestCancel_StopsQueuedItemsAndReportsThePosition — the main path. A mixed campaign: 5 queued, 1 sent, 1 failed,
// 1 assigned (in flight). The call answers {cancelled:5, in_flight:1, already_terminal:2}; every queued item is
// now `cancelled` with completed_at, terminal_reason and exactly one more job_events row (queued -> cancelled,
// actor system); the settled and in-flight items are NOT touched in state and gain no event; the campaign
// report counts it all and reads progress with cancelled as terminal.
func TestCancel_StopsQueuedItemsAndReportsThePosition(t *testing.T) {
	e := newTestEnv(t, "cancelmain")
	user := e.seedUser(t, "cancelmain@example.com")
	campaign := e.newCampaignID(t)

	var queued []string
	for i := 0; i < 5; i++ {
		jobID, _ := e.seedCampaignJob(t, campaign, user, "queued")
		queued = append(queued, jobID)
	}
	e.campaignJobRow(t, campaign, user, "sent")
	e.campaignJobRow(t, campaign, user, "failed")
	inFlightJob, _ := e.seedCampaignJob(t, campaign, user, "assigned")

	eventsBefore := e.eventCountForCampaign(t, campaign)
	resp := e.do(t, "POST", "/v1/cancel", cancelBody(campaign, user)).requireStatus(t, 200)

	// The exact response contract: these four keys, these values.
	if resp.body["campaign_id"] != campaign {
		t.Fatalf("campaign_id = %v, want %s: %s", resp.body["campaign_id"], campaign, resp.raw)
	}
	want := map[string]float64{"cancelled": 5, "in_flight": 1, "already_terminal": 2}
	for k, w := range want {
		if got, ok := resp.body[k].(float64); !ok || got != w {
			t.Fatalf("%s = %v, want %v: %s", k, resp.body[k], w, resp.raw)
		}
	}
	if len(resp.body) != 4 {
		t.Fatalf("response has %d keys, want exactly campaign_id/cancelled/in_flight/already_terminal: %s", len(resp.body), resp.raw)
	}

	// The ground truth, from the database.
	counts := e.stateCounts(t, campaign)
	if counts["cancelled"] != 5 || counts["queued"] != 0 || counts["assigned"] != 1 || counts["sent"] != 1 || counts["failed"] != 1 {
		t.Fatalf("states after cancel = %v, want cancelled=5 queued=0 assigned=1 sent=1 failed=1", counts)
	}
	for _, id := range queued {
		var state string
		var completed, flagged, lease bool
		var reason *string
		if err := e.admin.QueryRow(bgctx(),
			`SELECT state::text, completed_at IS NOT NULL, cancel_requested_at IS NOT NULL, lease_expires_at IS NULL, terminal_reason
			   FROM send_managed.jobs WHERE id=$1`, id).Scan(&state, &completed, &flagged, &lease, &reason); err != nil {
			t.Fatalf("read cancelled job: %v", err)
		}
		if state != "cancelled" || !completed || !flagged || !lease || reason == nil || *reason != "cancelled" {
			t.Fatalf("queued job %s after cancel: state=%s completed=%v flagged=%v leaseNull=%v reason=%v, want cancelled/true/true/true/cancelled",
				id, state, completed, flagged, lease, reason)
		}
		// One event per transition: genesis + queued->cancelled, by the system actor.
		var evs int
		var lastFrom, lastTo, lastActor string
		if err := e.admin.QueryRow(bgctx(),
			`SELECT count(*), (array_agg(from_state::text ORDER BY seq DESC))[1], (array_agg(to_state::text ORDER BY seq DESC))[1],
			        (array_agg(actor::text ORDER BY seq DESC))[1]
			   FROM send_managed.job_events WHERE job_id=$1`, id).Scan(&evs, &lastFrom, &lastTo, &lastActor); err != nil {
			t.Fatalf("read events: %v", err)
		}
		if evs != 2 || lastFrom != "queued" || lastTo != "cancelled" || lastActor != "system" {
			t.Fatalf("events of cancelled job %s: n=%d last=%s->%s by %s, want 2 events, the last queued->cancelled by system", id, evs, lastFrom, lastTo, lastActor)
		}
	}
	if added := e.eventCountForCampaign(t, campaign) - eventsBefore; added != 5 {
		t.Fatalf("cancel wrote %d job_events rows, want exactly 5 (one per cancelled item, none for the 3 it did not move)", added)
	}
	// The in-flight item: flagged, state untouched.
	var ifState string
	var ifFlag bool
	if err := e.admin.QueryRow(bgctx(), `SELECT state::text, cancel_requested_at IS NOT NULL FROM send_managed.jobs WHERE id=$1`, inFlightJob).Scan(&ifState, &ifFlag); err != nil {
		t.Fatalf("read in-flight job: %v", err)
	}
	if ifState != "assigned" || !ifFlag {
		t.Fatalf("in-flight job after cancel: state=%s flagged=%v, want assigned/true — cancel never changes an in-flight item's state", ifState, ifFlag)
	}

	// The campaign report: cancelled counted, terminal includes it, in flight still reads in flight.
	rep := e.do(t, "GET", "/v1/report?campaign_id="+campaign+"&user_id="+user, nil).requireStatus(t, 200)
	tot := rep.body["totals"].(map[string]any)
	for k, w := range map[string]float64{"total": 8, "cancelled": 5, "sent": 1, "failed": 1, "in_flight": 1, "pending": 0, "waiting_for_capacity": 0, "bounced": 0} {
		if tot[k] != w {
			t.Fatalf("totals.%s = %v, want %v: %s", k, tot[k], w, rep.raw)
		}
	}
	prog := rep.body["progress"].(map[string]any)
	if prog["terminal"] != 7.0 || prog["is_complete"] != false {
		t.Fatalf("progress = %v, want terminal=7 (sent+failed+cancelled) and NOT complete (one item still in flight)", prog)
	}
	for _, raw := range rep.body["items"].(map[string]any)["results"].([]any) {
		it := raw.(map[string]any)
		if it["state"] == "cancelled" {
			if it["reason"] != "cancelled" || it["completed_at"] == nil || it["provider_message_id"] != nil {
				t.Fatalf("cancelled item = %v, want reason=cancelled, completed_at set, no provider_message_id", it)
			}
		}
	}

	// Once the in-flight item finishes the campaign reads COMPLETE: cancelled is terminal.
	e.actorExec(t, "box", `UPDATE send_managed.jobs SET state='sending' WHERE id=$1`, inFlightJob)
	e.actorExec(t, "box", `UPDATE send_managed.jobs SET state='sent', provider_message_id='PMID-CANCEL-API', completed_at=NOW(), lease_expires_at=NULL WHERE id=$1`, inFlightJob)
	rep = e.do(t, "GET", "/v1/report?campaign_id="+campaign+"&user_id="+user, nil).requireStatus(t, 200)
	prog = rep.body["progress"].(map[string]any)
	if prog["terminal"] != 8.0 || prog["is_complete"] != true || prog["fraction_complete"] != 1.0 {
		t.Fatalf("progress after the last in-flight item finished = %v, want terminal=8 is_complete=true fraction=1", prog)
	}
}

// TestCancel_IsIdempotent — a second cancel is a NO-OP that returns the same answer. The first call changed
// something (so a route that never does anything cannot pass), the second writes nothing at all: no state
// change, no new event, and the body is byte-identical.
func TestCancel_IsIdempotent(t *testing.T) {
	e := newTestEnv(t, "cancelidem")
	user := e.seedUser(t, "cancelidem@example.com")
	campaign := e.newCampaignID(t)
	for i := 0; i < 3; i++ {
		e.campaignJobRow(t, campaign, user, "queued")
	}
	e.campaignJobRow(t, campaign, user, "sent")
	e.seedCampaignJob(t, campaign, user, "sending")

	eventsBefore := e.eventCountForCampaign(t, campaign)
	first := e.do(t, "POST", "/v1/cancel", cancelBody(campaign, user)).requireStatus(t, 200)
	if first.body["cancelled"] != 3.0 {
		t.Fatalf("first cancel = %s, want cancelled=3 (the control: the first call DID something)", first.raw)
	}
	eventsAfterFirst := e.eventCountForCampaign(t, campaign)
	if eventsAfterFirst-eventsBefore != 3 {
		t.Fatalf("first cancel wrote %d events, want 3", eventsAfterFirst-eventsBefore)
	}
	statesAfterFirst := e.stateCounts(t, campaign)

	second := e.do(t, "POST", "/v1/cancel", cancelBody(campaign, user)).requireStatus(t, 200)
	third := e.do(t, "POST", "/v1/cancel", cancelBody(campaign, user)).requireStatus(t, 200)
	if second.raw != first.raw || third.raw != first.raw {
		t.Fatalf("repeat cancels differ from the first:\n 1st: %s\n 2nd: %s\n 3rd: %s", first.raw, second.raw, third.raw)
	}
	if after := e.eventCountForCampaign(t, campaign); after != eventsAfterFirst {
		t.Fatalf("repeat cancels wrote %d more job_events rows, want 0 (a no-op writes nothing)", after-eventsAfterFirst)
	}
	statesNow := e.stateCounts(t, campaign)
	for k, v := range statesAfterFirst {
		if statesNow[k] != v {
			t.Fatalf("states moved on a repeat cancel: %v -> %v", statesAfterFirst, statesNow)
		}
	}
}

// TestCancel_IsTenantScoped — tenant B cancelling tenant A's campaign id (with B's own, valid user_id) touches
// NOTHING of A's and gets the same answer as for a campaign that does not exist at all; tenant A, in the same
// database, can cancel it. A cancel with the right campaign but the WRONG user_id is the cross-tenant case.
func TestCancel_IsTenantScoped(t *testing.T) {
	e := newTestEnv(t, "canceltenant")
	userA := e.seedUser(t, "cancelA@example.com")
	userB := e.seedUser(t, "cancelB@example.com")
	campA := e.newCampaignID(t)
	campB := e.newCampaignID(t)
	for i := 0; i < 3; i++ {
		e.campaignJobRow(t, campA, userA, "queued")
	}
	e.campaignJobRow(t, campB, userB, "queued")
	eventsBefore := e.eventCountForCampaign(t, campA)

	cross := e.do(t, "POST", "/v1/cancel", cancelBody(campA, userB)).requireStatus(t, 200)
	ghost := e.do(t, "POST", "/v1/cancel", cancelBody(e.newCampaignID(t), userB)).requireStatus(t, 200)
	for _, r := range []resp{cross, ghost} {
		for _, k := range []string{"cancelled", "in_flight", "already_terminal"} {
			if r.body[k] != 0.0 {
				t.Fatalf("a foreign / unknown campaign answered %s, want all-zero counts: %s", k, r.raw)
			}
		}
	}
	// "The same shape": identical apart from the campaign id the caller itself sent.
	if strings.ReplaceAll(cross.raw, campA, "X") != strings.ReplaceAll(ghost.raw, ghost.str("campaign_id"), "X") {
		t.Fatalf("a foreign campaign and an unknown one are distinguishable:\n foreign: %s\n unknown: %s", cross.raw, ghost.raw)
	}
	if got := e.stateCounts(t, campA); got["queued"] != 3 || got["cancelled"] != 0 {
		t.Fatalf("tenant B's cancel touched tenant A's campaign: states=%v, want queued=3", got)
	}
	if after := e.eventCountForCampaign(t, campA); after != eventsBefore {
		t.Fatalf("tenant B's cancel wrote %d events on tenant A's campaign", after-eventsBefore)
	}
	// Tenant B's OWN campaign is untouched by any of the above.
	if got := e.stateCounts(t, campB); got["queued"] != 1 {
		t.Fatalf("tenant B's own campaign = %v, want queued=1", got)
	}

	// CONTROL: the owner CAN cancel it, in the same database, so the zeros above were scoping, not a dead route.
	own := e.do(t, "POST", "/v1/cancel", cancelBody(campA, userA)).requireStatus(t, 200)
	if own.body["cancelled"] != 3.0 {
		t.Fatalf("the owning tenant's cancel = %s, want cancelled=3", own.raw)
	}
	if got := e.stateCounts(t, campB); got["queued"] != 1 || got["cancelled"] != 0 {
		t.Fatalf("tenant A's cancel touched tenant B's campaign: %v", got)
	}
}

// TestCancel_ValidationAndErrorEnvelope — same envelope and codes as the sibling routes: a missing or
// malformed campaign_id / user_id is a 400 validation_failed whose details name the field and say required vs
// malformed (the vocabulary run and report use), an unknown field is rejected, no token is a 401, and a wrong
// verb is a coded 405 that advertises POST.
func TestCancel_ValidationAndErrorEnvelope(t *testing.T) {
	e := newTestEnv(t, "cancelvalid")
	user := e.seedUser(t, "cancelvalid@example.com")
	campaign := e.newCampaignID(t)

	for _, tc := range []struct {
		name string
		body any
		want string // "field:issue"
	}{
		{"campaign_id missing", map[string]any{"user_id": user}, "campaign_id:required"},
		{"campaign_id malformed", map[string]any{"campaign_id": "not-a-uuid", "user_id": user}, "campaign_id:malformed"},
		{"user_id missing", map[string]any{"campaign_id": campaign}, "user_id:required"},
		{"user_id malformed", map[string]any{"campaign_id": campaign, "user_id": "nope"}, "user_id:malformed"},
	} {
		r := e.do(t, "POST", "/v1/cancel", tc.body).requireStatus(t, 400)
		if r.errorCode() != codeValidationFailed {
			t.Fatalf("%s: code = %q, want validation_failed: %s", tc.name, r.errorCode(), r.raw)
		}
		if got := r.errorDetailFields(); len(got) != 1 || got[0] != tc.want {
			t.Fatalf("%s: details = %v, want [%s]: %s", tc.name, got, tc.want, r.raw)
		}
	}
	// An empty body names the first missing field; an unknown field is rejected outright (DisallowUnknownFields).
	if r := e.do(t, "POST", "/v1/cancel", nil).requireStatus(t, 400); r.errorCode() != codeValidationFailed {
		t.Fatalf("empty body: %s", r.raw)
	}
	r := e.do(t, "POST", "/v1/cancel", map[string]any{"campaign_id": campaign, "user_id": user, "force": true}).requireStatus(t, 400)
	if r.errorCode() != codeValidationFailed {
		t.Fatalf("unknown field: code = %q: %s", r.errorCode(), r.raw)
	}

	// Auth: the same service token as the sibling routes, and nothing else.
	if r := e.doUnauthenticated(t, "POST", "/v1/cancel"); r.status != 401 || r.errorCode() != codeUnauthenticated {
		t.Fatalf("no token: status=%d body=%s, want 401 unauthenticated", r.status, r.raw)
	}

	// Wrong verb: a coded 405 advertising POST.
	g := e.doRaw(t, "GET", "/v1/cancel")
	if g.status != 405 {
		t.Fatalf("GET /v1/cancel: status=%d, want 405: %s", g.status, g.raw)
	}
	assertErrorEnvelope(t, g, codeMethodNotAllowed)
	if allow := g.header.Get("Allow"); allow != "POST" {
		t.Fatalf("GET /v1/cancel: Allow=%q, want POST", allow)
	}
}

// TestCancel_ACancelledItemCanNeverBeRerunAndFreesTheCampaignSlot — a cancelled item is terminal: a repeat
// /v1/run for the SAME job_item_id answers the existing job (now `cancelled`) and creates nothing; and because
// cancelled is not an "active" state, the user's one-campaign-at-a-time slot is free again — a DIFFERENT
// campaign is accepted. CONTROL: before the cancel, that same different-campaign run is refused 409.
func TestCancel_ACancelledItemCanNeverBeRerunAndFreesTheCampaignSlot(t *testing.T) {
	e := newTestEnv(t, "cancelrerun")
	user := e.seedUser(t, "cancelrerun@example.com")
	c1 := e.newCampaignID(t)
	c2 := e.newCampaignID(t)

	item1 := e.seedJobItemCampaign(t, c1, user, "rerun1@example.com")
	run := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item1, "user_id": user}).requireStatus(t, 202)
	jobID := run.str("job_id")

	// CONTROL: the slot is held while C1 is active.
	item2 := e.seedJobItemCampaign(t, c2, user, "rerun2@example.com")
	if r := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item2, "user_id": user}); r.status != 409 {
		t.Fatalf("a second campaign while C1 is active: status=%d, want 409 (the control for the slot assertion below): %s", r.status, r.raw)
	}

	e.do(t, "POST", "/v1/cancel", cancelBody(c1, user)).requireStatus(t, 200)
	jobsBefore := e.jobRowCount(t)

	// Re-running the cancelled item answers the existing, cancelled job — it is never re-queued.
	again := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item1, "user_id": user}).requireStatus(t, 200)
	if again.str("job_id") != jobID || again.str("state") != "cancelled" {
		t.Fatalf("re-run of a cancelled item = %s, want 200 with the SAME job_id %s in state cancelled", again.raw, jobID)
	}
	if n := e.jobRowCount(t); n != jobsBefore {
		t.Fatalf("job rows %d -> %d: re-running a cancelled item created a row", jobsBefore, n)
	}
	if got := e.stateCounts(t, c1); got["cancelled"] != 1 || got["queued"] != 0 {
		t.Fatalf("C1 after the re-run = %v, want cancelled=1 queued=0", got)
	}

	// The slot is free: a DIFFERENT campaign is now accepted.
	if r := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item2, "user_id": user}); r.status != 202 {
		t.Fatalf("a second campaign AFTER cancelling C1: status=%d, want 202 (cancelled is terminal, not active): %s", r.status, r.raw)
	}
}

// TestCancel_WorksAtEveryDispatchRung — stopping work must work when everything else is stopped. At the
// deepest rung (`stopped`), where /v1/run is refused 503, cancel still answers 200 and still cancels.
// CONTROL: /v1/run IS refused at that rung, so the rung really is in force.
func TestCancel_WorksAtEveryDispatchRung(t *testing.T) {
	e := newTestEnv(t, "cancelrung")
	user := e.seedUser(t, "cancelrung@example.com")

	// Intake is refused at the two deepest rungs: the control that the rung is genuinely in force.
	refusesIntake := map[string]bool{"serving": false, "accept_and_defer": false, "refuse_new": true, "stopped": true}
	for _, rung := range []string{"serving", "accept_and_defer", "refuse_new", "stopped"} {
		e.setRungConfigStr(t, "dispatch_mode", rung)
		campaign := e.newCampaignID(t)
		e.campaignJobRow(t, campaign, user, "queued")
		e.campaignJobRow(t, campaign, user, "queued")

		if refusesIntake[rung] {
			item := e.seedJobItemCampaign(t, e.newCampaignID(t), user, "rung-"+rung+"@example.com")
			if r := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item, "user_id": user}); r.status != 503 {
				t.Fatalf("control: /v1/run at the %s rung = %d, want 503 — the rung is not in force: %s", rung, r.status, r.raw)
			}
		}
		r := e.do(t, "POST", "/v1/cancel", cancelBody(campaign, user)).requireStatus(t, 200)
		if r.body["cancelled"] != 2.0 {
			t.Fatalf("cancel at the %s rung = %s, want cancelled=2", rung, r.raw)
		}
		if got := e.stateCounts(t, campaign); got["cancelled"] != 2 || got["queued"] != 0 {
			t.Fatalf("states after a cancel at the %s rung = %v, want cancelled=2 queued=0", rung, got)
		}
	}
}

// TestCancel_JobStateVocabularyPublishesCancelled — describe reads job_state LIVE from pg_enum, so the new value
// follows with no code edit: it is published, last (additive: the six existing values keep their places).
func TestCancel_JobStateVocabularyPublishesCancelled(t *testing.T) {
	e := newTestEnv(t, "cancelvocab")
	vals := e.describeVocabulary(t, "job_state")
	want := []string{"queued", "assigned", "sending", "sent", "failed", "bounced", "cancelled"}
	if strings.Join(vals, ",") != strings.Join(want, ",") {
		t.Fatalf("describe job_state = %v, want %v (the six original values in place, cancelled appended)", vals, want)
	}
}
