// Package platformapi is the Control's platform-facing surface (Domain A): quote / run / report /
// describe (Phase 3 §6). It is the END-USER (platform) surface, distinct from the operator admin
// surface (Domain B, internal/adminapi). The two auth domains NEVER share a credential (CLAUDE.md
// Rule 6): Domain A authenticates with the PLATFORM SERVICE TOKEN over the private network;
// Domain B uses the operator platform_admin bearer key. This package holds neither the credential
// Decryptor nor the Encryptor — it only creates/reads jobs.
//
// Two-vocabulary rule (Rule 5): every response is a clean envelope; a failure carries only
// {code, message, request_id} — never SQL/SQLSTATE, internal reason, or the worker's error_reason.
//
// Per-tenant isolation is ENFORCED by this module (OD-25, RULED option b): every platform request
// carries an explicit tenant selector (`user_id`) — `run` in the body, `report` in the query —
// and every platform-facing query scopes `WHERE user_id = $tenant`. A job that exists but belongs
// to another tenant returns the SAME 404-style not-found as a non-existent job (never 403), so a
// caller cannot see, nor even confirm the existence of, another tenant's jobs. This is
// defense-in-depth: even a platform-side bug that sends the wrong `user_id` cannot cross tenant
// boundaries in the module. See `Docs/history/open-decisions.md` OD-25.
package platformapi

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Kicker is the hook run() calls to nudge the dispatcher after creating a job. It must return
// IMMEDIATELY (async kickoff — run never blocks on the send, §6.2). Satisfied by a function that
// signals the dispatcher's run loop.
type Kicker func()

// Server is the platform API. It holds the DB pool (send_managed_app), the service-token
// authenticator, and an async dispatcher kick.
type Server struct {
	db   *pgxpool.Pool
	auth *Authenticator
	kick Kicker
	mux  *http.ServeMux
}

// NewServer wires the Domain-A routes. kick may be nil (run still succeeds; the dispatcher's own
// loop will pick the job up) but is normally the dispatcher nudge.
func NewServer(db *pgxpool.Pool, auth *Authenticator, kick Kicker) *Server {
	s := &Server{db: db, auth: auth, kick: kick, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler exposes the mux for mounting under /v1/* on the Control's HTTP server.
func (s *Server) Handler() http.Handler { return s.mux }

// routes registers the four Phase-3 platform routes (Go 1.22+ method+path patterns), each behind
// the service-token gate. Additive-only; the served set must match Docs/api-contract.md.
func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/quote", s.requireAuth(s.handleQuote))
	s.mux.HandleFunc("POST /v1/run", s.requireAuth(s.handleRun))
	s.mux.HandleFunc("GET /v1/report", s.requireAuth(s.handleReport))
	s.mux.HandleFunc("GET /v1/describe", s.requireAuth(s.handleDescribe))
}

// dbCtx bounds a per-request DB operation.
func dbCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 10*time.Second)
}
