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

// platformRoutes is the four Phase-3 platform routes. Additive-only; the served set must match
// Docs/api-contract.md (the contract_drift gate).
func (s *Server) platformRoutes() []route {
	return []route{
		{"POST", "/v1/quote", s.handleQuote},
		{"POST", "/v1/run", s.handleRun},
		{"GET", "/v1/report", s.handleReport},
		{"GET", "/v1/describe", s.handleDescribe},
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
