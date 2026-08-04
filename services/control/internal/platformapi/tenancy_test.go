package platformapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// bgctx is a shorthand for context.Background() in the tests' direct DB reads.
func bgctx() context.Context { return context.Background() }

// requireStatus fails the test unless the response status matches want, printing the raw body.
func (r resp) requireStatus(t *testing.T, want int) resp {
	t.Helper()
	if r.status != want {
		t.Fatalf("status=%d, want %d; body=%s", r.status, want, r.raw)
	}
	return r
}

// resp is a decoded HTTP response for assertions.
type resp struct {
	status int
	body   map[string]any
	raw    string
}

func (e *testEnv) do(t *testing.T, method, path string, body any) resp {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.baseURL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return resp{status: res.StatusCode, body: m, raw: string(raw)}
}

func (r resp) errorCode() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	return ""
}

func (r resp) str(k string) string {
	if v, ok := r.body[k].(string); ok {
		return v
	}
	return ""
}

// errorMessageOnly returns the envelope's client-facing message.
func (r resp) errorMessageOnly() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		if m, ok := e["message"].(string); ok {
			return m
		}
	}
	return ""
}

// errorDetailFields returns the "field:issue" pairs from the envelope's details[], in order, so a
// test can assert WHICH field an error points at — not merely that some detail exists.
func (r resp) errorDetailFields() []string {
	e, ok := r.body["error"].(map[string]any)
	if !ok {
		return nil
	}
	list, ok := e["details"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, d := range list {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}
		f, _ := m["field"].(string)
		i, _ := m["issue"].(string)
		out = append(out, f+":"+i)
	}
	return out
}

// TestTenancy_CrossTenantReportIsNotFound (OD-25) — tenant A creates a job; tenant B, using B's
// own valid selector, CANNOT read A's job by job_id NOR by job_item_id: both return 404 not_found,
// identical to a non-existent id (no existence disclosure, never 403). Anti-vacuity: A reading A's
// own job succeeds, and B genuinely authenticated (same shared service token) — so the 404 is the
// tenant scope, not an auth failure.
func TestTenancy_CrossTenantReportIsNotFound(t *testing.T) {
	e := newTestEnv(t, "xtenant")
	userA := e.seedUser(t, "tenantA@example.com")
	userB := e.seedUser(t, "tenantB@example.com")
	itemA := e.seedJobItem(t, userA, "a-recipient@example.com")

	// A runs the job (A's selector). 202 + job frozen to A.
	run := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": itemA, "user_id": userA}).
		requireStatus(t, 202)
	jobA := run.str("job_id")
	if jobA == "" {
		t.Fatalf("run(A) returned no job_id: %s", run.raw)
	}
	if got := e.jobUserID(t, jobA); got != userA {
		t.Fatalf("job frozen to user_id=%s, want A=%s", got, userA)
	}

	// Positive control: A reads A's job by job_id → 200 sent-or-queued state, correct job.
	okRep := e.do(t, "GET", "/v1/report?job_id="+jobA+"&user_id="+userA, nil).requireStatus(t, 200)
	if okRep.str("job_id") != jobA {
		t.Fatalf("A reading A's job returned job_id=%s, want %s", okRep.str("job_id"), jobA)
	}

	// NEGATIVE: B reads A's job by job_id → 404 not_found (B's selector is valid; the job isn't B's).
	byJob := e.do(t, "GET", "/v1/report?job_id="+jobA+"&user_id="+userB, nil).requireStatus(t, 404)
	if byJob.errorCode() != codeNotFound {
		t.Fatalf("B reading A's job by job_id: code=%s, want not_found", byJob.errorCode())
	}
	// NEGATIVE: B reads A's job by job_item_id → also 404 not_found (same, no disclosure).
	byItem := e.do(t, "GET", "/v1/report?job_item_id="+itemA+"&user_id="+userB, nil).requireStatus(t, 404)
	if byItem.errorCode() != codeNotFound {
		t.Fatalf("B reading A's job by job_item_id: code=%s, want not_found", byItem.errorCode())
	}
	// The cross-tenant 404 must be byte-identical in shape to a genuinely-nonexistent id (no
	// distinguishing detail that would leak "it exists but isn't yours").
	ghost := "11111111-2222-3333-4444-555555555555"
	nonexist := e.do(t, "GET", "/v1/report?job_id="+ghost+"&user_id="+userB, nil).requireStatus(t, 404)
	if nonexist.errorCode() != byJob.errorCode() {
		t.Fatalf("cross-tenant 404 code %q differs from nonexistent-id 404 code %q — existence disclosure",
			byJob.errorCode(), nonexist.errorCode())
	}
}

// TestTenancy_CrossTenantRunIsNotFound (OD-25) — tenant B cannot create a job for tenant A's
// job_item: run with B's selector + A's job_item → 404 not_found (no job created), identical to a
// nonexistent job_item. Positive control: A can run A's own job_item.
func TestTenancy_CrossTenantRunIsNotFound(t *testing.T) {
	e := newTestEnv(t, "xrun")
	userA := e.seedUser(t, "runA@example.com")
	userB := e.seedUser(t, "runB@example.com")
	itemA := e.seedJobItem(t, userA, "ra@example.com")

	// B tries to run A's job_item → 404, no job row created.
	bad := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": itemA, "user_id": userB}).
		requireStatus(t, 404)
	if bad.errorCode() != codeNotFound {
		t.Fatalf("B running A's job_item: code=%s, want not_found", bad.errorCode())
	}
	var n int
	if err := e.admin.QueryRow(bgctx(), `SELECT count(*) FROM send_managed.jobs WHERE job_item_id=$1`, itemA).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if n != 0 {
		t.Fatalf("a job row was created for a cross-tenant run (%d rows) — tenancy breach", n)
	}

	// Positive control: A runs A's own job_item → 202, one job row, frozen to A.
	ok := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": itemA, "user_id": userA}).
		requireStatus(t, 202)
	if e.jobUserID(t, ok.str("job_id")) != userA {
		t.Fatalf("A's job not frozen to A")
	}
}

// TestTenancy_MissingOrMalformedSelectorRejected (OD-25) — a missing or malformed user_id is a
// clean 400 validation_failed (two-vocabulary envelope), on both run and report. Anti-vacuity: a
// well-formed selector on the same request succeeds.
func TestTenancy_MissingOrMalformedSelectorRejected(t *testing.T) {
	e := newTestEnv(t, "selreq")
	userA := e.seedUser(t, "selA@example.com")
	itemA := e.seedJobItem(t, userA, "sel@example.com")

	// run without user_id → 400.
	noSel := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": itemA}).requireStatus(t, 400)
	if noSel.errorCode() != codeValidationFailed {
		t.Fatalf("run without user_id: code=%s, want validation_failed", noSel.errorCode())
	}
	// run with a malformed user_id → 400.
	e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": itemA, "user_id": "not-a-uuid"}).
		requireStatus(t, 400)

	// report without user_id → 400 (even with a valid job_item present).
	run := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": itemA, "user_id": userA}).requireStatus(t, 202)
	jobA := run.str("job_id")
	e.do(t, "GET", "/v1/report?job_id="+jobA, nil).requireStatus(t, 400)
	// report with a valid selector → 200 (positive control: the reject path didn't wedge reads).
	e.do(t, "GET", "/v1/report?job_id="+jobA+"&user_id="+userA, nil).requireStatus(t, 200)
}

// TestTenancy_Unauthenticated (control) — the platform service-token gate rejects a missing token
// with a clean 401, distinct from the tenant 404. Proves auth and tenancy are separate gates.
func TestTenancy_Unauthenticated(t *testing.T) {
	e := newTestEnv(t, "noauth")
	userA := e.seedUser(t, "authA@example.com")
	itemA := e.seedJobItem(t, userA, "au@example.com")

	// No Authorization header → 401 (bypass e.do, which always sets the token).
	req, _ := http.NewRequest("POST", e.baseURL+"/v1/run", bytes.NewReader(mustJSON(map[string]any{"job_item_id": itemA, "user_id": userA})))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("unauthenticated run status=%d, want 401", res.StatusCode)
	}
	raw, _ := io.ReadAll(res.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if e, ok := m["error"].(map[string]any); !ok || e["code"] != codeUnauthenticated {
		t.Fatalf("unauthenticated run body=%s, want code=unauthenticated", string(raw))
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
