package platformapi

// dispatch_rung.go — fix-plan item 12's INTAKE half. The rung ladder, as this package needs it.
//
// WHY THIS IS A SECOND COPY OF THE LADDER, AND WHAT KEEPS IT FROM DRIFTING.
//
// platformapi does not import internal/dispatch. That is structural package isolation (OD-7's
// precedent, the same reason readUnitPriceCredits is a local one-key reader rather than a call into
// dispatch.LoadConfig, and the same reason internal/adminapi has its own config readers). Not even the
// test files import it — verified, not assumed.
//
// So the rung names and their ordering necessarily exist twice, and a second copy that can silently
// disagree with the first is precisely the defect class this whole alignment pass has been closing: a
// gate whose two halves drift apart while both look correct. A comment telling the next person to
// update both would be worse than nothing, because it reads as protection and provides none.
//
// The mechanism instead is TestDispatchRung_LadderMatchesTheDispatchPackageExactly, which READS
// internal/dispatch/rung.go's source and fails if either the rung names or the severity ordering
// differ from the table below. Both copies are in one repo and one commit, so unlike a cross-module
// code comparison (which can only pin a transcription), this comparison is AUTHORITATIVE: a rung
// added, renamed, or reordered on either side turns the test red immediately.
//
// WHAT THIS FILE DELIBERATELY DOES NOT DUPLICATE. Only the intake question is answered here. Whether
// claims are allowed and whether reclaim may run are dispatch-side concerns with no meaning on an HTTP
// handler, and copying those predicates would create two answers to questions this package cannot
// act on. The severity table is duplicated because the ORDERING is what "refuse_new and everything
// below it" means; the predicates are not.

import (
	"context"
	"encoding/json"
	"log"
)

// The rung names, spelled exactly as internal/dispatch/rung.go spells them, and their severity
// ordering. Held identical to that file by the source-comparison test named above.
const (
	rungServing        = "serving"
	rungAcceptAndDefer = "accept_and_defer"
	rungRefuseNew      = "refuse_new"
	rungStopped        = "stopped"
)

var rungSeverity = map[string]int{
	rungServing:        0,
	rungAcceptAndDefer: 1,
	rungRefuseNew:      2,
	rungStopped:        3,
}

// intakeAllowed reports whether POST /v1/run may accept new work, reading dispatch_mode and
// dispatch_enabled fresh (config-not-code, Rule 1 — a rung pull takes effect on the next request, no
// redeploy and no restart).
//
// It composes the two dials the same way dispatch.resolveRung does — as a MAXIMUM severity, with
// dispatch_enabled=false a floor of accept_and_defer — because two independent levers on one control
// is how half a rung stays pulled (M1's ADR-0015 §1). Here the composition is trivially visible:
// accept_and_defer permits intake, so the floor never affects this predicate's answer. That is not a
// reason to omit it. Writing the floor explicitly means a future rung inserted between
// accept_and_defer and refuse_new gets the composition right without anyone re-deriving it, and the
// no-drift test compares the ordering that makes it true.
//
// FAIL-OPEN ON A READ OR PARSE FAILURE. Returns true. The reasoning is at the call site in handlers.go
// and is the opposite of the claim path's: accepting a job during a config blip is recoverable (it
// sits in `queued`), refusing one may lose customer work permanently. An UNRECOGNIZED mode value is a
// different case and is NOT fail-open — see below.
func (s *Server) intakeAllowed(ctx context.Context) bool {
	mode, ok := s.readDispatchModeRaw(ctx)
	if !ok {
		// Transient/absent: accept. Note that an absent row means the Control itself cannot boot
		// (dispatch.LoadConfig hard-errors on it), so this branch in practice only covers a read blip.
		return true
	}
	if _, known := rungSeverity[mode]; !known {
		// An unrecognized mode resolves to accept_and_defer Control-side (fail toward the safer rung,
		// §1.11.4) — and accept_and_defer ACCEPTS intake, so this surface must accept too or the two
		// halves would disagree about the same garbage value. Logged at level=error to match the
		// Control-side line, so one `grep 'level=error'` finds both.
		log.Printf("platformapi: dispatch_mode=%q unrecognized — intake continues to accept (the Control resolves this value to %q, which accepts) level=error", mode, rungAcceptAndDefer)
		return true
	}
	return rungSeverity[mode] < rungSeverity[rungRefuseNew]
}

// readDispatchModeRaw reads the raw stored mode. It is a one-key local reader for the same reason
// readUnitPriceCredits is (package isolation, OD-7); this package has no generic config-string helper
// to borrow. Returns ok=false on a read or parse failure, which both callers treat explicitly rather
// than folding into an empty-string "unrecognized" — an unreadable row and a garbage value are
// different conditions with different safe answers.
func (s *Server) readDispatchModeRaw(ctx context.Context) (string, bool) {
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = 'dispatch_mode'`).Scan(&raw); err != nil {
		return "", false
	}
	var wrap struct {
		V *string `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil {
		return "", false
	}
	return *wrap.V, true
}
