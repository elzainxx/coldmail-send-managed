package platformapi

// item_reason.go — M2-G1: WHY an item is in the state it is in.
//
// THE GAP. GET /v1/report carried only {state, attempt_count, provider_message_id, completed_at}. A caller
// that saw `failed` could not tell a pool that ran dry from an item that failed six times, and a caller
// that saw `queued` could not tell "never tried" from "tried and rejected" from "nothing can send right
// now". The platform settles money on these outcomes (a failed item is not charged, a bounced one is), so
// "the platform can tell why an item ended" is the acceptance test, not a nicety.
//
// THE SHAPE (additive, Rule 5 — no field renamed or removed). Every item of the single-job report AND of
// the campaign report gains two keys, ALWAYS present, null when there is nothing to say:
//
//	reason      — a stable string from the closed vocabulary below, published as describe's `item_reason`
//	error_class — the job's last classified failure, an existing send_managed.error_class value
//	              (bounce_permanent | transport_4xx | auth_failure | throttle | timeout | unknown), published
//	              as describe's `error_class`; null when there has been none, and ALWAYS null for `sent`
//
// reason by state:
//
//	sent      null
//	bounced   bounce_permanent
//	failed    pool_exhausted | attempts_exhausted         (jobs.terminal_reason; a failed row that pre-dates
//	                                                       that column is pool_exhausted, its only writer then)
//	cancelled cancelled
//	queued    why it is STILL queued, most immediate cause first:
//	            dispatch_paused       the effective dispatch rung is not `serving`
//	            waiting_for_capacity  the tenant has no eligible account right now (the same predicate as the
//	                                  campaign totals' waiting_for_capacity), or the last handoff found no
//	                                  worker (no_worker_available / push_failed)
//	            no_eligible_account   the dispatcher's last attempt found no account for this item
//	            <class>               the last failed SMTP attempt's class: transport_4xx | auth_failure |
//	                                  throttle | timeout | unknown
//	            null                  it simply has not been tried
//	assigned / sending   null (it is being worked on; error_class still shows the previous attempt's class)
//
// WHAT IS NEVER EXPOSED. No SMTP text, no account, no pool, no worker, no internal reason string beyond the
// vocabulary names above (CLAUDE.md Rule 5, the two-vocabulary rule). last_error_message is READ only to
// tell a dispatcher-side requeue (which never reached SMTP) from a real SMTP failure — both carry
// last_error_class = 'unknown' — and is never serialized.

import "strings"

// The item_reason vocabulary. ADDITIVE-ONLY once published (Rule 5): a value is appended, never renamed or
// removed. The five failure-class names are the SAME strings as the send_managed.error_class labels.
const (
	reasonPoolExhausted      = "pool_exhausted"
	reasonAttemptsExhausted  = "attempts_exhausted"
	reasonBouncePermanent    = "bounce_permanent"
	reasonCancelled          = "cancelled"
	reasonTransport4xx       = "transport_4xx"
	reasonAuthFailure        = "auth_failure"
	reasonThrottle           = "throttle"
	reasonTimeout            = "timeout"
	reasonUnknown            = "unknown"
	reasonNoEligibleAccount  = "no_eligible_account"
	reasonWaitingForCapacity = "waiting_for_capacity"
	reasonDispatchPaused     = "dispatch_paused"
)

// itemReasons is the published set, in contract order: terminal reasons first, then the queued reasons.
// describe_vocabularies.go publishes it; item_reason_test.go proves it is EXACTLY the set itemOutcome can
// produce (both directions), so a reason cannot be emitted without being declared or declared without
// being reachable.
var itemReasons = []string{
	reasonPoolExhausted,
	reasonAttemptsExhausted,
	reasonBouncePermanent,
	reasonCancelled,
	reasonTransport4xx,
	reasonAuthFailure,
	reasonThrottle,
	reasonTimeout,
	reasonUnknown,
	reasonNoEligibleAccount,
	reasonWaitingForCapacity,
	reasonDispatchPaused,
}

// itemFacts is everything itemOutcome needs, read from one send_managed.jobs row plus the two live facts
// that are properties of the tenant rather than of the row.
type itemFacts struct {
	State            string
	TerminalReason   *string // jobs.terminal_reason
	LastErrorClass   *string // jobs.last_error_class
	LastErrorMessage *string // jobs.last_error_message — READ ONLY to recognise a dispatcher-side requeue; never serialized

	DispatchPaused     bool // the effective dispatch rung is not `serving` (queued items only)
	WaitingForCapacity bool // the tenant has no eligible account right now (queued items only)
}

// itemOutcome derives the two published keys. It is a pure function of its input so the whole table can be
// asserted without a database, and so the DB-backed tests only have to prove that the inputs are read and
// the outputs are wired — not re-derive the logic.
func itemOutcome(f itemFacts) (reason, errorClass *string) {
	if f.State == "sent" {
		return nil, nil // delivered: nothing failed that the caller needs to act on
	}
	errorClass = f.LastErrorClass

	switch f.State {
	case "bounced":
		if errorClass == nil {
			errorClass = strPtr(reasonBouncePermanent) // applyBounced always sets it; defensive for a hand-written row
		}
		return strPtr(reasonBouncePermanent), errorClass
	case "failed":
		if f.TerminalReason == nil || *f.TerminalReason == "" {
			// A failed row with no recorded reason pre-dates send_managed_0028 (which backfills it) or was
			// written by an older Control during a rolling deploy. Pool exhaustion was the ONLY writer of
			// `failed` before the attempt cap, so that is what it is.
			return strPtr(reasonPoolExhausted), errorClass
		}
		switch *f.TerminalReason {
		case reasonPoolExhausted, reasonAttemptsExhausted:
			return strPtr(*f.TerminalReason), errorClass
		}
		// A reason this build does not know (a newer Control wrote it): say nothing rather than echo a
		// string the vocabulary never declared. Null is honest; an undeclared value is a client crash.
		return nil, errorClass
	case "cancelled":
		return strPtr(reasonCancelled), errorClass
	case "queued":
		return queuedReason(f), errorClass
	}
	// assigned / sending: in flight, nothing is blocking it.
	return nil, errorClass
}

// queuedReason picks why a queued item is still queued. Live conditions that stop EVERYTHING outrank the
// item's own history: during a pause the most useful answer is "the module is paused", whatever the item
// last failed with.
func queuedReason(f itemFacts) *string {
	if f.DispatchPaused {
		return strPtr(reasonDispatchPaused)
	}
	if f.WaitingForCapacity {
		return strPtr(reasonWaitingForCapacity)
	}
	if r, ok := dispatcherSideRequeueReason(f.LastErrorMessage); ok {
		return strPtr(r)
	}
	if f.LastErrorClass != nil {
		switch *f.LastErrorClass {
		case reasonTransport4xx, reasonAuthFailure, reasonThrottle, reasonTimeout, reasonUnknown:
			return strPtr(*f.LastErrorClass)
		}
	}
	return nil // never tried
}

// dispatcherSideRequeueReason recognises the four requeues the DISPATCHER makes without ever reaching SMTP
// (internal/dispatch dispatch.go requeue() / loop.go) by the exact internal reason they record in
// last_error_message, and maps them to published reasons:
//
//	no_eligible_account          -> no_eligible_account
//	no_worker_available          -> waiting_for_capacity   (waiting on a worker, which is capacity)
//	push_failed                  -> waiting_for_capacity   (the handoff did not happen; it will be retried)
//	header_injection:<field>     -> unknown                (a poisoned item; there is no better word)
//
// These strings are internal to the dispatch package, which this package deliberately does not import
// (OD-7 package isolation), so they are matched by value here and the correspondence is pinned by a test
// that drives the REAL dispatcher paths and reads the result through this package's report
// (internal/dispatch/report_reason_test.go) — a rename on either side fails there.
func dispatcherSideRequeueReason(msg *string) (string, bool) {
	if msg == nil {
		return "", false
	}
	m := *msg
	switch {
	case m == "no_eligible_account":
		return reasonNoEligibleAccount, true
	case m == "no_worker_available", m == "push_failed":
		return reasonWaitingForCapacity, true
	case strings.HasPrefix(m, "header_injection:"):
		return reasonUnknown, true
	}
	return "", false
}

func strPtr(s string) *string { return &s }
