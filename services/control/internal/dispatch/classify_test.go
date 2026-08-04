package dispatch

import (
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// TestClassify_Table drives the §1.1 mapping table with real SMTP response strings (the exact
// forms the T1 sink returns in Chunk 6's full-pipe suite) and asserts the resulting error_class.
// classifyReport is pure, so this is a faithful, non-vacuous oracle: each case's class is driven
// by the real reply code + enhanced code + text, first-match-wins, NOT by an injected class.
func TestClassify_Table(t *testing.T) {
	cases := []struct {
		name string
		rep  workerproto.Report
		want string
	}{
		// Row 1 — success has no class.
		{"accepted_no_class", workerproto.Report{Outcome: workerproto.OutcomeAccepted}, ""},

		// Row 1-of-2 — a reported bounce is always permanent (worker decided at RCPT TO).
		{"reported_bounce", workerproto.Report{Outcome: workerproto.OutcomeBounced, SMTPResponse: "550 5.1.1 No such user"}, classBouncePermanent},

		// Row 2 — 550 WITH recipient-invalid signal → bounce_permanent (enhanced code path).
		{"550_enh_511", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "550 5.1.1 No such user here"}, classBouncePermanent},
		// Row 2 — 550 WITH recipient-invalid signal → bounce_permanent (TEXT path, no enhanced code).
		{"550_text_unknown", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "550 mailbox unavailable"}, classBouncePermanent},
		{"551_text", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "551 User unknown; not local"}, classBouncePermanent},

		// Row 7 (NEGATIVE control for row 2) — a bare policy 550 with NO recipient signal must NOT
		// bounce; conservative → unknown (recipient may be valid, re-route via another account).
		{"550_policy_57x_is_unknown", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "550 5.7.1 Message blocked by policy"}, classUnknown},
		{"550_no_signal_is_unknown", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "550 Rejected"}, classUnknown},

		// Row 3 — auth_failure via reply code, enhanced code, and worker slugs.
		{"535_code", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "535 5.7.8 Authentication credentials invalid"}, classAuthFailure},
		{"534_code", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "534 5.7.9 Please use a different auth mechanism"}, classAuthFailure},
		{"530_code", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "530 5.7.0 Authentication required"}, classAuthFailure},
		{"538_encryption_required", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "538 5.7.11 Encryption required for requested auth"}, classAuthFailure},
		{"auth_failed_slug", workerproto.Report{Outcome: workerproto.OutcomeError, ErrorReason: workerproto.ReasonAuthFailed, SMTPResponse: ""}, classAuthFailure},
		{"starttls_unavail_slug_is_auth", workerproto.Report{Outcome: workerproto.OutcomeError, ErrorReason: workerproto.ReasonStartTLSUnavail, SMTPResponse: ""}, classAuthFailure},

		// Row 4 — throttle: 4xx WITH a rate/volume signal (text keyword OR provider 4.2.1).
		{"421_too_many", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "421 4.7.0 Too many messages, rate limited"}, classThrottle},
		{"450_rate", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "450 4.2.1 The user has exceeded their sending limit"}, classThrottle},
		{"452_quota", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "452 Insufficient system storage, quota exceeded"}, classThrottle},

		// Row 5 (NEGATIVE control for row 4) — a 4xx with NO rate signal is transport_4xx, NOT
		// throttle: it must not pause the account. This is the throttle/transient teeth.
		{"451_temp_local", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "451 4.3.0 Temporary local error, try again"}, classTransport4xx},
		{"450_greylist", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "450 4.2.0 Greylisted, please retry"}, classTransport4xx},
		{"data_4xx_slug", workerproto.Report{Outcome: workerproto.OutcomeError, ErrorReason: workerproto.ReasonDataRejected4xx, SMTPResponse: ""}, classTransport4xx},

		// Row 6 — timeout: connectivity/no-response/deadline slugs (policy == transport_4xx, class
		// distinct). Includes worker-internal transport slugs not in the canonical §2.4 set.
		{"connect_refused", workerproto.Report{Outcome: workerproto.OutcomeError, ErrorReason: workerproto.ReasonConnectRefused, SMTPResponse: ""}, classTimeout},
		{"timeout_slug", workerproto.Report{Outcome: workerproto.OutcomeError, ErrorReason: workerproto.ReasonTimeout, SMTPResponse: ""}, classTimeout},
		{"lease_expired", workerproto.Report{Outcome: workerproto.OutcomeError, ErrorReason: workerproto.ReasonLeaseExpired, SMTPResponse: ""}, classTimeout},
		{"connection_closed", workerproto.Report{Outcome: workerproto.OutcomeError, ErrorReason: reasonConnectionClosed, SMTPResponse: ""}, classTimeout},
		{"starttls_failed_is_timeout", workerproto.Report{Outcome: workerproto.OutcomeError, ErrorReason: reasonStartTLSFailed, SMTPResponse: ""}, classTimeout},

		// Row 7 — unknown: ambiguous permanent-ish 5xx not matched above, and the 5xx slug.
		{"554_generic", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "554 5.7.1 Transaction failed"}, classUnknown},
		{"552_quota_5xx", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "552 5.2.2 Mailbox full"}, classUnknown},
		{"data_5xx_slug", workerproto.Report{Outcome: workerproto.OutcomeError, ErrorReason: workerproto.ReasonDataRejected5xx, SMTPResponse: ""}, classUnknown},

		// Row 8 — unknown catch-all: unparseable, accepted_without_id, empty.
		{"garbage", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "wat"}, classUnknown},
		{"accepted_without_id", workerproto.Report{Outcome: workerproto.OutcomeError, ErrorReason: workerproto.ReasonAcceptedWithoutID, SMTPResponse: ""}, classUnknown},
		{"empty", workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "", ErrorReason: ""}, classUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyReport(c.rep)
			if got != c.want {
				t.Fatalf("classifyReport(%q / reason=%q) = %q, want %q", c.rep.SMTPResponse, c.rep.ErrorReason, got, c.want)
			}
		})
	}
}

// TestClassify_CodeDominatesText proves the ordering rule: the reply CODE decides the class; a
// text keyword only disambiguates WITHIN a code, never overrides it. A 535 (auth) that happens to
// contain the word "rate" must still classify as auth_failure, NOT throttle — code wins.
func TestClassify_CodeDominatesText(t *testing.T) {
	rep := workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "535 5.7.8 auth failed (too many attempts, rate limited)"}
	if got := classifyReport(rep); got != classAuthFailure {
		t.Fatalf("535 with 'rate' text = %q, want auth_failure (code dominates text)", got)
	}
	// And a bounce 550 with recipient text is not derailed by an incidental "try again later".
	rep2 := workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "550 5.1.1 no such user (try again later won't help)"}
	if got := classifyReport(rep2); got != classBouncePermanent {
		t.Fatalf("550 5.1.1 recipient bounce = %q, want bounce_permanent", got)
	}
}

// TestClassify_MultilineReply proves the code parser handles a real multiline SMTP reply (250-
// continuation lines then a final "NNN " line) — the code is taken from the reply, not garbled.
func TestClassify_MultilineReply(t *testing.T) {
	// Use a genuinely-transient final line with NO rate keyword ("try again later" is a throttle
	// signal per §1.1 row 4, so it would correctly classify as throttle — not what this test probes).
	rep := workerproto.Report{Outcome: workerproto.OutcomeError, SMTPResponse: "451-First line of a temporary error\r\n451 4.3.0 temporary local failure"}
	if got := classifyReport(rep); got != classTransport4xx {
		t.Fatalf("multiline 451 = %q, want transport_4xx", got)
	}
}
