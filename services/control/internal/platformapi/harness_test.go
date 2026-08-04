package platformapi

// Real-infra test support for the Phase 3 platform-API (Domain A) behavioral suite. These tests
// drive the ACTUAL platformapi.Server (production handlers) over REAL HTTP (httptest) against a
// REAL Postgres clone migrated 0000..0014 — never a mock (CLAUDE.md Rule 3). They exercise the
// OD-25 module-enforced tenancy: two real tenants with real committed job rows, and a genuine
// cross-tenant HTTP attempt.
//
// Isolation mirrors the adminapi harness: TEST_ADMIN_DATABASE_URL + TEST_TEMPLATE_DB; tests SKIP
// when unset so `go test` never silently passes on nothing.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

type testEnv struct {
	baseURL string
	token   string // the platform service token (plaintext) accepted by the server
	admin   *pgx.Conn
	pool    *pgxpool.Pool
}

// newTestEnv clones the template, builds a real Server (pool as send_managed_app, one service-token
// hash), and starts an httptest server. Registers cleanup.
func newTestEnv(t *testing.T, suffix string) *testEnv {
	t.Helper()
	adminDSN := requireAdminDSN(t)
	template := requireTemplate(t)

	cloneDSN, drop := cloneDB(t, adminDSN, template, suffix)
	t.Cleanup(drop)

	admin := connect(t, cloneDSN)
	moduleDSN := withUserPass(cloneDSN, "send_managed_app", modulePassword())
	pool, err := pgxpool.New(context.Background(), moduleDSN)
	if err != nil {
		t.Fatalf("module pool: %v", err)
	}
	t.Cleanup(pool.Close)

	token := randHex(24)
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.MinCost) // MinCost: fast tests
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	auth := NewAuthenticator([]string{string(hash)})

	srv := NewServer(pool, auth, nil) // kick=nil: run only needs to INSERT + return
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &testEnv{baseURL: ts.URL, token: token, admin: admin, pool: pool}
}

// seedUser inserts a real public.users row and returns its id.
func (e *testEnv) seedUser(t *testing.T, email string) string {
	t.Helper()
	var id string
	if err := e.admin.QueryRow(context.Background(),
		`INSERT INTO public.users (email, is_active) VALUES ($1, true) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedJobItem inserts a real public.job_items row owned by userID and returns its id. Its
// campaign_id defaults to userID itself (fine for single-campaign tests; use seedJobItemCampaign
// when a test needs two DISTINCT campaigns for the same user).
func (e *testEnv) seedJobItem(t *testing.T, userID, recipient string) string {
	t.Helper()
	return e.seedJobItemCampaign(t, userID, userID, recipient)
}

// seedJobItemCampaign inserts a real public.job_items row owned by userID under an EXPLICIT
// campaignID (Phase 4 Chunk 5's one-campaign-per-user tests need two job_items for the SAME user
// under two DIFFERENT campaigns — seedJobItem's userID-as-campaign_id default can't express that).
func (e *testEnv) seedJobItemCampaign(t *testing.T, campaignID, userID, recipient string) string {
	t.Helper()
	var id string
	if err := e.admin.QueryRow(context.Background(),
		`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, reply_to, status)
		 VALUES ($1, $2, $3, 'Subj', '<p>b</p>', 'reply@ourdomain.com', 'pending') RETURNING id`,
		campaignID, userID, recipient).Scan(&id); err != nil {
		t.Fatalf("seed job_item: %v", err)
	}
	return id
}

// jobUserID reads the user_id of a send_managed.jobs row (to assert the tenant was frozen correctly).
func (e *testEnv) jobUserID(t *testing.T, jobID string) string {
	t.Helper()
	var uid string
	if err := e.admin.QueryRow(context.Background(),
		`SELECT user_id::text FROM send_managed.jobs WHERE id=$1`, jobID).Scan(&uid); err != nil {
		t.Fatalf("job user_id: %v", err)
	}
	return uid
}

// --- clone/connect helpers (mirror the adminapi/dispatch harnesses) ---

func requireAdminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_ADMIN_DATABASE_URL not set; skipping real-infra platform API test")
	}
	return dsn
}

func requireTemplate(t *testing.T) string {
	t.Helper()
	tpl := os.Getenv("TEST_TEMPLATE_DB")
	if tpl == "" {
		t.Skip("TEST_TEMPLATE_DB not set; skipping (needs a migrated template DB)")
	}
	return tpl
}

func modulePassword() string {
	if p := os.Getenv("SEND_MANAGED_MODULE_PASSWORD"); p != "" {
		return p
	}
	return "devmodulepass"
}

func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func cloneDB(t *testing.T, adminDSN, templateDB, suffix string) (string, func()) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("clone admin connect: %v", err)
	}
	defer admin.Close(ctx)
	name := "test_pf_" + suffix
	_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdent(name)))
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", quoteIdent(name), quoteIdent(templateDB))); err != nil {
		t.Fatalf("clone db: %v", err)
	}
	cloneDSN := replaceDBName(adminDSN, name)
	cleanup := func() {
		c, err := pgx.Connect(context.Background(), adminDSN)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(),
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()", name)
		_, _ = c.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdent(name)))
	}
	return cloneDSN, cleanup
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func replaceDBName(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		if i := strings.LastIndex(dsn, "/"); i >= 0 {
			return dsn[:i+1] + name
		}
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

func withUserPass(dsn, user, pass string) string {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return dsn
	}
	return dsn[:i+3] + user + ":" + pass + "@" + rest[at+1:]
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
