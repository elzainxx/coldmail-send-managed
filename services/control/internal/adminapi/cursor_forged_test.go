package adminapi

// cursor_forged_test.go — Phase 3 close-out B2 on Domain B: every paginated operator route
// (GET /v1/admin/accounts, GET /v1/admin/pools, GET /v1/admin/accounts/{id}/events) answers a bad, malformed or
// FORGED cursor with 400 validation_failed and the standard envelope, never a 500; and a cursor minted by ONE
// route (or for ONE account's events) is refused the SAME way by every other and returns nothing.
//
// THE DEFECT. parseCursor accepted any base64url value that contained a pipe, so `base64url("x|y")` reached
// `$n::timestamptz` / `$n::uuid` in SQL and the cast failed: a 500 for a value typed into a query string.
//
// Per route, with its paired control:
//   - every forged value: 400, code validation_failed, details [cursor:malformed], a request_id, the same fixed
//     message, no items — and never a 5xx;
//   - a cursor taken from ANOTHER route's page (and, for events, from another account's) is the same 400, with
//     nothing of the other route's rows in the body;
//   - CONTROL: the route's own genuine cursors page it to the end EXACTLY (the checker the keyset suite uses),
//     and a genuine cursor is accepted on its own route.

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

func cursorB64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func forgedAdminCursors() map[string]string {
	const goodID = "6f0c1e52-3b7a-4d19-9a4e-2c8f5b1d7e30"
	const goodTS = "2026-01-01T10:00:05.100000Z"
	return map[string]string{
		"x|y, the case that was a 500":     cursorB64("x|y"),
		"timestamp ok, id not a uuid":      cursorB64(goodTS + "|not-a-uuid"),
		"timestamp not a time, id ok":      cursorB64("yesterday|" + goodID),
		"SQL in the id":                    cursorB64(goodTS + "|' OR 1=1 --"),
		"SQL in the timestamp":             cursorB64("2026-01-01'; DROP TABLE x;--|" + goodID),
		"a v1 cursor with a bad timestamp": cursorB64("v1|nope|" + goodID + "|0000000000000000"),
		"a v1 cursor with a bad id":        cursorB64("v1|" + goodTS + "|nope|0000000000000000"),
		"an unknown cursor version":        cursorB64("v9|" + goodTS + "|" + goodID + "|0000000000000000"),
		"a v1 cursor with a forged scope":  cursorB64("v1|" + goodTS + "|" + goodID + "|0123456789abcdef"),
		"not base64url":                    "!!!not-base64!!!",
		"a pipe and nothing else":          cursorB64("|"),
		"oversized":                        strings.Repeat("A", 4096),
	}
}

// requireRefusedCursor asserts the fixed shape of a refusal and returns the message for comparison.
func requireRefusedCursor(t *testing.T, name string, r resp) string {
	t.Helper()
	if r.status >= 500 {
		t.Fatalf("%s: answered %d (a server error) for a caller-supplied value; body=%s", name, r.status, string(r.raw))
	}
	if r.status != 400 {
		t.Fatalf("%s: status=%d, want 400; body=%s", name, r.status, string(r.raw))
	}
	if r.errorCode() != "validation_failed" {
		t.Fatalf("%s: error code %q, want validation_failed; body=%s", name, r.errorCode(), string(r.raw))
	}
	if got := r.errorDetailIssue("cursor"); got != "malformed" {
		t.Fatalf("%s: details[cursor].issue = %q, want malformed; body=%s", name, got, string(r.raw))
	}
	if !strings.HasPrefix(r.errorRequestID(), "req_") {
		t.Fatalf("%s: the envelope has no request_id; body=%s", name, string(r.raw))
	}
	if _, has := r.body["items"]; has {
		t.Fatalf("%s: a refused cursor still returned items: %s", name, string(r.raw))
	}
	return r.errorMessage()
}

func (e *testEnv) listWithCursor(t *testing.T, basePath, cursor string) resp {
	t.Helper()
	path := basePath + "?limit=2"
	if cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	return e.do(t, "GET", path, nil)
}

// TestCursorForged_EveryAdminListRoute_Is400NeverA500 — all three routes, in ONE database (their fixtures do not
// overlap: each truth query reads its own table).
func TestCursorForged_EveryAdminListRoute_Is400NeverA500(t *testing.T) {
	e := newTestEnv(t, "admcursorforged")
	cases := listingCases()
	for _, lc := range cases {
		lc.seed(t, e, 5)
	}
	if len(cases) != 3 {
		t.Fatalf("expected the 3 paginated admin routes, listingCases() has %d — a route was added or removed: cover it here", len(cases))
	}

	for _, lc := range cases {
		base := lc.basePath(t, e)
		want := lc.truth(t, e)
		if len(want) < 5 {
			t.Fatalf("%s: only %d rows seeded — the control walk below would be vacuous", lc.name, len(want))
		}

		// every forged value is a clean 400, with one fixed message
		var firstMsg string
		for name, raw := range forgedAdminCursors() {
			msg := requireRefusedCursor(t, lc.name+" / "+name, e.listWithCursor(t, base, raw))
			if firstMsg == "" {
				firstMsg = msg
			} else if msg != firstMsg {
				t.Fatalf("%s / %s: message %q differs from the other refusals' %q", lc.name, name, msg, firstMsg)
			}
		}

		// CONTROL: the route's own genuine cursors page it to the end exactly
		pages, terminated := e.walkList(t, base, 2, len(want)+3, func(pg listPage) string { return pg.next })
		if v := checkListing(pages, want, 2, terminated); len(v) != 0 {
			t.Fatalf("control: %s: genuine cursors did not page exactly: %v (pages=%v)", lc.name, v, pages)
		}
		if len(pages) < 3 {
			t.Fatalf("control: %s paged in %d page(s) — fewer than 3, so no genuine cursor was exercised", lc.name, len(pages))
		}
	}
}

// TestCursorOfAnotherRoute_IsRefusedTheSameWay — a cursor minted by one admin list route is refused by every
// other; the events cursor is bound to its account.
func TestCursorOfAnotherRoute_IsRefusedTheSameWay(t *testing.T) {
	e := newTestEnv(t, "admcursorscope")
	cases := listingCases()
	for _, lc := range cases {
		lc.seed(t, e, 5)
	}
	bases := make([]string, len(cases))
	cursors := make([]string, len(cases))
	for i, lc := range cases {
		bases[i] = lc.basePath(t, e)
		pg := e.fetchListPage(t, bases[i], 2, "")
		if pg.next == "" {
			t.Fatalf("%s: no next_cursor on page 1", lc.name)
		}
		cursors[i] = pg.next
	}
	refMsg := requireRefusedCursor(t, "reference", e.listWithCursor(t, bases[0], cursorB64("x|y")))

	// every ordered pair of DIFFERENT routes
	pairs := 0
	for from := range cases {
		for to := range cases {
			if from == to {
				continue
			}
			name := "cursor of [" + cases[from].name + "] on [" + cases[to].name + "]"
			r := e.listWithCursor(t, bases[to], cursors[from])
			if msg := requireRefusedCursor(t, name, r); msg != refMsg {
				t.Fatalf("%s: message %q differs from a malformed cursor's %q — they must be indistinguishable", name, msg, refMsg)
			}
			for _, id := range e.idsQuery(t, `SELECT id::text FROM send_managed.pools UNION SELECT id::text FROM send_managed.accounts`) {
				if strings.Contains(string(r.raw), id) {
					t.Fatalf("%s: the refusal body carries a row id %s", name, id)
				}
			}
			pairs++
		}
	}
	if pairs != 6 {
		t.Fatalf("exercised %d route pairs, want 6", pairs)
	}

	// the events cursor is bound to ITS account: the same cursor on another account's events route is refused
	eventsIdx := -1
	for i, lc := range cases {
		if strings.Contains(lc.name, "/events") {
			eventsIdx = i
		}
	}
	if eventsIdx < 0 {
		t.Fatal("no events route among the listing cases")
	}
	otherAccount := "00000000-0000-4000-8000-0000000000aa"
	r := e.listWithCursor(t, "/v1/admin/accounts/"+otherAccount+"/events", cursors[eventsIdx])
	if msg := requireRefusedCursor(t, "events cursor on another account", r); msg != refMsg {
		t.Fatalf("events cursor on another account: message %q differs from %q", msg, refMsg)
	}

	// CONTROL: each cursor on its OWN route is accepted and returns the second page
	for i, lc := range cases {
		r := e.listWithCursor(t, bases[i], cursors[i])
		if r.status != 200 {
			t.Fatalf("control: %s: its own genuine cursor answered %d; body=%s", lc.name, r.status, string(r.raw))
		}
		items, _ := r.body["items"].([]any)
		if len(items) != 2 {
			t.Fatalf("control: %s: page 2 has %d items, want 2", lc.name, len(items))
		}
	}
}
