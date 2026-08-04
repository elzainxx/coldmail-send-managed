package adminapi

import (
	"strings"
	"testing"
)

// TestT4_PoolOwnerRules — the §5.2 matrix: dedicated requires an owner; shared forbids one. Every
// failure is a clean 422, never a raw 23514.
func TestT4_PoolOwnerRules(t *testing.T) {
	e := newTestEnv(t, "t4")
	e.seedSystemUser(t)
	realUser := e.seedUser(t, "owner-t4@example.com")

	// shared + no owner => 201, user_id = SystemUserID.
	r1 := e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "shared", "name": "s1"}).requireStatus(t, 201)
	if r1.str("user_id") != SystemUserID {
		t.Fatalf("shared pool user_id = %q, want %q", r1.str("user_id"), SystemUserID)
	}
	if r1.body["owner_user_id"] != nil {
		t.Fatalf("shared pool owner_user_id = %v, want null", r1.body["owner_user_id"])
	}

	// shared + owner => 422 forbidden_value.
	r2 := e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "shared", "name": "s2", "owner_user_id": realUser}).requireStatus(t, 422)
	if r2.errorCode() != codeUnprocessable || r2.errorDetailIssue("owner_user_id") != issueForbiddenValue {
		t.Fatalf("shared+owner: code=%s issue=%s, want unprocessable/forbidden_value", r2.errorCode(), r2.errorDetailIssue("owner_user_id"))
	}

	// dedicated + owner => 201, user_id = owner.
	r3 := e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "dedicated", "name": "d1", "owner_user_id": realUser}).requireStatus(t, 201)
	if r3.str("user_id") != realUser || r3.str("owner_user_id") != realUser {
		t.Fatalf("dedicated pool user_id/owner = %q/%q, want %q", r3.str("user_id"), r3.str("owner_user_id"), realUser)
	}

	// dedicated + no owner => 422 required.
	r4 := e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "dedicated", "name": "d2"}).requireStatus(t, 422)
	if r4.errorDetailIssue("owner_user_id") != issueRequired {
		t.Fatalf("dedicated+no-owner: issue=%s, want required", r4.errorDetailIssue("owner_user_id"))
	}
	// Assert NO raw SQLSTATE / constraint text leaked in any failure body.
	for _, r := range []resp{r2, r4} {
		assertNoRawDBLeak(t, r.raw)
	}
}

// TestT5_DeletePoolWithAccounts — deleting a pool that still has accounts is a clean 409, not a
// raw DB error; after removing the account the pool deletes (204).
func TestT5_DeletePoolWithAccounts(t *testing.T) {
	e := newTestEnv(t, "t5")
	poolID, accID := e.createSharedPoolAndAccount(t, "s")

	r := e.do(t, "DELETE", "/v1/admin/pools/"+poolID, nil).requireStatus(t, 409)
	if r.errorCode() != codeConflict {
		t.Fatalf("delete pool w/ accounts: code=%s, want conflict", r.errorCode())
	}
	assertNoRawDBLeak(t, r.raw)
	if !strings.Contains(strings.ToLower(r.errorMessage()), "reassign") {
		t.Fatalf("delete-pool message missing reassign guidance: %s", r.raw)
	}

	// Delete the account, then the pool succeeds.
	e.do(t, "DELETE", "/v1/admin/accounts/"+accID, nil).requireStatus(t, 204)
	e.do(t, "DELETE", "/v1/admin/pools/"+poolID, nil).requireStatus(t, 204)
}

// TestT6_FallbackCycleRejected — A→B→A rejected at update; self-loop rejected; both clean 422.
func TestT6_FallbackCycleRejected(t *testing.T) {
	e := newTestEnv(t, "t6")
	e.seedSystemUser(t)
	mk := func(name string) string {
		return e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "shared", "name": name}).requireStatus(t, 201).str("id")
	}
	a := mk("A")
	b := mk("B")

	// B.fallback = A (ok).
	e.do(t, "PATCH", "/v1/admin/pools/"+b, map[string]any{"fallback_pool_id": a}).requireStatus(t, 200)
	// A.fallback = B would form A→B→A => 422 fallback_cycle.
	r := e.do(t, "PATCH", "/v1/admin/pools/"+a, map[string]any{"fallback_pool_id": b}).requireStatus(t, 422)
	if r.errorCode() != codeUnprocessable || r.errorDetailIssue("fallback_pool_id") != issueForbiddenValue {
		t.Fatalf("cycle: code=%s issue=%s, want unprocessable/forbidden_value", r.errorCode(), r.errorDetailIssue("fallback_pool_id"))
	}
	// A.fallback_pool_id unchanged in the DB (still null).
	var fb *string
	if err := e.admin.QueryRow(bgCtx(), "SELECT fallback_pool_id FROM send_managed.pools WHERE id=$1", a).Scan(&fb); err != nil {
		t.Fatalf("read A fallback: %v", err)
	}
	if fb != nil {
		t.Fatalf("A.fallback_pool_id = %v after rejected cycle, want null", *fb)
	}
	// Self-loop rejected (422, translated from the Phase 1 CHECK backstop or the app pre-check).
	rs := e.do(t, "PATCH", "/v1/admin/pools/"+a, map[string]any{"fallback_pool_id": a}).requireStatus(t, 422)
	assertNoRawDBLeak(t, rs.raw)
}

// TestT13_SharedPoolRequiresSystemUser — shared-pool create fails 409 when the reserved system
// user is NOT provisioned; dedicated still works; once provisioned, shared succeeds.
func TestT13_SharedPoolRequiresSystemUser(t *testing.T) {
	e := newTestEnv(t, "t13")
	// A) unprovisioned: shared create => 409 (internal reason system_user_not_provisioned).
	r := e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "shared", "name": "s"}).requireStatus(t, 409)
	if r.errorCode() != codeConflict {
		t.Fatalf("unprovisioned shared: code=%s, want conflict", r.errorCode())
	}
	// dedicated with a real owner still succeeds.
	owner := e.seedUser(t, "ded-t13@example.com")
	e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "dedicated", "name": "d", "owner_user_id": owner}).requireStatus(t, 201)

	// B) provisioned: shared create => 201, user_id = SystemUserID; account inherits it.
	e.seedSystemUser(t)
	sr := e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "shared", "name": "s2"}).requireStatus(t, 201)
	if sr.str("user_id") != SystemUserID {
		t.Fatalf("shared user_id = %q, want %q", sr.str("user_id"), SystemUserID)
	}
	ar := e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": sr.str("id"), "smtp_host": "smtp.example.com", "smtp_username": "u@example.com",
		"smtp_secret": "x", "from_address": "s@example.com",
	}).requireStatus(t, 201)
	if ar.str("user_id") != SystemUserID {
		t.Fatalf("shared-pool account user_id = %q, want %q", ar.str("user_id"), SystemUserID)
	}
}

// TestT10_ConfigValueTypeValidation — value_type + semantic validation + round-trip + freshness.
func TestT10_ConfigValueTypeValidation(t *testing.T) {
	e := newTestEnv(t, "t10")

	// 1) valid integer update round-trips; raw row is {"v":750}.
	e.do(t, "PUT", "/v1/admin/config/daily_limit_default", map[string]any{"value": 750}).requireStatus(t, 200)
	g := e.do(t, "GET", "/v1/admin/config/daily_limit_default", nil).requireStatus(t, 200)
	if v, _ := g.body["value"].(float64); v != 750 {
		t.Fatalf("config value after update = %v, want 750", g.body["value"])
	}
	var rawVal string
	if err := e.admin.QueryRow(bgCtx(), "SELECT value::text FROM send_managed.config WHERE key='daily_limit_default'").Scan(&rawVal); err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	if !strings.Contains(strings.ReplaceAll(rawVal, " ", ""), `"v":750`) {
		t.Fatalf("raw config value = %q, want {\"v\":750}", rawVal)
	}

	// 2) wrong type => 400, stored value unchanged (still 750).
	e.do(t, "PUT", "/v1/admin/config/daily_limit_default", map[string]any{"value": "lots"}).requireStatus(t, 400)
	g2 := e.do(t, "GET", "/v1/admin/config/daily_limit_default", nil).requireStatus(t, 200)
	if v, _ := g2.body["value"].(float64); v != 750 {
		t.Fatalf("config value after bad update = %v, want unchanged 750", g2.body["value"])
	}

	// 3) invalid IANA tz => 400 forbidden_value; valid tz => 200.
	rb := e.do(t, "PUT", "/v1/admin/config/daily_reset_timezone", map[string]any{"value": "Mars/Phobos"}).requireStatus(t, 400)
	if rb.errorDetailIssue("value") != issueForbiddenValue {
		t.Fatalf("bad tz: issue=%s, want forbidden_value", rb.errorDetailIssue("value"))
	}
	e.do(t, "PUT", "/v1/admin/config/daily_reset_timezone", map[string]any{"value": "America/New_York"}).requireStatus(t, 200)

	// 4) out-of-range hour => 400 out_of_range.
	rh := e.do(t, "PUT", "/v1/admin/config/daily_reset_hour", map[string]any{"value": 26}).requireStatus(t, 400)
	if rh.errorDetailIssue("value") != issueOutOfRange {
		t.Fatalf("hour 26: issue=%s, want out_of_range", rh.errorDetailIssue("value"))
	}

	// 5) unknown key => 404.
	e.do(t, "PUT", "/v1/admin/config/unknown_key", map[string]any{"value": 1}).requireStatus(t, 404)

	// 6) freshness: a fresh GET returns the updated value (write is durable + visible w/o restart).
	g6 := e.do(t, "GET", "/v1/admin/config/daily_limit_default", nil).requireStatus(t, 200)
	if v, _ := g6.body["value"].(float64); v != 750 {
		t.Fatalf("freshness read = %v, want 750", g6.body["value"])
	}
	// list returns items.
	lst := e.do(t, "GET", "/v1/admin/config", nil).requireStatus(t, 200)
	if _, ok := lst.body["items"].([]any); !ok {
		t.Fatal("config list missing items array")
	}
}

// assertNoRawDBLeak fails if a response body contains a raw SQLSTATE / DB-internal string.
func assertNoRawDBLeak(t *testing.T, raw []byte) {
	t.Helper()
	low := strings.ToLower(string(raw))
	for _, banned := range []string{"23503", "23505", "23514", "42501", "sqlstate", "foreign key", "unique constraint", "check constraint", "violates"} {
		if strings.Contains(low, banned) {
			t.Fatalf("response leaked raw DB detail %q: %s", banned, string(raw))
		}
	}
}
