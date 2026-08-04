package adminapi

import (
	"testing"
)

// Regression tests for the Phase 2 code-review findings (commit "review fixes").

// TestPatchSmtpPortNullIsCleanReject — regression for the CRITICAL nil-pointer panic: a PATCH
// with `smtp_port: null` (present-but-null) must return a clean 400 validation_failed with a
// malformed detail — NOT crash the request goroutine with a bare connection reset. Because the
// httptest client would surface a panic-induced reset as a transport error, a successful HTTP
// round trip returning 400 also proves the panic is gone.
func TestPatchSmtpPortNullIsCleanReject(t *testing.T) {
	e := newTestEnv(t, "portnull")
	p := e.mkSharedPool(t, "P")
	acc := e.mkAccount(t, p, "h.example.com", "u@example.com")

	// smtp_port: null → 400 malformed (mirrors the smtp_secret null/empty handling).
	r := e.doRawBody(t, "PATCH", "/v1/admin/accounts/"+acc, `{"smtp_port": null}`).requireStatus(t, 400)
	if r.errorCode() != codeValidationFailed {
		t.Fatalf("smtp_port null: code=%s, want validation_failed", r.errorCode())
	}
	if r.errorDetailIssue("smtp_port") != issueMalformed {
		t.Fatalf("smtp_port null: issue=%s, want malformed", r.errorDetailIssue("smtp_port"))
	}
	// The account is unchanged (still port 587) and the server is still healthy for the next call.
	g := e.do(t, "GET", "/v1/admin/accounts/"+acc, nil).requireStatus(t, 200)
	if v, _ := g.body["smtp_port"].(float64); v != 587 {
		t.Fatalf("smtp_port = %v after rejected null PATCH, want 587 unchanged", g.body["smtp_port"])
	}
	// A valid port PATCH still works (anti-vacuity: the reject path didn't wedge the account).
	e.do(t, "PATCH", "/v1/admin/accounts/"+acc, map[string]any{"smtp_port": 2525}).requireStatus(t, 200)
}

// TestSmtpHostRejectsPortSuffix — regression for the hostRe LOW finding: a "host:port" string in
// smtp_host is rejected (§3 "no scheme/port/path"); a bare hostname and a bare IPv6 literal are
// accepted.
func TestSmtpHostRejectsPortSuffix(t *testing.T) {
	e := newTestEnv(t, "hostport")
	p := e.mkSharedPool(t, "P")

	// host with :port → 400 malformed.
	bad := e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": p, "smtp_host": "smtp.example.com:2525", "smtp_username": "u1@example.com",
		"smtp_secret": "s", "from_address": "s@example.com",
	}).requireStatus(t, 400)
	if bad.errorDetailIssue("smtp_host") != issueMalformed {
		t.Fatalf("host:port: issue=%s, want malformed", bad.errorDetailIssue("smtp_host"))
	}
	// scheme in host → 400.
	e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": p, "smtp_host": "https://smtp.example.com", "smtp_username": "u2@example.com",
		"smtp_secret": "s", "from_address": "s@example.com",
	}).requireStatus(t, 400)
	// bare hostname → 201 (positive control).
	e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": p, "smtp_host": "smtp.example.com", "smtp_username": "u3@example.com",
		"smtp_secret": "s", "from_address": "s@example.com",
	}).requireStatus(t, 201)
	// bare IPv6 literal → 201 (colons allowed only for a valid IP literal).
	e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": p, "smtp_host": "2001:db8::1", "smtp_username": "u4@example.com",
		"smtp_secret": "s", "from_address": "s@example.com",
	}).requireStatus(t, 201)
}

// TestDailyLimitSanityCeiling — OD-24 (ruling option (c)): daily_limit is bounded by a generous
// hardcoded sanity ceiling (DailyLimitCeiling), a fat-finger guard, not a config dial. Above the
// ceiling → clean 400 out_of_range (never a raw DB error); a realistic high value (50,000) is
// accepted; the >0 lower bound still rejects 0. Applies on account create, account PATCH, and the
// daily_limit_default config write. Anti-vacuity: each rejection is paired with an accepted control.
func TestDailyLimitSanityCeiling(t *testing.T) {
	e := newTestEnv(t, "dlceiling")
	p := e.mkSharedPool(t, "P")

	// --- create: above ceiling rejected, realistic high value accepted, 0 rejected ---
	over := DailyLimitCeiling + 1
	rc := e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": p, "smtp_host": "h1.example.com", "smtp_username": "over@example.com",
		"smtp_secret": "s", "from_address": "s@example.com", "daily_limit": over,
	}).requireStatus(t, 400)
	if rc.errorCode() != codeValidationFailed || rc.errorDetailIssue("daily_limit") != issueOutOfRange {
		t.Fatalf("create over-ceiling: code=%s issue=%s, want validation_failed/out_of_range", rc.errorCode(), rc.errorDetailIssue("daily_limit"))
	}
	assertNoRawDBLeak(t, rc.raw)
	// realistic high value (50k) accepted — comfortably above every real provider mailbox limit.
	acc := e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": p, "smtp_host": "h2.example.com", "smtp_username": "ok@example.com",
		"smtp_secret": "s", "from_address": "s@example.com", "daily_limit": 50_000,
	}).requireStatus(t, 201).str("id")
	// 0 still rejected (lower bound intact).
	e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": p, "smtp_host": "h3.example.com", "smtp_username": "zero@example.com",
		"smtp_secret": "s", "from_address": "s@example.com", "daily_limit": 0,
	}).requireStatus(t, 400)

	// --- PATCH: above ceiling rejected (bytes/limit unchanged), at-ceiling accepted ---
	rp := e.do(t, "PATCH", "/v1/admin/accounts/"+acc, map[string]any{"daily_limit": over}).requireStatus(t, 400)
	if rp.errorDetailIssue("daily_limit") != issueOutOfRange {
		t.Fatalf("patch over-ceiling: issue=%s, want out_of_range", rp.errorDetailIssue("daily_limit"))
	}
	g := e.do(t, "GET", "/v1/admin/accounts/"+acc, nil).requireStatus(t, 200)
	if v, _ := g.body["daily_limit"].(float64); v != 50_000 {
		t.Fatalf("daily_limit = %v after rejected over-ceiling PATCH, want 50000 unchanged", g.body["daily_limit"])
	}
	e.do(t, "PATCH", "/v1/admin/accounts/"+acc, map[string]any{"daily_limit": DailyLimitCeiling}).requireStatus(t, 200)

	// --- config daily_limit_default: above ceiling rejected, realistic value accepted ---
	rcfg := e.do(t, "PUT", "/v1/admin/config/daily_limit_default", map[string]any{"value": over}).requireStatus(t, 400)
	if rcfg.errorDetailIssue("value") != issueOutOfRange {
		t.Fatalf("config over-ceiling: issue=%s, want out_of_range", rcfg.errorDetailIssue("value"))
	}
	e.do(t, "PUT", "/v1/admin/config/daily_limit_default", map[string]any{"value": 25_000}).requireStatus(t, 200)
}

// TestPoolImmutableFieldsRejected — regression for the dead-code LOW: PATCH /pools with kind or
// owner_user_id now returns the informative 422 forbidden_value (the fields are declared in
// poolUpdateReq so strict-decode does not blanket-400 them first).
func TestPoolImmutableFieldsRejected(t *testing.T) {
	e := newTestEnv(t, "poolimmut")
	owner := e.seedUser(t, "owner-immut@example.com")
	pid := e.do(t, "POST", "/v1/admin/pools", map[string]any{
		"kind": "dedicated", "name": "d", "owner_user_id": owner,
	}).requireStatus(t, 201).str("id")

	rk := e.do(t, "PATCH", "/v1/admin/pools/"+pid, map[string]any{"kind": "shared"}).requireStatus(t, 422)
	if rk.errorCode() != codeUnprocessable || rk.errorDetailIssue("kind") != issueForbiddenValue {
		t.Fatalf("kind immutable: code=%s issue=%s, want unprocessable/forbidden_value", rk.errorCode(), rk.errorDetailIssue("kind"))
	}
	ro := e.do(t, "PATCH", "/v1/admin/pools/"+pid, map[string]any{"owner_user_id": owner}).requireStatus(t, 422)
	if ro.errorDetailIssue("owner_user_id") != issueForbiddenValue {
		t.Fatalf("owner_user_id immutable: issue=%s, want forbidden_value", ro.errorDetailIssue("owner_user_id"))
	}
	// A legitimate PATCH (name) still succeeds — the immutable-field guard didn't over-reject.
	e.do(t, "PATCH", "/v1/admin/pools/"+pid, map[string]any{"name": "renamed"}).requireStatus(t, 200)
}
