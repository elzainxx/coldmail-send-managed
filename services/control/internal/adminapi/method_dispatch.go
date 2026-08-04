package adminapi

// Mux-level 405/404 through the error envelope (audit D17 / fix-plan item 7b), operator surface.
//
// THE DEFECT. Every route is a Go 1.22 method+path pattern, so net/http.ServeMux answers a METHOD
// MISMATCH or an UNMATCHED /v1/admin/* path itself, before requireAuth or any handler runs, with
// `405 Method Not Allowed` / `404 page not found` as text/plain — no `code`, no `request_id`. The
// contract says request_id is mandatory on EVERY error; that was false on this surface too.
//
// SHAPE OF THE FIX, and why not the fix plan's first suggestion (method-less fallback patterns):
// ServeMux PANICS AT REGISTRATION when a method-less literal coexists with a method+wildcard
// sibling. Module 3's platform surface has exactly that pair, so the approach cannot be applied
// uniformly across the three modules — and a fix that works on two surfaces and crashes the third
// is not the same fix. Instead the real mux is consulted as a match ORACLE, and a PARALLEL SHAPE
// MUX (same route table, methods stripped) supplies the distinction mux.Handler(r) cannot: it
// returns an empty pattern for a method mismatch AND for an unmatched path.
//
// Duplicated per package rather than shared with platformapi. This is OD-7 package isolation, the
// same discipline that duplicates the apiError/writeError/newRequestID/logger set: the two surfaces
// carry DIFFERENT code vocabularies (this one has ten codes, platformapi six) and different
// credentials, and each package's copy resolves against its OWN codeStatus. A shared helper would
// have to be parameterized over both, which is how the two vocabularies start leaking into each
// other.
//
// See internal/platformapi/method_dispatch.go for the two empirical traps this shape avoids: a
// direct call to the handler Handler(r) returns LOSES r.PathValue (so serving always goes through
// mux.ServeHTTP), and duplicate method-less registrations panic (so shape paths are deduped).

import (
	"net/http"
	"sort"
	"strings"
)

// buildShapeMux builds the method-stripped twin of the real mux plus the per-path Allow value.
// Paths are deduped (registering the same method-less pattern twice panics), and HEAD is advertised
// wherever GET is served because ServeMux answers HEAD from a GET pattern.
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
		// Never invoked — the wrapper uses Handler(r) as a match oracle only. Must be non-nil.
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
// The rid is minted here because neither path ever reaches requireAuth — that is the defect. The
// internal reason is logged against the rid and never serialized (two-vocabulary rule).
//
// A 405 on this surface is UNAUTHENTICATED, exactly as the plain-text 405 was: the operator key is
// not consulted. That discloses no more than contract_drift already asserts (a mounted route
// answers 401, a missing one 404), and it discloses strictly less than the alternative of running
// auth first would, since it never inspects the presented key at all.
func (s *Server) methodDispatch(real *http.ServeMux, shape *http.ServeMux, allow map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := real.Handler(r); pattern != "" {
			real.ServeHTTP(w, r) // through the MUX, so {id}/{key} wildcards bind
			return
		}
		rid := newRequestID()
		r = r.WithContext(withRequestID(r.Context(), rid))

		if _, pattern := shape.Handler(r); pattern != "" {
			if a := allow[pattern]; a != "" {
				w.Header().Set("Allow", a) // RFC 9110 requires Allow on a 405
			}
			e := newErr(codeMethodNotAllowed, "mux_method_not_allowed: "+r.Method+" on "+pattern)
			s.logError(rid, r, e)
			writeError(w, rid, e)
			return
		}
		e := newErr(codeNotFound, "mux_no_route: "+r.Method)
		s.logError(rid, r, e)
		writeError(w, rid, e)
	})
}
