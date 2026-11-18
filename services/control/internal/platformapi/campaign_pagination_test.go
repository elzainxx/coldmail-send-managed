package platformapi

// campaign_pagination_test.go — M2-G5: the campaign report's items cursor skipped one item at every page
// boundary.
//
// THE DEFECT, from campaign_items' own history: the query fetches pageSize+1 rows only to learn whether
// another page exists, and the cursor was then built from the LAST ROW SCANNED — the extra, (pageSize+1)th
// row, which was never returned. The next page starts strictly after the cursor row, so that row was
// skipped: one item lost per page boundary, silently (every page looked complete, next_cursor was
// well-formed, the last page ended on null). It only ever showed on a campaign larger than page_size, and
// nothing in the suite had ever asked for a second page.
//
// WHAT THESE TESTS ASSERT, and why each half is there:
//
//   - the REAL endpoint, walked page by page by following its own next_cursor, returns EXACTLY the
//     campaign's item set — same members, same order, no gap, no duplicate — for a total that is not a
//     multiple of the page size, one that is, one that is exactly one page, and a page size of one;
//   - the walk ends on a null cursor after exactly ceil(total/page_size) pages, every page but the last
//     full;
//   - a PAIRED CONTROL walks the same data with the OLD cursor construction (the cursor taken from the
//     extra row) fed through the SAME checker, and the checker must report a violation naming the lost
//     item. A checker that cannot fail would pass the real endpoint for the wrong reason; this is what
//     proves it can fail, and it fails on precisely the behaviour that was shipped.
//   - the checker is itself unit-tested against a gap, a duplicate and a short page, with no database.
//
// THE DATA is built to be hostile to a keyset: some rows share a created_at to the microsecond (so the id
// tiebreak decides their order) and every created_at carries a sub-second part (so a cursor that rounded it
// to the second would misorder the page boundary).

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pagingItem is one row of the campaign in the order the endpoint must return it.
type pagingItem struct {
	jobItemID string
	createdAt time.Time
	id        string
}

// seedPagingCampaign inserts n queued jobs in one campaign for userID with deliberately awkward created_at
// values, and returns them in the (created_at, id) order the endpoint must serve them in.
func (e *testEnv) seedPagingCampaign(t *testing.T, campaignID, userID string, n int) []pagingItem {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 123456000, time.UTC)
	for i := 0; i < n; i++ {
		itemID := e.seedJobItemCampaign(t, campaignID, userID, fmt.Sprintf("pg-%d-%s@example.com", i, randHex(3)))
		// i/2: rows come in PAIRS that share a created_at exactly, so the id tiebreak is load-bearing.
		created := base.Add(time.Duration(i/2) * time.Second)
		if _, err := e.admin.Exec(bgctx(),
			`INSERT INTO send_managed.jobs (job_item_id, campaign_id, user_id, recipient_email, rendered_subject,
			                                 rendered_body, reply_to, state, next_attempt_at, created_at)
			 VALUES ($1,$2,$3,'x@example.com','S','<p>b</p>','reply@ourdomain.com','queued',NOW(),$4)`,
			itemID, campaignID, userID, created); err != nil {
			t.Fatalf("seed paging job %d: %v", i, err)
		}
	}
	rows, err := e.admin.Query(bgctx(),
		`SELECT job_item_id::text, created_at, id::text FROM send_managed.jobs
		  WHERE campaign_id=$1 ORDER BY created_at ASC, id ASC`, campaignID)
	if err != nil {
		t.Fatalf("read paging order: %v", err)
	}
	defer rows.Close()
	var out []pagingItem
	for rows.Next() {
		var it pagingItem
		if err := rows.Scan(&it.jobItemID, &it.createdAt, &it.id); err != nil {
			t.Fatalf("scan paging order: %v", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("paging order rows: %v", err)
	}
	if len(out) != n {
		t.Fatalf("seeded %d paging jobs, read back %d", n, len(out))
	}
	return out
}

// reportPage is one fetched page of the campaign report's items.
type reportPage struct {
	ids        []string
	nextCursor string // "" when the endpoint said null
	pageSize   int
}

func (e *testEnv) fetchReportPage(t *testing.T, campaignID, userID string, pageSize int, cursor string) reportPage {
	t.Helper()
	path := "/v1/report?campaign_id=" + campaignID + "&user_id=" + userID + "&page_size=" + strconv.Itoa(pageSize)
	if cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	r := e.do(t, "GET", path, nil).requireStatus(t, 200)
	box, ok := r.body["items"].(map[string]any)
	if !ok {
		t.Fatalf("no items object: %s", r.raw)
	}
	var pg reportPage
	results, _ := box["results"].([]any)
	for _, raw := range results {
		m, _ := raw.(map[string]any)
		id, _ := m["job_item_id"].(string)
		pg.ids = append(pg.ids, id)
	}
	pg.nextCursor, _ = box["next_cursor"].(string)
	if ps, ok := box["page_size"].(float64); ok {
		pg.pageSize = int(ps)
	}
	return pg
}

// walkPages fetches page after page. nextCursor decides the cursor for the following page from the page just
// read (the real strategy follows the endpoint's own next_cursor; the control strategy rebuilds it the way
// the buggy code did). It returns every page and whether the walk ended on its own; a runaway walk (a cursor
// that never advances) is cut off at maxPages and reported as not terminated.
func (e *testEnv) walkPages(t *testing.T, campaignID, userID string, pageSize, maxPages int,
	nextCursor func(pg reportPage) string) (pages [][]string, terminated bool) {
	t.Helper()
	cursor := ""
	for i := 0; i < maxPages; i++ {
		pg := e.fetchReportPage(t, campaignID, userID, pageSize, cursor)
		pages = append(pages, pg.ids)
		cursor = nextCursor(pg)
		if cursor == "" {
			return pages, true
		}
	}
	return pages, false
}

// checkPaging is THE checker: it compares what the walk returned with what the campaign holds and returns
// every violation it finds (empty = exact). It is pure so it can be unit-tested without a database.
func checkPaging(pages [][]string, want []string, pageSize int, terminated bool) []string {
	var v []string
	if !terminated {
		v = append(v, "the walk never reached a null next_cursor (a cursor that does not advance)")
	}
	seen := map[string]int{}
	var flat []string
	for _, p := range pages {
		for _, id := range p {
			seen[id]++
			flat = append(flat, id)
		}
	}
	wantSet := map[string]bool{}
	for _, id := range want {
		wantSet[id] = true
	}
	for _, id := range want {
		if seen[id] == 0 {
			v = append(v, "MISSING (a gap): "+id)
		}
	}
	for id, n := range seen {
		if n > 1 {
			v = append(v, fmt.Sprintf("DUPLICATE (%dx): %s", n, id))
		}
		if !wantSet[id] {
			v = append(v, "UNEXPECTED item: "+id)
		}
	}
	if len(v) == 0 && strings.Join(flat, ",") != strings.Join(want, ",") {
		v = append(v, "same members but a different ORDER than (created_at, id)")
	}
	for i, p := range pages {
		last := i == len(pages)-1
		if !last && len(p) != pageSize {
			v = append(v, fmt.Sprintf("page %d has %d items, want a full page of %d", i+1, len(p), pageSize))
		}
		if last && len(want) > 0 && (len(p) == 0 || len(p) > pageSize) {
			v = append(v, fmt.Sprintf("the last page has %d items, want 1..%d", len(p), pageSize))
		}
	}
	if wantPages := (len(want) + pageSize - 1) / pageSize; len(want) > 0 && terminated && len(pages) != wantPages {
		v = append(v, fmt.Sprintf("%d pages, want ceil(%d/%d) = %d", len(pages), len(want), pageSize, wantPages))
	}
	return v
}

// TestPagingChecker_ReportsGapDuplicateShortPageAndNonTermination — the checker itself, no database: an exact
// walk is clean, and each defect it exists to catch is reported with its reason.
func TestPagingChecker_ReportsGapDuplicateShortPageAndNonTermination(t *testing.T) {
	want := []string{"a", "b", "c", "d", "e", "f", "g"}
	if v := checkPaging([][]string{{"a", "b", "c"}, {"d", "e", "f"}, {"g"}}, want, 3, true); len(v) != 0 {
		t.Fatalf("an exact walk was reported as violating: %v", v)
	}
	cases := []struct {
		name       string
		pages      [][]string
		terminated bool
		wantSub    string
	}{
		{"a gap at the boundary (the shipped bug)", [][]string{{"a", "b", "c"}, {"e", "f", "g"}}, true, "MISSING (a gap): d"},
		{"a duplicate", [][]string{{"a", "b", "c"}, {"c", "d", "e"}, {"f", "g"}}, true, "DUPLICATE"},
		{"a short middle page", [][]string{{"a", "b"}, {"c", "d", "e"}, {"f", "g"}}, true, "want a full page"},
		{"a cursor that never advances", [][]string{{"a", "b", "c"}, {"a", "b", "c"}}, false, "never reached a null"},
		{"an unexpected item", [][]string{{"a", "b", "c"}, {"d", "e", "f"}, {"z"}}, true, "UNEXPECTED"},
	}
	for _, tc := range cases {
		v := checkPaging(tc.pages, want, 3, tc.terminated)
		if len(v) == 0 {
			t.Fatalf("%s: the checker reported NO violation — it cannot fail, so it proves nothing", tc.name)
		}
		if !strings.Contains(strings.Join(v, " | "), tc.wantSub) {
			t.Fatalf("%s: violations %v do not mention %q — it failed for a different reason than the one under test", tc.name, v, tc.wantSub)
		}
	}
}

// TestCampaignReport_PaginationNoGapNoDuplicate — the real endpoint, several pages, exact set equality.
func TestCampaignReport_PaginationNoGapNoDuplicate(t *testing.T) {
	e := newTestEnv(t, "pagingreal")
	user := e.seedUser(t, "paging@example.com")

	cases := []struct {
		name            string
		total, pageSize int
	}{
		{"total NOT a multiple of the page size", 7, 3},
		{"total an exact multiple of the page size", 6, 3},
		{"exactly one full page", 3, 3},
		{"a page size of one", 4, 1},
		{"a single item", 1, 3},
	}
	for _, tc := range cases {
		campaign := e.newCampaignID(t)
		items := e.seedPagingCampaign(t, campaign, user, tc.total)
		want := make([]string, len(items))
		for i, it := range items {
			want[i] = it.jobItemID
		}

		pages, terminated := e.walkPages(t, campaign, user, tc.pageSize, tc.total+3,
			func(pg reportPage) string { return pg.nextCursor }) // follow the endpoint's own cursor
		if v := checkPaging(pages, want, tc.pageSize, terminated); len(v) != 0 {
			t.Fatalf("%s (total=%d page_size=%d): the endpoint's own pagination is wrong: %v\npages=%v\nwant =%v",
				tc.name, tc.total, tc.pageSize, v, pages, want)
		}
		// page_size is echoed as served (default/clamp rules are documented separately).
		if first := e.fetchReportPage(t, campaign, user, tc.pageSize, ""); first.pageSize != tc.pageSize {
			t.Fatalf("%s: page_size echoed as %d, want %d", tc.name, first.pageSize, tc.pageSize)
		}
	}
}

// TestCampaignReport_PaginationControl_TheOldCursorIsCaughtBySameChecker — the PAIRED CONTROL. The same data
// is walked through the same endpoint and the same checker, but the cursor for each next page is rebuilt the
// way the shipped code built it: from the EXTRA row (the first row of the following page) instead of the
// last row returned. The checker must report the skipped item for the totals where the bug bites, and must
// report nothing where it cannot (a campaign that fits one page) — which is exactly the profile that hid the
// defect.
func TestCampaignReport_PaginationControl_TheOldCursorIsCaughtBySameChecker(t *testing.T) {
	e := newTestEnv(t, "pagingctrl")
	user := e.seedUser(t, "pagingctrl@example.com")

	for _, tc := range []struct {
		name            string
		total, pageSize int
		wantViolation   bool
	}{
		{"not a multiple", 7, 3, true},
		{"an exact multiple", 6, 3, true},
		{"a campaign that fits one page (the only case the bug could not show on)", 3, 3, false},
	} {
		campaign := e.newCampaignID(t)
		items := e.seedPagingCampaign(t, campaign, user, tc.total)
		want := make([]string, len(items))
		for i, it := range items {
			want[i] = it.jobItemID
		}

		start := 0 // index of the first row the current page returns
		oldWay := func(pg reportPage) string {
			extra := start + tc.pageSize // the (pageSize+1)th row: fetched only to learn there is more
			if extra >= len(items) {
				return "" // no extra row existed, so the old code returned next_cursor = null
			}
			c := encodeReportCursor(user, campaign, items[extra].createdAt.Format(time.RFC3339Nano), items[extra].id)
			start = extra + 1 // the next page begins STRICTLY AFTER the cursor row: the extra row is never served
			return c
		}
		pages, terminated := e.walkPages(t, campaign, user, tc.pageSize, tc.total+3, oldWay)
		v := checkPaging(pages, want, tc.pageSize, terminated)
		if tc.wantViolation {
			if len(v) == 0 {
				t.Fatalf("%s: the OLD cursor construction passed the checker (pages=%v) — the checker cannot fail, so the real-endpoint test above proves nothing", tc.name, pages)
			}
			if !strings.Contains(strings.Join(v, " | "), "MISSING (a gap): "+want[tc.pageSize]) {
				t.Fatalf("%s: violations %v do not name the item at the first page boundary (%s) — it failed for a different reason than the shipped bug", tc.name, v, want[tc.pageSize])
			}
		} else if len(v) != 0 {
			t.Fatalf("%s: the old construction violated the checker on a campaign that fits one page: %v — the control is mis-built", tc.name, v)
		}
	}
}
