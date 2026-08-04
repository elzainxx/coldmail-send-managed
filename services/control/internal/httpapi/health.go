// Package httpapi serves the Control's health stub. Phase-0 scope only: a read-only endpoint
// that reports TRUE state (DB reachable, singleton lock held). The full operator health
// surface (Domain B auth, queue/worker/account metrics) is Phase 2/9.5 — not built here.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coldmail-send-managed/control/internal/db"
	"github.com/coldmail-send-managed/control/internal/singleton"
)

// HealthServer answers /healthz and /readyz, and (Phase 2) mounts the operator admin surface
// under /v1/admin/* when an admin handler is provided. Health remains unauthenticated (a liveness/
// readiness probe); the admin routes carry their own Domain-B operator-key auth internally.
type HealthServer struct {
	http  *http.Server
	db    *db.DB
	guard *singleton.Guard
}

// NewHealthServer wires the health routes and mounts, when provided:
//   - adminHandler   under /v1/admin/*   (Domain B operator surface, Phase 2)
//   - platformHandler for /v1/quote,/v1/run,/v1/report,/v1/describe (Domain A, Phase 3)
//
// Passing nil for either keeps that surface unmounted (fail-closed when its credential is
// unconfigured). The two handlers own disjoint route sets and carry their own auth.
func NewHealthServer(addr string, database *db.DB, guard *singleton.Guard, adminHandler, platformHandler http.Handler) *HealthServer {
	h := &HealthServer{db: database, guard: guard}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.handleHealthz)
	mux.HandleFunc("/readyz", h.handleReadyz)
	if adminHandler != nil {
		// The admin server owns every /v1/admin/* route (method+path patterns, own auth).
		mux.Handle("/v1/admin/", adminHandler)
	}
	if platformHandler != nil {
		// The platform server owns exactly /v1/quote,/v1/run,/v1/report,/v1/describe (Domain A).
		// Register each explicitly so the mux does not shadow /v1/admin/*.
		mux.Handle("/v1/quote", platformHandler)
		mux.Handle("/v1/run", platformHandler)
		mux.Handle("/v1/report", platformHandler)
		mux.Handle("/v1/describe", platformHandler)
	}
	// Catch-all: anything this mux does not route (an unknown path, or a known path when its surface
	// is unmounted) answers the CODED envelope rather than ServeMux's `404 page not found` text
	// (fix-plan item 7b / audit D17). Without it, only paths reaching a mounted sub-surface got a
	// coded 404 and every other miss returned plain text with no request_id — so a client's
	// `error.code` branch worked or nil-deref'd depending on which path it typo'd.
	//
	// This is the ROOT mux's own fallback, deliberately not delegated to either sub-surface: at this
	// level there is no route table to compare against, so there is no 405-vs-404 distinction to
	// make. A wrong VERB on a real route never reaches here (the sub-surface's own dispatch owns
	// that, and it is the only layer that knows which methods a path serves).
	//
	// The two health routes are registered WITHOUT a method above, so they accept any verb and never
	// fall through to this handler — an unchanged pre-existing property, not something 7b relaxed.
	mux.HandleFunc("/", handleNoRoute)
	h.http = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return h
}

// handleNoRoute answers an unrouted path with the coded envelope. It mints its own request_id: no
// sub-surface auth or handler ran, so there is none to inherit — which is exactly why the plain-text
// default had no id to report.
//
// The path is NOT echoed into the body (nothing a caller sends is reflected) and the logged path has
// its control bytes neutralized — an arbitrary unmatched path reaches this log line, and a decoded
// %0A would otherwise forge a second log entry (the same live defect fixed in both api packages'
// sanitizePath).
func handleNoRoute(w http.ResponseWriter, r *http.Request) {
	rid := newRequestID()
	log.Printf("control request_id=%s method=%s path=%s outcome=error code=not_found reason=root_mux_no_route level=warn",
		rid, r.Method, sanitizeLogPath(r.URL.Path))
	writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
		"code":       "not_found",
		"message":    "The requested resource was not found.",
		"request_id": rid,
	}})
}

// newRequestID mints an opaque correlation id in the same `req_<32 hex>` shape as the two API
// packages, so one log-grep pattern serves all three surfaces.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req_0000000000000000"
	}
	return "req_" + hex.EncodeToString(b[:])
}

// sanitizeLogPath drops any query string and neutralizes control characters, so one request can
// never write two log lines.
func sanitizeLogPath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, p)
}

func (h *HealthServer) ListenAndServe() error            { return h.http.ListenAndServe() }
func (h *HealthServer) Shutdown(c context.Context) error { return h.http.Shutdown(c) }

// handleHealthz is liveness: the process is up.
func (h *HealthServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "at": time.Now().UTC()})
}

// handleReadyz is readiness: DB reachable AND singleton lock genuinely held. Reports the
// real state (not a hardcoded ok) so the harness can assert against it.
//
// FIX-PLAN ITEM 10 (contract §1.9.2): the `checks{}` map is ADDITIVE. The three modules shared not
// one readiness field name — this surface returned {status, db, singleton_lock, at}, Module 1
// {ready, config, pepper, supabaseKey} — so a single dashboard needed a parser per module. The
// uniform part is the ENVELOPE (status/checks/at); the per-module dependency NAMES live inside
// checks{}, which is what lets one shape describe three modules without pretending they have the
// same dependencies. A consumer walks checks{} and renders whatever it finds; it never needs to know
// that this module's names are db and singleton_lock and Module 1's are db and pepper.
//
// The two top-level booleans STAY, unchanged, un-nested. Rule 5 is additive-only, and these are not
// hypothetical consumers: Docs/monitoring.md §2 tells an operator to alert on `db:false` or
// `singleton_lock:false` at the top level, the harness greps `"singleton_lock":true` as its
// anti-vacuity proof that the first Control really holds the lock, and operator-runbook.md links a
// whole recovery procedure off the field name. Nesting them would silently break a live uptime
// monitor — the change would look like a success until the next incident went unnoticed.
//
// Both copies are derived from the SAME two variables, so they cannot disagree by construction —
// and TestReadyz_ChecksMapAgreesWithTheTopLevelFields pins that, because "by construction" stops
// being true the moment someone adds a third dependency to only one of the two places.
func (h *HealthServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	dbOK := h.db != nil && h.db.Pool.Ping(ctx) == nil
	lockOK := h.guard != nil && h.guard.Healthy(ctx)

	code := http.StatusOK
	if !dbOK || !lockOK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"status": statusWord(dbOK && lockOK),
		// The contract's uniform envelope (item 10).
		"checks": map[string]bool{
			"db":             dbOK,
			"singleton_lock": lockOK,
		},
		// Pre-existing top-level fields — see the additive-only note above.
		"db":             dbOK,
		"singleton_lock": lockOK,
		"at":             time.Now().UTC(),
	})
}

func statusWord(ok bool) string {
	if ok {
		return "ready"
	}
	return "not_ready"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
