package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SystemUserID is the reserved platform/system user_id (§0.1, ruling 2): the all-zero UUID.
// It is the user_id of shared pools and of accounts assigned to shared pools ("owned by the
// platform/module, not by any end user"). gen_random_uuid() (UUID v4) can never produce it, so
// it never collides with a real public.users(id). The module has SELECT-only on public.users and
// CANNOT seed it — the platform must (a coordination item); a pre-flight check gates shared-pool
// creates until the row exists (§0.1).
const SystemUserID = "00000000-0000-0000-0000-000000000000"

// Server is the operator admin API. It holds the DB pool (as send_managed_app), the Encrypt-only
// credential Encryptor, and the operator-key Authenticator. It registers its routes on a mux
// that the Control mounts under /v1/admin/*.
//
// handler is what Handler() returns: the mux WRAPPED in methodDispatch, so a method mismatch or an
// unmatched /v1/admin/* path leaves through the error envelope rather than ServeMux's plain text
// (fix-plan item 7b). Built once at construction.
type Server struct {
	db      *pgxpool.Pool
	enc     *Encryptor
	auth    *Authenticator
	mux     *http.ServeMux
	handler http.Handler
}

// NewServer wires the admin routes. db is the send_managed_app pool (never superuser); enc is the
// Encrypt-only credential primitive; auth is the operator-key gate.
func NewServer(db *pgxpool.Pool, enc *Encryptor, auth *Authenticator) *Server {
	s := &Server{db: db, enc: enc, auth: auth, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler exposes the served handler so the Control can mount it (e.g. under a parent mux).
//
// This returns the WRAPPED handler, never s.mux directly — returning the bare mux would silently
// restore the plain-text 405/404 (every route would still work, so nothing would look broken).
func (s *Server) Handler() http.Handler { return s.handler }

// route is one served admin route. adminRoutes() below is the single declaration site: both the
// real mux and item 7b's method-dispatch shape mux are built from it.
type route struct {
	method  string
	pattern string
	handler http.HandlerFunc
}

// adminRoutes is every Phase 2 route (method + path patterns, Go 1.22+ mux). Additive-only: routes
// never rename; later phases add. The served set here must match Docs/api-contract.md (the
// contract_drift gate).
//
// This was 21 individual s.mux.HandleFunc calls until fix-plan item 7b, which needs the route set as
// DATA (to build the method-stripped shape mux that distinguishes a wrong verb from a wrong path).
// The table is otherwise a faithful transcription — same methods, same patterns, same handlers, same
// order, same grouping comments.
func (s *Server) adminRoutes() []route {
	return []route{
		// Accounts
		{"POST", "/v1/admin/accounts", s.handleAccountCreate},
		{"GET", "/v1/admin/accounts", s.handleAccountList},
		{"GET", "/v1/admin/accounts/{id}", s.handleAccountGet},
		{"PATCH", "/v1/admin/accounts/{id}", s.handleAccountUpdate},
		{"DELETE", "/v1/admin/accounts/{id}", s.handleAccountDelete},
		{"GET", "/v1/admin/accounts/{id}/events", s.handleAccountEvents},

		// Account lifecycle transitions (manual, audited)
		{"POST", "/v1/admin/accounts/{id}/quarantine", s.handleAccountQuarantine},
		{"POST", "/v1/admin/accounts/{id}/unquarantine", s.handleAccountUnquarantine},
		{"POST", "/v1/admin/accounts/{id}/disable", s.handleAccountDisable},
		{"POST", "/v1/admin/accounts/{id}/enable", s.handleAccountEnable},
		{"POST", "/v1/admin/accounts/{id}/pause", s.handleAccountPause},
		{"POST", "/v1/admin/accounts/{id}/unpause", s.handleAccountUnpause},

		// Pools
		{"POST", "/v1/admin/pools", s.handlePoolCreate},
		{"GET", "/v1/admin/pools", s.handlePoolList},
		{"GET", "/v1/admin/pools/{id}", s.handlePoolGet},
		{"PATCH", "/v1/admin/pools/{id}", s.handlePoolUpdate},
		{"DELETE", "/v1/admin/pools/{id}", s.handlePoolDelete},

		// Config
		{"GET", "/v1/admin/config", s.handleConfigList},
		{"GET", "/v1/admin/config/{key}", s.handleConfigGet},
		{"PUT", "/v1/admin/config/{key}", s.handleConfigPut},

		// Observability (Phase 9.5 B3/B4, §3)
		{"GET", "/v1/admin/health", s.handleHealth},
	}
}

// routes mounts every adminRoutes() entry behind requireAuth (the operator-key gate) and builds
// item 7b's method-dispatch wrapper from the SAME table. No route reaches s.mux.HandleFunc any
// other way, so a route present in adminRoutes() is structurally guaranteed to be authenticated
// AND to have a shape entry deciding 405-vs-404 for it.
func (s *Server) routes() {
	rs := s.adminRoutes()
	for _, rt := range rs {
		s.mux.HandleFunc(rt.method+" "+rt.pattern, s.requireAuth(rt.handler))
	}
	shape, allow := buildShapeMux(rs)
	s.handler = s.methodDispatch(s.mux, shape, allow)
}

// fail writes the clean external envelope for e and logs the internal reason against rid. It is
// the single choke point every handler uses to end an error path (two-vocabulary rule).
func (s *Server) fail(w http.ResponseWriter, r *http.Request, e *apiError) {
	rid := requestIDFrom(r.Context())
	s.logError(rid, r, e)
	writeError(w, rid, e)
}

// dbCtx bounds a per-request DB operation so a slow/locked statement cannot hang a handler.
func dbCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 10*time.Second)
}
