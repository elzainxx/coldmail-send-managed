package adminapi

// listing_cursor_test.go — M2-G5's same-bug sweep over every keyset listing on the operator surface:
// GET /v1/admin/accounts, GET /v1/admin/pools, GET /v1/admin/accounts/{id}/events.
//
// WHAT THE SWEEP FOUND. The (pageSize+1) mistake the campaign report had is NOT present here — every one of
// these handlers takes the cursor from items[limit-1], the last row it RETURNS. But a second flaw sat
// beside it: the cursor was built from the resource's `created_at`, which these handlers serialize
// TRUNCATED TO THE SECOND (`to_char(... 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`). The keyset predicate
// `(created_at, id) > (cursor_created_at, cursor_id)` compares against the real, microsecond-precision
// column. A cursor that has rounded its sort key DOWN to …:05Z therefore compares as EARLIER than the row it
// names (that row is at …:05.347), so the next page re-admits that row and every other row of the same
// second: DUPLICATES on the second page, and — when more than `limit` rows share one second, as a bulk
// import or a scripted setup produces — the cursor never advances and a client paging forward loops on the
// same page forever. No test had ever requested a second page of any listing.
//
// THE TESTS build the case that exposes it (rows created within ONE second, distinct to the microsecond),
// walk each listing page by page by following its own next_cursor, and require EXACT set equality with no
// gap and no duplicate and termination on a null cursor — for a total that is not a multiple of the page
// size and for one that is. The PAIRED CONTROL feeds the same data through the same checker with the cursor
// rebuilt the OLD way (from the second-truncated created_at the response itself carries) and requires the
// checker to report the violation, so the checker is shown to be able to fail on precisely this defect.
//
// (GET /v1/admin/config is NOT a keyset listing — it is unpaginated, ordered by its unique primary key —
// and GET /v1/admin/health's lists are bounded snapshots, not pages; the sweep's table in the module
// report names each with its verdict.)

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sameSecond returns the i-th (1-based) timestamp of a run that all fall within ONE second but differ to the
// microsecond: 2026-01-01T10:00:05.{i*100}ms.
func sameSecond(i int) time.Time {
	return time.Date(2026, 1, 1, 10, 0, 5, i*100_000_000, time.UTC)
}

type listPage struct {
	items []map[string]any
	ids   []string
	next  string // "" when the endpoint said null
}

func (e *testEnv) fetchListPage(t *testing.T, basePath string, limit int, cursor string) listPage {
	t.Helper()
	path := basePath + "?limit=" + strconv.Itoa(limit)
	if cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	r := e.do(t, "GET", path, nil).requireStatus(t, 200)
	var pg listPage
	raw, _ := r.body["items"].([]any)
	for _, x := range raw {
		m, _ := x.(map[string]any)
		pg.items = append(pg.items, m)
		id, _ := m["id"].(string)
		pg.ids = append(pg.ids, id)
	}
	pg.next, _ = r.body["next_cursor"].(string)
	return pg
}

// walkList pages through a listing, deciding each next cursor with strategy(pageJustRead).
func (e *testEnv) walkList(t *testing.T, basePath string, limit, maxPages int, strategy func(listPage) string) (pages [][]string, terminated bool) {
	t.Helper()
	cursor := ""
	for i := 0; i < maxPages; i++ {
		pg := e.fetchListPage(t, basePath, limit, cursor)
		pages = append(pages, pg.ids)
		cursor = strategy(pg)
		if cursor == "" {
			return pages, true
		}
	}
	return pages, false
}

// checkListing is the checker: every violation of "exactly the expected rows, in order, in full pages, ending
// on a null cursor". Empty result = the walk was exact.
func checkListing(pages [][]string, want []string, limit int, terminated bool) []string {
	var v []string
	if !terminated {
		v = append(v, "the walk never reached a null next_cursor (the cursor does not advance)")
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
		if seen[id] == 0 {
			v = append(v, "MISSING (a gap): "+id)
		}
	}
	for id, n := range seen {
		if n > 1 {
			v = append(v, fmt.Sprintf("DUPLICATE (%dx): %s", n, id))
		}
		if !wantSet[id] {
			v = append(v, "UNEXPECTED row: "+id)
		}
	}
	if len(v) == 0 && strings.Join(flat, ",") != strings.Join(want, ",") {
		v = append(v, "same members, wrong ORDER")
	}
	for i, p := range pages {
		if i < len(pages)-1 && len(p) != limit {
			v = append(v, fmt.Sprintf("page %d has %d rows, want a full page of %d", i+1, len(p), limit))
		}
	}
	if terminated && len(want) > 0 {
		if wantPages := (len(want) + limit - 1) / limit; len(pages) != wantPages {
			v = append(v, fmt.Sprintf("%d pages, want ceil(%d/%d) = %d", len(pages), len(want), limit, wantPages))
		}
	}
	return v
}

// TestListingChecker_ReportsDuplicatesAndNonTermination — the checker alone, no database.
func TestListingChecker_ReportsDuplicatesAndNonTermination(t *testing.T) {
	want := []string{"a", "b", "c", "d", "e"}
	if v := checkListing([][]string{{"a", "b"}, {"c", "d"}, {"e"}}, want, 2, true); len(v) != 0 {
		t.Fatalf("an exact walk was reported as violating: %v", v)
	}
	for _, tc := range []struct {
		name       string
		pages      [][]string
		terminated bool
		wantSub    string
	}{
		{"the second-truncation defect: the same page again", [][]string{{"a", "b"}, {"a", "b"}}, false, "never reached a null"},
		{"duplicates across the boundary", [][]string{{"a", "b"}, {"b", "c"}, {"d", "e"}}, true, "DUPLICATE"},
		{"a gap", [][]string{{"a", "b"}, {"d", "e"}}, true, "MISSING (a gap): c"},
	} {
		v := checkListing(tc.pages, want, 2, tc.terminated)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " | "), tc.wantSub) {
			t.Fatalf("%s: violations = %v, want one mentioning %q", tc.name, v, tc.wantSub)
		}
	}
}

// listingCase is one listing under test: how to seed N rows within one second, and how to read the truth.
type listingCase struct {
	name     string
	basePath func(t *testing.T, e *testEnv) string // may seed fixtures the path depends on
	seed     func(t *testing.T, e *testEnv, n int)
	truth    func(t *testing.T, e *testEnv) []string // expected ids in (created_at, id) order
}

func (e *testEnv) idsQuery(t *testing.T, sql string, args ...any) []string {
	t.Helper()
	rows, err := e.admin.Query(bgCtx(), sql, args...)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func listingCases() []listingCase {
	var acctPool, eventAcct string
	return []listingCase{
		{
			name:     "GET /v1/admin/accounts",
			basePath: func(t *testing.T, e *testEnv) string { return "/v1/admin/accounts" },
			seed: func(t *testing.T, e *testEnv, n int) {
				e.seedSystemUser(t)
				if err := e.admin.QueryRow(bgCtx(),
					`INSERT INTO send_managed.pools (user_id, kind, name) VALUES ($1,'shared','lc-accounts') RETURNING id::text`,
					SystemUserID).Scan(&acctPool); err != nil {
					t.Fatalf("seed pool: %v", err)
				}
				for i := 1; i <= n; i++ {
					if _, err := e.admin.Exec(bgCtx(),
						`INSERT INTO send_managed.accounts (user_id, pool_id, smtp_host, smtp_port, smtp_username,
						        credential_ciphertext, credential_key_id, credential_nonce, from_address, from_name, created_at)
						 VALUES ($1,$2,'smtp-sink',587,$3,'\x00','test-key','\x00','u@ourdomain.com','Lister',$4)`,
						SystemUserID, acctPool, fmt.Sprintf("lc-%d@ourdomain.com", i), sameSecond(i)); err != nil {
						t.Fatalf("seed account %d: %v", i, err)
					}
				}
			},
			truth: func(t *testing.T, e *testEnv) []string {
				return e.idsQuery(t, `SELECT id::text FROM send_managed.accounts ORDER BY created_at ASC, id ASC`)
			},
		},
		{
			name:     "GET /v1/admin/pools",
			basePath: func(t *testing.T, e *testEnv) string { return "/v1/admin/pools" },
			seed: func(t *testing.T, e *testEnv, n int) {
				e.seedSystemUser(t)
				for i := 1; i <= n; i++ {
					if _, err := e.admin.Exec(bgCtx(),
						`INSERT INTO send_managed.pools (user_id, kind, name, created_at) VALUES ($1,'shared',$2,$3)`,
						SystemUserID, fmt.Sprintf("lc-pool-%d", i), sameSecond(i)); err != nil {
						t.Fatalf("seed pool %d: %v", i, err)
					}
				}
			},
			truth: func(t *testing.T, e *testEnv) []string {
				return e.idsQuery(t, `SELECT id::text FROM send_managed.pools ORDER BY created_at ASC, id ASC`)
			},
		},
		{
			name: "GET /v1/admin/accounts/{id}/events",
			basePath: func(t *testing.T, e *testEnv) string {
				return "/v1/admin/accounts/" + eventAcct + "/events"
			},
			seed: func(t *testing.T, e *testEnv, n int) {
				e.seedSystemUser(t)
				var pool string
				if err := e.admin.QueryRow(bgCtx(),
					`INSERT INTO send_managed.pools (user_id, kind, name) VALUES ($1,'shared','lc-events') RETURNING id::text`,
					SystemUserID).Scan(&pool); err != nil {
					t.Fatalf("seed pool: %v", err)
				}
				if err := e.admin.QueryRow(bgCtx(),
					`INSERT INTO send_managed.accounts (user_id, pool_id, smtp_host, smtp_port, smtp_username,
					        credential_ciphertext, credential_key_id, credential_nonce, from_address, from_name)
					 VALUES ($1,$2,'smtp-sink',587,'lc-ev@ourdomain.com','\x00','test-key','\x00','u@ourdomain.com','Lister') RETURNING id::text`,
					SystemUserID, pool).Scan(&eventAcct); err != nil {
					t.Fatalf("seed account: %v", err)
				}
				for i := 1; i <= n; i++ {
					if _, err := e.admin.Exec(bgCtx(),
						`INSERT INTO send_managed.account_events (account_id, user_id, actor, action, created_at)
						 VALUES ($1,$2,'admin','updated',$3)`, eventAcct, SystemUserID, sameSecond(i)); err != nil {
						t.Fatalf("seed event %d: %v", i, err)
					}
				}
			},
			truth: func(t *testing.T, e *testEnv) []string {
				return e.idsQuery(t, `SELECT id::text FROM send_managed.account_events WHERE account_id=$1 ORDER BY created_at ASC, id ASC`, eventAcct)
			},
		},
	}
}

// TestListings_KeysetCursorIsExactAcrossPages — every keyset listing, rows all created within ONE second.
func TestListings_KeysetCursorIsExactAcrossPages(t *testing.T) {
	for _, lc := range listingCases() {
		for _, tc := range []struct {
			name         string
			total, limit int
		}{
			{"total NOT a multiple of the limit", 5, 2},
			{"total an exact multiple of the limit", 6, 2},
		} {
			suffix := "lk" + strconv.Itoa(len(lc.name)) + strconv.Itoa(tc.total)
			e := newTestEnv(t, suffix)
			lc.seed(t, e, tc.total)
			base := lc.basePath(t, e)
			want := lc.truth(t, e)
			if len(want) != tc.total {
				t.Fatalf("%s: seeded %d rows, the truth query returned %d — the fixture is wrong", lc.name, tc.total, len(want))
			}

			pages, terminated := e.walkList(t, base, tc.limit, tc.total+3, func(pg listPage) string { return pg.next })
			if v := checkListing(pages, want, tc.limit, terminated); len(v) != 0 {
				t.Fatalf("%s [%s] (total=%d limit=%d): %v\npages=%v\nwant =%v", lc.name, tc.name, tc.total, tc.limit, v, pages, want)
			}
		}
	}
}

// TestListings_KeysetControl_TheOldSecondTruncatedCursorIsCaught — the PAIRED CONTROL. The same data, the same
// endpoint, the same checker; only the cursor is rebuilt the old way, from the `created_at` the response
// itself carries (second precision) and the last returned id. The checker MUST report a violation for every
// listing — a checker that passes this would have passed the shipped defect.
func TestListings_KeysetControl_TheOldSecondTruncatedCursorIsCaught(t *testing.T) {
	for _, lc := range listingCases() {
		e := newTestEnv(t, "lc"+strconv.Itoa(len(lc.name)))
		lc.seed(t, e, 5)
		base := lc.basePath(t, e)
		want := lc.truth(t, e)

		oldWay := func(pg listPage) string {
			if pg.next == "" || len(pg.items) == 0 {
				return "" // the endpoint saw no further page; the old and new constructions agree here
			}
			last := pg.items[len(pg.items)-1]
			createdAt, _ := last["created_at"].(string) // "2026-01-01T10:00:05Z" — truncated to the second
			id, _ := last["id"].(string)
			if !strings.HasSuffix(createdAt, "05Z") || strings.Contains(createdAt, ".") {
				t.Fatalf("%s: the serialized created_at is %q, expected the second-truncated form this control depends on", lc.name, createdAt)
			}
			// The OLD construction, byte for byte: a legacy two-field cursor (still accepted, see pagecursor) built from the
			// second-truncated created_at the response carries.
			return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + id))
		}
		pages, terminated := e.walkList(t, base, 2, 8, oldWay)
		v := checkListing(pages, want, 2, terminated)
		if len(v) == 0 {
			t.Fatalf("%s: the OLD second-truncated cursor PASSED the checker (pages=%v) — the checker cannot fail on this defect, so the exactness test above proves nothing", lc.name, pages)
		}
		joined := strings.Join(v, " | ")
		if !strings.Contains(joined, "DUPLICATE") && !strings.Contains(joined, "never reached a null") {
			t.Fatalf("%s: violations %v name neither a duplicate nor a non-advancing cursor — the control failed for a different reason than the defect", lc.name, v)
		}
	}
}
