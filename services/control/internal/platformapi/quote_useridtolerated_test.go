package platformapi

// quote_useridtolerated_test.go — audit D4 / fix-plan item 5: POST /v1/quote ACCEPTS an optional
// user_id so that ONE request body works against both senders' quote. Real Postgres, real HTTP
// against the production Server (CLAUDE.md Rule 3).
//
// THE DEFECT, precisely. Both senders decode with DisallowUnknownFields. Module 3's quoteReq requires
// user_id (its answer is a per-user feasibility assessment, so that requirement is correct and is NOT
// what changed). Module 2's had only recipient_count. So there was NO body both modules accepted:
//
//	{"recipient_count":10}                 → M2 200 · M3 400 user_id:required
//	{"recipient_count":10,"user_id":"..."} → M3 200 · M2 400 (unknown field)
//
// A platform client therefore had to construct a DIFFERENT body per module for a route the contract
// declares uniform — the exact per-module special-casing the alignment work exists to remove.
//
// WHAT IS DELIBERATELY NOT ASSERTED HERE: that user_id changes the answer. It must not. M2's cost is
// recipient_count × unit_price_credits with no tenant term, and inventing one to make the field
// "meaningful" would be a behaviour change smuggled in under an alignment fix. These tests pin the
// opposite property — the field is INERT — which is a stronger and more falsifiable claim.

import "testing"

// TestQuote_AcceptsOptionalUserID — the whole item, from four sides: absent still works, present is
// accepted, present does NOT change the number, and a malformed value is still rejected.
func TestQuote_AcceptsOptionalUserID(t *testing.T) {
	e := newTestEnv(t, "quoteuserid")

	// A REAL seeded user, not a hand-written UUID literal. The point of this test is that a body the
	// platform would genuinely send to BOTH modules works here, and against module 3 that body carries
	// a user_id that must exist. Using a real one keeps the two sides comparable.
	userID := e.seedUser(t, "quote-userid@example.test")

	// (1) ABSENT — the pre-existing module-2 caller. Rule 5 is additive-only, so this must be
	// byte-for-byte unaffected; that is the half of the item that could regress silently.
	without := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": 10}).requireStatus(t, 200)

	// (2) PRESENT — the shared body. Before this change: 400, unknown field.
	// FAIL-VERIFY: delete UserID from quoteReq and this line fails with 400.
	with := e.do(t, "POST", "/v1/quote",
		map[string]any{"recipient_count": 10, "user_id": userID}).requireStatus(t, 200)

	// (3) THE LOAD-BEARING ASSERTION: the two responses are IDENTICAL, byte for byte. Not "both 200",
	// and not "both have cost 10" — identical. That is what makes "accepted and ignored" a checked
	// property rather than a comment: any future change that lets user_id influence tier, unit price,
	// cost, or any additive field yet to be added fails here without anyone having to remember to
	// extend this test. A weaker assertion (cost only) would pass while tier silently forked.
	if with.raw != without.raw {
		t.Fatalf("user_id changed the quote response — it is documented as accepted-and-IGNORED.\n"+
			"without user_id: %s\nwith user_id:    %s", without.raw, with.raw)
	}
	// Anti-vacuity on the comparison itself: two empty strings are also equal. Assert the body is a
	// real quote before treating equality as evidence.
	if cost, ok := with.body["cost"].(float64); !ok || cost != 10 {
		t.Fatalf("quote(10) cost=%v, want 10 — if the body were empty the equality check above would "+
			"pass vacuously: %s", with.body["cost"], with.raw)
	}

	// (4) MALFORMED IS STILL REJECTED. "Accept and ignore" must not degrade into "accept anything": a
	// caller that supplied a garbage user_id and got a silent 200 would have no way to learn the value
	// was wrong, while the SAME body sent to module 3 returns 400 user_id:malformed. Silently accepting
	// a typo here and rejecting it there reintroduces per-module surprise on the very route this item
	// unified.
	// FAIL-VERIFY: drop the `req.UserID != "" && !uuidRe.MatchString(...)` guard and this fails.
	for _, bad := range []string{"not-a-uuid", "12345", userID + "x", "'; DROP TABLE users; --"} {
		r := e.do(t, "POST", "/v1/quote",
			map[string]any{"recipient_count": 10, "user_id": bad}).requireStatus(t, 400)
		if r.errorCode() != codeValidationFailed {
			t.Fatalf("user_id=%q: code=%s, want %s", bad, r.errorCode(), codeValidationFailed)
		}
		// The SAME slug module 3 and M2's own run/report use for the same fault, so one client renders
		// all of them identically — the whole point of the item.
		got := r.errorDetailFields()
		if len(got) != 1 || got[0] != "user_id:"+issueMalformed {
			t.Fatalf("user_id=%q: details=%v, want exactly [user_id:%s] — the same slug run/report and "+
				"module 3's quote report for a malformed tenant selector: %s", bad, got, issueMalformed, r.raw)
		}
		// Rule 5's two-vocabulary boundary: the internal reason slug and the caller-supplied value
		// never cross it.
		if msg := r.errorMessageOnly(); msg != fixedMessages[codeValidationFailed] {
			t.Fatalf("user_id=%q: message=%q, want the fixed %q", bad, msg, fixedMessages[codeValidationFailed])
		}
		if containsAny(r.raw, "DROP TABLE") {
			t.Fatalf("the rejection echoed the caller-supplied user_id back into the body: %s", r.raw)
		}
	}

	// (5) EMPTY STRING is treated as ABSENT, not as an error — and this is a decision, not an
	// oversight. On a non-pointer string "" and omitted are indistinguishable after decoding, so
	// rejecting "" would make user_id de-facto required for any client that always sends the key, which
	// is exactly the additive-only violation this item was careful to avoid.
	empty := e.do(t, "POST", "/v1/quote",
		map[string]any{"recipient_count": 10, "user_id": ""}).requireStatus(t, 200)
	if empty.raw != without.raw {
		t.Fatalf("user_id=\"\" is meant to be indistinguishable from absent:\nempty:  %s\nabsent: %s",
			empty.raw, without.raw)
	}

	// (6) A NONEXISTENT-BUT-WELL-FORMED user_id is ACCEPTED. Module 2 does not look the tenant up
	// because its cost does not depend on one, and adding a lookup would (a) be a behaviour change and
	// (b) turn quote into a tenant-existence oracle for anyone holding a service token. Module 3 MAY
	// answer differently here — its feasibility block genuinely needs the user to exist — and that
	// divergence is fine: the client sends one body and both return 200-or-a-coded-400, which is the
	// uniformity the contract asks for. Pinned so a future "validate harder" change is a deliberate
	// choice rather than an accident.
	ghost := e.do(t, "POST", "/v1/quote",
		map[string]any{"recipient_count": 10, "user_id": "00000000-0000-4000-8000-000000000000"}).
		requireStatus(t, 200)
	if ghost.raw != without.raw {
		t.Fatalf("a well-formed but unknown user_id changed the quote (or was looked up); module 2's "+
			"cost has no tenant term and quote must not become an existence oracle:\nghost:  %s\nabsent: %s",
			ghost.raw, without.raw)
	}

	// Nothing above wrote anything: quote is side-effect-free, including on its new rejection path.
	var jobs int
	if err := e.admin.QueryRow(bgctx(), `SELECT count(*) FROM send_managed.jobs`).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("quote created %d job row(s); want 0", jobs)
	}
}
