package platformapi

// dispatch_rung_test.go — fix-plan item 12's INTAKE half, proven against the real Server over real
// HTTP against a real Postgres clone (CLAUDE.md Rule 3).
//
// WHAT THIS FILE IS FOR, AND WHY IT IS NOT REDUNDANT WITH internal/dispatch's rung suite. That suite
// proves the ladder's CLAIM and RECLAIM behaviour through the dispatch loop. It cannot see this
// package: platformapi does not import internal/dispatch, so the intake predicate here is a second
// implementation of the same ordering, reached over HTTP, and a second implementation is exactly what
// needs its own behavioural proof. "The dispatch tests pass" says nothing about whether POST /v1/run
// actually refuses.

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// setRungConfigStr overwrites a seeded string send_managed.config value (default_value untouched,
// Rule 1). This package's tests had no generic config setter — only setUnitPriceCredits, hardcoded to
// one int key — so the ladder's suite brings its own.
func (e *testEnv) setRungConfigStr(t *testing.T, key, v string) {
	t.Helper()
	if _, err := e.admin.Exec(bgctx(),
		`UPDATE send_managed.config SET value = jsonb_build_object('v', $2::text) WHERE key=$1`, key, v); err != nil {
		t.Fatalf("set config %s: %v", key, err)
	}
}

// setRungConfigBool overwrites a seeded boolean value — dispatch_enabled is the first boolean any test
// in this package needs to move.
func (e *testEnv) setRungConfigBool(t *testing.T, key string, v bool) {
	t.Helper()
	if _, err := e.admin.Exec(bgctx(),
		`UPDATE send_managed.config SET value = jsonb_build_object('v', $2::boolean) WHERE key=$1`, key, v); err != nil {
		t.Fatalf("set config %s: %v", key, err)
	}
}

// setRungConfigRaw writes a value the validator would never accept — used to plant a garbage
// dispatch_mode. The admin API's write path rejects a bad enum (config_validate.go), so the only way
// to test what the READ path does with one is to bypass the write path, which is precisely the case
// that matters: the row could have been set by an older migration, a manual psql session, or a
// restore.
func (e *testEnv) setRungConfigRaw(t *testing.T, key, rawJSON string) {
	t.Helper()
	if _, err := e.admin.Exec(bgctx(),
		`UPDATE send_managed.config SET value = $2::jsonb WHERE key=$1`, key, rawJSON); err != nil {
		t.Fatalf("set raw config %s: %v", key, err)
	}
}

// deleteConfigRow removes a config row outright, to exercise the absent-row branch of the intake
// gate's fail-open path.
func (e *testEnv) deleteConfigRow(t *testing.T, key string) {
	t.Helper()
	if _, err := e.admin.Exec(bgctx(),
		`DELETE FROM send_managed.config WHERE key=$1`, key); err != nil {
		t.Fatalf("delete config %s: %v", key, err)
	}
}

// jobRowCount counts every send_managed.jobs row in the clone — the assertion that a REFUSED run
// created nothing. A 503 that still inserts a job row is worse than no gate at all: the caller is told
// its work was declined while the work sits queued, waiting to be sent the moment the rung is
// released.
func (e *testEnv) jobRowCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(bgctx(), `SELECT count(*) FROM send_managed.jobs`).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

// TestDispatchRung_IntakeIsRefusedAtRefuseNewAndBelowAndNowhereElse walks the whole ladder against
// the real POST /v1/run and pins BOTH directions: the two shallow rungs accept, the two deep rungs
// refuse with 503 `paused`, and nothing in between is ambiguous.
//
// The two-sided shape is the point. A test that only asserted "stopped refuses" would pass just as
// happily if the gate refused at EVERY rung — including `accept_and_defer`, whose entire reason for
// existing is that callers keep submitting while dispatch is halted. M1's inert accept_and_defer is
// the defect this item exists to remove; a one-sided test here would let this module reintroduce it
// from the opposite direction.
func TestDispatchRung_IntakeIsRefusedAtRefuseNewAndBelowAndNowhereElse(t *testing.T) {
	e := newTestEnv(t, "rungintake")
	userID := e.seedUser(t, "rungintake@example.com")

	cases := []struct {
		rung       string
		wantStatus int
	}{
		{rungServing, 202},
		{rungAcceptAndDefer, 202},
		{rungRefuseNew, 503},
		{rungStopped, 503},
	}

	accepted, refused := 0, 0
	for _, tc := range cases {
		e.setRungConfigStr(t, "dispatch_mode", tc.rung)
		before := e.jobRowCount(t)

		item := e.seedJobItem(t, userID, "intake-"+tc.rung+"@example.com")
		got := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item, "user_id": userID})
		if got.status != tc.wantStatus {
			t.Errorf("rung=%s: POST /v1/run returned %d, want %d: %s", tc.rung, got.status, tc.wantStatus, got.raw)
			continue
		}

		after := e.jobRowCount(t)
		switch tc.wantStatus {
		case 202:
			accepted++
			if after != before+1 {
				t.Errorf("rung=%s: accepted the run (202) but job rows went %d->%d — a 202 that creates no job is a lie in the other direction", tc.rung, before, after)
			}
			if got.str("job_id") == "" {
				t.Errorf("rung=%s: 202 with no job_id: %s", tc.rung, got.raw)
			}
		case 503:
			refused++
			if code := got.errorCode(); code != "paused" {
				t.Errorf("rung=%s: 503 carried code %q, want \"paused\" — the status alone does not tell a caller whether to retry; a generic 503 reads as an outage: %s", tc.rung, code, got.raw)
			}
			if after != before {
				t.Errorf("rung=%s: REFUSED the run (503) but job rows went %d->%d — the caller was told its work was declined while the work was in fact queued and will be sent the moment the rung is released", tc.rung, before, after)
			}
		}
	}

	// Non-degeneracy: both dispositions actually occurred. Without this, a gate that answered 202 (or
	// 503) uniformly could still satisfy a loop whose expectations had drifted to match it.
	if accepted == 0 || refused == 0 {
		t.Fatalf("the ladder produced %d accepted and %d refused rung(s) — a gate with only one disposition is not a ladder", accepted, refused)
	}
}

// TestDispatchRung_ValidationStillAnswersBeforeTheRungDoes pins the gate's PLACEMENT, not just its
// existence: at `stopped`, a malformed request gets 400, not 503.
//
// This is a real hazard and not a stylistic preference. If the rung gate ran first, every caller
// debugging its own bad payload during an incident would be told "the service is paused" and would
// retry the same broken request forever. The gate's own comment in handlers.go claims this ordering;
// this test is what makes the claim load-bearing.
func TestDispatchRung_ValidationStillAnswersBeforeTheRungDoes(t *testing.T) {
	e := newTestEnv(t, "rungorder")
	userID := e.seedUser(t, "rungorder@example.com")
	e.setRungConfigStr(t, "dispatch_mode", rungStopped)

	// Missing job_item_id entirely — a validation failure, at the deepest rung.
	bad := e.do(t, "POST", "/v1/run", map[string]any{"user_id": userID})
	if bad.status == 503 {
		t.Errorf("a malformed run at rung=stopped returned 503 `paused` — the rung gate is running BEFORE validation, so a caller with a broken payload is told the service is paused and will retry the same broken request: %s", bad.raw)
	}
	if bad.status != 400 {
		t.Errorf("a malformed run at rung=stopped returned %d, want 400: %s", bad.status, bad.raw)
	}

	// And a well-formed request at the same rung IS refused — otherwise the assertion above would be
	// satisfied by a gate that does not exist at all.
	item := e.seedJobItem(t, userID, "rungorder-ok@example.com")
	good := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item, "user_id": userID}).requireStatus(t, 503)
	if code := good.errorCode(); code != "paused" {
		t.Errorf("well-formed run at rung=stopped: code=%q, want \"paused\": %s", code, good.raw)
	}
}

// TestDispatchRung_QuoteIsNeverGatedByTheRung — POST /v1/quote answers at every rung, including
// `stopped`.
//
// Quote is a pure feasibility question that reserves nothing and sends nothing. Gating it would make
// the ladder a bigger blast radius than it advertises: an operator pulling `stopped` to halt sending
// would silently also break every caller's pre-flight check, and the caller has no way to tell that
// from an outage. The property is asserted here rather than left as "we didn't add the call" because
// "nobody added the gate to this handler" is not a mechanism — the next person to add a pause check
// may well add it to both.
func TestDispatchRung_QuoteIsNeverGatedByTheRung(t *testing.T) {
	e := newTestEnv(t, "rungquote")
	userID := e.seedUser(t, "rungquote@example.com")

	for _, rung := range []string{rungServing, rungAcceptAndDefer, rungRefuseNew, rungStopped} {
		e.setRungConfigStr(t, "dispatch_mode", rung)
		got := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": 10, "user_id": userID})
		if got.status != 200 {
			t.Errorf("rung=%s: POST /v1/quote returned %d, want 200 — quote reserves nothing and sends nothing; gating it makes the ladder's blast radius larger than it advertises: %s", rung, got.status, got.raw)
		}
	}

	// Also at the deepest rung with the legacy lever down too — the composed worst case.
	e.setRungConfigBool(t, "dispatch_enabled", false)
	e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": 10, "user_id": userID}).requireStatus(t, 200)
}

// TestDispatchRung_EnabledFalseDoesNotRefuseIntake — the legacy boolean, alone, must NOT refuse
// intake.
//
// dispatch_enabled=false composes as a FLOOR of accept_and_defer (ADR-0016's ruling: the row has too
// many live consumers to drop, so it becomes a severity floor rather than a second independent
// lever). accept_and_defer accepts intake. So an operator who flipped the old boolean before this
// item existed must see EXACTLY the intake behaviour they saw before — that is the compatibility
// claim, and this is the test that makes it one.
//
// This test is also the one that would catch the most tempting wrong implementation: reading
// dispatch_enabled in the intake gate and refusing on false. That version would look correct, pass
// every rung-name test, and silently start rejecting customer work on an unchanged config.
func TestDispatchRung_EnabledFalseDoesNotRefuseIntake(t *testing.T) {
	e := newTestEnv(t, "rungenabled")
	userID := e.seedUser(t, "rungenabled@example.com")
	e.setRungConfigBool(t, "dispatch_enabled", false)

	for _, rung := range []struct {
		mode       string
		wantStatus int
	}{
		{rungServing, 202},        // floored to accept_and_defer, which accepts
		{rungAcceptAndDefer, 202}, // unchanged
		{rungRefuseNew, 503},      // the mode is deeper than the floor; the mode wins
		{rungStopped, 503},
	} {
		e.setRungConfigStr(t, "dispatch_mode", rung.mode)
		item := e.seedJobItem(t, userID, "enabled-"+rung.mode+"@example.com")
		got := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item, "user_id": userID})
		if got.status != rung.wantStatus {
			t.Errorf("dispatch_enabled=false, mode=%s: POST /v1/run returned %d, want %d — dispatch_enabled is a FLOOR of accept_and_defer (which accepts), never an intake refusal; an operator who flipped the old boolean must see the intake behaviour they saw before item 12: %s",
				rung.mode, got.status, rung.wantStatus, got.raw)
		}
	}
}

// TestDispatchRung_UnrecognizedModeStillAcceptsIntake — a garbage dispatch_mode must not refuse
// customer work.
//
// The Control resolves an unrecognized value to accept_and_defer (fail toward the safer rung), and
// accept_and_defer ACCEPTS intake. If this surface refused instead, the two halves of one control
// would disagree about the same garbage value: the Control would say "keep accepting, I just won't
// send" while the API said "go away." Refusing is also the irreversible direction — a refused
// submission may never be retried, a queued one is merely waiting.
func TestDispatchRung_UnrecognizedModeStillAcceptsIntake(t *testing.T) {
	e := newTestEnv(t, "rungbadmode")
	userID := e.seedUser(t, "rungbadmode@example.com")

	for i, raw := range []string{
		`{"v":"srving"}`,      // a typo of the shallowest rung
		`{"v":"stopped "}`,    // a trailing space on the DEEPEST rung — must not be guessed at
		`{"v":""}`,            // empty
		`{"v":"paused"}`,      // a plausible-sounding word that is not a rung
		`{"v":123}`,           // wrong JSON type entirely
		`{"not_v":"stopped"}`, // right type, wrong envelope
	} {
		e.setRungConfigRaw(t, "dispatch_mode", raw)
		item := e.seedJobItem(t, userID, "badmode-"+randHex(4)+"@example.com")
		got := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item, "user_id": userID})
		if got.status != 202 {
			t.Errorf("case %d dispatch_mode=%s: POST /v1/run returned %d, want 202 — an unreadable policy must not refuse customer work; the Control resolves this to accept_and_defer, which accepts, and refusing here would make the two halves of one control disagree about the same value: %s",
				i, raw, got.status, got.raw)
		}
	}

	// The absent-row branch of the same fail-open path.
	e.deleteConfigRow(t, "dispatch_mode")
	item := e.seedJobItem(t, userID, "badmode-absent@example.com")
	e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item, "user_id": userID}).requireStatus(t, 202)
}

// TestDispatchRung_ReportPausedIsTrueAtEveryRungWhereWorkIsNotFlowing — GET /v1/report's
// `dispatch_paused` across the full 4x2 matrix of both dials.
//
// The field is retained verbatim (Rule 5, additive-only) and its MEANING is unchanged — "the
// effective rung is not serving." What changed at send_managed_0025 is that it now derives from both
// dials instead of from dispatch_enabled alone. Before this item, a Control halted by any mechanism
// other than the boolean reported `dispatch_paused: false` to every polling UI: work visibly not
// moving, with the dashboard insisting nothing was wrong. That is the failure this matrix pins.
func TestDispatchRung_ReportPausedIsTrueAtEveryRungWhereWorkIsNotFlowing(t *testing.T) {
	e := newTestEnv(t, "rungreport")
	userID := e.seedUser(t, "rungreport@example.com")
	campaignID := e.newCampaignID(t)
	e.campaignJobRow(t, campaignID, userID, "queued")

	readPaused := func(t *testing.T) (bool, string) {
		t.Helper()
		got := e.do(t, "GET", "/v1/report?campaign_id="+campaignID+"&user_id="+userID, nil).requireStatus(t, 200)
		paused, ok := got.body["dispatch_paused"].(bool)
		if !ok {
			t.Fatalf("dispatch_paused missing or not a bool — the field is RETAINED (Rule 5) and must never disappear: %s", got.raw)
		}
		return paused, got.raw
	}

	for _, tc := range []struct {
		mode    string
		enabled bool
		want    bool
	}{
		{rungServing, true, false}, // the ONLY combination where work is flowing
		{rungAcceptAndDefer, true, true},
		{rungRefuseNew, true, true},
		{rungStopped, true, true},
		{rungServing, false, true}, // the legacy floor: this is the pre-0025 answer, unchanged
		{rungAcceptAndDefer, false, true},
		{rungRefuseNew, false, true},
		{rungStopped, false, true},
	} {
		e.setRungConfigStr(t, "dispatch_mode", tc.mode)
		e.setRungConfigBool(t, "dispatch_enabled", tc.enabled)
		got, raw := readPaused(t)
		if got != tc.want {
			t.Errorf("mode=%s enabled=%t: dispatch_paused=%t, want %t — a UI told \"not paused\" while work is not moving is the exact ambiguity that makes an operator stop trusting the dashboard: %s",
				tc.mode, tc.enabled, got, tc.want, raw)
		}
	}

	// Non-degeneracy: the field is not hardwired true. Restoring the one flowing combination must
	// bring it back to false, or every assertion above is satisfied by a constant.
	e.setRungConfigStr(t, "dispatch_mode", rungServing)
	e.setRungConfigBool(t, "dispatch_enabled", true)
	if got, raw := readPaused(t); got {
		t.Errorf("dispatch_paused stayed true at serving+enabled — the field is hardwired, so every assertion above is vacuous: %s", raw)
	}

	// A garbage mode with the boolean up: the Control resolves it to accept_and_defer, which IS
	// paused. Reporting not-paused here would tell a UI work is flowing while the Control claims
	// nothing.
	e.setRungConfigRaw(t, "dispatch_mode", `{"v":"srving"}`)
	if got, raw := readPaused(t); !got {
		t.Errorf("dispatch_paused=false for an unrecognized mode — the Control resolves it to accept_and_defer and claims nothing, so the UI is being told work is flowing while none is: %s", raw)
	}
}

// TestDispatchRung_ReportSurvivesAnAbsentModeRow — the report must not 500 when dispatch_mode is
// unreadable; it reports the pre-0025 answer for exactly that input.
//
// Separated from the matrix above because the disposition is deliberately DIFFERENT from the garbage
// case: an absent row means the Control cannot boot at all (dispatch.LoadConfig hard-errors on it),
// so this branch in practice only covers a read blip, and answering with the historical value beats
// failing a read-only report.
func TestDispatchRung_ReportSurvivesAnAbsentModeRow(t *testing.T) {
	e := newTestEnv(t, "rungreportgone")
	userID := e.seedUser(t, "rungreportgone@example.com")
	campaignID := e.newCampaignID(t)
	e.campaignJobRow(t, campaignID, userID, "queued")
	e.deleteConfigRow(t, "dispatch_mode")

	got := e.do(t, "GET", "/v1/report?campaign_id="+campaignID+"&user_id="+userID, nil).requireStatus(t, 200)
	if paused, ok := got.body["dispatch_paused"].(bool); !ok || paused {
		t.Errorf("dispatch_paused=%v with dispatch_enabled=true and no mode row, want false (the pre-0025 answer for this exact input): %s", got.body["dispatch_paused"], got.raw)
	}

	// With the legacy boolean ALSO down, the floor still applies with no mode row at all.
	e.setRungConfigBool(t, "dispatch_enabled", false)
	got2 := e.do(t, "GET", "/v1/report?campaign_id="+campaignID+"&user_id="+userID, nil).requireStatus(t, 200)
	if paused, ok := got2.body["dispatch_paused"].(bool); !ok || !paused {
		t.Errorf("dispatch_paused=%v with dispatch_enabled=false and no mode row, want true: %s", got2.body["dispatch_paused"], got2.raw)
	}
}

// --- the no-drift mechanism, this package's own half -------------------------------------------

// TestDispatchRung_LadderMatchesTheDispatchPackageExactly is the test dispatch_rung.go's header names
// as the reason this package is allowed to hold a SECOND copy of the ladder.
//
// internal/dispatch has its own copy of this comparison, plus a discovery half that fails if a FOURTH
// copy of the ladder appears anywhere under internal/. This one is not redundant with it: it lives in
// the package that holds the copy, so a developer editing platformapi and running only
// `go test ./internal/platformapi/` finds the drift. A guard that only runs when you test a DIFFERENT
// package is a guard that will be discovered by production.
//
// It compares SOURCE TEXT against ../dispatch/rung.go rather than comparing this package's compiled
// map against anything, because a Go-value comparison cannot see a constant renamed on the other
// side — the value would still be "serving" while the identifier that names it changed.
func TestDispatchRung_LadderMatchesTheDispatchPackageExactly(t *testing.T) {
	wantNames, wantOrder := parseRungLadder(t, "../dispatch/rung.go")
	gotNames, gotOrder := parseRungLadder(t, "dispatch_rung.go")

	if len(wantNames) != 4 || len(wantOrder) != 4 {
		t.Fatalf("parsed %d name(s)/%d severity entr(ies) from ../dispatch/rung.go, want 4/4 — the authoritative side is not being read correctly, so this comparison proves nothing: %v / %v",
			len(wantNames), len(wantOrder), wantNames, wantOrder)
	}
	if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
		t.Errorf("this package declares rung names %v, internal/dispatch declares %v — the ladder has DRIFTED; POST /v1/run would answer for a rung the Control does not have (or vice versa)", gotNames, wantNames)
	}
	if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("this package orders severity %v, internal/dispatch orders %v — a drifted ORDERING is the worse failure: both compile, and \"refuse_new and below\" quietly means two different things on the two surfaces", gotOrder, wantOrder)
	}

	// The runtime map must agree with this file's own source too. Without this, someone could edit the
	// severity literals in the map while leaving a stale comment block that the regex happens to match
	// — the source comparison would pass while the code behaved differently.
	if len(rungSeverity) != len(gotOrder) {
		t.Fatalf("rungSeverity has %d entries but this file's source declares %d — the source scan and the compiled map disagree about how big the ladder is", len(rungSeverity), len(gotOrder))
	}
	for _, entry := range gotOrder {
		parts := strings.SplitN(entry, "=", 2)
		var matched bool
		for name, sev := range rungSeverity {
			if strings.EqualFold(strings.ReplaceAll(name, "_", ""), parts[0]) && itoa(sev) == parts[1] {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("source declares severity %s but the compiled rungSeverity map has no such entry: %v", entry, rungSeverity)
		}
	}
}

var (
	rungNameRe = regexp.MustCompile(`(?m)^\s*[Rr]ung\w+\s*=\s*"([a-z_]+)"`)
	rungSevRe  = regexp.MustCompile(`(?m)^\s*[Rr]ung(\w+):\s*(\d+),`)
)

// parseRungLadder extracts the rung slugs (declaration order) and the severity table (as "name=n",
// declaration order) from a Go source file. Fatals if either pattern stops matching: a pattern that
// matches nothing would make every comparison vacuously equal, so the drift check would go quietly
// green at exactly the moment it stopped working.
func parseRungLadder(t *testing.T, path string) (names, order []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(raw)
	for _, m := range rungNameRe.FindAllStringSubmatch(src, -1) {
		names = append(names, m[1])
	}
	for _, m := range rungSevRe.FindAllStringSubmatch(src, -1) {
		order = append(order, strings.ToLower(m[1])+"="+m[2])
	}
	if len(names) == 0 || len(order) == 0 {
		t.Fatalf("%s: parsed %d rung constant(s) and %d severity entr(ies) — the patterns no longer match this file's shape, so every comparison against it would be vacuous", path, len(names), len(order))
	}
	return names, order
}

// itoa avoids a strconv import for one call.
func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
