package platformapi

// api_version_test.go — fix-plan item 14, the senders' half: this module never publishes a contract
// version that disagrees with the paths it serves.
//
// WHAT WAS WRONG (measured on this tree, not inferred). handleDescribe published
// `"api_version": "v1"` as a bare literal and NOTHING asserted it:
//
//   - `grep -rn api_version services/control --include=*_test.go` → no matches. No test named it.
//   - tests/harness/checks/contract_drift.sh checks describe's error_codes[] against codeStatus and
//     its vocabularies against pg_enum. It does not read api_version.
//   - Every served path carries its own hardcoded "/v1" in platformRoutes().
//
// So the version a client reads and the version the URLs speak were two unrelated strings that
// agreed by luck. A /v2 migration touching the routes and forgetting describe would ship a module
// telling callers it speaks v1 on paths that no longer exist — and the module's own suite would be
// green, because until this file existed nothing in it could tell.
//
// WHY THIS IS THE ADOPTION OF M1's HALF, AND WHY IT IS NEW WORK. M1's mechanism is
// verdict.Response.Valid(), which checks r.APIVersion == verdict.APIVersion and is called by BOTH
// non-test writers (api.go, orchestrator.go) BEFORE the response is written, so an out-of-contract
// version is never emitted. The PROPERTY transfers; the implementation cannot. M1 has a versioned
// response STRUCT with a version FIELD on every reply; this module's platform surface has no shared
// response type and reports the version on exactly one route. What transfers is: the module declares
// its version once, derives what it publishes from that declaration, and fails rather than emitting
// a version it cannot vouch for. Here "declared once" is the route table itself, because that is the
// string a client's URLs actually have to match.
//
// THE TESTS BELOW SPLIT ALONG WHAT EACH CAN SEE.
//   1. apiVersionOf's contract, in-process, including the disagreement case that CANNOT be staged
//      via HTTP (the real table agrees, so that input is unreachable from outside).
//   2. the REFUSAL fires when a table cannot name one version (behavioural: recover() the panic).
//   3. the refusal is ON THE CONSTRUCTION PATH (structural: a source scan of routes()/wire(), because
//      break B94 showed no behavioural test in this file can see routes() bypassing wire()).
//   4. the route table has one version, so nobody adds /v2/foo alongside /v1/bar and quietly makes
//      apiVersionOf return "" (which would take the module down at boot).
//   5. describe publishes THIS value, over real HTTP against a real clone — the wiring test.
//
// GROUP 2 EXISTS BECAUSE A BREAK-VERIFICATION FOUND ITS ABSENCE, and it is the most useful thing in
// this file. The first version of item 14 put the refusal in handleDescribe as `if apiVersion == ""
// { 500 }`. Break B91 deleted that branch and the whole package stayed GREEN — platformRoutes() is a
// literal that always agrees, so nothing could stage the input that fires it. A guard no test can
// reach is not a guard; it is a comment that compiles. The refusal moved to wire(), which takes the
// table as a parameter precisely so the bad case is reachable, and it panics at construction instead
// of 500ing one endpoint in production. handleDescribe now has no branch at all, because the
// invariant is established before any request exists.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestAPIVersion_DerivationContract pins apiVersionOf's behaviour, including the two failure inputs
// that a live probe cannot reach.
//
// The "" cases matter because handleDescribe branches on them: it fails the request rather than
// publishing `"api_version": ""`. That branch is only correct if "" genuinely means "underivable",
// so the meaning is asserted here rather than assumed at the call site.
func TestAPIVersion_DerivationContract(t *testing.T) {
	h := func(http.ResponseWriter, *http.Request) {}
	cases := []struct {
		name string
		rs   []route
		want string
		why  string
	}{
		{
			name: "the real table",
			rs:   (&Server{}).platformRoutes(),
			want: "v1",
			why:  "the production route set must derive the version this module publishes",
		},
		{
			name: "one route",
			rs:   []route{{"GET", "/v1/describe", h}},
			want: "v1",
			why:  "a single route is enough to derive a version",
		},
		{
			name: "deeper paths",
			rs:   []route{{"GET", "/v1/a/b/{id}", h}, {"POST", "/v1/c", h}},
			want: "v1",
			why:  "only the FIRST segment is the version; wildcards and depth are irrelevant",
		},
		{
			name: "a future v2",
			rs:   []route{{"GET", "/v2/quote", h}, {"POST", "/v2/run", h}},
			want: "v2",
			why:  "the derivation is not hardcoded to v1 — a /v2 migration is a route-table edit and describe follows automatically, which is the whole point of item 14's senders half",
		},
		{
			name: "routes DISAGREE",
			rs:   []route{{"GET", "/v1/quote", h}, {"POST", "/v2/run", h}},
			want: "",
			why:  "two versions in one table means no single api_version is truthful; handleDescribe must 500 rather than pick one",
		},
		{
			name: "empty table",
			rs:   nil,
			want: "",
			why:  "an empty table has no version to report; publishing \"\" would tell a client this module does not version its API",
		},
	}
	for _, c := range cases {
		if got := apiVersionOf(c.rs); got != c.want {
			t.Errorf("apiVersionOf(%s) = %q, want %q\n  %s", c.name, got, c.want, c.why)
		}
	}
}

// TestAPIVersion_WiringRefusesATableWithoutOneVersion is the control that break B91 proved was
// missing, and it is the reason wire() takes the route table as a parameter at all.
//
// THE HOLE B91 FOUND. The refusal used to live in handleDescribe: `if apiVersion == "" { 500 }`.
// Deleting that branch outright left THIS ENTIRE PACKAGE GREEN — every test above included. The
// branch was unreachable, because platformRoutes() is a hardcoded literal that always agrees on
// "/v1", so no HTTP-level test could ever stage the input that fires it. It read like a handled case
// and was in fact dead code with a comment on it. That is the exact failure family this fix plan
// exists to close, and it appeared inside the fix for item 14 itself.
//
// WHY CONSTRUCTION RATHER THAN A TEST-ONLY SEAM ON THE HANDLER. Two things improve at once. The
// input becomes stageable — wire(rs) accepts any table, so the bad case is a normal function call
// rather than something requiring a fake mux or an injected route provider. And the behaviour gets
// louder in production: a table that cannot name one version is a bug in this file, so the process
// refuses to boot instead of serving quote/run/report normally while describe 500s for whoever calls
// it next. Both bad inputs are covered, because "empty" and "disagreeing" reach the panic by
// different routes through apiVersionOf and a future edit could easily fix one and not the other.
func TestAPIVersion_WiringRefusesATableWithoutOneVersion(t *testing.T) {
	h := func(http.ResponseWriter, *http.Request) {}
	cases := []struct {
		name    string
		rs      []route
		wantMsg string
	}{
		{
			name:    "routes disagree on a version",
			rs:      []route{{"GET", "/v1/describe", h}, {"POST", "/v2/run", h}},
			wantMsg: "/v2/run",
		},
		{
			name:    "empty table",
			rs:      nil,
			wantMsg: "does not name exactly one api_version",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A bare &Server{} is correct here: wire() touches only s.mux and s.handler, and the panic
			// fires before either is used. No DB, no auth — this is a pure wiring assertion.
			s := &Server{mux: http.NewServeMux()}
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("wire(%s) returned normally. A server whose route table cannot name one\n"+
						"  api_version must not be constructible: describe would publish a version that is\n"+
						"  not the one its own paths speak, or none at all. This is the check whose absence\n"+
						"  break B91 could not detect when it lived in handleDescribe.", c.name)
				}
				msg, ok := r.(string)
				if !ok {
					t.Fatalf("wire panicked with %T (%v), want a string explaining the misconfiguration", r, r)
				}
				// The message must NAME the offending table, not merely say "invalid": whoever hits this
				// at boot has no request and no stack context beyond the panic line.
				if !strings.Contains(msg, c.wantMsg) {
					t.Errorf("panic message does not mention %q, so a reader cannot tell WHICH routes are\n"+
						"  wrong:\n  %s", c.wantMsg, msg)
				}
			}()
			s.wire(c.rs)
		})
	}
}

// TestAPIVersion_ConstructionGoesThroughTheRefusal is the STRUCTURAL half: the guard is not merely
// present in a function, it is on the only path that builds a server.
//
// WHY A SOURCE SCAN, WHICH THIS PROJECT OTHERWISE DISTRUSTS. Break B94 rewrote routes() to register
// the mux itself — the exact drift where a tested helper survives but production stops calling it —
// and EVERY behavioural test above stayed green, including the one that recovers wire()'s panic. They
// cannot see it: they call wire() directly, so an orphaned wire() still panics on cue while NewServer
// quietly skips the check. Verified, not assumed (B94 green before this test existed, red after).
//
// The alternative was a test-only route-table field on Server so routes() could be fed a broken table.
// Rejected: it puts a seam in production wiring that exists solely for this assertion, and it would
// still not catch a routes() that ignored the field. This mirrors item 14's M1 half, where
// TestRoutes_HandlerIsALoopOverRoutes covers the same structural property for the same reason and
// break B83 established that the scan sees what the behavioural tests provably cannot.
//
// The scan is deliberately narrow — two facts about one function, both about CALL STRUCTURE rather
// than formatting, so it does not fire on unrelated edits to this file.
func TestAPIVersion_ConstructionGoesThroughTheRefusal(t *testing.T) {
	// The behavioural half first: a real construction path yields a derivable version.
	s := &Server{mux: http.NewServeMux()}
	s.routes()
	if s.handler == nil {
		t.Fatal("routes() left handler nil — it no longer performs the wiring these tests assert about")
	}
	if got := s.APIVersion(); got == "" {
		t.Fatal("a server built through routes() reports no api_version, which mustAPIVersion should have refused")
	}

	bodies := funcBodies(t, "server.go")
	body := bodies["routes"]
	if body == "" {
		t.Fatal("no routes() method in server.go — this test's target was renamed, so the property it\n" +
			"  holds is now unheld. Update the test rather than deleting it.")
	}
	if !strings.Contains(body, "s.wire(") {
		t.Errorf("routes() does not call s.wire(). Registration must go through wire(), because that is\n"+
			"  where mustAPIVersion refuses a table that cannot name one api_version. routes() body:\n%s", body)
	}
	if strings.Contains(body, "HandleFunc") {
		t.Errorf("routes() registers handlers directly. Any registration path that bypasses wire() also\n"+
			"  bypasses the api_version refusal — and no behavioural test can see that (break B94: this\n"+
			"  edit left every other test in this file green). routes() body:\n%s", body)
	}

	// And within wire(), the refusal must come BEFORE the first registration: refusing after the mux is
	// populated would still panic, but a partially-wired Server is a worse thing to hand a recover().
	wireBody := bodies["wire"]
	if wireBody == "" {
		t.Fatal("no wire() method in server.go — see above; the target was renamed, not the property removed")
	}
	must, reg := strings.Index(wireBody, "mustAPIVersion("), strings.Index(wireBody, "HandleFunc")
	if must < 0 {
		t.Fatalf("wire() does not call mustAPIVersion — nothing refuses an unnameable version:\n%s", wireBody)
	}
	if reg >= 0 && must > reg {
		t.Errorf("wire() registers routes before calling mustAPIVersion; the refusal must precede any\n"+
			"  registration so a rejected table never half-builds a server:\n%s", wireBody)
	}
}

// funcBodies parses a file and returns each function's body source keyed by name, comments stripped.
//
// go/ast RATHER THAN STRING SCANNING, and this is not fastidiousness — the string version was WRONG
// and its own break-verification caught it. Extracting "signature up to the first \n}\n" made
// routes()'s body (a ONE-LINE function, `func (s *Server) routes() { s.wire(...) }`) run on to the end
// of wire(), so the "routes() must not call HandleFunc" assertion saw wire()'s HandleFunc and failed
// on the CORRECT tree. A control that fires on a healthy repo is worse than no control: it gets
// deleted or muted, and the property goes with it. The parser knows exactly where a func ends.
//
// Comments are stripped because they legitimately discuss the calls being asserted about — wire()'s
// own header mentions registration, and this test must read code, not prose.
func funcBodies(t *testing.T, filename string) map[string]string {
	t.Helper()
	src, err := os.ReadFile(filename)
	if err != nil {
		// Fatal, never Skip: a skip here would silently retire the only control on this property, which
		// is the failure mode this whole fix plan exists to remove.
		t.Fatalf("cannot read %s, so the structural assertion cannot run: %v", filename, err)
	}
	// ParseComments is omitted, so ast drops them; the printer then emits code only.
	file, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", filename, err)
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, token.NewFileSet(), fn.Body); err != nil {
			t.Fatalf("cannot print %s's body: %v", fn.Name.Name, err)
		}
		out[fn.Name.Name] = buf.String()
	}
	if len(out) == 0 {
		t.Fatalf("%s parsed to zero functions — the assertions below would pass by having nothing to\n"+
			"  inspect, which is the vacuous-test failure this file is careful about", filename)
	}
	return out
}

// TestAPIVersion_RouteTableSpeaksOneVersion is the control on the route table itself.
//
// If someone adds a route under a different prefix, apiVersionOf returns "" and every GET /v1/describe
// answers 500 — a real outage, discovered in production, from an edit that looks additive. This fails
// at compile-test time instead, and names the offending pattern.
//
// It also pins the SHAPE of the version, which the derivation cannot: apiVersionOf would happily
// return "admin" from a table of /admin/... paths. The published api_version is a contract token a
// client may switch on, so "v" followed by digits is asserted.
func TestAPIVersion_RouteTableSpeaksOneVersion(t *testing.T) {
	rs := (&Server{}).platformRoutes()
	if len(rs) == 0 {
		t.Fatal("platformRoutes() is empty — this test would be vacuous, and describe would 500")
	}
	version := apiVersionOf(rs)
	if version == "" {
		var got []string
		for _, rt := range rs {
			got = append(got, rt.pattern)
		}
		t.Fatalf("the route table serves more than one version prefix, so no truthful api_version exists:\n  %s\n"+
			"  Every platform route must sit under ONE version. A /v2 migration moves them all at once;\n"+
			"  serving both from one table means describe cannot answer, and it answers 500 rather than guessing.",
			strings.Join(got, "\n  "))
	}
	if !regexp.MustCompile(`^v[0-9]+$`).MatchString(version) {
		t.Errorf("derived api_version %q is not a version token (want v<digits>). The published value is\n"+
			"  part of the platform contract and a client may switch on it; a first path segment that is\n"+
			"  not a version means these routes are not mounted where the contract says they are.", version)
	}
	if got := (&Server{}).APIVersion(); got != version {
		t.Errorf("(*Server).APIVersion()=%q but apiVersionOf(platformRoutes())=%q — the method must be a\n"+
			"  pure read of the table, since handleDescribe publishes what the method returns", got, version)
	}
}

// TestAPIVersion_DescribePublishesTheDerivedVersion is the WIRING test, over real HTTP against a real
// Postgres clone (CLAUDE.md Rule 3), because that is the only level at which the thing that can
// regress is visible: handleDescribe going back to a literal.
//
// A unit test calling APIVersion() would pass with `"api_version": "v1"` still hardcoded in the
// response map. That is precisely the state this file exists to end, so the assertion has to read the
// SERVED body.
func TestAPIVersion_DescribePublishesTheDerivedVersion(t *testing.T) {
	e := newTestEnv(t, "apiversion")

	// The path comes from the route table, not a literal: if the routes ever move to /v2 this test
	// follows them, which is the same property the production code now has.
	describePath := ""
	for _, rt := range (&Server{}).platformRoutes() {
		if strings.HasSuffix(rt.pattern, "/describe") {
			describePath = rt.pattern
		}
	}
	if describePath == "" {
		t.Fatal("no /describe route in platformRoutes() — this test cannot fetch what it asserts about")
	}

	desc := e.do(t, "GET", describePath, nil).requireStatus(t, 200)
	got, ok := desc.body["api_version"].(string)
	if !ok {
		t.Fatalf("describe published no api_version string: %s", desc.raw)
	}
	want := (&Server{}).APIVersion()
	if want == "" {
		t.Fatal("APIVersion() is empty — TestAPIVersion_RouteTableSpeaksOneVersion explains why")
	}
	if got != want {
		t.Fatalf("describe published api_version=%q but the served route table says %q.\n"+
			"  These must be the same string BECAUSE THEY ARE THE SAME FACT: a client reads api_version\n"+
			"  from describe and then builds URLs. If describe says v1 and the paths are /v2/..., every\n"+
			"  request that client makes 404s and the module told it to do that.", got, want)
	}
	// And the served PATH this very response came back on must carry that version — the end-to-end
	// form of the property: a real request, a real 200, and the version it reports is the version in
	// the URL that produced it.
	if !strings.HasPrefix(describePath, "/"+got+"/") {
		t.Fatalf("this response was fetched from %s but describe reports api_version=%q", describePath, got)
	}
	t.Logf("GET %s -> api_version=%q, derived from the served route table", describePath, got)
}
