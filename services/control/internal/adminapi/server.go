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
type Server struct {
	db   *pgxpool.Pool
	enc  *Encryptor
	auth *Authenticator
	mux  *http.ServeMux
}

// NewServer wires the admin routes. db is the send_managed_app pool (never superuser); enc is the
// Encrypt-only credential primitive; auth is the operator-key gate.
func NewServer(db *pgxpool.Pool, enc *Encryptor, auth *Authenticator) *Server {
	s := &Server{db: db, enc: enc, auth: auth, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler exposes the mux so the Control can mount it (e.g. under a parent mux).
func (s *Server) Handler() http.Handler { return s.mux }

// routes registers every Phase 2 route (method + path patterns, Go 1.22+ mux). Every route is
// behind requireAuth (the operator-key gate). Additive-only: routes never rename; later phases
// add. The served set here must match Docs/api-contract.md (the contract_drift gate).
func (s *Server) routes() {
	// Accounts
	s.mux.HandleFunc("POST /v1/admin/accounts", s.requireAuth(s.handleAccountCreate))
	s.mux.HandleFunc("GET /v1/admin/accounts", s.requireAuth(s.handleAccountList))
	s.mux.HandleFunc("GET /v1/admin/accounts/{id}", s.requireAuth(s.handleAccountGet))
	s.mux.HandleFunc("PATCH /v1/admin/accounts/{id}", s.requireAuth(s.handleAccountUpdate))
	s.mux.HandleFunc("DELETE /v1/admin/accounts/{id}", s.requireAuth(s.handleAccountDelete))
	s.mux.HandleFunc("GET /v1/admin/accounts/{id}/events", s.requireAuth(s.handleAccountEvents))

	// Account lifecycle transitions (manual, audited)
	s.mux.HandleFunc("POST /v1/admin/accounts/{id}/quarantine", s.requireAuth(s.handleAccountQuarantine))
	s.mux.HandleFunc("POST /v1/admin/accounts/{id}/unquarantine", s.requireAuth(s.handleAccountUnquarantine))
	s.mux.HandleFunc("POST /v1/admin/accounts/{id}/disable", s.requireAuth(s.handleAccountDisable))
	s.mux.HandleFunc("POST /v1/admin/accounts/{id}/enable", s.requireAuth(s.handleAccountEnable))
	s.mux.HandleFunc("POST /v1/admin/accounts/{id}/pause", s.requireAuth(s.handleAccountPause))
	s.mux.HandleFunc("POST /v1/admin/accounts/{id}/unpause", s.requireAuth(s.handleAccountUnpause))

	// Pools
	s.mux.HandleFunc("POST /v1/admin/pools", s.requireAuth(s.handlePoolCreate))
	s.mux.HandleFunc("GET /v1/admin/pools", s.requireAuth(s.handlePoolList))
	s.mux.HandleFunc("GET /v1/admin/pools/{id}", s.requireAuth(s.handlePoolGet))
	s.mux.HandleFunc("PATCH /v1/admin/pools/{id}", s.requireAuth(s.handlePoolUpdate))
	s.mux.HandleFunc("DELETE /v1/admin/pools/{id}", s.requireAuth(s.handlePoolDelete))

	// Config
	s.mux.HandleFunc("GET /v1/admin/config", s.requireAuth(s.handleConfigList))
	s.mux.HandleFunc("GET /v1/admin/config/{key}", s.requireAuth(s.handleConfigGet))
	s.mux.HandleFunc("PUT /v1/admin/config/{key}", s.requireAuth(s.handleConfigPut))

	// Observability (Phase 9.5 B3/B4, §3)
	s.mux.HandleFunc("GET /v1/admin/health", s.requireAuth(s.handleHealth))
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
