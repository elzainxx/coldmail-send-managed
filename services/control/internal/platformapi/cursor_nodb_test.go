package platformapi

// cursor_nodb_test.go — the DATABASE-FREE half of the close-out B2 cursor tests. The refusal of a bad cursor is
// decided in parseReportCursor BEFORE any query, so it can be proved through the real HTTP handler on a Server
// whose database is nil: if the cursor ever reached SQL the handler would dereference nil and the request would
// not answer 400. That makes these tests runnable anywhere (they need no Postgres and cannot skip), and it is the
// reason a cursor that "reaches the database" cannot hide behind a green run. The route tests against real
// Postgres (cursor_forged_test.go) prove the same on the full path plus the paired control that a genuine
// cursor pages the campaign to its end.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

const (
	nodbUserA     = "11111111-1111-4111-8111-111111111111"
	nodbUserB     = "22222222-2222-4222-8222-222222222222"
	nodbCampaignA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	nodbCampaignB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	nodbItemID    = "6f0c1e52-3b7a-4d19-9a4e-2c8f5b1d7e30"
	nodbTS        = "2026-01-01T00:00:00.123456Z"
)

// newNoDBServer builds the REAL Server and handler over a nil pool, with one service token minted at run time.
func newNoDBServer(t *testing.T) (baseURL, token string) {
	t.Helper()
	token = randHex(24)
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	srv := NewServer(nil, NewAuthenticator([]string{string(hash)}), nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, token
}

func nodbReport(t *testing.T, baseURL, token, campaign, user, cursor string) resp {
	t.Helper()
	path := "/v1/report?campaign_id=" + campaign + "&user_id=" + user + "&page_size=2"
	if cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	req, err := http.NewRequest("GET", baseURL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v (a handler panic on a nil database closes the connection: the cursor reached SQL)", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return resp{status: res.StatusCode, body: m, raw: string(raw)}
}

// TestCursor_ForgedValuesAre400BeforeAnyQuery — every forged value, through the real handler, no database.
func TestCursor_ForgedValuesAre400BeforeAnyQuery(t *testing.T) {
	base, token := newNoDBServer(t)
	n := 0
	for name, raw := range forgedCursors() {
		requireMalformedCursor(t, name, nodbReport(t, base, token, nodbCampaignA, nodbUserA, raw))
		n++
	}
	if n < 10 {
		t.Fatalf("only %d forged cursors were exercised — the list shrank", n)
	}
}

// TestCursor_OtherScopeIsRefusedLikeAMalformedOne — a cursor minted for another user, campaign or request is the
// SAME 400, decided before any query, so nothing of the other scope can be read.
func TestCursor_OtherScopeIsRefusedLikeAMalformedOne(t *testing.T) {
	base, token := newNoDBServer(t)
	minted := encodeReportCursor(nodbUserA, nodbCampaignA, nodbTS, nodbItemID)
	_, refMsg, refDetails := requireMalformedCursor(t, "reference", nodbReport(t, base, token, nodbCampaignA, nodbUserA, b64u("x|y")))
	for name, probe := range map[string]struct{ camp, user string }{
		"the same cursor on another campaign":                  {nodbCampaignB, nodbUserA},
		"the same cursor requested as another user":            {nodbCampaignA, nodbUserB},
		"the same cursor on another campaign and another user": {nodbCampaignB, nodbUserB},
	} {
		_, msg, details := requireMalformedCursor(t, name, nodbReport(t, base, token, probe.camp, probe.user, minted))
		if msg != refMsg || !reflect.DeepEqual(details, refDetails) {
			t.Fatalf("%s: refused as (%q, %v), a malformed cursor as (%q, %v) — they must be indistinguishable", name, msg, details, refMsg, refDetails)
		}
	}
}

// TestCursor_GenuineCursorPassesTheCursorStage — the CONTROL of the two tests above, without a database: a cursor
// minted for the request's own user and campaign is accepted by parseReportCursor and yields the same position, and
// the legacy two-field cursor (every cursor minted before the change) is still accepted.
func TestCursor_GenuineCursorPassesTheCursorStage(t *testing.T) {
	mk := func(cursor string) *http.Request {
		return httptest.NewRequest("GET", "/v1/report?cursor="+url.QueryEscape(cursor), nil)
	}
	genuine := encodeReportCursor(nodbUserA, nodbCampaignA, nodbTS, nodbItemID)
	cur, err := parseReportCursor(mk(genuine), nodbUserA, nodbCampaignA)
	if err != nil || cur == nil || cur.CreatedAt != nodbTS || cur.ID != nodbItemID {
		t.Fatalf("a genuine cursor for its own scope was refused or altered: %+v %v", cur, err)
	}
	legacy := b64u(nodbTS + "|" + nodbItemID)
	if cur, err := parseReportCursor(mk(legacy), nodbUserA, nodbCampaignA); err != nil || cur == nil {
		t.Fatalf("a legacy two-field cursor was refused: %+v %v", cur, err)
	}
	if cur, err := parseReportCursor(httptest.NewRequest("GET", "/v1/report", nil), nodbUserA, nodbCampaignA); err != nil || cur != nil {
		t.Fatalf("no cursor must be no position and no error, got %+v %v", cur, err)
	}
	if _, err := parseReportCursor(mk(genuine), nodbUserB, nodbCampaignA); err == nil || !strings.Contains(err.internalReason, "cursor_wrong_scope") {
		t.Fatalf("the same cursor as another user must be refused for its scope, got %v", err)
	}
}
