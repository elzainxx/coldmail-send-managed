package adminapi

// Real-infra test support for the Phase 2 behavioral suite. These tests drive the ACTUAL
// adminapi.Server (the real production handler code) over REAL HTTP (httptest) against a REAL
// Postgres clone — never a mock (CLAUDE.md Rule 3). Each test gets its own template-clone DB
// (real committed rows, cross-connection) so it can seed public.users, transplant ciphertext,
// and set counters directly, exactly like the Phase 1 crypto/roles tests.
//
// Isolation mirrors tests/harness: TEST_ADMIN_DATABASE_URL (admin/superuser DSN) + TEST_TEMPLATE_DB
// (a DB migrated 0000..0014 with send_managed_app's password set). Tests SKIP when unset, so `go
// test` never silently passes on nothing.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const testKeyID = "cred-dek-test"

// testEnv bundles everything a behavioral test drives: the httptest server URL, the operator
// bearer key, the harness-held AES key (same the server encrypts with) + its id, an admin
// connection for direct DB truth, and the clone DSN.
type testEnv struct {
	baseURL  string
	adminKey string
	key      []byte // 32-byte AES-256 key held by the harness AND the server (dev single-key)
	admin    *pgx.Conn
	pool     *pgxpool.Pool
	logBuf   *syncBuffer
	cloneDSN string
}

// newTestEnv clones the template, builds a real Server (pool as send_managed_app, Encrypt-only
// Encryptor with a fresh key), and starts an httptest server. Registers all cleanup.
func newTestEnv(t *testing.T, suffix string) *testEnv {
	t.Helper()
	adminDSN := adminDSN(t)
	template := requireTemplate(t)

	cloneDSN, dropClone := cloneDB(t, adminDSN, template, suffix)
	t.Cleanup(dropClone)

	// Admin connection (superuser) for direct truth + seeding public.users.
	admin := connect(t, cloneDSN)

	// Server pool as send_managed_app (the role the real Control runs as).
	moduleDSN := withUserPass(cloneDSN, "send_managed_app", modulePassword())
	pool, err := pgxpool.New(context.Background(), moduleDSN)
	if err != nil {
		t.Fatalf("module pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Fresh 32-byte key held by the harness; the server encrypts with the SAME key (dev single
	// master key, Phase 1 §0) so the harness can decrypt what the server wrote.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	enc, err := NewEncryptor(key, testKeyID)
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}

	// One operator key + its bcrypt hash.
	adminKey := randHex(24)
	hash, err := bcrypt.GenerateFromPassword([]byte(adminKey), bcrypt.MinCost) // MinCost: fast tests
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	auth := NewAuthenticator([]string{string(hash)})

	// Capture logs to assert the secret never appears (T1/T3). Restore the default logger after.
	logBuf := &syncBuffer{}
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(logBuf)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	srv := NewServer(pool, enc, auth)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &testEnv{
		baseURL:  ts.URL,
		adminKey: adminKey,
		key:      key,
		admin:    admin,
		pool:     pool,
		logBuf:   logBuf,
		cloneDSN: cloneDSN,
	}
}

// --- clone/connect helpers (self-contained; mirror tests/harness) ---

func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_ADMIN_DATABASE_URL not set; skipping real-infra admin API test")
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
	name := "test_adm_" + suffix
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

// bgCtx is a shorthand for context.Background() in the test files' direct DB writes.
func bgCtx() context.Context { return context.Background() }

// syncBuffer is a minimal concurrency-safe buffer for capturing log output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
