package platformapi

// GET /v1/report's not-found must name the selector the CALLER SENT (Rule 3 real-infra: real HTTP,
// real Postgres clone).
//
// Before this test, the /v1/report not-found branch hardcoded details[{field:"job_id"}] even when the
// caller looked up by job_item_id — pointing the operator at a field they never supplied, on exactly
// the path (a polling integration keyed by job_item_id) where they have no job_id to go check. The
// same handler's VALIDATION branches already distinguish the two selectors, and store.go's run/quote
// not-founds already name job_item_id, so this one site was the outlier. Found walking
// Docs/coldmail-send-managed-local-test-guide.md Stage 5 step 5 (tenant isolation), where the cross-tenant
// job_item_id probe returned a job_id detail.
//
// THE TENSION this test pins, and why both halves must be asserted together: making the error more
// specific is exactly how existence disclosure gets introduced (OD-25 forbids revealing that a row
// exists but belongs to someone else). Echoing the caller's OWN selector choice is safe — it is
// input they already hold — but only as long as the cross-tenant answer stays indistinguishable from
// the nonexistent-row answer. So this test asserts the field is correct AND that cross-tenant and
// ghost responses remain identical in code, status, message, and details.

import "testing"

// TestReportNotFoundNamesTheSelectorSent — a job_id lookup reports field job_id; a job_item_id
// lookup reports field job_item_id; and neither discloses existence.
func TestReportNotFoundNamesTheSelectorSent(t *testing.T) {
	e := newTestEnv(t, "rep_selector")
	userA := e.seedUser(t, "selA@example.com")
	userB := e.seedUser(t, "selB@example.com")
	itemA := e.seedJobItem(t, userA, "sel-a@example.com")

	run := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": itemA, "user_id": userA}).
		requireStatus(t, 202)
	jobA := run.str("job_id")
	if jobA == "" {
		t.Fatalf("run(A) returned no job_id: %s", run.raw)
	}

	// --- the load-bearing assertion: each selector names ITSELF. ---
	byJob := e.do(t, "GET", "/v1/report?job_id="+jobA+"&user_id="+userB, nil).requireStatus(t, 404)
	if got := byJob.errorDetailFields(); len(got) != 1 || got[0] != "job_id:not_found" {
		t.Fatalf("job_id lookup: details = %v, want [job_id:not_found]", got)
	}
	byItem := e.do(t, "GET", "/v1/report?job_item_id="+itemA+"&user_id="+userB, nil).requireStatus(t, 404)
	if got := byItem.errorDetailFields(); len(got) != 1 || got[0] != "job_item_id:not_found" {
		t.Fatalf("job_item_id lookup: details = %v, want [job_item_id:not_found] "+
			"(the caller sent no job_id, so pointing at job_id is unactionable)", got)
	}
	// Anti-vacuity (Rule 10): the two must genuinely DIFFER. A handler that emitted one hardcoded
	// field for both would satisfy at most one check above; this fails it outright.
	if byJob.errorDetailFields()[0] == byItem.errorDetailFields()[0] {
		t.Fatal("both selectors report the SAME details field — the selector is hardcoded, not derived")
	}

	// --- OD-25 half: more specific must NOT mean more disclosing. A cross-tenant miss and a
	// genuinely-nonexistent id must be indistinguishable for the SAME selector. ---
	ghostJob := "11111111-2222-4333-8444-555555555555"
	ghostItem := "22222222-3333-4444-8555-666666666666"
	ghostByJob := e.do(t, "GET", "/v1/report?job_id="+ghostJob+"&user_id="+userB, nil).requireStatus(t, 404)
	ghostByItem := e.do(t, "GET", "/v1/report?job_item_id="+ghostItem+"&user_id="+userB, nil).requireStatus(t, 404)

	sameShape := func(name string, xtenant, ghost resp) {
		t.Helper()
		if xtenant.errorCode() != ghost.errorCode() {
			t.Fatalf("%s: cross-tenant code %q != nonexistent code %q — existence disclosure",
				name, xtenant.errorCode(), ghost.errorCode())
		}
		gx, gg := xtenant.errorDetailFields(), ghost.errorDetailFields()
		if len(gx) != len(gg) || (len(gx) == 1 && gx[0] != gg[0]) {
			t.Fatalf("%s: cross-tenant details %v != nonexistent details %v — existence disclosure", name, gx, gg)
		}
		if xtenant.errorMessageOnly() != ghost.errorMessageOnly() {
			t.Fatalf("%s: cross-tenant message %q != nonexistent message %q — existence disclosure",
				name, xtenant.errorMessageOnly(), ghost.errorMessageOnly())
		}
	}
	sameShape("by job_id", byJob, ghostByJob)
	sameShape("by job_item_id", byItem, ghostByItem)

	// Positive control: the correctly-scoped read still works, so the 404s above are the tenant
	// scope and the missing row — not a broken endpoint.
	okRep := e.do(t, "GET", "/v1/report?job_id="+jobA+"&user_id="+userA, nil).requireStatus(t, 200)
	if okRep.str("job_id") != jobA {
		t.Fatalf("A reading A's job returned job_id=%q, want %q", okRep.str("job_id"), jobA)
	}
}
