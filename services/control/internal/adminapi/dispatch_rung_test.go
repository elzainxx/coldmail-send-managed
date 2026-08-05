package adminapi

// dispatch_rung_test.go — fix-plan item 12's OPERATOR-SURFACE half: what GET /v1/admin/health reports
// at each rung, and what PUT /v1/admin/config/dispatch_mode accepts. Real Postgres, real HTTP against
// the actual production Server (CLAUDE.md Rule 3).
//
// WHY THIS PACKAGE NEEDS ITS OWN RUNG SUITE. internal/adminapi does not import internal/dispatch, so
// resolveRung here is a THIRD implementation of the same composition. The dispatch package's tests
// prove the Control obeys the ladder; they cannot prove the dashboard describes it correctly, and a
// dashboard that describes the wrong rung during an incident is the failure item 12 exists to remove
// (M1's D2: a control that looks normal while being ignored).

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// setHealthConfigStr writes a string config value directly (setHealthConfigBool/Int already exist in
// health_test.go; dispatch_mode is the first string key this package's tests need to move).
func (e *testEnv) setHealthConfigStr(t *testing.T, key, v string) {
	t.Helper()
	if _, err := e.admin.Exec(bgCtx(),
		`UPDATE send_managed.config SET value = jsonb_build_object('v', $2::text) WHERE key=$1`, key, v); err != nil {
		t.Fatalf("set config %s: %v", key, err)
	}
}

// setHealthConfigRaw writes a value the write-path validator would reject — the only way to observe
// what the READ path does with a garbage dispatch_mode, which is the case that matters: such a row
// could arrive from an older migration, a manual psql session, or a restore.
func (e *testEnv) setHealthConfigRaw(t *testing.T, key, rawJSON string) {
	t.Helper()
	if _, err := e.admin.Exec(bgCtx(),
		`UPDATE send_managed.config SET value = $2::jsonb WHERE key=$1`, key, rawJSON); err != nil {
		t.Fatalf("set raw config %s: %v", key, err)
	}
}

// readHealthDispatch returns the health response's dispatch object plus the raw body for messages.
func (e *testEnv) readHealthDispatch(t *testing.T) (map[string]any, string) {
	t.Helper()
	r := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	d, ok := r.body["dispatch"].(map[string]any)
	if !ok {
		t.Fatalf("health response has no dispatch object: %s", string(r.raw))
	}
	return d, string(r.raw)
}

// TestDispatchRung_HealthNamesTheRungAndKeepsThePausedBoolean — the full 4x2 matrix of both dials
// against BOTH reported fields.
//
// Two properties in one matrix, because they are only meaningful together:
//
//  1. dispatch_rung (ADDITIVE, item 12) names WHICH rung is pulled. A boolean cannot: three of the
//     four rungs are "paused" and they differ in whether intake and the reclaim sweep still run. An
//     operator staring at `dispatch_paused: true` during an incident cannot tell whether new work is
//     still being accepted, and that ambiguity is what makes them stop trusting the dashboard.
//
//  2. dispatch_paused (RETAINED, Rule 5) keeps its meaning — "the effective rung is not serving" —
//     and every input that made it true before send_managed_0025 still makes it true. What changed is
//     that it no longer reads `false` while the Control is halted by the newer dial.
//
// The two together also pin the pair's CONSISTENCY: paused must be exactly "rung != serving". A
// version where the boolean and the string could disagree would be worse than either field alone,
// because a reader has no way to know which one to believe.
func TestDispatchRung_HealthNamesTheRungAndKeepsThePausedBoolean(t *testing.T) {
	e := newTestEnv(t, "rungheal")

	cases := []struct {
		mode       string
		enabled    bool
		wantRung   string
		wantPaused bool
	}{
		{rungServing, true, rungServing, false}, // the ONLY combination where work is flowing
		{rungAcceptAndDefer, true, rungAcceptAndDefer, true},
		{rungRefuseNew, true, rungRefuseNew, true},
		{rungStopped, true, rungStopped, true},
		// dispatch_enabled=false is a FLOOR of accept_and_defer, not a second independent lever: it
		// raises a shallower rung to accept_and_defer and leaves a deeper one alone. Reporting the
		// composed answer is the whole point — an operator who pulled one dial and forgot the other
		// must see the rung the Control is ACTUALLY at, which is how half a rung stays pulled.
		{rungServing, false, rungAcceptAndDefer, true},
		{rungAcceptAndDefer, false, rungAcceptAndDefer, true},
		{rungRefuseNew, false, rungRefuseNew, true},
		{rungStopped, false, rungStopped, true},
	}

	sawServing, sawPaused := false, false
	for _, tc := range cases {
		e.setHealthConfigStr(t, "dispatch_mode", tc.mode)
		e.setHealthConfigBool(t, "dispatch_enabled", tc.enabled)

		d, raw := e.readHealthDispatch(t)

		gotRung, ok := d["dispatch_rung"].(string)
		if !ok {
			t.Fatalf("mode=%s enabled=%t: dispatch_rung missing or not a string — a boolean cannot express WHICH of the three paused rungs is pulled, which is the field's entire purpose: %s", tc.mode, tc.enabled, raw)
		}
		if gotRung != tc.wantRung {
			t.Errorf("mode=%s enabled=%t: dispatch_rung=%q, want %q — the surface must report the rung the Control is ACTUALLY at, not the raw value of one dial", tc.mode, tc.enabled, gotRung, tc.wantRung)
		}

		gotPaused, ok := d["dispatch_paused"].(bool)
		if !ok {
			t.Fatalf("mode=%s enabled=%t: dispatch_paused missing or not a bool — the field is RETAINED (Rule 5) and must never disappear: %s", tc.mode, tc.enabled, raw)
		}
		if gotPaused != tc.wantPaused {
			t.Errorf("mode=%s enabled=%t: dispatch_paused=%t, want %t", tc.mode, tc.enabled, gotPaused, tc.wantPaused)
		}

		// The consistency pin: the two fields must never disagree about the same state.
		if gotPaused != (gotRung != rungServing) {
			t.Errorf("mode=%s enabled=%t: dispatch_paused=%t but dispatch_rung=%q — the boolean and the string disagree, which is worse than either alone: a reader has no way to know which to believe", tc.mode, tc.enabled, gotPaused, gotRung)
		}

		if gotRung == rungServing {
			sawServing = true
		}
		if gotPaused {
			sawPaused = true
		}
	}

	// Non-degeneracy in both directions: neither field is a constant.
	if !sawServing || !sawPaused {
		t.Fatalf("across 8 combinations the surface reported serving=%t and paused=%t — a field with one value is not reporting anything", sawServing, sawPaused)
	}
}

// TestDispatchRung_HealthReportsWhatTheControlWillDoNotWhatTheRowSays — a garbage dispatch_mode is
// reported as the rung the Control resolves it to, never echoed back verbatim.
//
// Echoing an unrecognized value would be the same looks-normal-while-ignored failure the whole item
// exists to remove, one layer up: an operator would read `dispatch_rung: "srving"` off the dashboard
// and conclude their pull had landed, when in fact the Control is at accept_and_defer and sending
// nothing. Reporting the RESOLVED rung means the dashboard and the Control cannot disagree.
func TestDispatchRung_HealthReportsWhatTheControlWillDoNotWhatTheRowSays(t *testing.T) {
	e := newTestEnv(t, "rungbadheal")

	// An unrecognized STRING is resolvable: the Control degrades it to accept_and_defer, so this
	// surface can and must report that rung.
	for _, raw := range []string{
		`{"v":"srving"}`,   // a typo of the shallowest rung
		`{"v":"stopped "}`, // trailing space on the DEEPEST rung — must not be guessed at
		`{"v":""}`,         // empty
		`{"v":"paused"}`,   // a plausible word that is not a rung
		`{"v":"SERVING"}`,  // wrong case
	} {
		e.setHealthConfigRaw(t, "dispatch_mode", raw)
		e.setHealthConfigBool(t, "dispatch_enabled", true)

		d, body := e.readHealthDispatch(t)
		gotRung, _ := d["dispatch_rung"].(string)
		if gotRung != rungAcceptAndDefer {
			t.Errorf("dispatch_mode=%s: dispatch_rung=%q, want %q — the surface must report what the Control WILL DO; echoing an unrecognized value back tells the operator their pull landed when the Control is somewhere else entirely: %s",
				raw, gotRung, rungAcceptAndDefer, body)
		}
		if paused, _ := d["dispatch_paused"].(bool); !paused {
			t.Errorf("dispatch_mode=%s: dispatch_paused=false — the Control resolves this to accept_and_defer and claims nothing, so the dashboard is reporting work is flowing while none is: %s", raw, body)
		}
	}

	// A value that is not a STRING AT ALL is a different case, and the disposition is deliberately
	// different: the read fails, and this endpoint fails the request rather than invent a rung — the
	// same reasoning as the deleted-row test below. The distinction is not pedantry. An unrecognized
	// string is a value the Control has a defined answer for, so reporting that answer is accurate. A
	// broken envelope means this surface does not know what the Control read either, and guessing
	// "accept_and_defer" would be indistinguishable on the dashboard from an operator having actually
	// set that rung.
	for _, raw := range []string{
		`{"v":123}`,           // wrong JSON type
		`{"v":true}`,          // ditto
		`{"not_v":"stopped"}`, // right type, wrong envelope
	} {
		e.setHealthConfigRaw(t, "dispatch_mode", raw)
		r := e.do(t, "GET", "/v1/admin/health", nil)
		if r.status == 200 {
			t.Errorf("dispatch_mode=%s: health returned 200 — it invented a rung from a value it could not read, and on the dashboard that is indistinguishable from an operator having set it: %s", raw, string(r.raw))
		}
	}
}

// TestDispatchRung_ConfigPUTRejectsANonRungAndPersistsNothing — the write-time enum gate.
//
// The Control fails safe on a garbage value regardless (it resolves to accept_and_defer), so this
// check is not what prevents a catastrophe; it is what tells the OPERATOR. The two dispositions of a
// typo differ in what stops: someone who meant `serving` and typed `srving` would, without this gate,
// have their mail silently halted at accept_and_defer — the safe rung, but emphatically not the one
// they asked for. Rejecting the write means they learn it from the response to their own PUT rather
// than from a support ticket.
//
// Anti-vacuity: after each rejection the STORED value is read back and confirmed unchanged. "The
// response said 400" does not establish that nothing was written.
func TestDispatchRung_ConfigPUTRejectsANonRungAndPersistsNothing(t *testing.T) {
	e := newTestEnv(t, "rungcfgput")

	before := e.do(t, "GET", "/v1/admin/config/dispatch_mode", nil).requireStatus(t, 200)
	if got, _ := before.body["value"].(string); got != rungServing {
		t.Fatalf("seeded dispatch_mode=%q, want %q — the fixture is not what this test assumes: %s", got, rungServing, string(before.raw))
	}

	for _, bad := range []any{
		"srving",           // typo
		"stopped ",         // trailing space on a real rung name
		"SERVING",          // wrong case
		"",                 // empty
		"paused",           // plausible non-rung
		"accept-and-defer", // hyphens instead of underscores
		"off",              // borrowed from autoquarantine_mode's vocabulary
		"shadow",           // ditto
	} {
		r := e.do(t, "PUT", "/v1/admin/config/dispatch_mode", map[string]any{"value": bad})
		if r.status != 400 {
			t.Errorf("PUT dispatch_mode=%q returned %d, want 400 — a typo that is accepted silently halts mail at a rung the operator did not choose: %s", bad, r.status, string(r.raw))
			continue
		}
		if code := r.errorCode(); code != codeValidationFailed {
			t.Errorf("PUT dispatch_mode=%q: code=%q, want %q: %s", bad, code, codeValidationFailed, string(r.raw))
		}
		if issue := r.errorDetailIssue("value"); issue != issueForbiddenValue && issue != issueMalformed {
			t.Errorf("PUT dispatch_mode=%q: issue=%q, want forbidden_value or malformed: %s", bad, issue, string(r.raw))
		}

		after := e.do(t, "GET", "/v1/admin/config/dispatch_mode", nil).requireStatus(t, 200)
		if got, _ := after.body["value"].(string); got != rungServing {
			t.Fatalf("PUT dispatch_mode=%q was REJECTED (400) but the stored value is now %q — the rejection was cosmetic and the bad value landed anyway", bad, got)
		}
	}

	// A non-string type must be rejected by the value_type gate, and equally must not persist.
	badType := e.do(t, "PUT", "/v1/admin/config/dispatch_mode", map[string]any{"value": 3})
	if badType.status != 400 {
		t.Errorf("PUT dispatch_mode=3 returned %d, want 400: %s", badType.status, string(badType.raw))
	}
	afterType := e.do(t, "GET", "/v1/admin/config/dispatch_mode", nil).requireStatus(t, 200)
	if got, _ := afterType.body["value"].(string); got != rungServing {
		t.Fatalf("a rejected non-string PUT changed the stored value to %q", got)
	}
}

// TestDispatchRung_ConfigPUTAcceptsEveryRungAndTakesEffect — the positive control, without which the
// rejection test above would be satisfied by a gate that rejects EVERYTHING.
//
// This asserts more than a 200: after each accepted PUT, GET /v1/admin/health reports that rung. That
// is the end-to-end operator loop (pull the dial, read the dashboard) and it is the one that would
// break if the write landed in a row nothing reads — the config-that-nothing-consumes failure this
// pass has found repeatedly.
func TestDispatchRung_ConfigPUTAcceptsEveryRungAndTakesEffect(t *testing.T) {
	e := newTestEnv(t, "rungcfgok")

	for _, rung := range []string{rungAcceptAndDefer, rungRefuseNew, rungStopped, rungServing} {
		r := e.do(t, "PUT", "/v1/admin/config/dispatch_mode", map[string]any{"value": rung})
		r.requireStatus(t, 200)
		if got, _ := r.body["value"].(string); got != rung {
			t.Errorf("PUT dispatch_mode=%q echoed value=%q: %s", rung, got, string(r.raw))
		}

		// The loop that matters: the dashboard must now name the rung the operator just set. A 200 on
		// a write that no reader consumes is the config-that-does-nothing failure.
		d, body := e.readHealthDispatch(t)
		if got, _ := d["dispatch_rung"].(string); got != rung {
			t.Errorf("PUT dispatch_mode=%q then GET /v1/admin/health reports dispatch_rung=%q — the write landed somewhere the dashboard does not read, so an operator cannot confirm their own pull: %s", rung, got, body)
		}
		if paused, _ := d["dispatch_paused"].(bool); paused != (rung != rungServing) {
			t.Errorf("PUT dispatch_mode=%q: dispatch_paused=%t, want %t: %s", rung, paused, rung != rungServing, body)
		}
	}
}

// TestDispatchRung_HealthFailsRatherThanGuessWhenTheModeIsUnreadable — with the row DELETED, the
// health endpoint fails the request instead of reporting a rung it cannot determine.
//
// This is deliberately the OPPOSITE disposition from platformapi's intake gate, and the difference is
// the point. Intake fails OPEN because refusing a submission may lose customer work permanently while
// accepting one is recoverable. This endpoint's entire job is to state the system's condition, so a
// health surface that guesses one field is worse than one that admits it could not answer: an operator
// acts on what the dashboard says.
//
// An absent row also means the Control itself cannot boot (dispatch.LoadConfig hard-errors on it), so
// this response is not merely defensible, it is accurate — something IS broken.
func TestDispatchRung_HealthFailsRatherThanGuessWhenTheModeIsUnreadable(t *testing.T) {
	e := newTestEnv(t, "rungnorow")

	// Sanity: it answers 200 before the row goes away, so the failure below is caused by the deletion
	// and not by an unrelated fixture problem.
	e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)

	if _, err := e.admin.Exec(bgCtx(), `DELETE FROM send_managed.config WHERE key='dispatch_mode'`); err != nil {
		t.Fatalf("delete dispatch_mode row: %v", err)
	}

	r := e.do(t, "GET", "/v1/admin/health", nil)
	if r.status == 200 {
		t.Fatalf("health returned 200 with no dispatch_mode row — it reported a rung it cannot know, and an operator acts on what this endpoint says: %s", string(r.raw))
	}
	if r.status != 500 {
		t.Errorf("health returned %d with no dispatch_mode row, want 500: %s", r.status, string(r.raw))
	}
}

// --- the no-drift mechanism, this package's own half -------------------------------------------

// TestDispatchRung_LadderMatchesTheDispatchPackageExactly is the local half of the anti-drift
// mechanism dispatch_rung.go's header promises.
//
// internal/dispatch holds the authoritative comparison plus a discovery half that fails if a FOURTH
// copy of the ladder appears anywhere under internal/. This copy is not redundant: it runs when
// someone edits THIS package and runs only `go test ./internal/adminapi/`. A guard that fires only
// when you test a different package is a guard production discovers first.
func TestDispatchRung_LadderMatchesTheDispatchPackageExactly(t *testing.T) {
	wantNames, wantOrder := parseRungLadderSrc(t, "../dispatch/rung.go")
	gotNames, gotOrder := parseRungLadderSrc(t, "dispatch_rung.go")

	if len(wantNames) != 4 || len(wantOrder) != 4 {
		t.Fatalf("parsed %d name(s)/%d severity entr(ies) from ../dispatch/rung.go, want 4/4 — the authoritative side is not being read, so this comparison proves nothing: %v / %v",
			len(wantNames), len(wantOrder), wantNames, wantOrder)
	}
	if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
		t.Errorf("this package declares rung names %v, internal/dispatch declares %v — the ladder has DRIFTED; the dashboard would name a rung the Control does not have (or vice versa)", gotNames, wantNames)
	}
	if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("this package orders severity %v, internal/dispatch orders %v — a drifted ORDERING is the worse failure: both compile, and the composed floor silently means two different things on the two surfaces", gotOrder, wantOrder)
	}

	// The compiled map must agree with this file's own source, so that editing the literals while
	// leaving a stale comment block the regex happens to match cannot pass.
	if len(rungSeverity) != len(gotOrder) {
		t.Fatalf("rungSeverity has %d entries but this file's source declares %d — the scan and the compiled map disagree about the ladder's size", len(rungSeverity), len(gotOrder))
	}
}

var (
	rungNameSrcRe = regexp.MustCompile(`(?m)^\s*[Rr]ung\w+\s*=\s*"([a-z_]+)"`)
	rungSevSrcRe  = regexp.MustCompile(`(?m)^\s*[Rr]ung(\w+):\s*(\d+),`)
)

// parseRungLadderSrc extracts the rung slugs and the severity table (as "name=n") in declaration
// order. Fatals if either pattern stops matching: a pattern matching nothing would make every
// comparison vacuously equal, turning the drift check green exactly when it stopped working.
func parseRungLadderSrc(t *testing.T, path string) (names, order []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(raw)
	for _, m := range rungNameSrcRe.FindAllStringSubmatch(src, -1) {
		names = append(names, m[1])
	}
	for _, m := range rungSevSrcRe.FindAllStringSubmatch(src, -1) {
		order = append(order, strings.ToLower(m[1])+"="+m[2])
	}
	if len(names) == 0 || len(order) == 0 {
		t.Fatalf("%s: parsed %d rung constant(s) and %d severity entr(ies) — the patterns no longer match this file's shape, so every comparison against it would be vacuous", path, len(names), len(order))
	}
	return names, order
}
