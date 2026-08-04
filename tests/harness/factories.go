package harness

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

// acctSeq gives each inserted account a distinct smtp_username so multiple accounts can live in
// the SAME pool without colliding on the Phase 2 per-pool uniqueness index (send_managed_0013:
// UNIQUE(pool_id, smtp_host, smtp_username)). The AAD/transplant tests only need distinct rows,
// not distinct credentials, so a unique username per insert satisfies the constraint without
// changing what those tests exercise.
var acctSeq atomic.Int64

// Typed factories that insert REAL committed rows into a cloned DB (CLAUDE.md Rule 3 — never
// mocks). They satisfy the real FKs (public.users, public.job_items, send_managed.pools) so Phase 1
// behavioral tests exercise the actual schema, constraints, and triggers.

// SeedUser inserts a real public.users row and returns its id.
func SeedUser(t *testing.T, c *pgx.Conn, email string) string {
	t.Helper()
	var id string
	err := c.QueryRow(context.Background(),
		"INSERT INTO public.users (email, is_active) VALUES ($1, true) RETURNING id", email).Scan(&id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// SeedJobItem inserts a real public.job_items row (the platform-owned source of a job) and
// returns its id. campaign_id/user_id are caller-supplied for tenancy realism.
func SeedJobItem(t *testing.T, c *pgx.Conn, campaignID, userID, recipient string) string {
	t.Helper()
	var id string
	err := c.QueryRow(context.Background(),
		`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status)
		 VALUES ($1, $2, $3, 'Subject', 'Body', 'pending') RETURNING id`,
		campaignID, userID, recipient).Scan(&id)
	if err != nil {
		t.Fatalf("seed job_item: %v", err)
	}
	return id
}

// SeedSharedPool inserts a real shared send_managed.pools row (owner_user_id NULL) and returns its id.
func SeedSharedPool(t *testing.T, c *pgx.Conn, userID, name string) string {
	t.Helper()
	var id string
	err := c.QueryRow(context.Background(),
		`INSERT INTO send_managed.pools (user_id, kind, name) VALUES ($1, 'shared', $2) RETURNING id`,
		userID, name).Scan(&id)
	if err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	return id
}

// SeedWorker inserts a real send_managed.boxes row (client-supplied UUID) and returns its id.
func SeedWorker(t *testing.T, c *pgx.Conn, userID string) string {
	t.Helper()
	var id string
	err := c.QueryRow(context.Background(),
		`INSERT INTO send_managed.boxes (id, user_id, token_hash) VALUES (gen_random_uuid(), $1, 'x') RETURNING id`,
		userID).Scan(&id)
	if err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	return id
}

// SeedAccount inserts a minimal real send_managed.accounts row (placeholder credential) and returns
// its id. For tests that only need a valid assigned_account_id FK target.
func SeedAccount(t *testing.T, c *pgx.Conn, userID, poolID string) string {
	t.Helper()
	return insertAccountRow(t, c, userID, poolID, []byte("\x00"), "k", []byte("\x00"))
}

// insertAccountRow is the shared minimal-account insert used by SeedAccount and the crypto
// tests' insertAccount helper.
func insertAccountRow(t *testing.T, c *pgx.Conn, userID, poolID string, ct []byte, keyID string, nonce []byte) string {
	t.Helper()
	// Unique username per insert so two accounts can share a pool without tripping the Phase 2
	// per-pool uniqueness index (send_managed_0013). AAD binding is proven by distinct row ids, not by
	// distinct credentials, so this does not weaken any Phase 1 assertion.
	username := fmt.Sprintf("user%d@example.com", acctSeq.Add(1))
	var id string
	err := c.QueryRow(context.Background(),
		`INSERT INTO send_managed.accounts
		   (user_id, pool_id, smtp_host, smtp_port, smtp_username, credential_ciphertext, credential_key_id, credential_nonce, from_address)
		 VALUES ($1,$2,'smtp.example.com',587,$6,$3,$4,$5,'sender@example.com')
		 RETURNING id`,
		userID, poolID, ct, keyID, nonce, username).Scan(&id)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return id
}

// InsertQueuedJob inserts a real send_managed.jobs row in state 'queued' via the sanctioned path
// (actor GUC set for the genesis event) and returns its id. Uses a fresh job_item each call
// (job_item_id is UNIQUE). Runs in one transaction so the genesis-event trigger sees the GUC.
func InsertQueuedJob(t *testing.T, c *pgx.Conn, userID, campaignID, recipient string) string {
	t.Helper()
	ctx := context.Background()
	jobItemID := SeedJobItem(t, c, campaignID, userID, recipient)
	var id string
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor', 'system', true)"); err != nil {
		t.Fatalf("set actor: %v", err)
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO send_managed.jobs (job_item_id, campaign_id, user_id, recipient_email, rendered_subject, rendered_body, state, next_attempt_at)
		 VALUES ($1, $2, $3, $4, 'Subject', 'Body', 'queued', NOW()) RETURNING id`,
		jobItemID, campaignID, userID, recipient).Scan(&id)
	if err != nil {
		t.Fatalf("insert queued job: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit job: %v", err)
	}
	return id
}

// aesGCMEncrypt is a REFERENCE AES-256-GCM implementation for the behavioral tests: 12-byte
// nonce, 16-byte tag appended to ciphertext (the combined-mode layout the schema stores in
// credential_ciphertext), AAD passed by the caller. The key lives ONLY in the test harness
// (never the DB). It proves the Phase 1 credential COLUMN SHAPE round-trips and that no
// plaintext reaches the DB.
//
// AAD binding is LOCKED-ADOPTED: AAD = account.id ‖ credential_key_id. Phase 2 §14 OD-3 ruled
// "DECIDED-NOW (adopt)" (prevents cross-row ciphertext transplant; adds Phase 2 test T2b), and
// Phase 1 implements + tests it here (T4 part D). This is the locked encode contract Phase 2
// carries forward — it is NOT re-opened. The Phase 1 SCHEMA stores no AAD (3 credential columns
// only); the AAD is recomputed at decrypt from the row's own id + credential_key_id.
func aesGCMEncrypt(t *testing.T, key, plaintext, nonce, aad []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes new cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new gcm: %v", err)
	}
	// Seal appends ciphertext‖tag to dst (nil here) — the combined-mode layout the schema stores.
	return gcm.Seal(nil, nonce, plaintext, aad)
}

// aesGCMDecrypt reverses aesGCMEncrypt. Returns (plaintext, nil) on success or (nil, err) on
// GCM authentication failure (tamper / wrong key / wrong AAD) — fails closed, releases no
// plaintext.
func aesGCMDecrypt(key, ciphertextWithTag, nonce, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertextWithTag, aad)
}

// aadFor builds the credential AAD = raw 16-byte account UUID ‖ utf8(credential_key_id).
func aadFor(accountUUID [16]byte, keyID string) []byte {
	return append(accountUUID[:], []byte(keyID)...)
}
