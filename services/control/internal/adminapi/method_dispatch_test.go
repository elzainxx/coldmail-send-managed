package adminapi

// Behavioral tests for fix-plan item 7b (audit D17) on the OPERATOR surface: a mux-level method
// mismatch or unmatched /v1/admin/* path must leave through the SAME error envelope as every other
// failure, with a request_id.
//
// This surface is where the WILDCARD-BINDING property can actually be proved. platformapi's four
// routes are all literals; twelve routes here carry {id} and three carry {key}, so this is the only
// place a test can catch the trap the design was verified against: serving the handler that
// mux.Handler(r) returns loses r.PathValue, leaving every {id} route with an empty id. A wrapper
// with that bug would pass every envelope-shape assertion in this file and quietly break account
// lookups.
//
// Real Server, real Postgres clone, real HTTP (CLAUDE.md Rule 3) — the same harness as every other
// test in this package.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMethodDispatch_WrongVerbIsACodedEnvelopeNotPlainText is the flagship control. Before item 7b
// a wrong verb on a real admin route produced `Method Not Allowed\n` as text/plain with no code and
// no request_id — so an operator tool that reads error.code on every non-2xx nil-deref'd, and there
// was no id to correlate with the server's own logs.
func TestMethodDispatch_WrongVerbIsACodedEnvelopeNotPlainText(t *testing.T) {
	e := newTestEnv(t, "mdwrongverb")

	// DELETE on a collection that serves only POST/GET.
	r := e.doWithHeaders(t, "DELETE", "/v1/admin/pools")
	if r.status != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /v1/admin/pools: status=%d want 405; body=%s", r.status, string(r.raw))
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type=%q want application/json — an operator tool parsing JSON on every "+
			"error gets a parse failure instead of an error it can branch on", ct)
	}
	if r.errorCode() != codeMethodNotAllowed {
		t.Fatalf("error.code=%q want %q; body=%s", r.errorCode(), codeMethodNotAllowed, string(r.raw))
	}
	if r.errorRequestID() == "" {
		t.Fatalf("no request_id — the contract says it is mandatory on EVERY error; body=%s", string(r.raw))
	}
	if allow := r.header.Get("Allow"); allow != "GET, HEAD, POST" {
		t.Fatalf("Allow=%q want %q", allow, "GET, HEAD, POST")
	}
	if strings.Contains(string(r.raw), "Method Not Allowed") {
		t.Fatalf("body still carries ServeMux's plain text: %s", string(r.raw))
	}
	// Two-vocabulary rule: the internal reason is logged, never serialized.
	if strings.Contains(string(r.raw), "mux_method_not_allowed") {
		t.Fatalf("body leaks the internal reason: %s", string(r.raw))
	}
}

// TestMethodDispatch_EveryRouteRejectsAWrongVerbThroughTheEnvelope walks the WHOLE route table, so a
// route added later is covered without editing this test. 21 routes over 11 distinct paths — the
// per-path method sets here are genuinely varied (GET+POST, GET+PATCH+DELETE, GET+PUT, POST-only),
// which is what makes the derived-Allow assertion meaningful rather than a restatement of one case.
func TestMethodDispatch_EveryRouteRejectsAWrongVerbThroughTheEnvelope(t *testing.T) {
	e := newTestEnv(t, "mdallroutes")
	routes := (&Server{}).adminRoutes()
	if len(routes) == 0 {
		t.Fatal("adminRoutes() is empty — this test would be vacuous")
	}

	served := map[string]map[string]bool{}
	for _, rt := range routes {
		if served[rt.pattern] == nil {
			served[rt.pattern] = map[string]bool{}
		}
		served[rt.pattern][rt.method] = true
		if rt.method == http.MethodGet {
			served[rt.pattern][http.MethodHead] = true // ServeMux answers HEAD from GET
		}
	}

	for pattern, methods := range served {
		var wrong string
		for _, m := range []string{"OPTIONS", http.MethodDelete, http.MethodPut, http.MethodPatch, http.MethodPost, http.MethodGet} {
			if !methods[m] {
				wrong = m
				break
			}
		}
		if wrong == "" {
			t.Fatalf("%s serves every probed method — cannot test a wrong verb on it", pattern)
		}
		// Substitute a concrete value for each wildcard so the request reaches the pattern at all.
		concrete := fillWildcards(pattern)
		r := e.doWithHeaders(t, wrong, concrete)
		if r.status != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status=%d want 405; body=%s", wrong, concrete, r.status, string(r.raw))
		}
		if r.errorCode() != codeMethodNotAllowed {
			t.Fatalf("%s %s: code=%q want %q", wrong, concrete, r.errorCode(), codeMethodNotAllowed)
		}
		if r.errorRequestID() == "" {
			t.Fatalf("%s %s: no request_id; body=%s", wrong, concrete, string(r.raw))
		}
		want := make([]string, 0, len(methods))
		for m := range methods {
			want = append(want, m)
		}
		if got, wantJoined := splitAndSort(r.header.Get("Allow")), strings.Join(sorted(want), ","); strings.Join(got, ",") != wantJoined {
			t.Fatalf("%s: Allow=%q want the served set %v", pattern, r.header.Get("Allow"), sorted(want))
		}
	}
}

// TestMethodDispatch_UnmatchedAdminPathIsACodedEnvelope covers the 404 half.
func TestMethodDispatch_UnmatchedAdminPathIsACodedEnvelope(t *testing.T) {
	e := newTestEnv(t, "mdnopath")
	for _, path := range []string{
		"/v1/admin/nope",
		"/v1/admin/accounts/abc/nope",    // a real prefix plus an unknown leaf
		"/v1/admin/pools/abc/quarantine", // a real ACTION on the wrong resource type
		"/v1/admin/CONFIG",               // case matters
		"/v1/admin/",
	} {
		r := e.doWithHeaders(t, "GET", path)
		if r.status != http.StatusNotFound {
			t.Fatalf("GET %s: status=%d want 404; body=%s", path, r.status, string(r.raw))
		}
		if r.errorCode() != codeNotFound {
			t.Fatalf("GET %s: code=%q want %q; body=%s", path, r.errorCode(), codeNotFound, string(r.raw))
		}
		if r.errorRequestID() == "" {
			t.Fatalf("GET %s: no request_id; body=%s", path, string(r.raw))
		}
		if strings.Contains(string(r.raw), "page not found") {
			t.Fatalf("GET %s body still carries ServeMux's plain text: %s", path, string(r.raw))
		}
		if allow := r.header.Get("Allow"); allow != "" {
			t.Fatalf("GET %s: 404 carries Allow=%q — implies the path exists", path, allow)
		}
	}
}

// TestMethodDispatch_405AndUnknownPath404AreDistinguishable — a wrapper answering 404 for both would
// satisfy every envelope-shape assertion above and still be useless to the caller.
func TestMethodDispatch_405AndUnknownPath404AreDistinguishable(t *testing.T) {
	e := newTestEnv(t, "mddistinct")
	wrongVerb := e.doWithHeaders(t, "DELETE", "/v1/admin/config")
	noSuchPath := e.doWithHeaders(t, "DELETE", "/v1/admin/configx")
	if wrongVerb.status == noSuchPath.status {
		t.Fatalf("wrong verb and unknown path both answer %d — indistinguishable", wrongVerb.status)
	}
	if wrongVerb.status != http.StatusMethodNotAllowed || noSuchPath.status != http.StatusNotFound {
		t.Fatalf("wrongVerb=%d (want 405), noSuchPath=%d (want 404)", wrongVerb.status, noSuchPath.status)
	}
	if wrongVerb.errorCode() == noSuchPath.errorCode() {
		t.Fatalf("both answer code=%q — a client switching on code cannot tell them apart", wrongVerb.errorCode())
	}
}

// TestMethodDispatch_WildcardsStillBindThroughTheWrapper is THE regression guard for the empirically
// verified trap: `h, _ := mux.Handler(r); h.ServeHTTP(w, r)` loses r.PathValue — the returned handler
// is not bound to the matched pattern's wildcards, so {id} arrives as "". Serving through
// mux.ServeHTTP preserves it.
//
// Proved end-to-end on real data: create an account, then GET it by id. If the wildcard did not
// bind, the handler would look up the empty string and answer 400/404 instead of returning the row.
// A wrapper with that bug passes every other test in this file.
func TestMethodDispatch_WildcardsStillBindThroughTheWrapper(t *testing.T) {
	e := newTestEnv(t, "mdwildcard")
	pool := e.mkSharedPool(t, "WildcardPool")
	acct := e.mkAccount(t, pool, "wc.example.com", "wc@example.com")

	// {id} on a GET.
	got := e.do(t, "GET", "/v1/admin/accounts/"+acct, nil).requireStatus(t, 200)
	if got.str("id") != acct {
		t.Fatalf("GET /v1/admin/accounts/{id} returned id=%q want %q — the {id} wildcard did not "+
			"bind through the wrapper (r.PathValue was empty)", got.str("id"), acct)
	}
	// {id} on a PATCH (a different method through the same wrapper).
	e.do(t, "PATCH", "/v1/admin/accounts/"+acct, map[string]any{"daily_limit": 7}).requireStatus(t, 200)
	after := e.do(t, "GET", "/v1/admin/accounts/"+acct, nil).requireStatus(t, 200)
	if v, _ := after.body["daily_limit"].(float64); int(v) != 7 {
		t.Fatalf("PATCH by {id} did not apply (daily_limit=%v) — the wildcard did not bind", after.body["daily_limit"])
	}
	// {id} on a two-segment pattern with a literal tail.
	e.do(t, "GET", "/v1/admin/accounts/"+acct+"/events", nil).requireStatus(t, 200)
	// {id} on a POST action.
	e.do(t, "POST", "/v1/admin/accounts/"+acct+"/quarantine", map[string]any{"reason": "test"}).requireStatus(t, 200)
	// {key} on a config GET — a DIFFERENT wildcard name, so this is not the same code path by luck.
	cfg := e.do(t, "GET", "/v1/admin/config/daily_reset_hour", nil).requireStatus(t, 200)
	if cfg.str("key") != "daily_reset_hour" {
		t.Fatalf("GET /v1/admin/config/{key} returned key=%q — the {key} wildcard did not bind", cfg.str("key"))
	}
}

// TestMethodDispatch_AWrongVerbOnAWildcardRouteReportsThePATTERNNotThePath is the disclosure check on
// the 405 path: the internal log reason names the matched PATTERN ("/v1/admin/accounts/{id}"), which
// is route metadata, while the caller-supplied id must not be echoed into the response body at all.
func TestMethodDispatch_AWrongVerbOnAWildcardRouteReportsThePATTERNNotThePath(t *testing.T) {
	e := newTestEnv(t, "mdnoecho")
	marker := "a1b2c3d4-dead-beef-cafe-000000000001"
	r := e.doWithHeaders(t, "PUT", "/v1/admin/accounts/"+marker)
	if r.status != http.StatusMethodNotAllowed {
		t.Fatalf("PUT on an {id} route: status=%d want 405; body=%s", r.status, string(r.raw))
	}
	if strings.Contains(string(r.raw), marker) {
		t.Fatalf("the 405 body echoes the caller-supplied id back: %s", string(r.raw))
	}
	if r.errorMessage() != fixedMessages[codeMethodNotAllowed] {
		t.Fatalf("message=%q want the fixed sentence %q — an interpolated message is how untrusted "+
			"input reaches a client-visible string", r.errorMessage(), fixedMessages[codeMethodNotAllowed])
	}
}

// TestMethodDispatch_RequestIDIsUniqueAndCorrelatesToALogLine: the request_id must be the SAME id in
// the body and in the log line, or it cannot do the one job it exists for. The plain-text 405 had
// neither.
func TestMethodDispatch_RequestIDIsUniqueAndCorrelatesToALogLine(t *testing.T) {
	e := newTestEnv(t, "mdrid")
	ridRe := regexp.MustCompile(`^req_[0-9a-f]{32}$`)
	seen := map[string]bool{}
	for _, c := range [][2]string{
		{"DELETE", "/v1/admin/config"},
		{"PUT", "/v1/admin/pools"},
		{"GET", "/v1/admin/nope"},
		{"POST", "/v1/admin/nope"},
	} {
		r := e.doWithHeaders(t, c[0], c[1])
		rid := r.errorRequestID()
		if !ridRe.MatchString(rid) {
			t.Fatalf("%s %s: request_id=%q does not match %s", c[0], c[1], rid, ridRe)
		}
		if seen[rid] {
			t.Fatalf("%s %s: request_id %q reissued — two different failures would correlate to one "+
				"log line", c[0], c[1], rid)
		}
		seen[rid] = true
		if !strings.Contains(e.logBuf.String(), "request_id="+rid) {
			t.Fatalf("%s %s: request_id %q is in the body but NOT in any log line — the id an "+
				"operator is handed leads nowhere", c[0], c[1], rid)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("collected %d distinct request_ids from 4 requests", len(seen))
	}
}

// TestMethodDispatch_ShapeMuxCoversExactlyTheServedPaths — structural anti-drift, both directions.
func TestMethodDispatch_ShapeMuxCoversExactlyTheServedPaths(t *testing.T) {
	routes := (&Server{}).adminRoutes()
	_, allow := buildShapeMux(routes)
	if len(allow) == 0 {
		t.Fatal("buildShapeMux produced no paths — vacuous")
	}
	wantPaths := map[string]bool{}
	for _, rt := range routes {
		wantPaths[rt.pattern] = true
	}
	if len(allow) != len(wantPaths) {
		t.Fatalf("shape mux covers %d paths, the route table has %d distinct patterns", len(allow), len(wantPaths))
	}
	for p := range wantPaths {
		if allow[p] == "" {
			t.Fatalf("route table serves %s but the shape mux has no Allow for it — a wrong verb "+
				"there would answer 404 instead of 405", p)
		}
	}
	for p := range allow {
		if !wantPaths[p] {
			t.Fatalf("shape mux carries %s, which is not served — a wrong verb there would answer "+
				"405, implying a route that does not exist", p)
		}
	}
}

// TestMethodDispatch_EveryTableRouteIsAuthenticated is the property the route-table refactor must
// not have broken. Before item 7b these were 21 individual HandleFunc calls each wrapping
// requireAuth by hand; converting them to a table introduced the possibility of a route registered
// without the gate. Nothing but routes() registers a route, so this asserts the loop.
func TestMethodDispatch_EveryTableRouteIsAuthenticated(t *testing.T) {
	e := newTestEnv(t, "mdauth")
	routes := (&Server{}).adminRoutes()
	if len(routes) != 21 {
		t.Fatalf("adminRoutes() has %d routes, expected 21 — if a route was added, extend "+
			"Docs/api-contract.md and the contract_drift gate too, then update this count", len(routes))
	}
	for _, rt := range routes {
		concrete := fillWildcards(rt.pattern)
		r := e.doRaw(t, rt.method, concrete, nil, "") // NO bearer at all
		if r.status != http.StatusUnauthorized {
			t.Fatalf("%s %s without a key: status=%d want 401 — this route is not behind requireAuth; body=%s",
				rt.method, concrete, r.status, string(r.raw))
		}
		if r.errorCode() != codeUnauthenticated {
			t.Fatalf("%s %s: code=%q want %q", rt.method, concrete, r.errorCode(), codeUnauthenticated)
		}
	}
}

// TestMethodDispatch_HandlerIsTheWrapperNotTheBareMux — returning s.mux would restore the plain-text
// 405/404 with every route still working, so nothing would look broken.
func TestMethodDispatch_HandlerIsTheWrapperNotTheBareMux(t *testing.T) {
	srv := &Server{auth: NewAuthenticator(nil), mux: http.NewServeMux()}
	srv.routes()
	if srv.handler == nil {
		t.Fatal("routes() left s.handler nil — Handler() would return a nil handler")
	}
	if _, isBareMux := srv.Handler().(*http.ServeMux); isBareMux {
		t.Fatal("Handler() returns the bare *http.ServeMux, which answers 405/404 itself as plain " +
			"text — exactly the defect item 7b fixes, and invisible without this assertion")
	}
}

// TestSanitizePath_NeutralizesControlCharacters — the log-forgery hole. This surface is where it was
// REPRODUCED: {key} matches whatever a caller puts in that segment, r.URL.Path is DECODED, and every
// log line is one Printf.
func TestSanitizePath_NeutralizesControlCharacters(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/v1/admin/config", "/v1/admin/config"},
		{"/v1/admin/config/x?y=1", "/v1/admin/config/x"},
		{"/v1/admin/config/x\nadmin request_id=FORGED level=error", "/v1/admin/config/x?admin request_id=FORGED level=error"},
		{"/v1/admin/x\r\ny", "/v1/admin/x??y"},
		{"/v1/admin/x\ty", "/v1/admin/x?y"},
		{"/v1/admin/x\x00y", "/v1/admin/x?y"},
		{"/v1/admin/x\x7fy", "/v1/admin/x?y"},
	}
	for _, c := range cases {
		got := sanitizePath(c.in)
		if got != c.want {
			t.Fatalf("sanitizePath(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(got, "\r\n") {
			t.Fatalf("sanitizePath(%q) = %q still contains a line break", c.in, got)
		}
	}
}

// TestSanitizePath_ForgedPathCannotSplitALogLine is the end-to-end control: only a real request
// proves the value that REACHES the log went through sanitizePath. Uses a wildcard route, which is
// how the hole was found — and is reachable without a valid key, since the reject log line runs too.
func TestSanitizePath_ForgedPathCannotSplitALogLine(t *testing.T) {
	e := newTestEnv(t, "mdlogforge")
	before := len(strings.Split(strings.TrimRight(e.logBuf.String(), "\n"), "\n"))

	req, err := http.NewRequest("GET", e.baseURL+
		"/v1/admin/config/x%0Aadmin%20request_id=FORGED%20outcome=error%20code=internal_error%20level=error", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.adminKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = res.Body.Close()

	out := strings.TrimRight(e.logBuf.String(), "\n")
	if out == "" {
		t.Fatal("nothing was logged — this test would pass vacuously")
	}
	added := strings.Split(out, "\n")[before:]
	if len(added) != 1 {
		t.Fatalf("one request produced %d log lines:\n%s\nA decoded %%0A split the line; the "+
			"remainder is byte-indistinguishable from a genuine entry with an attacker-chosen "+
			"request_id.", len(added), strings.Join(added, "\n"))
	}
	if strings.Contains(added[0], "\nadmin ") {
		t.Fatalf("forged text survived unneutralized: %q", added[0])
	}
}

// TestSanitizePath_IsUsedByEveryLogSiteThatLogsAPath — coverage guard: a NEW log line formatting the
// request path directly would reintroduce the hole while the tests above stayed green. Walks the AST
// (mode 0 drops comments, so sanitizePath's own doc comment — which must name r.URL.Path to explain
// the defect — is not mistaken for code).
func TestSanitizePath_IsUsedByEveryLogSiteThatLogsAPath(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "sanitizePath" {
					return false
				}
			}
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Path" {
				return true
			}
			inner, ok := sel.X.(*ast.SelectorExpr)
			if !ok || inner.Sel.Name != "URL" {
				return true
			}
			pos := fset.Position(sel.Pos())
			t.Errorf("%s:%d reads .URL.Path outside sanitizePath.\n"+
				"If it reaches a log line, a decoded %%0A splits it and forges a second entry with "+
				"an attacker-chosen request_id. Wrap it in sanitizePath.", pos.Filename, pos.Line)
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no production files scanned — vacuous")
	}
}

// --- helpers ---

// respWithHeaders is a resp plus the response headers, which the shared client discards. The 405
// tests need Allow and Content-Type.
type respWithHeaders struct {
	resp
	header http.Header
}

// doWithHeaders issues an authenticated request with NO body, keeping the response headers. The
// bearer is set so a 405/404 is not explained away as an auth failure — the point is that the mux
// answers BEFORE auth runs.
func (e *testEnv) doWithHeaders(t *testing.T, method, path string) respWithHeaders {
	t.Helper()
	req, err := http.NewRequest(method, e.baseURL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.adminKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := resp{status: res.StatusCode, raw: raw}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &r.body)
	}
	return respWithHeaders{resp: r, header: res.Header}
}

// fillWildcards substitutes a concrete value for each {…} segment so a request can reach the
// pattern. The values are syntactically valid but refer to nothing — every assertion using this is
// about ROUTING (405/404/401), which is decided before any handler looks anything up.
func fillWildcards(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i, s := range segs {
		if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
			continue
		}
		if s == "{key}" {
			segs[i] = "daily_reset_hour" // a real config key; {id} needs no such care
			continue
		}
		segs[i] = "00000000-0000-0000-0000-000000000000"
	}
	return strings.Join(segs, "/")
}

func splitAndSort(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return sorted(parts)
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
