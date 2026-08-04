package adminapi

import (
	"encoding/json"
	"testing"
)

// mkPool creates a shared pool (system user seeded) and returns its id.
func (e *testEnv) mkSharedPool(t *testing.T, name string) string {
	t.Helper()
	e.seedSystemUser(t)
	return e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "shared", "name": name}).requireStatus(t, 201).str("id")
}

// mkAccount creates an account in poolID with the given host/username and returns its id.
func (e *testEnv) mkAccount(t *testing.T, poolID, host, user string) string {
	t.Helper()
	return e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": poolID, "smtp_host": host, "smtp_username": user,
		"smtp_secret": "s-" + randHex(3), "from_address": "s@example.com",
	}).requireStatus(t, 201).str("id")
}

// TestT7_PerPoolUniqueness — duplicate (host,user) within a pool => 409; same pair in a different
// pool => 201; a move that would collide => 409.
func TestT7_PerPoolUniqueness(t *testing.T) {
	e := newTestEnv(t, "t7")
	p1 := e.mkSharedPool(t, "P1")
	p2 := e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "shared", "name": "P2"}).requireStatus(t, 201).str("id")
	p3 := e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "shared", "name": "P3"}).requireStatus(t, 201).str("id")

	// (host=h, user=u) in P1 => 201.
	e.mkAccount(t, p1, "h.example.com", "u@example.com")

	// Second (h,u) in P1 => 409 duplicate.
	r := e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": p1, "smtp_host": "h.example.com", "smtp_username": "u@example.com",
		"smtp_secret": "x", "from_address": "s@example.com",
	}).requireStatus(t, 409)
	if r.errorCode() != codeConflict {
		t.Fatalf("dup in pool: code=%s, want conflict", r.errorCode())
	}
	assertNoRawDBLeak(t, r.raw)

	// (h,u) in P2 => 201 (allowed across pools).
	a2 := e.mkAccount(t, p2, "h.example.com", "u@example.com")

	// Move P2's account to P1 (would collide) => 409; move to empty P3 => 200.
	rm := e.do(t, "PATCH", "/v1/admin/accounts/"+a2, map[string]any{"pool_id": p1}).requireStatus(t, 409)
	assertNoRawDBLeak(t, rm.raw)
	e.do(t, "PATCH", "/v1/admin/accounts/"+a2, map[string]any{"pool_id": p3}).requireStatus(t, 200)
}

// TestT8_QuarantineRoundTripAudit — quarantine → 200 + exactly one 'quarantine' event; repeat →
// 422 + no new event; unquarantine → 200 + one event; repeat → 422; quarantine leaves other flags.
func TestT8_QuarantineRoundTripAudit(t *testing.T) {
	e := newTestEnv(t, "t8")
	p := e.mkSharedPool(t, "P")
	acc := e.mkAccount(t, p, "h.example.com", "u@example.com")
	base := e.eventCount(t, acc) // includes the 'created' event

	// quarantine => 200, quarantined=true, +1 event.
	q := e.do(t, "POST", "/v1/admin/accounts/"+acc+"/quarantine", nil).requireStatus(t, 200)
	if !statusBool(q, "quarantined") {
		t.Fatal("quarantine did not set quarantined=true")
	}
	if e.eventCount(t, acc) != base+1 {
		t.Fatalf("event count after quarantine = %d, want %d", e.eventCount(t, acc), base+1)
	}
	action, actor, from, to, rid := e.latestEvent(t, acc)
	if action != "quarantine" || actor != "admin" {
		t.Fatalf("latest event action/actor = %s/%s, want quarantine/admin", action, actor)
	}
	if rid == nil || *rid == "" {
		t.Fatal("event request_id is null/empty")
	}
	if !flagJSON(from, "quarantined", false) || !flagJSON(to, "quarantined", true) {
		t.Fatalf("quarantine from/to flags wrong: from=%s to=%s", from, to)
	}

	// quarantine again => 422 already_in_state, NO new event.
	e.do(t, "POST", "/v1/admin/accounts/"+acc+"/quarantine", nil).requireStatus(t, 422)
	if e.eventCount(t, acc) != base+1 {
		t.Fatalf("illegal repeat wrote an event: count=%d, want %d", e.eventCount(t, acc), base+1)
	}

	// unquarantine => 200, +1 event.
	e.do(t, "POST", "/v1/admin/accounts/"+acc+"/unquarantine", nil).requireStatus(t, 200)
	if e.eventCount(t, acc) != base+2 {
		t.Fatalf("event count after unquarantine = %d, want %d", e.eventCount(t, acc), base+2)
	}
	// unquarantine again => 422 not_in_state, no new event.
	e.do(t, "POST", "/v1/admin/accounts/"+acc+"/unquarantine", nil).requireStatus(t, 422)
	if e.eventCount(t, acc) != base+2 {
		t.Fatalf("illegal repeat wrote an event: count=%d", e.eventCount(t, acc))
	}

	// quarantine did not alter paused/disabled/score.
	g := e.do(t, "GET", "/v1/admin/accounts/"+acc, nil).requireStatus(t, 200)
	if statusBool(g, "paused") || statusBool(g, "disabled") {
		t.Fatal("quarantine round-trip altered paused/disabled")
	}
	if score, _ := g.body["account_status_score"].(float64); score != 100 {
		t.Fatalf("account_status_score = %v, want 100 (untouched)", g.body["account_status_score"])
	}
}

// TestT8b_ManualPauseUnpauseGuard — pause/unpause manual round-trip; unpause refuses to clear a
// daily_limit pause (422 not_manual_pause, no event, pause unchanged).
func TestT8b_ManualPauseUnpauseGuard(t *testing.T) {
	e := newTestEnv(t, "t8b")
	p := e.mkSharedPool(t, "P")
	acc := e.mkAccount(t, p, "h.example.com", "u@example.com")

	// pause => 200, paused=true, reason=manual.
	pr := e.do(t, "POST", "/v1/admin/accounts/"+acc+"/pause", nil).requireStatus(t, 200)
	if !statusBool(pr, "paused") || statusStr(pr, "pause_reason") != "manual" {
		t.Fatalf("pause: paused/reason = %v/%q, want true/manual", statusBool(pr, "paused"), statusStr(pr, "pause_reason"))
	}
	// unpause => 200, paused=false, reason=null.
	ur := e.do(t, "POST", "/v1/admin/accounts/"+acc+"/unpause", nil).requireStatus(t, 200)
	if statusBool(ur, "paused") || ur.body["status"].(map[string]any)["pause_reason"] != nil {
		t.Fatal("unpause did not clear paused/pause_reason")
	}

	// Force a daily_limit pause directly, then unpause must REFUSE (422 not_manual_pause).
	e.setPauseDaily(t, acc)
	base := e.eventCount(t, acc)
	rr := e.do(t, "POST", "/v1/admin/accounts/"+acc+"/unpause", nil).requireStatus(t, 422)
	if rr.errorCode() != codeUnprocessable {
		t.Fatalf("unpause daily_limit: code=%s, want unprocessable", rr.errorCode())
	}
	if e.eventCount(t, acc) != base {
		t.Fatalf("refused unpause wrote an event: count changed from %d", base)
	}
	// The pause is unchanged (still paused, daily_limit).
	g := e.do(t, "GET", "/v1/admin/accounts/"+acc, nil).requireStatus(t, 200)
	if !statusBool(g, "paused") || statusStr(g, "pause_reason") != "daily_limit" {
		t.Fatal("refused unpause altered the daily_limit pause")
	}
}

// TestT9_AccountEventsAppendOnly — as send_managed_app, UPDATE and DELETE on account_events fail;
// INSERT/SELECT succeed; deleting the account THROUGH THE API leaves a 'deleted' audit row whose
// account_id is nulled by the FK, and earlier lifecycle events also survive with nulled account_id.
func TestT9_AccountEventsAppendOnly(t *testing.T) {
	e := newTestEnv(t, "t9")
	p := e.mkSharedPool(t, "P")
	acc := e.mkAccount(t, p, "h.example.com", "u@example.com")
	// Produce one lifecycle event.
	e.do(t, "POST", "/v1/admin/accounts/"+acc+"/quarantine", nil).requireStatus(t, 200)

	m := e.moduleConn(t)
	var evID string
	if err := m.QueryRow(bgCtx(), "SELECT id FROM send_managed.account_events WHERE account_id=$1 LIMIT 1", acc).Scan(&evID); err != nil {
		t.Fatalf("read event id: %v", err)
	}
	// UPDATE fails (privilege 42501 or trigger).
	if _, err := m.Exec(bgCtx(), "UPDATE send_managed.account_events SET action='x' WHERE id=$1", evID); err == nil {
		t.Fatal("UPDATE account_events succeeded, want rejection")
	}
	// Direct DELETE fails (42501).
	if _, err := m.Exec(bgCtx(), "DELETE FROM send_managed.account_events WHERE id=$1", evID); err == nil {
		t.Fatal("direct DELETE account_events succeeded, want 42501")
	}
	// INSERT + SELECT succeed (anti-vacuity positive).
	var freshUser string
	_ = e.admin.QueryRow(bgCtx(), "INSERT INTO public.users (email,is_active) VALUES ('t9b@x.co',true) RETURNING id").Scan(&freshUser)
	if _, err := m.Exec(bgCtx(),
		"INSERT INTO send_managed.account_events (account_id,user_id,actor,action) VALUES ($1,$2,'admin','updated')", acc, freshUser); err != nil {
		t.Fatalf("module INSERT account_events failed (should succeed): %v", err)
	}

	// The reject-trigger has teeth even for a PRIVILEGED (superuser) connection: a genuine
	// CONTENT update is rejected (proves the OD-23 FK-aware trigger only permits account_id->NULL,
	// not arbitrary mutation). A content change is forbidden regardless of the account_id value.
	if _, err := e.admin.Exec(bgCtx(), "UPDATE send_managed.account_events SET action='tampered' WHERE id=$1", evID); err == nil {
		t.Fatal("superuser content UPDATE on account_events succeeded — the append-only trigger has no teeth")
	}
	// And a content change that ALSO nulls account_id must still be rejected (not smuggled in via
	// the FK-maintenance carve-out).
	if _, err := e.admin.Exec(bgCtx(), "UPDATE send_managed.account_events SET account_id=NULL, action='tampered' WHERE id=$1", evID); err == nil {
		t.Fatal("account_id->NULL bundled with a content change was permitted — carve-out too wide")
	}

	// Delete the account through the API => 'deleted' audit row with account_id nulled.
	e.do(t, "DELETE", "/v1/admin/accounts/"+acc, nil).requireStatus(t, 204)
	var deletedRows, nulledRows int
	if err := e.admin.QueryRow(bgCtx(),
		"SELECT count(*) FROM send_managed.account_events WHERE action='deleted' AND account_id IS NULL").Scan(&deletedRows); err != nil {
		t.Fatalf("count deleted audit: %v", err)
	}
	if deletedRows < 1 {
		t.Fatal("no 'deleted' audit row with nulled account_id survived the account delete")
	}
	// The earlier events for this account now have account_id nulled too (FK SET NULL).
	if err := e.admin.QueryRow(bgCtx(),
		"SELECT count(*) FROM send_managed.account_events WHERE account_id=$1", acc).Scan(&nulledRows); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if nulledRows != 0 {
		t.Fatalf("account_events still reference the deleted account id (%d rows), want 0 (FK SET NULL)", nulledRows)
	}
}

// TestT11_LowerDailyLimitBelowCounter — lowering daily_limit below daily_sent_count => clean 409
// (no raw 23514); stored limit unchanged; a valid lower value >= counter succeeds.
func TestT11_LowerDailyLimitBelowCounter(t *testing.T) {
	e := newTestEnv(t, "t11")
	p := e.mkSharedPool(t, "P")
	acc := e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": p, "smtp_host": "h.example.com", "smtp_username": "u@example.com",
		"smtp_secret": "s", "from_address": "s@example.com", "daily_limit": 500,
	}).requireStatus(t, 201).str("id")
	e.setDailySentCount(t, acc, 300)

	r := e.do(t, "PATCH", "/v1/admin/accounts/"+acc, map[string]any{"daily_limit": 200}).requireStatus(t, 409)
	if r.errorCode() != codeConflict || r.errorDetailIssue("daily_limit") != issueConflict {
		t.Fatalf("lower below counter: code=%s issue=%s, want conflict/conflict", r.errorCode(), r.errorDetailIssue("daily_limit"))
	}
	assertNoRawDBLeak(t, r.raw)
	// Stored limit unchanged (500).
	g := e.do(t, "GET", "/v1/admin/accounts/"+acc, nil).requireStatus(t, 200)
	if v, _ := g.body["daily_limit"].(float64); v != 500 {
		t.Fatalf("daily_limit = %v after rejected lower, want 500", g.body["daily_limit"])
	}
	// >= counter succeeds.
	e.do(t, "PATCH", "/v1/admin/accounts/"+acc, map[string]any{"daily_limit": 400}).requireStatus(t, 200)
}

// TestT12_CrossPoolDuplicateIndependentCounters — same (host,user) in two pools => two rows with
// independent daily_sent_count.
func TestT12_CrossPoolDuplicateIndependentCounters(t *testing.T) {
	e := newTestEnv(t, "t12")
	p1 := e.mkSharedPool(t, "P1")
	p2 := e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "shared", "name": "P2"}).requireStatus(t, 201).str("id")
	a1 := e.mkAccount(t, p1, "same.example.com", "same@example.com")
	a2 := e.mkAccount(t, p2, "same.example.com", "same@example.com")
	if a1 == a2 {
		t.Fatal("expected two distinct account rows")
	}
	// Bump a1's counter; a2's stays independent.
	e.setDailySentCount(t, a1, 5)
	g2 := e.do(t, "GET", "/v1/admin/accounts/"+a2, nil).requireStatus(t, 200)
	if v, _ := g2.body["daily_sent_count"].(float64); v != 0 {
		t.Fatalf("a2 daily_sent_count = %v, want 0 (independent counter)", g2.body["daily_sent_count"])
	}
}

// TestT14_EndToEndLifecycle — create → get → quarantine → get → unquarantine → get, entirely
// through the API, each step producing its account_events row.
func TestT14_EndToEndLifecycle(t *testing.T) {
	e := newTestEnv(t, "t14")
	p := e.mkSharedPool(t, "P")
	acc := e.mkAccount(t, p, "h.example.com", "u@example.com")

	e.do(t, "GET", "/v1/admin/accounts/"+acc, nil).requireStatus(t, 200)
	e.do(t, "POST", "/v1/admin/accounts/"+acc+"/quarantine", nil).requireStatus(t, 200)
	g := e.do(t, "GET", "/v1/admin/accounts/"+acc, nil).requireStatus(t, 200)
	if !statusBool(g, "quarantined") {
		t.Fatal("not quarantined after quarantine")
	}
	e.do(t, "POST", "/v1/admin/accounts/"+acc+"/unquarantine", nil).requireStatus(t, 200)
	g2 := e.do(t, "GET", "/v1/admin/accounts/"+acc, nil).requireStatus(t, 200)
	if statusBool(g2, "quarantined") {
		t.Fatal("still quarantined after unquarantine")
	}
	// The events read endpoint returns the trail (created, quarantine, unquarantine).
	ev := e.do(t, "GET", "/v1/admin/accounts/"+acc+"/events", nil).requireStatus(t, 200)
	items, _ := ev.body["items"].([]any)
	if len(items) < 3 {
		t.Fatalf("events trail has %d items, want >= 3 (created, quarantine, unquarantine)", len(items))
	}
	actions := map[string]bool{}
	for _, it := range items {
		m := it.(map[string]any)
		actions[m["action"].(string)] = true
	}
	for _, want := range []string{"created", "quarantine", "unquarantine"} {
		if !actions[want] {
			t.Fatalf("events trail missing action %q; got %v", want, actions)
		}
	}
}

// TestAuth_WrongOrMissingKeyRejected — an unauthenticated / wrong-key admin request => 401 (the
// operator-key gate proven, like Phase 0's wrong-token 401). Anti-vacuity: the correct key works.
func TestAuth_WrongOrMissingKeyRejected(t *testing.T) {
	e := newTestEnv(t, "auth")
	e.seedSystemUser(t)

	// Missing key => 401.
	miss := e.doRaw(t, "GET", "/v1/admin/config", nil, "")
	miss.requireStatus(t, 401)
	if miss.errorCode() != codeUnauthenticated {
		t.Fatalf("missing key: code=%s, want unauthenticated", miss.errorCode())
	}
	// Wrong key => 401.
	wrong := e.doRaw(t, "GET", "/v1/admin/config", nil, "definitely-not-the-key")
	wrong.requireStatus(t, 401)
	// Anti-vacuity: the correct key succeeds on the same route.
	e.do(t, "GET", "/v1/admin/config", nil).requireStatus(t, 200)
}

// --- small status/flag assertion helpers ---

func statusBool(r resp, field string) bool {
	st, ok := r.body["status"].(map[string]any)
	if !ok {
		return false
	}
	b, _ := st[field].(bool)
	return b
}

func statusStr(r resp, field string) string {
	st, ok := r.body["status"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := st[field].(string)
	return s
}

// flagJSON reports whether a from_flags/to_flags JSONB blob has field == want.
func flagJSON(raw []byte, field string, want bool) bool {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	b, _ := m[field].(bool)
	return b == want
}
