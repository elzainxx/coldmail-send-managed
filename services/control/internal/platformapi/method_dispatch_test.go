package platformapi

// Behavioral tests for fix-plan item 7b (audit D17): a mux-level method mismatch or unmatched path
// must leave through the SAME error envelope as every other failure, with a request_id.
//
// These drive the ACTUAL production handler over REAL HTTP against a REAL Postgres clone (CLAUDE.md
// Rule 3, the same harness every other test in this package uses). The mux behaviour under test is
// net/http's, so a unit test against a hand-built mux would prove nothing about what this module
// SERVES — the wiring (Handler() returning the wrapper rather than the bare mux) is the part that
// can regress, and only an HTTP-level test sees it.

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMethodDispatch_WrongVerbIsACodedEnvelopeNotPlainText is the flagship control. Before item 7b
// this returned `Method Not Allowed\n` as text/plain with no code and no request_id, so a client
// doing body.error.code on a non-2xx nil-deref'd.
func TestMethodDispatch_WrongVerbIsACodedEnvelopeNotPlainText(t *testing.T) {
	e := newTestEnv(t, "mdwrongverb")

	// GET on POST-only /v1/quote. Every route is probed with a wrong verb below; this case is
	// spelled out on its own because it is the exact request the audit's D17 reproduction used.
	r := e.doRaw(t, "GET", "/v1/quote")
	if r.status != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/quote: status=%d want 405; body=%s", r.status, r.raw)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("GET /v1/quote: Content-Type=%q want application/json — a client parsing JSON on "+
			"every error gets a parse failure instead of an error it can branch on", ct)
	}
	assertErrorEnvelope(t, r, codeMethodNotAllowed)
	if allow := r.header.Get("Allow"); allow != "POST" {
		t.Fatalf("GET /v1/quote: Allow=%q want %q — RFC 9110 requires Allow on a 405, and a client "+
			"that got here needs to be told which verb to use", allow, "POST")
	}
	// The plain-text body must be gone, not merely accompanied by JSON.
	if strings.Contains(r.raw, "Method Not Allowed") {
		t.Fatalf("GET /v1/quote body still carries ServeMux's plain text: %s", r.raw)
	}
}

// TestMethodDispatch_EveryRouteRejectsAWrongVerbThroughTheEnvelope is the anti-vacuity companion:
// the property must hold for EVERY served route, derived from the route table rather than a
// hand-written list, so a route added later is covered without editing this test.
func TestMethodDispatch_EveryRouteRejectsAWrongVerbThroughTheEnvelope(t *testing.T) {
	e := newTestEnv(t, "mdallroutes")
	srv := &Server{} // route table only; no deps touched
	routes := srv.platformRoutes()
	if len(routes) == 0 {
		t.Fatal("platformRoutes() is empty — this test would be vacuous")
	}

	// Methods each path legitimately serves, so the probe picks one it does NOT.
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

	probed := 0
	for pattern, methods := range served {
		var wrong string
		for _, m := range []string{http.MethodDelete, http.MethodPut, http.MethodPatch, http.MethodPost, http.MethodGet} {
			if !methods[m] {
				wrong = m
				break
			}
		}
		if wrong == "" {
			t.Fatalf("%s serves every method probed — cannot test a wrong verb on it", pattern)
		}
		r := e.doRaw(t, wrong, pattern)
		if r.status != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: status=%d want 405; body=%s", wrong, pattern, r.status, r.raw)
		}
		assertErrorEnvelope(t, r, codeMethodNotAllowed)
		// Allow must name exactly what the table says the path serves.
		want := make([]string, 0, len(methods))
		for m := range methods {
			want = append(want, m)
		}
		gotAllow := splitAndSort(r.header.Get("Allow"))
		if strings.Join(gotAllow, ",") != strings.Join(sorted(want), ",") {
			t.Fatalf("%s: Allow=%q want the served set %v", pattern, r.header.Get("Allow"), sorted(want))
		}
		probed++
	}
	if probed != len(served) {
		t.Fatalf("probed %d of %d paths", probed, len(served))
	}
}

// TestMethodDispatch_UnmatchedPathIsACodedEnvelope covers the 404 half.
func TestMethodDispatch_UnmatchedPathIsACodedEnvelope(t *testing.T) {
	e := newTestEnv(t, "mdnopath")
	for _, path := range []string{
		"/v1/nope",
		"/v1/quote/extra",  // a real route with a trailing segment
		"/v1/quote/",       // trailing slash: NOT the same pattern
		"/",                //
		"/v1/QUOTE",        // case matters in a path
		"/v1/describe-not", // prefix of a real route
	} {
		r := e.doRaw(t, "GET", path)
		if r.status != http.StatusNotFound {
			t.Fatalf("GET %s: status=%d want 404; body=%s", path, r.status, r.raw)
		}
		assertErrorEnvelope(t, r, codeNotFound)
		if strings.Contains(r.raw, "page not found") {
			t.Fatalf("GET %s body still carries ServeMux's plain text: %s", path, r.raw)
		}
		// A 404 must not carry Allow — there is no path whose methods it could describe.
		if allow := r.header.Get("Allow"); allow != "" {
			t.Fatalf("GET %s: 404 carries Allow=%q — implies the path exists", path, allow)
		}
	}
}

// TestMethodDispatch_405AndUnknownPath404AreDistinguishable is the property a client actually needs:
// "wrong verb" and "no such route" must be TELLABLE APART. A wrapper that answered 404 for both
// would satisfy every envelope-shape assertion above and still be useless.
func TestMethodDispatch_405AndUnknownPath404AreDistinguishable(t *testing.T) {
	e := newTestEnv(t, "mddistinct")
	wrongVerb := e.doRaw(t, "DELETE", "/v1/report")   // path exists, verb does not
	noSuchPath := e.doRaw(t, "DELETE", "/v1/reportx") // path does not exist
	if wrongVerb.status == noSuchPath.status {
		t.Fatalf("wrong verb and unknown path both answer %d — indistinguishable", wrongVerb.status)
	}
	if wrongVerb.status != http.StatusMethodNotAllowed || noSuchPath.status != http.StatusNotFound {
		t.Fatalf("wrongVerb=%d (want 405), noSuchPath=%d (want 404)", wrongVerb.status, noSuchPath.status)
	}
	if errCode(t, wrongVerb) == errCode(t, noSuchPath) {
		t.Fatalf("both answer code=%q — a client switching on code cannot tell them apart", errCode(t, wrongVerb))
	}
}

// TestMethodDispatch_EveryErrorPathCarriesAUniqueRequestID: the contract's "request_id is MANDATORY
// on every error" clause was FALSE on these two paths (no body at all, let alone an id). Unique per
// request, since a constant would correlate every failure to the same log line.
func TestMethodDispatch_EveryErrorPathCarriesAUniqueRequestID(t *testing.T) {
	e := newTestEnv(t, "mdrid")
	ridRe := regexp.MustCompile(`^req_[0-9a-f]{32}$`)
	seen := map[string]bool{}
	for _, c := range [][2]string{
		{"GET", "/v1/quote"}, {"DELETE", "/v1/quote"},
		{"POST", "/v1/report"}, {"GET", "/v1/nope"}, {"POST", "/v1/nope"},
	} {
		r := e.doRaw(t, c[0], c[1])
		rid := errField(t, r, "request_id")
		if !ridRe.MatchString(rid) {
			t.Fatalf("%s %s: request_id=%q does not match the %s shape every other surface mints",
				c[0], c[1], rid, ridRe)
		}
		if seen[rid] {
			t.Fatalf("%s %s: request_id %q was already issued — a reused id correlates two "+
				"different failures to one log line", c[0], c[1], rid)
		}
		seen[rid] = true
	}
	if len(seen) != 5 {
		t.Fatalf("collected %d distinct request_ids from 5 requests", len(seen))
	}
}

// TestMethodDispatch_MethodNotAllowedIsPublishedInDescribe ties 7b to item 8: a client building an
// exhaustive switch off describe.error_codes[] must find the new code there. codeStatus is the
// single source both writeError and describeErrorCodes() read, so this is really asserting the new
// code went into the MAP and not just into a string constant nothing resolves.
func TestMethodDispatch_MethodNotAllowedIsPublishedInDescribe(t *testing.T) {
	e := newTestEnv(t, "mddescribe")
	desc := e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 200)
	raw, ok := desc.body["error_codes"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("describe has no error_codes[]: %s", desc.raw)
	}
	found := false
	for _, item := range raw {
		m := item.(map[string]any)
		if m["code"] == codeMethodNotAllowed {
			found = true
			if int(m["http_status"].(float64)) != http.StatusMethodNotAllowed {
				t.Fatalf("describe publishes %s with http_status=%v, want 405", codeMethodNotAllowed, m["http_status"])
			}
		}
	}
	if !found {
		t.Fatalf("%s is emittable by the mux fallback but describe does not publish it: %s",
			codeMethodNotAllowed, desc.raw)
	}
}

// TestMethodDispatch_RealRoutesStillBindPathWildcards is the regression guard for the trap this
// design was verified against: calling the handler mux.Handler(r) RETURNS loses r.PathValue, so a
// wrapper that served that handler directly would leave every {id} route seeing an empty id. This
// package's four routes have no wildcards, so the property is proved where it can be: a real
// request with a real body must still reach the real handler and answer normally through the
// wrapper.
//
// The wildcard half of this property is proved in adminapi's twin test, on routes that HAVE
// wildcards ({id}, {key}).
func TestMethodDispatch_RealRoutesStillBindPathWildcards(t *testing.T) {
	e := newTestEnv(t, "mdpassthrough")
	// A genuine POST /v1/quote must behave exactly as before the wrapper existed.
	r := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": 3}).requireStatus(t, 200)
	if _, ok := r.body["cost"]; !ok {
		t.Fatalf("POST /v1/quote through the wrapper lost its response body: %s", r.raw)
	}
	// A GET with a query string still reaches the report handler (the wrapper must not eat queries),
	// and the handler's OWN validation must be what answers.
	user := e.seedUser(t, "mdpassthrough@example.com")
	rr := e.do(t, "GET", "/v1/report?user_id="+user+"&job_id=00000000-0000-0000-0000-000000000000", nil)
	if rr.status != http.StatusNotFound {
		t.Fatalf("GET /v1/report with a real query: status=%d want 404 from the HANDLER; body=%s", rr.status, rr.raw)
	}
	if rr.errorCode() != codeNotFound {
		t.Fatalf("report not-found code=%q want %q", rr.errorCode(), codeNotFound)
	}
	// The HANDLER's 404 names the selector in details[]; the mux fallback's never carries details.
	// That is what distinguishes "the route ran and found nothing" from "the wrapper shadowed it".
	obj, _ := rr.body["error"].(map[string]any)
	if _, hasDetails := obj["details"]; !hasDetails {
		t.Fatalf("report's 404 has no details[] — it came from the mux fallback, so the wrapper is "+
			"shadowing a real route: %s", rr.raw)
	}
	// And a request the handler must REJECT (missing tenant selector) still reaches it: a wrapper
	// that answered 404/405 first would turn this 400 into something else.
	bad := e.do(t, "GET", "/v1/report?job_id=00000000-0000-0000-0000-000000000000", nil)
	if bad.status != http.StatusBadRequest || bad.errorCode() != codeValidationFailed {
		t.Fatalf("GET /v1/report without user_id: status=%d code=%q, want 400/%s from the handler; body=%s",
			bad.status, bad.errorCode(), codeValidationFailed, bad.raw)
	}
}

// TestMethodDispatch_HeadOnAGetRouteIsServedNotRejected: ServeMux answers HEAD from a GET pattern,
// so HEAD must NOT be treated as a wrong verb — and Allow must say so wherever GET is served.
func TestMethodDispatch_HeadOnAGetRouteIsServedNotRejected(t *testing.T) {
	e := newTestEnv(t, "mdhead")
	r := e.doRaw(t, "HEAD", "/v1/describe")
	if r.status == http.StatusMethodNotAllowed {
		t.Fatalf("HEAD /v1/describe answered 405, but ServeMux serves HEAD from a GET pattern — "+
			"the shape mux is shadowing a route the real mux matches; body=%s", r.raw)
	}
	// And a wrong verb on that same path must advertise HEAD alongside GET.
	r2 := e.doRaw(t, "DELETE", "/v1/describe")
	if allow := r2.header.Get("Allow"); !strings.Contains(allow, "HEAD") {
		t.Fatalf("DELETE /v1/describe: Allow=%q omits HEAD, which the path does serve", allow)
	}
}

// TestMethodDispatch_ShapeMuxCoversExactlyTheServedPaths is the structural anti-drift assertion: a
// path in the shape mux but not the real mux would answer 405 for a route that does not exist; a
// path in the real mux but not the shape mux would answer 404 for a wrong verb on a route that does.
// Both are derived from the same table, and this proves the derivation rather than assuming it.
func TestMethodDispatch_ShapeMuxCoversExactlyTheServedPaths(t *testing.T) {
	srv := &Server{}
	routes := srv.platformRoutes()
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
			t.Fatalf("route table serves %s but the shape mux has no Allow value for it — a wrong "+
				"verb there would answer 404 (no such path) instead of 405", p)
		}
	}
	for p := range allow {
		if !wantPaths[p] {
			t.Fatalf("shape mux carries %s, which the route table does not serve — a wrong verb "+
				"there would answer 405, implying a route that does not exist", p)
		}
	}
}

// TestMethodDispatch_HandlerIsTheWrapperNotTheBareMux: Handler() returning s.mux would silently
// restore the plain-text 405 with every route still working. Asserted through behaviour rather than
// by comparing pointers, because the wrapper is the only thing that can answer a 405 as JSON.
func TestMethodDispatch_HandlerIsTheWrapperNotTheBareMux(t *testing.T) {
	srv := &Server{auth: NewAuthenticator(nil), mux: http.NewServeMux()}
	srv.routes()
	if srv.handler == nil {
		t.Fatal("routes() left s.handler nil — Handler() would return a nil handler and every " +
			"request would panic")
	}
	if _, isBareMux := srv.Handler().(*http.ServeMux); isBareMux {
		t.Fatal("Handler() returns the bare *http.ServeMux — the mux answers 405/404 itself as plain " +
			"text, which is exactly the defect item 7b fixes. Every route would still work, so this " +
			"regression is invisible without this assertion.")
	}
}

// TestMethodDispatch_MuxAnswersBeforeAuthSoTheResultIsCredentialIndependent pins WHERE in the chain
// the wrapper sits. The plain-text 405 it replaces was emitted before requireAuth ran, so a wrong
// verb answered 405 with or without a token; the fix must not silently change that to "401 unless
// you authenticate first", which would make the surface's method vocabulary a credentialed secret
// and, worse, would mean an unauthenticated wrong verb still bypassed the envelope.
func TestMethodDispatch_MuxAnswersBeforeAuthSoTheResultIsCredentialIndependent(t *testing.T) {
	e := newTestEnv(t, "mdpreauth")
	withToken := e.doRaw(t, "DELETE", "/v1/quote")
	noToken := e.doUnauthenticated(t, "DELETE", "/v1/quote")
	if withToken.status != http.StatusMethodNotAllowed {
		t.Fatalf("authenticated wrong verb: status=%d want 405; body=%s", withToken.status, withToken.raw)
	}
	if noToken.status != withToken.status {
		t.Fatalf("wrong verb answers %d with a token but %d without — the mux no longer answers "+
			"before auth; body=%s", withToken.status, noToken.status, noToken.raw)
	}
	if noToken.errorCode() != codeMethodNotAllowed {
		t.Fatalf("unauthenticated wrong verb code=%q want %q; body=%s",
			noToken.errorCode(), codeMethodNotAllowed, noToken.raw)
	}
}

// TestMethodDispatch_ForgedPathCannotSplitTheLogLine is the END-TO-END control on the log-forgery
// fix: the unit test below proves sanitizePath's mapping, but only a real request proves the value
// that reaches the log line went THROUGH it. A raw `%0A` in an unmatched path is served by 7b's new
// fallback, which logs an arbitrary attacker-chosen path — the exact widening that made this worth
// fixing now.
//
// Captures the package's log output, which is why this must not run in parallel with anything.
func TestMethodDispatch_ForgedPathCannotSplitTheLogLine(t *testing.T) {
	e := newTestEnv(t, "mdlogforge")

	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0) // no timestamp prefix, so a line count is exactly a log-entry count
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	// A path whose DECODED form is a plausible second log entry. Built as a raw URL so the %0A
	// survives to the server rather than being re-encoded by net/url.
	req, err := http.NewRequest("GET", e.baseURL+
		"/v1/x%0Aplatformapi%20error%20rid=FORGED%20%20code=internal_error%20level=error", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("forged path: status=%d want 404", res.StatusCode)
	}

	out := strings.TrimRight(buf.String(), "\n")
	if out == "" {
		t.Fatal("the fallback logged nothing — an unmatched path must be observable, and this test " +
			"would otherwise pass vacuously")
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 1 {
		t.Fatalf("one request produced %d log lines:\n%s\nA decoded %%0A split the line; the "+
			"remainder is indistinguishable from a genuine entry with an attacker-chosen request_id.",
			len(lines), out)
	}
	if strings.Contains(out, "rid=FORGED") && !strings.Contains(out, "?platformapi") {
		t.Fatalf("the forged text reached the log unneutralized: %s", out)
	}
}

// TestSanitizePath_NeutralizesControlCharacters covers the live log-forgery hole found while
// auditing 7b's logging: r.URL.Path is DECODED, so `%0A` in a path arrives as a real newline, and
// every log line here is one Printf — one request could write two log lines, the second forging a
// plausible error entry with an attacker-chosen request_id.
func TestSanitizePath_NeutralizesControlCharacters(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/v1/quote", "/v1/quote"},
		{"/v1/report?user_id=abc", "/v1/report"},
		{"/v1/x\nplatformapi error rid=FORGED level=error", "/v1/x?platformapi error rid=FORGED level=error"},
		{"/v1/x\r\ny", "/v1/x??y"},
		{"/v1/x\ty", "/v1/x?y"},
		{"/v1/x\x00y", "/v1/x?y"},
		{"/v1/x\x7fy", "/v1/x?y"},
		{"/v1/x\ny?q=\n", "/v1/x?y"}, // query stripped first, then control bytes in what remains
	}
	for _, c := range cases {
		got := sanitizePath(c.in)
		if got != c.want {
			t.Fatalf("sanitizePath(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(got, "\r\n") {
			t.Fatalf("sanitizePath(%q) = %q still contains a line break — one request could write "+
				"two log lines", c.in, got)
		}
	}
}

// TestSanitizePath_IsUsedByEveryLogSiteThatLogsAPath is the coverage guard on the fix above: a NEW
// log line formatting the request path directly would reintroduce the hole while the unit test above
// stayed green. The end-to-end test proves today's fallback is safe; this proves the NEXT one will be.
//
// Walks the AST rather than grepping. A textual scan reports the prose in sanitizePath's own comment
// (which necessarily names r.URL.Path to explain the defect), and the obvious workarounds — skipping
// `//` lines — are exactly the kind of approximate matching that later admits a real miss.
// parser.ParseFile with mode 0 drops comments entirely, so what remains is only code.
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
			// A sanitized use is a call sanitizePath(<anything>) — do not descend into it.
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "sanitizePath" {
					return false
				}
			}
			// Anything reaching here that reads <req>.URL.Path is an UNSANITIZED use.
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Path" {
				return true
			}
			inner, ok := sel.X.(*ast.SelectorExpr)
			if !ok || inner.Sel.Name != "URL" {
				return true
			}
			pos := fset.Position(sel.Pos())
			t.Errorf("%s:%d reads %s.URL.Path outside sanitizePath.\n"+
				"If this value reaches a log line, a decoded %%0A splits it and forges a second "+
				"entry with an attacker-chosen request_id. Wrap it in sanitizePath.",
				pos.Filename, pos.Line, exprString(inner.X))
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no production files scanned — vacuous")
	}
}

func exprString(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "<expr>"
}

// --- helpers ---

// doRaw issues a request with NO body and NO Content-Type, returning headers as well as the parsed
// body. e.do cannot express these cases: it marshals a body and always sets Authorization, and it
// discards response headers (this test needs Allow and Content-Type).
//
// The Authorization header is set, so a 405/404 here is NOT explained away as an auth failure — the
// point is that the mux answers BEFORE auth runs, and the same result must appear either way (which
// the unauthenticated case below asserts explicitly).
func (e *testEnv) doRaw(t *testing.T, method, path string) rawResp {
	t.Helper()
	req, err := http.NewRequest(method, e.baseURL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return rawResp{status: res.StatusCode, header: res.Header, body: m, raw: string(raw)}
}

type rawResp struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

// assertErrorEnvelope checks the full external envelope contract: code, a non-empty message, a
// request_id, and NO internal reason anywhere in the body (two-vocabulary rule).
func assertErrorEnvelope(t *testing.T, r rawResp, wantCode string) {
	t.Helper()
	if got := errCode(t, r); got != wantCode {
		t.Fatalf("error.code=%q want %q; body=%s", got, wantCode, r.raw)
	}
	if msg := errField(t, r, "message"); msg == "" {
		t.Fatalf("error.message is empty; body=%s", r.raw)
	}
	if rid := errField(t, r, "request_id"); rid == "" {
		t.Fatalf("error.request_id is absent — the contract says it is mandatory on EVERY error; body=%s", r.raw)
	}
	// The internal reason is logged, never serialized.
	for _, leak := range []string{"mux_method_not_allowed", "mux_no_route"} {
		if strings.Contains(r.raw, leak) {
			t.Fatalf("body leaks the internal reason %q: %s", leak, r.raw)
		}
	}
}

func errCode(t *testing.T, r rawResp) string {
	t.Helper()
	return errField(t, r, "code")
}

func errField(t *testing.T, r rawResp, field string) string {
	t.Helper()
	obj, ok := r.body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body has no error object (so a client's body.error.%s would nil-deref): %s", field, r.raw)
	}
	s, _ := obj[field].(string)
	return s
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
