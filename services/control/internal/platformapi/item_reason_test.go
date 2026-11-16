package platformapi

// item_reason_test.go — M2-G1: `reason` and `error_class` on every report item.
//
// Two layers, deliberately:
//
//   1. THE MAPPING (itemOutcome), as a pure function over a table — every state, every queued branch, the
//      precedence between the live conditions and the item's history, and the vocabulary closure. No
//      database: the table is the specification, and it is cheap enough to be exhaustive.
//   2. THE WIRING, over real HTTP against a real clone: the stored facts are READ (not re-derived), both
//      report shapes carry both keys on EVERY item (null when empty), the live conditions are computed from
//      the live database, and nothing but a vocabulary name ever crosses the wire.
//
// The third layer — that the facts the REAL dispatcher / outcome handler / reclaim / exhaustion / cancel
// paths write are the facts this mapping reads — lives in internal/dispatch/report_reason_test.go, because
// only that package can drive those paths.

import (
	"sort"
	"strings"
	"testing"
)

func sp(s string) *string { return &s }

func deref(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

// TestItemOutcome_Table — the whole mapping.
func TestItemOutcome_Table(t *testing.T) {
	cases := []struct {
		name       string
		f          itemFacts
		wantReason string // "<null>" for JSON null
		wantClass  string
	}{
		// sent: nothing to say, even though the row still carries an earlier failure's class.
		{"sent", itemFacts{State: "sent"}, "<null>", "<null>"},
		{"sent after a failed attempt keeps neither", itemFacts{State: "sent", LastErrorClass: sp("transport_4xx")}, "<null>", "<null>"},

		// bounced
		{"bounced", itemFacts{State: "bounced", LastErrorClass: sp("bounce_permanent")}, "bounce_permanent", "bounce_permanent"},
		{"bounced row with no class recorded still says bounce_permanent", itemFacts{State: "bounced"}, "bounce_permanent", "bounce_permanent"},

		// failed: the two writers, and the legacy row
		{"failed by pool exhaustion", itemFacts{State: "failed", TerminalReason: sp("pool_exhausted")}, "pool_exhausted", "<null>"},
		{"failed by the attempt cap", itemFacts{State: "failed", TerminalReason: sp("attempts_exhausted"), LastErrorClass: sp("unknown")}, "attempts_exhausted", "unknown"},
		{"failed row that pre-dates terminal_reason is read as pool_exhausted", itemFacts{State: "failed"}, "pool_exhausted", "<null>"},
		{"failed row with an EMPTY terminal_reason likewise", itemFacts{State: "failed", TerminalReason: sp("")}, "pool_exhausted", "<null>"},
		{"a reason this version does not publish is NOT echoed", itemFacts{State: "failed", TerminalReason: sp("some_future_reason")}, "<null>", "<null>"},

		// cancelled
		{"cancelled", itemFacts{State: "cancelled", TerminalReason: sp("cancelled")}, "cancelled", "<null>"},
		{"cancelled after a failed attempt shows the class", itemFacts{State: "cancelled", LastErrorClass: sp("transport_4xx")}, "cancelled", "transport_4xx"},

		// in flight: not blocked, so no reason; the class of the previous attempt still shows
		{"assigned", itemFacts{State: "assigned"}, "<null>", "<null>"},
		{"sending after a failed attempt", itemFacts{State: "sending", LastErrorClass: sp("timeout")}, "<null>", "timeout"},

		// queued: never tried
		{"queued, never tried", itemFacts{State: "queued"}, "<null>", "<null>"},

		// queued: the last failed attempt's class IS the reason
		{"queued after transport_4xx", itemFacts{State: "queued", LastErrorClass: sp("transport_4xx"), LastErrorMessage: sp(": 451 4.3.0 temporary")}, "transport_4xx", "transport_4xx"},
		{"queued after auth_failure", itemFacts{State: "queued", LastErrorClass: sp("auth_failure")}, "auth_failure", "auth_failure"},
		{"queued after throttle", itemFacts{State: "queued", LastErrorClass: sp("throttle")}, "throttle", "throttle"},
		{"queued after timeout", itemFacts{State: "queued", LastErrorClass: sp("timeout"), LastErrorMessage: sp("lease expired")}, "timeout", "timeout"},
		{"queued after unknown (a real SMTP failure)", itemFacts{State: "queued", LastErrorClass: sp("unknown"), LastErrorMessage: sp(": 554 5.7.1 policy")}, "unknown", "unknown"},

		// queued: dispatcher-side requeues never reached SMTP
		{"queued after no_eligible_account", itemFacts{State: "queued", LastErrorClass: sp("unknown"), LastErrorMessage: sp("no_eligible_account")}, "no_eligible_account", "unknown"},
		{"queued after no_worker_available", itemFacts{State: "queued", LastErrorClass: sp("unknown"), LastErrorMessage: sp("no_worker_available")}, "waiting_for_capacity", "unknown"},
		{"queued after push_failed", itemFacts{State: "queued", LastErrorClass: sp("unknown"), LastErrorMessage: sp("push_failed")}, "waiting_for_capacity", "unknown"},
		{"queued after the header-injection guard", itemFacts{State: "queued", LastErrorClass: sp("unknown"), LastErrorMessage: sp("header_injection:subject")}, "unknown", "unknown"},

		// queued: live conditions outrank the item's own history, and pause outranks capacity
		{"queued, no eligible account right now", itemFacts{State: "queued", WaitingForCapacity: true}, "waiting_for_capacity", "<null>"},
		{"waiting outranks the last class", itemFacts{State: "queued", LastErrorClass: sp("transport_4xx"), WaitingForCapacity: true}, "waiting_for_capacity", "transport_4xx"},
		{"queued while the module is paused", itemFacts{State: "queued", DispatchPaused: true}, "dispatch_paused", "<null>"},
		{"pause outranks waiting and history", itemFacts{State: "queued", LastErrorClass: sp("throttle"), DispatchPaused: true, WaitingForCapacity: true}, "dispatch_paused", "throttle"},

		// the live flags mean nothing for a settled item
		{"a paused module does not rewrite a sent item", itemFacts{State: "sent", DispatchPaused: true, WaitingForCapacity: true}, "<null>", "<null>"},
		{"a paused module does not rewrite a failed item", itemFacts{State: "failed", TerminalReason: sp("attempts_exhausted"), DispatchPaused: true}, "attempts_exhausted", "<null>"},
	}
	for _, tc := range cases {
		reason, class := itemOutcome(tc.f)
		if got := deref(reason); got != tc.wantReason {
			t.Errorf("%s: reason = %s, want %s", tc.name, got, tc.wantReason)
		}
		if got := deref(class); got != tc.wantClass {
			t.Errorf("%s: error_class = %s, want %s", tc.name, got, tc.wantClass)
		}
	}
}

// TestItemReasonVocabulary_IsExactlyWhatItemOutcomeCanProduce — the closure, both directions, over a grid of
// every input combination. A reason emitted but not declared would reach a client undeclared; a reason
// declared but unreachable is dead handling a client writes and never exercises.
func TestItemReasonVocabulary_IsExactlyWhatItemOutcomeCanProduce(t *testing.T) {
	states := []string{"queued", "assigned", "sending", "sent", "failed", "bounced", "cancelled"}
	terminals := []*string{nil, sp(""), sp("pool_exhausted"), sp("attempts_exhausted"), sp("cancelled"), sp("some_future_reason")}
	classes := []*string{nil, sp("bounce_permanent"), sp("transport_4xx"), sp("auth_failure"), sp("throttle"), sp("timeout"), sp("unknown")}
	msgs := []*string{nil, sp("no_eligible_account"), sp("no_worker_available"), sp("push_failed"),
		sp("header_injection:subject"), sp("header_injection:reply_to"), sp(": 554 5.7.1 policy"), sp("lease expired")}

	produced := map[string]bool{}
	combos := 0
	for _, st := range states {
		for _, tr := range terminals {
			for _, cl := range classes {
				for _, mg := range msgs {
					for _, paused := range []bool{false, true} {
						for _, waiting := range []bool{false, true} {
							combos++
							reason, _ := itemOutcome(itemFacts{State: st, TerminalReason: tr, LastErrorClass: cl,
								LastErrorMessage: mg, DispatchPaused: paused, WaitingForCapacity: waiting})
							if reason != nil {
								produced[*reason] = true
							}
						}
					}
				}
			}
		}
	}
	if combos < 1000 {
		t.Fatalf("only %d input combinations enumerated — the grid is broken and this closure check is vacuous", combos)
	}

	declared := map[string]bool{}
	for _, r := range itemReasons {
		if declared[r] {
			t.Fatalf("itemReasons lists %q twice", r)
		}
		declared[r] = true
	}
	var undeclared, unreachable []string
	for r := range produced {
		if !declared[r] {
			undeclared = append(undeclared, r)
		}
	}
	for r := range declared {
		if !produced[r] {
			unreachable = append(unreachable, r)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(unreachable)
	if len(undeclared) > 0 {
		t.Fatalf("itemOutcome can emit reasons that itemReasons (and so describe) does not declare: %v", undeclared)
	}
	if len(unreachable) > 0 {
		t.Fatalf("itemReasons declares reasons that itemOutcome can never emit: %v — describe would advertise a value no response carries", unreachable)
	}
}

// TestItemReason_ErrorClassIsOnlyEverTheStoredClassOrBounce — error_class is passed through from the stored
// column (the six values of send_managed.error_class, which describe publishes live), with exactly two
// rules of its own: a SENT item has none, and a BOUNCED item with no class recorded says bounce_permanent.
// The function invents no class and drops none.
func TestItemReason_ErrorClassIsOnlyEverTheStoredClassOrBounce(t *testing.T) {
	for _, st := range []string{"queued", "assigned", "sending", "sent", "failed", "bounced", "cancelled"} {
		for _, cl := range []*string{nil, sp("transport_4xx"), sp("auth_failure"), sp("throttle"), sp("timeout"), sp("unknown"), sp("bounce_permanent")} {
			_, got := itemOutcome(itemFacts{State: st, LastErrorClass: cl})
			switch {
			case st == "sent":
				if got != nil {
					t.Errorf("sent: error_class = %s, want null", deref(got))
				}
			case st == "bounced":
				want := "bounce_permanent"
				if cl != nil {
					want = *cl
				}
				if deref(got) != want {
					t.Errorf("bounced (stored class %s): error_class = %s, want %s", deref(cl), deref(got), want)
				}
			default:
				if deref(got) != deref(cl) {
					t.Errorf("%s: error_class = %s, want the stored %s (nothing invented, nothing dropped)", st, deref(got), deref(cl))
				}
			}
		}
	}
}

// setJobFields applies a fixture UPDATE to one job by id (no state change, so no actor GUC is needed).
func (e *testEnv) setJobFields(t *testing.T, jobID, setClause string, args ...any) {
	t.Helper()
	all := append([]any{jobID}, args...)
	if _, err := e.admin.Exec(bgctx(), `UPDATE send_managed.jobs SET `+setClause+` WHERE id=$1`, all...); err != nil {
		t.Fatalf("setJobFields(%s): %v", setClause, err)
	}
}

// TestReport_ReasonAndErrorClassOnEveryItem_BothShapes — the wiring over real HTTP. Rows are written the way
// the production paths leave them, and BOTH report shapes must carry both keys on every item — present and
// null when empty, never omitted — with the same values.
func TestReport_ReasonAndErrorClassOnEveryItem_BothShapes(t *testing.T) {
	e := newTestEnv(t, "reasonwire")
	user := e.seedUser(t, "reasonwire@example.com")
	campaign := e.newCampaignID(t)

	// A pool with an eligible account, so "queued" items are NOT waiting for capacity.
	pool := e.seedPool(t)
	e.seedEligibleAccount(t, pool)
	if _, err := e.admin.Exec(bgctx(),
		`UPDATE send_managed.pools SET owner_user_id=$1, kind='dedicated' WHERE id=$2`, user, pool); err != nil {
		t.Fatalf("make the pool this tenant's: %v", err)
	}

	type want struct{ state, reason, class string } // "" = JSON null
	byItem := map[string]want{}
	add := func(state, set string, w want, args ...any) {
		jobID, itemID := e.seedCampaignJob(t, campaign, user, state)
		if set != "" {
			e.setJobFields(t, jobID, set, args...)
		}
		byItem[itemID] = w
	}

	add("sent", "", want{"sent", "", ""})
	add("sent", `last_error_class='transport_4xx'`, want{"sent", "", ""}) // delivered after a failed attempt
	add("bounced", `last_error_class='bounce_permanent'`, want{"bounced", "bounce_permanent", "bounce_permanent"})
	add("failed", `terminal_reason='pool_exhausted'`, want{"failed", "pool_exhausted", ""})
	add("failed", `terminal_reason='attempts_exhausted', last_error_class='unknown', failed_attempt_count=6, first_failed_at=NOW()`, want{"failed", "attempts_exhausted", "unknown"})
	add("failed", "", want{"failed", "pool_exhausted", ""}) // a row that pre-dates terminal_reason
	add("cancelled", `terminal_reason='cancelled'`, want{"cancelled", "cancelled", ""})
	add("assigned", "", want{"assigned", "", ""})
	add("queued", "", want{"queued", "", ""}) // never tried
	add("queued", `last_error_class='throttle'`, want{"queued", "throttle", "throttle"})
	add("queued", `last_error_class='unknown', last_error_message='no_eligible_account'`, want{"queued", "no_eligible_account", "unknown"})
	add("queued", `last_error_class='unknown', last_error_message='push_failed'`, want{"queued", "waiting_for_capacity", "unknown"})

	// The campaign shape.
	rep := e.do(t, "GET", "/v1/report?campaign_id="+campaign+"&user_id="+user+"&page_size=200", nil).requireStatus(t, 200)
	results := rep.body["items"].(map[string]any)["results"].([]any)
	if len(results) != len(byItem) {
		t.Fatalf("campaign report has %d items, seeded %d: %s", len(results), len(byItem), rep.raw)
	}
	for _, raw := range results {
		it := raw.(map[string]any)
		w, ok := byItem[it["job_item_id"].(string)]
		if !ok {
			t.Fatalf("unexpected item %v", it)
		}
		if _, ok := it["reason"]; !ok {
			t.Fatalf("item has no `reason` KEY (it must be present, null when empty): %v", it)
		}
		if _, ok := it["error_class"]; !ok {
			t.Fatalf("item has no `error_class` KEY (it must be present, null when empty): %v", it)
		}
		gotReason, _ := it["reason"].(string)
		gotClass, _ := it["error_class"].(string)
		if it["state"] != w.state || gotReason != w.reason || gotClass != w.class {
			t.Fatalf("campaign item = %v, want state=%q reason=%q error_class=%q", it, w.state, w.reason, w.class)
		}
	}

	// The single-job shape, for every job of the campaign, by job_item_id.
	for itemID, w := range byItem {
		r := e.do(t, "GET", "/v1/report?job_item_id="+itemID+"&user_id="+user, nil).requireStatus(t, 200)
		if _, ok := r.body["reason"]; !ok {
			t.Fatalf("single report has no `reason` KEY: %s", r.raw)
		}
		if _, ok := r.body["error_class"]; !ok {
			t.Fatalf("single report has no `error_class` KEY: %s", r.raw)
		}
		gotReason, _ := r.body["reason"].(string)
		gotClass, _ := r.body["error_class"].(string)
		if r.body["state"] != w.state || gotReason != w.reason || gotClass != w.class {
			t.Fatalf("single report = %s, want state=%q reason=%q error_class=%q", r.raw, w.state, w.reason, w.class)
		}
	}
}

// TestReport_NothingButVocabularyNamesCrossTheWire — the two-vocabulary rule for the new keys: a job row
// stuffed with SMTP text, a hostname and an address in last_error_message must not appear anywhere in either
// report. (The mapping only READS that column, to recognise a dispatcher-side requeue; it must never echo
// it.) Positive control: the keys it DOES publish are present, so the response is not simply empty.
func TestReport_NothingButVocabularyNamesCrossTheWire(t *testing.T) {
	e := newTestEnv(t, "reasonleak")
	user := e.seedUser(t, "reasonleak@example.com")
	campaign := e.newCampaignID(t)
	jobID, itemID := e.seedCampaignJob(t, campaign, user, "queued")
	const secret = "554 5.7.1 relay denied by mx1.internal.example.net for ops@secret-host.example"
	e.setJobFields(t, jobID, `last_error_class='unknown', last_error_message=$2`, secret)

	single := e.do(t, "GET", "/v1/report?job_id="+jobID+"&user_id="+user, nil).requireStatus(t, 200)
	camp := e.do(t, "GET", "/v1/report?campaign_id="+campaign+"&user_id="+user, nil).requireStatus(t, 200)
	for name, raw := range map[string]string{"single report": single.raw, "campaign report": camp.raw} {
		for _, leak := range []string{"mx1.internal", "secret-host", "relay denied", "554 5.7.1", "ops@"} {
			if strings.Contains(raw, leak) {
				t.Fatalf("the %s leaked %q from last_error_message: %s", name, leak, raw)
			}
		}
		if !strings.Contains(raw, `"error_class":"unknown"`) {
			t.Fatalf("the %s does not carry error_class=unknown, so the leak assertions above ran over a response that was missing the fields under test: %s", name, raw)
		}
	}
	_ = itemID
}

// TestReport_QueuedReasonFollowsTheLiveDatabase — a queued item's reason is computed from the CURRENT
// conditions, in both shapes: with no eligible account it is waiting_for_capacity, with one it is the item's
// own history, and while the module is paused it is dispatch_paused. Each flip is a real write to the real
// tables the production code reads, and the campaign aggregate agrees with the item-level reason.
func TestReport_QueuedReasonFollowsTheLiveDatabase(t *testing.T) {
	e := newTestEnv(t, "reasonlive")
	user := e.seedUser(t, "reasonlive@example.com")
	campaign := e.newCampaignID(t)
	jobID, itemID := e.seedCampaignJob(t, campaign, user, "queued")
	e.setJobFields(t, jobID, `last_error_class='timeout', last_error_message='lease expired'`)

	reasonOf := func(label string) string {
		t.Helper()
		single := e.do(t, "GET", "/v1/report?job_id="+jobID+"&user_id="+user, nil).requireStatus(t, 200)
		camp := e.do(t, "GET", "/v1/report?campaign_id="+campaign+"&user_id="+user, nil).requireStatus(t, 200)
		var citem map[string]any
		for _, raw := range camp.body["items"].(map[string]any)["results"].([]any) {
			if m := raw.(map[string]any); m["job_item_id"] == itemID {
				citem = m
			}
		}
		if citem == nil {
			t.Fatalf("%s: item missing from the campaign report: %s", label, camp.raw)
		}
		a, _ := single.body["reason"].(string)
		b, _ := citem["reason"].(string)
		if a != b {
			t.Fatalf("%s: the two report shapes disagree on the reason: single=%q campaign=%q", label, a, b)
		}
		return a
	}

	// No pool at all for this tenant: nothing can send.
	if got := reasonOf("no capacity"); got != "waiting_for_capacity" {
		t.Fatalf("with NO eligible account the reason = %q, want waiting_for_capacity", got)
	}
	totals := e.do(t, "GET", "/v1/report?campaign_id="+campaign+"&user_id="+user, nil).body["totals"].(map[string]any)
	if totals["waiting_for_capacity"] != 1.0 {
		t.Fatalf("totals.waiting_for_capacity = %v, want 1 — the item-level reason and the aggregate must say the same thing", totals["waiting_for_capacity"])
	}

	// An eligible account appears: the reason falls back to the item's own history.
	pool := e.seedPool(t)
	e.seedEligibleAccount(t, pool)
	if got := reasonOf("capacity"); got != "timeout" {
		t.Fatalf("with an eligible account the reason = %q, want the item's own last class, timeout", got)
	}
	totals = e.do(t, "GET", "/v1/report?campaign_id="+campaign+"&user_id="+user, nil).body["totals"].(map[string]any)
	if totals["waiting_for_capacity"] != 0.0 || totals["pending"] != 1.0 {
		t.Fatalf("totals = %v, want waiting_for_capacity=0 pending=1 once capacity exists", totals)
	}

	// The module is paused: that outranks everything.
	e.setRungConfigStr(t, "dispatch_mode", "accept_and_defer")
	if got := reasonOf("paused"); got != "dispatch_paused" {
		t.Fatalf("while paused the reason = %q, want dispatch_paused", got)
	}
	e.setRungConfigStr(t, "dispatch_mode", "serving")
	if got := reasonOf("unpaused"); got != "timeout" {
		t.Fatalf("after unpausing the reason = %q, want timeout again", got)
	}
}
