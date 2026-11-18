package dispatch

import (
	"fmt"
	"strings"
	"time"
)

// buildHeaders constructs the §8 minimum guaranteed header set. The Control owns Message-ID and
// Date generation, so send_logs.headers and the bytes the worker sends are the SAME set (§8.1). The
// worker adds only transport-mandatory MIME framing; it never invents or reorders these (§8.1).
//
//	From       "Name" <addr>, or bare <addr> when from_name is NULL (P3-5: address-only).
//	To         recipient (single in Phase 3).
//	Subject    rendered subject (frozen at job creation).
//	Reply-To   omitted when the job has no reply_to (nullable, §8.1).
//	Message-ID <messageID> (Control-generated; doubles as the §4.3 idempotency fallback id).
//	Date       RFC 5322 send time.
//
// messageID must already be angle-bracketed "<...>". sentAt is the send-time timestamp.
func buildHeaders(fromName, fromAddress, recipient, subject, replyTo, messageID string, sentAt time.Time) map[string]string {
	h := map[string]string{
		"From":       formatFrom(fromName, fromAddress),
		"To":         recipient,
		"Subject":    subject,
		"Message-ID": messageID,
		"Date":       sentAt.Format(time.RFC1123Z), // RFC 5322-compatible date
	}
	if strings.TrimSpace(replyTo) != "" {
		h["Reply-To"] = replyTo
	}
	return h
}

// formatFrom renders the From header: quoted display-name + angle-addr when a name is present,
// bare angle-addr otherwise (P3-5 — no platform-wide default display name).
func formatFrom(fromName, fromAddress string) string {
	if strings.TrimSpace(fromName) == "" {
		return "<" + fromAddress + ">"
	}
	// Quote the display name and escape any embedded quotes/backslashes (RFC 5322 quoted-string).
	escaped := strings.ReplaceAll(fromName, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return fmt.Sprintf("%q <%s>", escaped, fromAddress)
}

// messageIDFor builds the Control-generated Message-ID "<uuid@sending-domain>" (§8.1). The
// sending-domain is derived from from_address; the uuid source is the job id (Phase 8 P8-5
// coordinated refinement — a deterministic-from-job_id id STRENGTHENS the idempotency backstop:
// a crash-after-250 resend of the SAME job looks like the SAME message, and distinct jobs still
// get distinct unique ids). Phase 3 adopts the job-id source now since it costs nothing and never
// weakens idempotency.
func messageIDFor(jobID, fromAddress string) string {
	return "<" + jobID + "@" + sendingDomain(fromAddress) + ">"
}

// sendingDomain extracts the domain from an email address (the part after the last '@').
func sendingDomain(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 && i+1 < len(addr) {
		return addr[i+1:]
	}
	return "localhost"
}

// hasCRLF reports whether s contains a CR or LF (or a NUL). These are the SMTP header/body
// separators; a value carrying one could inject extra headers (e.g. a Bcc) or split into the body
// when composed into the DATA payload. The Control owns the header set, so it rejects such a value
// here rather than emit poisoned wire bytes (defense-in-depth even though Phase 3 content is
// first-party platform content).
func hasCRLF(s string) bool {
	return strings.ContainsAny(s, "\r\n\x00")
}

// validateNoInjection checks every header-bound value + the envelope recipient/from for CR/LF/NUL.
// Returns the offending field name (or "" if clean) so the caller can requeue with a slug rather
// than transmit a header/body-injection payload (§8.1; SMTP header-injection guard).
func validateNoInjection(fromName, fromAddress, recipient, subject, replyTo string) string {
	switch {
	case hasCRLF(fromName):
		return "from_name"
	case hasCRLF(fromAddress):
		return "from_address"
	case hasCRLF(recipient):
		return "recipient"
	case hasCRLF(subject):
		return "subject"
	case hasCRLF(replyTo):
		return "reply_to"
	}
	return ""
}

// sendText is the plain-text alternative that goes on the wire (workerproto.Send.Text) for a claimed job:
// the job's rendered_text (frozen at run from the platform's public.job_items.rendered_text), or "" when the
// message has none. NULL, the empty string and a string with no non-whitespace character are ONE meaning,
// "no text part" (Phase 3 close-out B3: a new field cannot be ambiguous), and "" is omitted from the frame
// (`text,omitempty`), so such a message is built by the worker exactly as it always was, byte for byte.
// Anything else is returned VERBATIM: no trimming, no re-wrapping, no normalisation here (the worker owns the
// MIME rendering). services/worker/src/assemble.js textPartOf applies the same rule on its side.
func sendText(rendered *string) string {
	if rendered == nil || strings.TrimSpace(*rendered) == "" {
		return ""
	}
	return *rendered
}
