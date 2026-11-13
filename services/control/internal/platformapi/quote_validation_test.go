package platformapi

// quote_validation_test.go — the details[] vocabulary on POST /v1/quote. Real Postgres, real HTTP
// against the production Server (CLAUDE.md Rule 3).
//
// WHAT THIS PINS, and why it is not covered by quote_test.go: quote_test.go proves the NUMBER is
// right. These prove the REJECTIONS are diagnosable. Two defects found walking Stage 7 of
// Docs/install-and-deploy.md#part-7--developing-and-testing-locally:
//
//  1. `{"recipient_count": -5}` reported details[] = [recipient_count:required] — telling the caller
//     a field it had plainly supplied was missing. run/report on the SAME server already distinguish
//     absent (required) from present-but-bad (malformed) on user_id, so this was an inconsistency
//     inside one API surface, not a design choice.
//  2. `{"recipient_count": "7"}` reported details[] = [] — validation_failed naming no field at all,
//     so a UI has nothing to attach the error to.
//
// ANTI-VACUITY (Rule 10 in spirit, applied to a validation test): every case here asserts that the
// two issue slugs genuinely DIFFER from each other, not merely that each is non-empty — a
// regression that re-collapses them to one slug fails these tests. And each rejection is paired
// with a positive control proving the same request shape SUCCEEDS with a valid value, so a test
// cannot pass because quote is broken for everything.

import (
	"strings"
	"testing"
)

// TestQuote_AbsentVsNegativeRecipientCountAreDistinctIssues — defect 1. An OMITTED/null
// recipient_count is `required`; a PRESENT-BUT-NEGATIVE one is `out_of_range`. Both are 400
// validation_failed (the code and status are unchanged — only details[].issue distinguishes them),
// and 0 is accepted as a legitimate count.
func TestQuote_AbsentVsNegativeRecipientCountAreDistinctIssues(t *testing.T) {
	e := newTestEnv(t, "quotevalidation")

	// Positive control FIRST: the endpoint works, so a rejection below is about the value, not the
	// endpoint. 0 is VALID (a zero-recipient campaign costs 0) — the boundary next to the defect.
	zero := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": 0}).requireStatus(t, 200)
	if cost, ok := zero.body["cost"].(float64); !ok || cost != 0 {
		t.Fatalf("quote(0) cost=%v, want 0 — zero is a valid count, not a rejection: %s", zero.body["cost"], zero.raw)
	}

	// ABSENT (field omitted entirely) → required.
	omitted := e.do(t, "POST", "/v1/quote", map[string]any{}).requireStatus(t, 400)
	omittedIssues := omitted.errorDetailFields()
	if len(omittedIssues) != 1 || omittedIssues[0] != "recipient_count:"+issueRequired {
		t.Fatalf("omitted recipient_count: details=%v, want exactly [recipient_count:%s]: %s",
			omittedIssues, issueRequired, omitted.raw)
	}

	// EXPLICIT null is the same thing as absent (*int stays nil) → required.
	null := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": nil}).requireStatus(t, 400)
	nullIssues := null.errorDetailFields()
	if len(nullIssues) != 1 || nullIssues[0] != "recipient_count:"+issueRequired {
		t.Fatalf("null recipient_count: details=%v, want exactly [recipient_count:%s]: %s",
			nullIssues, issueRequired, null.raw)
	}

	// PRESENT-BUT-NEGATIVE → out_of_range. The value decoded fine and is the right type; it is the
	// number that is outside the allowed domain.
	neg := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": -5}).requireStatus(t, 400)
	negIssues := neg.errorDetailFields()
	if len(negIssues) != 1 || negIssues[0] != "recipient_count:"+issueOutOfRange {
		t.Fatalf("recipient_count=-5: details=%v, want exactly [recipient_count:%s] — a supplied field must not be reported as missing: %s",
			negIssues, issueOutOfRange, neg.raw)
	}

	// THE LOAD-BEARING ASSERTION: the two genuinely differ. Without this a future change that
	// collapses both back into one slug still satisfies the checks above only if it picks the right
	// one for each — this makes the DISTINCTION itself the thing under test.
	if omittedIssues[0] == negIssues[0] {
		t.Fatalf("absent and negative recipient_count both report %q — a UI cannot tell \"you forgot this field\" from \"that number is not allowed\"", negIssues[0])
	}

	// Both are still the SAME external code and status: this is a details[] refinement, not a
	// contract change (Docs/api-contract.md — details[] is optional and additive).
	if omitted.errorCode() != codeValidationFailed || neg.errorCode() != codeValidationFailed {
		t.Fatalf("codes changed: omitted=%s negative=%s, want both %s",
			omitted.errorCode(), neg.errorCode(), codeValidationFailed)
	}

	// No internal reason leaked to the client either way (Rule 5, two-vocabulary). The message is the
	// one fixed sentence for the code — never "recipient_count_negative" or a decoder string.
	for label, r := range map[string]resp{"omitted": omitted, "negative": neg} {
		if got := r.errorMessageOnly(); got != fixedMessages[codeValidationFailed] {
			t.Fatalf("%s: message=%q, want the fixed %q (no internal reason across the boundary)",
				label, got, fixedMessages[codeValidationFailed])
		}
	}
}

// TestQuote_WrongTypeRecipientCountNamesTheField — defect 2. A wrong-TYPE value is well-formed JSON
// with exactly one field at fault, so details[] must NAME that field instead of being empty.
// Verified for both a string and a non-integer number.
func TestQuote_WrongTypeRecipientCountNamesTheField(t *testing.T) {
	e := newTestEnv(t, "quotewrongtype")

	// Positive control: the correct type succeeds, so the rejections below are about the type.
	ok := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": 7}).requireStatus(t, 200)
	if cost, _ := ok.body["cost"].(float64); cost != 7 {
		t.Fatalf("quote(7) cost=%v, want 7: %s", ok.body["cost"], ok.raw)
	}

	for _, bad := range []any{"7", 2.5, true, []any{7}, map[string]any{"v": 7}} {
		r := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": bad}).requireStatus(t, 400)
		if r.errorCode() != codeValidationFailed {
			t.Fatalf("recipient_count=%#v: code=%s, want %s", bad, r.errorCode(), codeValidationFailed)
		}
		got := r.errorDetailFields()
		if len(got) != 1 || got[0] != "recipient_count:"+issueMalformed {
			t.Fatalf("recipient_count=%#v: details=%v, want exactly [recipient_count:%s] — an empty details[] gives a UI no field to attach the error to: %s",
				bad, got, issueMalformed, r.raw)
		}
		// A wrong TYPE is malformed, distinct from the out_of_range a wrong VALUE gets — the same
		// distinction the sibling test pins, checked from the other side.
		if got[0] == "recipient_count:"+issueOutOfRange {
			t.Fatalf("recipient_count=%#v reported out_of_range; a type fault is malformed", bad)
		}
		// The client message stays the fixed sentence — the decoder's own text (which quotes the Go
		// type and the JSON token) never crosses the boundary (Rule 5).
		if msg := r.errorMessageOnly(); msg != fixedMessages[codeValidationFailed] {
			t.Fatalf("recipient_count=%#v: message=%q leaked decoder detail; want the fixed %q",
				bad, msg, fixedMessages[codeValidationFailed])
		}
	}

	// AN UNKNOWN FIELD gets NO detail, deliberately: its name is attacker-chosen text from the body,
	// and echoing it back into a structured field a UI renders is the two-vocabulary hazard
	// decodeStrict's *json.UnmarshalTypeError narrowing exists to avoid. Still a clean 400.
	unknown := e.do(t, "POST", "/v1/quote",
		map[string]any{"recipient_count": 7, "<script>evil</script>": 1}).requireStatus(t, 400)
	if unknown.errorCode() != codeValidationFailed {
		t.Fatalf("unknown field: code=%s, want %s", unknown.errorCode(), codeValidationFailed)
	}
	if d := unknown.errorDetailFields(); len(d) != 0 {
		t.Fatalf("unknown field produced details=%v — a body-supplied field name must never be echoed into details[]: %s", d, unknown.raw)
	}
	if unknown.raw != "" && containsAny(unknown.raw, "<script>", "evil") {
		t.Fatalf("unknown-field rejection echoed body content back to the client: %s", unknown.raw)
	}

	// Nothing above created state — quote is side-effect-free even on its error paths.
	var jobs int
	if err := e.admin.QueryRow(bgctx(), `SELECT count(*) FROM send_managed.jobs`).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("quote rejections created %d job row(s); want 0", jobs)
	}
}

// containsAny reports whether s contains any of the needles (all non-empty, so the check cannot be
// vacuously true).
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if n == "" {
			panic("containsAny: empty needle would match everything")
		}
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
