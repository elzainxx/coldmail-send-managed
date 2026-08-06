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
//
// handler is what Handler() returns: the mux WRAPPED in methodDispatch, so a method mismatch or an
// unmatched path leaves through the error envelope instead of ServeMux's plain text (fix-plan item
// 7b). It is built once at construction, not per request.
type Server struct {
	db      *pgxpool.Pool
	auth    *Authenticator
	kick    Kicker
	mux     *http.ServeMux
	handler http.Handler
}

// NewServer wires the Domain-A routes. kick may be nil (run still succeeds; the dispatcher's own
// loop will pick the job up) but is normally the dispatcher nudge.
func NewServer(db *pgxpool.Pool, auth *Authenticator, kick Kicker) *Server {
	s := &Server{db: db, auth: auth, kick: kick, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler exposes the served handler for mounting under /v1/* on the Control's HTTP server.
//
// This returns the WRAPPED handler, never s.mux directly: returning the bare mux would restore the
// plain-text 405/404 that item 7b exists to remove, and it would do so silently — every route
// would still work. That is why the wrapper is built in routes() and stored, rather than composed
// at each call site that mounts this surface.
func (s *Server) Handler() http.Handler { return s.handler }

// routes registers the platform routes (Go 1.22+ method+path patterns), each behind the
// service-token gate, and builds the method-dispatch wrapper from the SAME table (item 7b).
// Additive-only; the served set must match Docs/api-contract.md.
//
// platformRoutes() is the single declaration site: nothing reaches s.mux.HandleFunc except this
// loop, so a route is structurally guaranteed to be (a) authenticated and (b) present in the shape
// mux that decides 405-vs-404 — the same "one call site can register a route at all" discipline
// adminapi's route table uses.
func (s *Server) routes() { s.wire(s.platformRoutes()) }

// wire is routes() with the table as a PARAMETER, and it exists in this shape for one reason: it is
// the only way a test can hand this code a broken route table.
//
// routes() reads platformRoutes(), which is a hardcoded literal that always agrees on "/v1". So
// every guard that only fires on a BAD table is unreachable through routes() — which is precisely
// how break B91 slipped past: the api_version refusal lived inside handleDescribe, deleting it kept
// the whole suite green, and no control existed that could have noticed. Taking the table as an
// argument makes the refusal reachable (TestAPIVersion_WiringRefusesATableWithoutOneVersion calls
// wire with a two-version table and requires the panic), so the guard has a control instead of a
// comment. TestAPIVersion_ConstructionGoesThroughTheRefusal then holds that routes() still delegates
// here rather than registering the mux itself — break B94 proved no behavioural test can see that.
func (s *Server) wire(rs []route) {
	// Refuse to CONSTRUCT a server whose route table cannot name one api_version (fix-plan item 14,
	// senders' half). This runs before any registration, so a misconfigured table takes the process
	// down at boot rather than serving quote/run/report normally while describe 500s — and it means
	// handleDescribe can publish APIVersion() with no branch of its own, because no Server that can
	// serve a request can exist with an unnameable version. See mustAPIVersion's header.
	mustAPIVersion(rs)
	for _, rt := range rs {
		s.mux.HandleFunc(rt.method+" "+rt.pattern, s.requireAuth(rt.handler))
	}
	shape, allow := buildShapeMux(rs)
	s.handler = s.methodDispatch(s.mux, shape, allow)
}

// dbCtx bounds a per-request DB operation.
func dbCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 10*time.Second)
}
