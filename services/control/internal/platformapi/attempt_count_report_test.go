package platformapi

// attempt_count_report_test.go — Phase 3 close-out B4, the API half: `attempt_count` on GET /v1/report is the
// job's stored saturating counter, VERBATIM, in both shapes (single job and campaign items) — and a job that has not
// been attempted, and one that is cancelled before it is claimed, report 0.
//
// What the counter COUNTS is pinned where it is incremented, against the real dispatcher / outcome handler / reclaim
// loop: internal/dispatch/attempt_count_meaning_test.go. This file pins what the platform READS: no re-derivation, no
// offset, no rename; paired controls are the 0 (never attempted) and the 3 (saturated) ends of the same field.

import (
	"net/url"
	"testing"
)

func (e *testEnv) reportedAttemptCounts(t *testing.T, campaignID, userID, itemID string) (single, inCampaign float64) {
	t.Helper()
	r := e.do(t, "GET", "/v1/report?job_item_id="+itemID+"&user_id="+userID, nil).requireStatus(t, 200)
	single, ok := r.body["attempt_count"].(float64)
	if !ok {
		t.Fatalf("the single-job report has no numeric attempt_count: %s", r.raw)
	}
	c := e.do(t, "GET", "/v1/report?campaign_id="+url.QueryEscape(campaignID)+"&user_id="+userID, nil).requireStatus(t, 200)
	box, _ := c.body["items"].(map[string]any)
	results, _ := box["results"].([]any)
	for _, raw := range results {
		m, _ := raw.(map[string]any)
		if m["job_item_id"] == itemID {
			v, ok := m["attempt_count"].(float64)
			if !ok {
				t.Fatalf("the campaign item has no numeric attempt_count: %s", c.raw)
			}
			return single, v
		}
	}
	t.Fatalf("item %s is not in the campaign report: %s", itemID, c.raw)
	return 0, 0
}

func TestReport_AttemptCountIsTheStoredSaturatingCounterVerbatim(t *testing.T) {
	e := newTestEnv(t, "attemptcount")
	user := e.seedUser(t, "attemptcount@example.com")
	campaign := e.newCampaignID(t)
	item := e.seedJobItemCampaign(t, campaign, user, "attemptcount@example.com")
	job := e.runItem(t, item, user).requireStatus(t, 202).str("job_id")

	// A job that has never been attempted reads 0 in both shapes.
	if s, c := e.reportedAttemptCounts(t, campaign, user, item); s != 0 || c != 0 {
		t.Fatalf("a never-attempted job reports attempt_count single=%v campaign=%v, want 0/0", s, c)
	}
	// The stored value is what is reported, at every value the CHECK allows (0..max_attempts = 3), in both shapes.
	for _, n := range []int{1, 2, 3, 0} {
		if _, err := e.admin.Exec(bgctx(), `UPDATE send_managed.jobs SET attempt_count=$2 WHERE id=$1`, job, n); err != nil {
			t.Fatalf("set attempt_count: %v", err)
		}
		if s, c := e.reportedAttemptCounts(t, campaign, user, item); s != float64(n) || c != float64(n) {
			t.Fatalf("stored attempt_count %d is reported as single=%v campaign=%v", n, s, c)
		}
	}
	// It cannot exceed 3 — the contract says it saturates, and the database refuses a larger value.
	if _, err := e.admin.Exec(bgctx(), `UPDATE send_managed.jobs SET attempt_count=4 WHERE id=$1`, job); err == nil {
		t.Fatal("the database accepted attempt_count = 4 with max_attempts = 3: the documented ceiling is not enforced")
	}
}

func TestReport_ACancelledQueuedJobStillReportsAttemptCountZero(t *testing.T) {
	e := newTestEnv(t, "attemptcountcancel")
	user := e.seedUser(t, "attemptcountcancel@example.com")
	campaign := e.newCampaignID(t)
	item := e.seedJobItemCampaign(t, campaign, user, "attemptcountcancel@example.com")
	e.runItem(t, item, user).requireStatus(t, 202)
	e.do(t, "POST", "/v1/cancel", map[string]any{"campaign_id": campaign, "user_id": user}).requireStatus(t, 200)

	r := e.do(t, "GET", "/v1/report?job_item_id="+item+"&user_id="+user, nil).requireStatus(t, 200)
	if r.body["state"] != "cancelled" {
		t.Fatalf("precondition: the job is %v, want cancelled: %s", r.body["state"], r.raw)
	}
	if s, c := e.reportedAttemptCounts(t, campaign, user, item); s != 0 || c != 0 {
		t.Fatalf("a cancelled, never-claimed job reports attempt_count single=%v campaign=%v, want 0/0 (cancel does not count an attempt)", s, c)
	}
}
