package platformapi

// Mux-level 405/404 through the error envelope (audit D17 / fix-plan item 7b).
//
// THE DEFECT. Every route here is registered as a Go 1.22 method+path pattern
// ("POST /v1/quote"). net/http.ServeMux answers a METHOD MISMATCH or an UNMATCHED path itself,
// before requireAuth or any handler runs, with:
//
//	HTTP/1.1 405 Method Not Allowed        HTTP/1.1 404 Not Found
//	Content-Type: text/plain; charset=utf-8
//	Method Not Allowed                     404 page not found
//
// No `code`. No `request_id`. Not JSON. A client that does `body.error.code` on every non-2xx —
// which the contract invites, since it states request_id is mandatory on EVERY error — nil-derefs
// on a wrong-verb call. The contract clause was simply false in all three modules, and no gate
// noticed because contract_drift probes each route with its method-appropriate verb only.
//
// THE FIX, AND WHY IT IS SHAPED LIKE THIS. Two ways were considered:
//
//  1. Register method-less fallback patterns ("/v1/quote") alongside the real ones. This is what
//     the fix plan suggests first, and it is IMPOSSIBLE HERE: ServeMux PANICS AT REGISTRATION when
//     a method-less literal coexists with a method+wildcard sibling —
//     `pattern "/a/bulk" conflicts with pattern "GET /a/{id}": /a/bulk matches more methods than
//     GET /a/{id}, but has a more specific path pattern`. Module 3's platform surface has exactly
//     that pair (POST /v1/send-byo/accounts/bulk + GET /v1/send-byo/accounts/{id}), so the
//     approach would crash that module on startup. Verified against the real route sets, not
//     assumed.
//
//  2. Own method dispatch in a wrapper (this file). The wrapper asks the REAL mux whether the
//     request matches anything; if not, it asks a PARALLEL SHAPE MUX built from the same route
//     table with the methods stripped. A shape hit means "the path exists, the verb is wrong" —
//     405 with an Allow header. A shape miss means "no such path" — 404. Both go out through
//     writeError, so they are the same envelope every other failure uses.
//
// mux.Handler(r) alone CANNOT distinguish the two cases: it returns an empty pattern for a method
// mismatch AND for an unmatched path (verified). The second mux is what supplies the distinction.
//
// WHY IT DELEGATES TO real.ServeHTTP AND NOT TO THE HANDLER Handler(r) RETURNS. Calling the
// returned handler directly LOSES r.PathValue — the returned handler is not bound to the matched
// pattern's wildcards, so every {id} route would silently see an empty id. Verified: direct call
// gives id="", mux.ServeHTTP gives id="abc123". Handler(r) is used ONLY as a match ORACLE here;
// the mux always does the actual serving.
//
// WHAT THIS DELIBERATELY DOES NOT CHANGE:
//
//   - Route existence is still disclosed to an unauthenticated caller (404 for an unknown path,
//     401 for a real one). That is pre-existing and INTENTIONAL — contract_drift's whole
//     additive-only assertion is "a mounted route answers 401, a missing one 404". A wrong verb on
//     a real path already disclosed the same thing via the plain-text 405. No new disclosure: only
//     the SHAPE of the answer changes. This is distinct from the cross-tenant rule (a job id
//     belonging to another tenant is 404, never 403), which is about DATA, not routes.
//   - Redirect behaviour (`//v1/quote` -> 301) is untouched: the real mux matches those, so they
//     never reach the fallback paths.
//   - HEAD on a GET route still hits the real handler (ServeMux matches HEAD to GET patterns), so
//     the Allow header advertises HEAD wherever GET is served.

import (
	"net/http"
	"sort"
	"strings"
)

// route is one served platform route. The table below is the ONLY place a route is declared: both
// the real mux and the shape mux are built from it, so a route cannot be served without a shape
// entry (which would make it 404 on a wrong verb instead of 405), and a shape entry cannot exist
// for a route that is not served (which would make a nonexistent path 405 instead of 404).
type route struct {
	method  string
	pattern string
	handler http.HandlerFunc
}

// APIVersion is the frozen contract version of this module's platform surface. A breaking change
// means a /v2 prefix, never an in-place edit of what /v1 means.
//
// FIX-PLAN ITEM 14, THE SENDERS' HALF (adopted from module 1). M1 asserts its declared version
// against every response it writes — verdict.APIVersion, checked by Response.Valid(), which api.go
// and orchestrator.go both call BEFORE the write, so a response whose version does not match the
// module's declaration is never emitted. This module had the other half of item 14 (a contract_drift
// gate over the route set) and NOT that one. The trade goes both ways: M1 got the route gate, the
// senders get the version assertion.
//
// WHAT WAS ACTUALLY WRONG HERE, and it is not "no constant existed". handleDescribe published
// `"api_version": "v1"` as a bare literal with NOTHING asserting it: no test in this module named
// api_version, and the contract_drift gate — which does check describe's error_codes[] and its
// vocabularies against their sources — did not check this field at all. Meanwhile every served path
// carries its own hardcoded "/v1". So the version a client reads from describe and the version the
// URLs actually speak were two unrelated strings that happened to agree, and either could be edited
// without the other. A /v2 migration that renamed the routes and forgot describe would ship a
// module telling callers it speaks v1 on paths that no longer exist.
//
// WHY IT IS DERIVED AND NOT DECLARED. Writing `const APIVersion = "v1"` beside the table would fix
// the drift between describe and the constant while leaving the drift between the constant and the
// PATHS — a third string to keep in step, and the one that matters least is the one a reviewer would
// notice. Deriving it from the route table means the version cannot be wrong: it IS what the served
// paths say. This is the same reasoning as M1's Route slice (the served set is the compiler's view)
// and as deriving describe's error_codes[] from codeStatus rather than hand-listing them.
//
// WHAT ENFORCES THE ABOVE — TestAPIVersion_* in api_version_test.go, all five on every `go test`.
// Named exactly, because "see the tests" is how a header ends up pointing at a test that no longer
// exists (this project has hit that already):
//   - TestAPIVersion_DerivationContract — apiVersionOf over 6 tables, including the two that yield "".
//   - TestAPIVersion_WiringRefusesATableWithoutOneVersion — mustAPIVersion actually panics, for both
//     bad inputs, and the message names the offending routes.
//   - TestAPIVersion_ConstructionGoesThroughTheRefusal — the structural half: routes() registers only
//     via wire(), and wire() refuses BEFORE registering. Break B94 (routes() inlining the loop) left
//     every behavioural test green, so this one is a go/ast scan of server.go.
//   - TestAPIVersion_RouteTableSpeaksOneVersion — the real table agrees and matches ^v[0-9]+$.
//   - TestAPIVersion_DescribePublishesTheDerivedVersion — real HTTP, real clone: the served body's
//     api_version equals APIVersion(), which a unit test could not catch (B88).
func (s *Server) APIVersion() string { return apiVersionOf(s.platformRoutes()) }

// mustAPIVersion is apiVersionOf plus the refusal, and it runs at CONSTRUCTION (register), not per
// request.
//
// WHY IT MOVED HERE, which is worth recording because a break-verification found it. The guard was
// originally `if apiVersion == "" { fail the request }` inside handleDescribe. Deleting that branch
// outright left the entire suite GREEN (break B91): platformRoutes() is a hardcoded literal that
// always agrees, so the branch was unreachable from any HTTP test and nothing could observe its
// absence. Untested defensive code that no control can see is exactly the phantom this fix plan
// exists to remove — a reviewer reads the branch and believes the case is handled.
//
// At construction the same property is (a) directly reachable by a test, since register takes the
// table as an argument, and (b) strictly louder: a route table that cannot name one version is a
// programming error in this file, so the module refuses to BOOT rather than serving three routes
// fine and 500ing describe until someone calls it. Every non-test caller reaches this through
// NewServer, so no Server that serves traffic can exist with an unnameable version — which is what
// lets handleDescribe publish APIVersion() with no branch at all.
func mustAPIVersion(rs []route) string {
	v := apiVersionOf(rs)
	if v == "" {
		var got []string
		for _, rt := range rs {
			got = append(got, rt.method+" "+rt.pattern)
		}
		panic("platformapi: platformRoutes() does not name exactly one api_version — the table is " +
			"empty or its routes disagree on a version prefix, so no truthful api_version can be " +
			"published. Routes: [" + strings.Join(got, ", ") + "]")
	}
	return v
}

// apiVersionOf extracts the shared version prefix from a route table. It returns "" if the table is
// empty or the routes disagree — a caller must treat that as a bug, not as a version, which is what
// mustAPIVersion does at construction.
func apiVersionOf(rs []route) string {
	version := ""
	for _, rt := range rs {
		seg := strings.SplitN(strings.TrimPrefix(rt.pattern, "/"), "/", 2)[0]
		if version == "" {
			version = seg
			continue
		}
		if seg != version {
			return "" // routes disagree: two versions served from one table
		}
	}
	return version
}

// platformRoutes is the five platform routes: the four Phase-3 routes plus POST /v1/cancel (M2-G3).
// Additive-only; the served set must match Docs/api-contract.md (the contract_drift gate) AND the exact
// paths httpapi's root mux mounts for this surface (httpapi/health.go registers each one explicitly so
// the mux does not shadow /v1/admin/*) — a route added here and not there answers the root mux's coded
// 404, which is why contract_drift probes every entry of this table on the wire.
//
// The "/v1" in each pattern is what APIVersion() reads, so these strings are the single source of
// this module's contract version — describe does not carry a second copy.
func (s *Server) platformRoutes() []route {
	return []route{
		{"POST", "/v1/quote", s.handleQuote},
		{"POST", "/v1/run", s.handleRun},
		{"GET", "/v1/report", s.handleReport},
		{"GET", "/v1/describe", s.handleDescribe},
		{"POST", "/v1/cancel", s.handleCancel},
	}
}

// buildShapeMux builds the method-stripped twin of the real mux and the per-path Allow value.
//
// Paths are DEDUPED: registering the same method-less pattern twice panics
// (`pattern "/a" conflicts with pattern "/a": /a matches the same requests as /a`), and
// /v1/send-byo/accounts-style paths legitimately carry several methods. The Allow value is the
// sorted method set, plus HEAD wherever GET is served, because ServeMux answers HEAD from a GET
// pattern and a client reading Allow should not be told otherwise.
func buildShapeMux(routes []route) (*http.ServeMux, map[string]string) {
	methods := map[string][]string{}
	for _, rt := range routes {
		methods[rt.pattern] = append(methods[rt.pattern], rt.method)
	}
	shape := http.NewServeMux()
	allow := make(map[string]string, len(methods))
	for pattern, ms := range methods {
		if containsMethod(ms, http.MethodGet) && !containsMethod(ms, http.MethodHead) {
			ms = append(ms, http.MethodHead)
		}
		sort.Strings(ms)
		allow[pattern] = strings.Join(ms, ", ")
		// The shape handler is never invoked: the wrapper uses Handler(r) as a match oracle only.
		// It must still be non-nil — ServeMux panics on a nil handler.
		shape.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	}
	return shape, allow
}

func containsMethod(ms []string, want string) bool {
	for _, m := range ms {
		if m == want {
			return true
		}
	}
	return false
}

// methodDispatch wraps the real mux so a method mismatch or an unmatched path leaves through the
// SAME envelope as every other error, with a request_id.
//
// The rid is minted HERE rather than reused from requireAuth, because neither of these paths ever
// reaches requireAuth — that is the entire defect. The internal reason is logged against the rid
// and never serialized, the same two-vocabulary discipline as every handler.
func (s *Server) methodDispatch(real *http.ServeMux, shape *http.ServeMux, allow map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A non-empty pattern means the real mux has a handler for this method+path (including a
		// redirect match and HEAD-on-GET). Serve it through the MUX so wildcards bind.
		if _, pattern := real.Handler(r); pattern != "" {
			real.ServeHTTP(w, r)
			return
		}
		rid := newRequestID()
		r = r.WithContext(withRequestID(r.Context(), rid))

		if _, pattern := shape.Handler(r); pattern != "" {
			// The path exists; the verb does not. RFC 9110 requires Allow on a 405, and a client
			// that got here needs to know which verb to use.
			if a := allow[pattern]; a != "" {
				w.Header().Set("Allow", a)
			}
			e := newErr(codeMethodNotAllowed, "mux_method_not_allowed: "+r.Method+" on "+pattern)
			logError(rid, r, e)
			writeError(w, rid, e)
			return
		}
		// No such path on this surface.
		e := newErr(codeNotFound, "mux_no_route: "+r.Method)
		logError(rid, r, e)
		writeError(w, rid, e)
	})
}
