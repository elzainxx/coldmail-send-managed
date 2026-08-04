// classify.go — Phase 5 Chunk 1: the Control-side error classifier (§1). The worker reports RAW
// (an outcome, an internal error_reason slug, and the raw final smtp_response line — Phase 3
// §2.4); the Control — and ONLY the Control — maps that raw report to exactly one
// send_managed.error_class. This is the single classification point (§1, P5-1); no other component
// classifies. It is DETERMINISTIC, code-side, first-match-wins, and adds NO enum value.
//
// Ordering rule (§1): evaluate the §1.1 table top-down, first match wins, using (1) the worker
// outcome, (2) the worker error_reason slug, and (3) the reply code + RFC-3463 enhanced status code
// + text keywords parsed from smtp_response. Reply CODE dominates free text; a text keyword only
// DISAMBIGUATES within a code (throttle-vs-transient, bounce-vs-policy), never overrides a code's
// class. If parsing yields nothing usable → unknown (rows 7/8).
package dispatch

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// The six locked send_managed.error_class values (Phase 1 send_managed_0003 enum — additive-only forever).
// Phase 5 ASSIGNS these; it adds none.
const (
	classBouncePermanent = "bounce_permanent"
	classTransport4xx    = "transport_4xx"
	classAuthFailure     = "auth_failure"
	classThrottle        = "throttle"
	classTimeout         = "timeout"
	classUnknown         = "unknown"
)

// replyCodeRe captures the first 3-digit SMTP reply code at the start of any line (all lines of
// one reply share the code; a multiline reply's final line carries "NNN<space>"). "NNN-" is the
// continuation form, "NNN " the final — either yields the code.
var replyCodeRe = regexp.MustCompile(`(?m)^\s*([2-5]\d{2})[ \t-]`)

// enhancedRe captures an RFC-3463 enhanced status code "class.subject.detail" (class ∈ {2,4,5}).
// It is the primary disambiguator for the overloaded 550 (5.1.x addressing → bounce; 5.7.x
// policy → unknown) and for auth (5.7.8/5.7.0/5.7.11).
var enhancedRe = regexp.MustCompile(`\b([245])\.(\d{1,3})\.(\d{1,3})\b`)

// recipientInvalidText are the row-2 keywords that mark a 550/551/553 as a genuine
// recipient-permanent bounce (addressing fault) rather than a policy rejection (§1.1 note:
// prefer unknown over bounce when ambiguous — a wrongly-bounced valid recipient is unrecoverable).
var recipientInvalidText = []string{
	"no such user", "user unknown", "mailbox unavailable",
	"recipient rejected", "does not exist", "unknown recipient",
	"no mailbox", "invalid recipient",
}

// rateText are the row-4 keywords that mark a 4xx as provider rate-limiting / volume throttling
// (→ throttle: requeue + PAUSE the account) rather than a generic transient (→ transport_4xx: no
// account penalty). Because throttle sheds capacity by pausing, a 4xx is thrown here ONLY on a
// real rate signal; a bare transient 4xx falls through to transport_4xx (§1.1 rows 4 vs 5, and
// the test plan: 421 "too many/rate" → throttle, 451 "temporary local error" → transport_4xx).
var rateText = []string{
	"rate", "too many", "throttl", "try again later", "quota",
	"sending limit", "deferred due to rate", "message rate", "slow down",
}

// classifyReport maps a worker report to exactly one error_class (§1.1). It is a pure function of the
// report's raw fields — no DB, no config, no side effects — so it is trivially unit-testable and
// its result is reproducible from the inputs alone. outcome=accepted has no class (success path,
// never reaches here); it returns "" so a caller that mis-routes a success is not silently mapped
// to a failure class.
func classifyReport(rep workerproto.Report) string {
	switch rep.Outcome {
	case workerproto.OutcomeAccepted:
		return "" // success — not a classifiable failure (caller handles the sent path)
	case workerproto.OutcomeBounced:
		// Row 1-of-2: the worker already decided a hard recipient reject at RCPT TO (550/551/553).
		// A reported bounce is always a permanent recipient fault → bounce_permanent, no penalty.
		return classBouncePermanent
	}

	reason := rep.ErrorReason
	code := parseReplyCode(rep.SMTPResponse)
	enhanced := parseEnhanced(rep.SMTPResponse)
	lower := strings.ToLower(rep.SMTPResponse)

	// Row 2 — bounce_permanent: a 550/551/553 WITH a recipient-invalid signal (addressing 5.1.x
	// or explicit text). Ambiguous 550s (policy/blocklist) deliberately do NOT match here; they
	// fall to row 7 (unknown/requeue) so a possibly-valid recipient is retried, never lost.
	if code == 550 || code == 551 || code == 553 {
		if enhancedIn(enhanced, "5.1.1", "5.1.0", "5.1.3") || containsAny(lower, recipientInvalidText) {
			return classBouncePermanent
		}
	}

	// Row 3 — auth_failure: an authentication/credential fault (an ACCOUNT fault, not a recipient
	// fault) → requeue + quarantine. Triggers: auth reply codes (535/534/530, and 538
	// encryption-required-for-auth), auth enhanced codes (5.7.8/5.7.0/5.7.11), the worker's
	// auth_failed slug, or starttls_unavailable (Phase 3 aborted BEFORE AUTH on a port that
	// required it — the credential could not be presented securely, so the account is unusable as
	// configured; §1.1 note). A pure connectivity TLS handshake failure is NOT here — it is row 6.
	if code == 535 || code == 534 || code == 530 || code == 538 ||
		enhancedIn(enhanced, "5.7.8", "5.7.0", "5.7.11") ||
		reason == workerproto.ReasonAuthFailed || reason == workerproto.ReasonStartTLSUnavail {
		return classAuthFailure
	}

	// Row 4 — throttle: a 4xx carrying a rate/volume signal (enhanced 4.7.0 with rate text, a
	// provider variant like Gmail 4.2.1, or a rate keyword) → requeue + pause. Evaluated BEFORE
	// the generic 4xx (row 5) so a rate-limited transient pauses the account; a transient WITHOUT
	// a rate signal falls through to row 5 and does NOT pause (conservative: no needless capacity
	// shedding). "Local daily-limit-hit" is NOT an SMTP outcome — the locked consume enforces it
	// pre-send (§1.1 note); only the provider's own per-day cap surfacing as a 4xx is handled here.
	if is4xx(code) && (enhanced == "4.2.1" || containsAny(lower, rateText)) {
		return classThrottle
	}

	// Row 5 — transport_4xx: any other 4xx (greylisting 4.2.0/4.3.0, temporary local error 4.3.x,
	// "try again", a mid-session drop reported with a 4xx), or the worker's data_rejected_4xx slug.
	// Transient, likely to succeed on retry/another account → requeue with backoff, no penalty.
	if is4xx(code) || reason == workerproto.ReasonDataRejected4xx {
		return classTransport4xx
	}

	// Row 6 — timeout: connectivity / no-response / deadline. Same POLICY as transport_4xx
	// (requeue + backoff, no penalty), classified distinctly so the taxonomy is complete and
	// metrics are separable. Covers the connectivity/TLS-handshake slugs (connect_refused,
	// timeout, lease_expired, connection_closed, write_failed, starttls_failed — NOT the
	// auth-required starttls_unavailable of row 3). Detection of lease/heartbeat timeouts is
	// Phase 6; Phase 5 owns only the policy once a timeout is declared.
	switch reason {
	case workerproto.ReasonConnectRefused, workerproto.ReasonTimeout, workerproto.ReasonLeaseExpired,
		reasonConnectionClosed, reasonWriteFailed, reasonStartTLSFailed:
		return classTimeout
	}

	// Row 7 — unknown (ambiguous permanent-ish 5xx): a 5xx not matched above (554 generic reject,
	// a policy/blocklist 550 5.7.x, a 552 quota), or the worker's data_rejected_5xx slug. CONSERVATIVE
	// policy: requeue with backoff, NO account penalty — we neither bounce (recipient may be valid)
	// nor quarantine (not proven an account fault). Surfaced for operator review via error_class.
	if is5xx(code) || reason == workerproto.ReasonDataRejected5xx {
		return classUnknown
	}

	// Row 8 — unknown (catch-all): unparseable, accepted_without_id, or anything else. Same
	// conservative requeue-with-backoff policy as row 7.
	return classUnknown
}

// Worker error_reason slugs the worker CAN emit (smtp.js SMTPError.reason → stream.js report) that are
// NOT in the canonical Phase 3 §2.4 set enumerated in workerproto. They are connectivity/TLS faults,
// classified as timeout (row 6) — connectivity, no-response, or a handshake failure with no auth
// requirement. Declared here (not in workerproto) because they are worker-internal transport slugs; the
// classifier must still recognize them rather than fall through to the unknown catch-all.
const (
	reasonConnectionClosed = "connection_closed"
	reasonWriteFailed      = "write_failed"
	reasonStartTLSFailed   = "starttls_failed"
)

// parseReplyCode extracts the leading 3-digit SMTP reply code from a (possibly multiline) raw
// smtp_response. Returns 0 when there is no parseable code (empty/garbage → falls to unknown).
func parseReplyCode(smtpResponse string) int {
	m := replyCodeRe.FindStringSubmatch(smtpResponse)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// parseEnhanced extracts the first RFC-3463 enhanced status code ("5.1.1") from the response, or
// "" if none. Used only to disambiguate within a reply code, never as the sole class driver.
func parseEnhanced(smtpResponse string) string {
	m := enhancedRe.FindStringSubmatch(smtpResponse)
	if m == nil {
		return ""
	}
	return m[1] + "." + m[2] + "." + m[3]
}

func is4xx(code int) bool { return code >= 400 && code <= 499 }
func is5xx(code int) bool { return code >= 500 && code <= 599 }

// enhancedIn reports whether enhanced equals any of the given codes (exact match).
func enhancedIn(enhanced string, codes ...string) bool {
	for _, c := range codes {
		if enhanced == c {
			return true
		}
	}
	return false
}

// containsAny reports whether lower (already lowercased) contains any of the substrings.
func containsAny(lower string, subs []string) bool {
	for _, s := range subs {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}
