package platformapi

// cursor_forged_test.go — Phase 3 close-out B2: a bad, malformed or FORGED cursor on the campaign report
// (the one paginated route of Domain A) answers 400 with the standard envelope, never a 500; and a cursor
// minted for ANOTHER user or campaign is refused the SAME way and returns nothing of the other's data.
//
// THE DEFECT. parseReportCursor accepted any base64url value that contained a pipe. `base64url("x|y")` is
// well-formed base64 and has a pipe, so it passed, reached `$3::timestamptz` / `$4::uuid` in the SQL, and the
// cast failed: a 500 internal_error for a value a caller typed into a query string. (The contract said 400.)
//
// WHAT IS ASSERTED, with its paired control:
//   - every forged value in forgedCursors answers 400 validation_failed with details [cursor:malformed], the
//     fixed message and a request_id, and NEVER 5xx (reason asserted, not just "not 200");
//   - CONTROL: a genuine cursor, taken from the previous page's next_cursor, still pages to the end and returns
//     exactly the campaign's items (the same checker the pagination suite uses);
//   - a cursor minted for another campaign of the SAME user, for another user's campaign, or for the same
//     campaign requested as another user, is a 400 indistinguishable from the malformed one (same code, same
//     message, same details) and the body carries no item of any campaign;
//   - CONTROL: the same cursor on its own campaign and user is accepted and returns the next page.

import (
	"encoding/base64"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func b64u(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// forgedCursors are values a caller could put in ?cursor=. The first group is the case the research found to
// be a 500 (well-formed base64 with a pipe, but not a timestamp and a uuid).
func forgedCursors() map[string]string {
	const goodID = "6f0c1e52-3b7a-4d19-9a4e-2c8f5b1d7e30"
	const goodTS = "2026-01-01T00:00:00.123456Z"
	return map[string]string{
		"x|y, the case that was a 500":     b64u("x|y"),
		"timestamp ok, id not a uuid":      b64u(goodTS + "|not-a-uuid"),
		"timestamp not a time, id ok":      b64u("yesterday|" + goodID),
		"SQL in the id":                    b64u(goodTS + "|' OR 1=1 --"),
		"SQL in the timestamp":             b64u("2026-01-01'; DROP TABLE x;--|" + goodID),
		"a v1 cursor with a bad timestamp": b64u("v1|nope|" + goodID + "|0000000000000000"),
		"a v1 cursor with a bad id":        b64u("v1|" + goodTS + "|nope|0000000000000000"),
		"an unknown cursor version":        b64u("v9|" + goodTS + "|" + goodID + "|0000000000000000"),
		"a v1 cursor with a forged scope":  b64u("v1|" + goodTS + "|" + goodID + "|0123456789abcdef"),
		"not base64url":                    "!!!not-base64!!!",
		"base64 with padding":              base64.URLEncoding.EncodeToString([]byte(goodTS + "|" + goodID + "x")),
		"a pipe and nothing else":          b64u("|"),
		"oversized":                        strings.Repeat("A", 4096),
	}
}

// requireMalformedCursor asserts the fixed shape of the refusal and returns its identity for comparison.
func requireMalformedCursor(t *testing.T, name string, r resp) (code, message string, details []string) {
	t.Helper()
	if r.status >= 500 {
		t.Fatalf("%s: answered %d (a server error) for a caller-supplied value; body=%s", name, r.status, r.raw)
	}
	if r.status != 400 {
		t.Fatalf("%s: status=%d, want 400; body=%s", name, r.status, r.raw)
	}
	if r.errorCode() != "validation_failed" {
		t.Fatalf("%s: error code %q, want validation_failed; body=%s", name, r.errorCode(), r.raw)
	}
	if want := []string{"cursor:malformed"}; !reflect.DeepEqual(r.errorDetailFields(), want) {
		t.Fatalf("%s: details %v, want %v", name, r.errorDetailFields(), want)
	}
	eb, _ := r.body["error"].(map[string]any)
	if rid, _ := eb["request_id"].(string); !strings.HasPrefix(rid, "req_") {
		t.Fatalf("%s: the envelope has no request_id: %s", name, r.raw)
	}
	if _, has := r.body["items"]; has {
		t.Fatalf("%s: a refused cursor still returned items: %s", name, r.raw)
	}
	return r.errorCode(), r.errorMessageOnly(), r.errorDetailFields()
}

func (e *testEnv) reportWithCursor(t *testing.T, campaignID, userID, cursor string) resp {
	t.Helper()
	path := "/v1/report?campaign_id=" + campaignID + "&user_id=" + userID + "&page_size=2"
	if cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	return e.do(t, "GET", path, nil)
}

// TestCursorForged_CampaignReport_Is400NeverA500 — the route, every forged value, plus the control.
func TestCursorForged_CampaignReport_Is400NeverA500(t *testing.T) {
	e := newTestEnv(t, "cursorforged")
	user := e.seedUser(t, "cursorforged@example.com")
	campaign := e.newCampaignID(t)
	items := e.seedPagingCampaign(t, campaign, user, 5)

	var firstMessage string
	for name, raw := range forgedCursors() {
		if raw == "" {
			continue // an empty value is "no cursor": the first page (asserted below)
		}
		_, msg, _ := requireMalformedCursor(t, name, e.reportWithCursor(t, campaign, user, raw))
		if firstMessage == "" {
			firstMessage = msg
		} else if msg != firstMessage {
			t.Fatalf("%s: message %q differs from the other refusals' %q (the message is one fixed sentence)", name, msg, firstMessage)
		}
	}
	if firstMessage == "" {
		t.Fatal("no forged cursor was exercised — the test is vacuous")
	}

	// An EMPTY cursor parameter is "no cursor", not a forgery: the first page.
	if r := e.reportWithCursor(t, campaign, user, ""); r.status != 200 {
		t.Fatalf("control: no cursor answered %d, want 200; body=%s", r.status, r.raw)
	}

	// CONTROL (the paired half): genuine cursors, taken from the previous page, page the campaign to its end.
	want := make([]string, len(items))
	for i, it := range items {
		want[i] = it.jobItemID
	}
	pages, terminated := e.walkPages(t, campaign, user, 2, 8, func(pg reportPage) string { return pg.nextCursor })
	if v := checkPaging(pages, want, 2, terminated); len(v) != 0 {
		t.Fatalf("control: genuine cursors did not page the campaign exactly: %v (pages=%v)", v, pages)
	}
	if len(pages) != 3 {
		t.Fatalf("control: 5 items at page_size 2 must be 3 pages, got %d", len(pages))
	}
}

// TestCursorOfAnotherScope_IsRefusedTheSameWay — a cursor minted for ANOTHER user or campaign is a 400
// indistinguishable from a malformed cursor, and nothing of the other scope's data is returned.
func TestCursorOfAnotherScope_IsRefusedTheSameWay(t *testing.T) {
	e := newTestEnv(t, "cursorscope")
	userA := e.seedUser(t, "cursorscope-a@example.com")
	userB := e.seedUser(t, "cursorscope-b@example.com")
	campA := e.newCampaignID(t)
	campA2 := e.newCampaignID(t)
	campB := e.newCampaignID(t)
	itemsA := e.seedPagingCampaign(t, campA, userA, 5)
	// user A has ONE active campaign at a time in the real run path, but these rows are seeded directly, so a
	// second campaign for A is only data here.
	e.seedPagingCampaign(t, campA2, userA, 5)
	itemsB := e.seedPagingCampaign(t, campB, userB, 5)

	cursorFor := func(camp, user string) string {
		pg := e.fetchReportPage(t, camp, user, 2, "")
		if pg.nextCursor == "" {
			t.Fatalf("no next_cursor on page 1 of campaign %s", camp)
		}
		return pg.nextCursor
	}
	curA, curA2, curB := cursorFor(campA, userA), cursorFor(campA2, userA), cursorFor(campB, userB)

	// The reference refusal: a malformed cursor on A's own campaign.
	refCode, refMsg, refDetails := requireMalformedCursor(t, "malformed reference", e.reportWithCursor(t, campA, userA, b64u("x|y")))

	otherIDs := map[string]bool{}
	for _, it := range itemsB {
		otherIDs[it.jobItemID] = true
	}
	for name, probe := range map[string]struct{ camp, user, cursor string }{
		"another user's cursor on my campaign":                        {campA, userA, curB},
		"another campaign (same user) cursor on my campaign":          {campA, userA, curA2},
		"my cursor, my campaign id, but requested as another user":    {campA, userB, curA},
		"another user's cursor on their campaign requested as myself": {campB, userA, curB},
	} {
		r := e.reportWithCursor(t, probe.camp, probe.user, probe.cursor)
		code, msg, details := requireMalformedCursor(t, name, r)
		if code != refCode || msg != refMsg || !reflect.DeepEqual(details, refDetails) {
			t.Fatalf("%s: refused as (%s, %q, %v), but a malformed cursor is (%s, %q, %v) — the two must be indistinguishable", name, code, msg, details, refCode, refMsg, refDetails)
		}
		for id := range otherIDs {
			if strings.Contains(r.raw, id) {
				t.Fatalf("%s: the refusal body carries another scope's job_item_id %s", name, id)
			}
		}
	}

	// CONTROL: the same cursor on ITS OWN campaign and user is accepted and returns the next page exactly.
	r := e.reportWithCursor(t, campA, userA, curA).requireStatus(t, 200)
	box, _ := r.body["items"].(map[string]any)
	results, _ := box["results"].([]any)
	var got []string
	for _, raw := range results {
		m, _ := raw.(map[string]any)
		id, _ := m["job_item_id"].(string)
		got = append(got, id)
	}
	if want := []string{itemsA[2].jobItemID, itemsA[3].jobItemID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("control: page 2 of campaign A = %v, want %v", got, want)
	}
}
