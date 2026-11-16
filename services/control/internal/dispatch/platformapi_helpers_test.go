package dispatch

// platformapi_helpers_test.go — the dispatch tests that prove an OUTCOME ARRIVED AT THE CALLER (the
// report's `reason`, the cancel route's effect) must drive the REAL Domain-A server, not a re-implementation
// of what it would say. These helpers stand up the production platformapi.Server over httptest against the
// SAME real Postgres clone the dispatcher and outcome handler under test write to, so a test can push a job
// through the real dispatch/outcome paths and then read it back exactly as the platform does.
//
// internal/platformapi does not import internal/dispatch (OD-7 package isolation), so this direction —
// dispatch's TESTS importing platformapi — introduces no cycle and no production dependency.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/platformapi"
	"golang.org/x/crypto/bcrypt"
)

// platformClient is a real platformapi.Server behind httptest, with its own service token.
type platformClient struct {
	baseURL string
	token   string
}

// platformResp is a decoded response: status, the parsed JSON object (nil for a non-object), and the raw
// body (for leak assertions — a two-vocabulary check must read what was actually on the wire).
type platformResp struct {
	status int
	body   map[string]any
	raw    string
}

// newPlatformClient mounts the production Domain-A handler on the env's own pool (send_managed_app, the role
// the real Control runs as) and returns a client for it. MinCost bcrypt keeps the per-request auth cheap.
func (e *dispatchEnv) newPlatformClient(t *testing.T) *platformClient {
	t.Helper()
	token := "dispatch-test-token-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	srv := platformapi.NewServer(e.pool, platformapi.NewAuthenticator([]string{string(hash)}), nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &platformClient{baseURL: ts.URL, token: token}
}

// doErr is do() for a goroutine OTHER than the test's: it reports a transport failure as an error instead of
// calling t.Fatalf, which is only legal on the test goroutine (and which go vet's testinggoroutine check
// rejects inside a `go func`). The forced-interleave tests run the cancel call on a goroutine so the test
// goroutine can hold a transaction open and watch the cancel block behind it.
func (c *platformClient) doErr(method, path string, body any) (platformResp, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return platformResp{}, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, rdr)
	if err != nil {
		return platformResp{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return platformResp{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return platformResp{status: res.StatusCode, body: m, raw: string(raw)}, nil
}

func (c *platformClient) do(t *testing.T, method, path string, body any) platformResp {
	t.Helper()
	r, err := c.doErr(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return r
}

// cancel POSTs /v1/cancel and requires a 200.
func (c *platformClient) cancel(t *testing.T, campaignID, userID string) platformResp {
	t.Helper()
	r := c.do(t, "POST", "/v1/cancel", map[string]any{"campaign_id": campaignID, "user_id": userID})
	if r.status != 200 {
		t.Fatalf("POST /v1/cancel: status=%d, want 200; body=%s", r.status, r.raw)
	}
	return r
}

// jobReport GETs the single-job report by job_id and requires a 200.
func (c *platformClient) jobReport(t *testing.T, jobID, userID string) platformResp {
	t.Helper()
	r := c.do(t, "GET", "/v1/report?job_id="+jobID+"&user_id="+userID, nil)
	if r.status != 200 {
		t.Fatalf("GET /v1/report?job_id=%s: status=%d, want 200; body=%s", jobID, r.status, r.raw)
	}
	return r
}

// campaignReport GETs the campaign report and requires a 200.
func (c *platformClient) campaignReport(t *testing.T, campaignID, userID string) platformResp {
	t.Helper()
	r := c.do(t, "GET", "/v1/report?campaign_id="+campaignID+"&user_id="+userID+"&page_size=200", nil)
	if r.status != 200 {
		t.Fatalf("GET /v1/report?campaign_id=%s: status=%d, want 200; body=%s", campaignID, r.status, r.raw)
	}
	return r
}

// strOrNil reads an optional string key out of a decoded body: ("", false) when the key is JSON null OR
// absent, so a caller that must distinguish "present and null" from "absent" checks hasKey as well.
func (r platformResp) strOrNil(key string) (string, bool) {
	v, ok := r.body[key]
	if !ok || v == nil {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

func (r platformResp) hasKey(key string) bool {
	_, ok := r.body[key]
	return ok
}

// num reads a numeric key (JSON numbers decode as float64).
func (r platformResp) num(key string) int {
	f, _ := r.body[key].(float64)
	return int(f)
}

// totals returns the campaign report's totals object.
func (r platformResp) totals(t *testing.T) map[string]any {
	t.Helper()
	m, ok := r.body["totals"].(map[string]any)
	if !ok {
		t.Fatalf("response has no totals object: %s", r.raw)
	}
	return m
}

// items returns the campaign report's items.results as decoded objects.
func (r platformResp) items(t *testing.T) []map[string]any {
	t.Helper()
	box, ok := r.body["items"].(map[string]any)
	if !ok {
		t.Fatalf("response has no items object: %s", r.raw)
	}
	raw, _ := box["results"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, _ := it.(map[string]any)
		out = append(out, m)
	}
	return out
}

// seedCampaignJobs inserts n QUEUED jobs, all in ONE campaign, for userID, and returns the campaign id
// and the job ids in insertion order. (seedQueuedJobFor mints a fresh campaign per job, which cannot
// express "one campaign of N items" — the unit a cancel acts on.)
func (e *dispatchEnv) seedCampaignJobs(t *testing.T, userID string, n int) (campaignID string, jobIDs []string) {
	t.Helper()
	ctx := context.Background()
	if err := e.admin.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&campaignID); err != nil {
		t.Fatalf("campaign id: %v", err)
	}
	for i := 0; i < n; i++ {
		var jobItemID string
		recipient := "camp-" + campaignID[:8] + "-" + string(rune('a'+i)) + "@example.com"
		if err := e.admin.QueryRow(ctx,
			`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, reply_to, status)
			 VALUES ($1,$2,$3,'Subj','<p>b</p>','reply@ourdomain.com','pending') RETURNING id::text`,
			campaignID, userID, recipient).Scan(&jobItemID); err != nil {
			t.Fatalf("seed job_item: %v", err)
		}
		var jobID string
		tx, err := e.admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor','system',true)"); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("set actor: %v", err)
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO send_managed.jobs (job_item_id, campaign_id, user_id, recipient_email, rendered_subject, rendered_body, reply_to, state, next_attempt_at)
			 VALUES ($1,$2,$3,$4,'Subj','<p>b</p>','reply@ourdomain.com','queued',NOW()) RETURNING id::text`,
			jobItemID, campaignID, userID, recipient).Scan(&jobID); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("seed job: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit job: %v", err)
		}
		jobIDs = append(jobIDs, jobID)
	}
	return campaignID, jobIDs
}

// jobCols reads a few columns the cancel and cap tests assert on, in one round trip.
type jobCols struct {
	State              string
	FailedAttemptCount int
	FirstFailedSet     bool
	CancelRequestedSet bool
	TerminalReason     string // "" when NULL
	CompletedSet       bool
	LeaseNull          bool
	LastErrorClass     string // "" when NULL
	HasPMID            bool
}

func (e *dispatchEnv) jobColsOf(t *testing.T, jobID string) jobCols {
	t.Helper()
	var c jobCols
	var terminal, class, pmid *string
	var completed, lease, first, cancelReq *time.Time
	if err := e.admin.QueryRow(context.Background(),
		`SELECT state::text, failed_attempt_count, first_failed_at, cancel_requested_at, terminal_reason,
		        completed_at, lease_expires_at, last_error_class::text, provider_message_id
		   FROM send_managed.jobs WHERE id=$1`, jobID).
		Scan(&c.State, &c.FailedAttemptCount, &first, &cancelReq, &terminal, &completed, &lease, &class, &pmid); err != nil {
		t.Fatalf("read job cols %s: %v", jobID, err)
	}
	c.FirstFailedSet = first != nil
	c.CancelRequestedSet = cancelReq != nil
	if terminal != nil {
		c.TerminalReason = *terminal
	}
	c.CompletedSet = completed != nil
	c.LeaseNull = lease == nil
	if class != nil {
		c.LastErrorClass = *class
	}
	c.HasPMID = pmid != nil
	return c
}

// eventStates returns "from>to" for every job_events row of a job in seq order ("-" for the genesis from).
func (e *dispatchEnv) eventStates(t *testing.T, jobID string) []string {
	t.Helper()
	chain := e.jobEventChain(t, jobID)
	out := make([]string, 0, len(chain))
	for _, row := range chain {
		from := row[0]
		if from == "" {
			from = "-"
		}
		out = append(out, from+">"+row[1])
	}
	return out
}
