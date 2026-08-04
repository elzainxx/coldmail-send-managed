package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Phase 1 behavioral tests — credential encryption + role isolation + append-only log. REAL
// Postgres, REAL send_managed_app role (CLAUDE.md Rule 3). The AES-256-GCM key lives ONLY in this
// harness process, never in the DB — proving the platform-cannot-read property behaviorally.

// moduleDSN returns a DSN authenticated as the send_managed_app LOGIN role against the given clone.
// The password is the dev password set out-of-band by compose (scripts/dev.sh / migrate
// set-app-password). Tests skip if it can't connect (role/password not provisioned).
func moduleConn(t *testing.T, cloneDSN string) *pgx.Conn {
	t.Helper()
	pw := getenv("SEND_MANAGED_MODULE_PASSWORD")
	if pw == "" {
		pw = "devmodulepass"
	}
	dsn := withUserPass(cloneDSN, "send_managed_app", pw)
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect as send_managed_app: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// TestT4_CredentialRoundTripNoPlaintextTamperAAD — the crown-jewel credential test.
//
//	A) encrypt->store->read->decrypt round-trips to the original secret;
//	B) the raw credential_ciphertext bytes never contain the plaintext;
//	C) tamper (flip ciphertext byte / tag byte / nonce byte) => GCM auth failure, no plaintext;
//	D) AAD binding: the SAME ciphertext transplanted to a DIFFERENT account row fails to decrypt.
func TestT4_CredentialRoundTripNoPlaintextTamperAAD(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "t4cred")
	defer cleanup()

	c := Connect(t, cloneDSN)
	uid := SeedUser(t, c, "t4@example.com")
	poolID := SeedSharedPool(t, c, uid, "t4-pool")

	// 32-byte AES-256 key held ONLY here (never in the DB).
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	const keyID = "test-key-v1"
	secret := []byte("hunter2-smtp-secret")

	// Insert an account row with a placeholder, then read back its id to build the AAD, then
	// UPDATE with the correctly-bound ciphertext (AAD needs the real account.id).
	accID := insertAccount(t, c, uid, poolID, []byte("placeholder-ct-16b"), keyID, make([]byte, 12))

	accUUID := uuidBytes(t, accID)
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("rand nonce: %v", err)
	}
	aad := aadFor(accUUID, keyID)
	ct := aesGCMEncrypt(t, key, secret, nonce, aad)
	updateCredential(t, c, accID, ct, keyID, nonce)

	// --- A) round-trip ---
	gotCT, gotNonce, gotKeyID := readCredential(t, c, accID)
	if gotKeyID != keyID {
		t.Fatalf("key_id round-trip: got %q want %q", gotKeyID, keyID)
	}
	pt, err := aesGCMDecrypt(key, gotCT, gotNonce, aadFor(accUUID, gotKeyID))
	if err != nil {
		t.Fatalf("round-trip decrypt failed: %v", err)
	}
	if !bytes.Equal(pt, secret) {
		t.Fatalf("round-trip mismatch: got %q want %q", pt, secret)
	}

	// --- B) plaintext never appears in the stored ciphertext bytes ---
	if bytes.Contains(gotCT, secret) {
		t.Fatal("plaintext secret found inside credential_ciphertext bytes")
	}
	// Also assert via a raw SQL scan of the column (what a DB reader would see).
	var rawHex string
	if err := c.QueryRow(ctx, "SELECT encode(credential_ciphertext,'hex') FROM send_managed.accounts WHERE id=$1", accID).Scan(&rawHex); err != nil {
		t.Fatalf("read raw ct: %v", err)
	}
	if strings.Contains(rawHex, hex.EncodeToString(secret)) {
		t.Fatal("plaintext secret hex found in raw credential_ciphertext column")
	}

	// --- C) tamper detection (three sub-cases), each fails closed ---
	// c1: flip a ciphertext byte
	tampCT := append([]byte(nil), gotCT...)
	tampCT[0] ^= 0x01
	if _, err := aesGCMDecrypt(key, tampCT, gotNonce, aad); err == nil {
		t.Fatal("c1: tampered ciphertext decrypted (want auth failure)")
	}
	// c2: flip a tag byte (last 16 bytes)
	tampTag := append([]byte(nil), gotCT...)
	tampTag[len(tampTag)-1] ^= 0x01
	if _, err := aesGCMDecrypt(key, tampTag, gotNonce, aad); err == nil {
		t.Fatal("c2: tampered tag decrypted (want auth failure)")
	}
	// c3: flip a nonce byte
	tampNonce := append([]byte(nil), gotNonce...)
	tampNonce[0] ^= 0x01
	if _, err := aesGCMDecrypt(key, gotCT, tampNonce, aad); err == nil {
		t.Fatal("c3: altered nonce decrypted (want auth failure)")
	}

	// --- D) AAD row-binding (LOCKED-ADOPTED — Phase 2 §14 OD-3 adopt; see factories.go). AAD =
	// account.id ‖ credential_key_id binds ciphertext to its row: transplant this ciphertext+
	// nonce into a DIFFERENT account row and decrypt with THAT row's AAD => fails closed. This
	// is the locked encode contract Phase 2 carries forward (its test T2b). ---
	accID2 := insertAccount(t, c, uid, poolID, gotCT, keyID, gotNonce) // same ct/nonce, new row/id
	acc2UUID := uuidBytes(t, accID2)
	if _, err := aesGCMDecrypt(key, gotCT, gotNonce, aadFor(acc2UUID, keyID)); err == nil {
		t.Fatal("D: transplanted ciphertext decrypted under a different account's AAD (row-binding broken)")
	}
	// Sanity: it still decrypts under the ORIGINAL account's AAD (proves the failure above is
	// AAD-binding, not an unrelated break).
	if _, err := aesGCMDecrypt(key, gotCT, gotNonce, aadFor(accUUID, keyID)); err != nil {
		t.Fatalf("D control: original-AAD decrypt failed unexpectedly: %v", err)
	}
}

// TestT5_ModuleRoleDeniedPlatformWrite — as send_managed_app: SELECT on public.job_items/users
// succeeds, but INSERT/UPDATE/DELETE fail with 42501; writes to send_managed.jobs succeed. Proves the
// one-writer boundary with a REAL failed write (anti-vacuity: the paired SELECT + send_managed write
// succeed).
func TestT5_ModuleRoleDeniedPlatformWrite(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "t5role")
	defer cleanup()

	// Seed a platform row + pool as admin (privileged).
	admin := Connect(t, cloneDSN)
	uid := SeedUser(t, admin, "t5@example.com")
	itemID := SeedJobItem(t, admin, uid, uid, "rcpt-t5@example.com")

	m := moduleConn(t, cloneDSN)

	// SELECT on platform tables SUCCEEDS (read grant active) — anti-vacuity control.
	var n int
	if err := m.QueryRow(ctx, "SELECT count(*) FROM public.job_items WHERE id=$1", itemID).Scan(&n); err != nil {
		t.Fatalf("module SELECT public.job_items failed (should succeed): %v", err)
	}
	if n != 1 {
		t.Fatalf("module SELECT returned %d rows, want 1", n)
	}

	// INSERT / UPDATE / DELETE on platform tables MUST fail 42501.
	assertInsufficientPrivilege(t, m, "INSERT public.job_items",
		"INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status) VALUES ($1,$1,'x','s','b','pending')", uid)
	assertInsufficientPrivilege(t, m, "UPDATE public.job_items",
		"UPDATE public.job_items SET status='compromised' WHERE id=$1", itemID)
	assertInsufficientPrivilege(t, m, "DELETE public.job_items",
		"DELETE FROM public.job_items WHERE id=$1", itemID)
	assertInsufficientPrivilege(t, m, "UPDATE public.users",
		"UPDATE public.users SET is_active=false WHERE id=$1", uid)

	// Anti-vacuity positive: the module CAN write its own table (send_managed.jobs) via the sanctioned
	// path. Use a fresh job_item to satisfy the UNIQUE FK.
	var freshItem string
	if err := admin.QueryRow(ctx,
		"INSERT INTO public.job_items (campaign_id,user_id,recipient_email,rendered_subject,rendered_body,status) VALUES ($1,$1,'y','s','b','pending') RETURNING id", uid).Scan(&freshItem); err != nil {
		t.Fatalf("seed fresh job_item: %v", err)
	}
	tx, err := m.Begin(ctx)
	if err != nil {
		t.Fatalf("module begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor','system',true)"); err != nil {
		t.Fatalf("set actor: %v", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO send_managed.jobs (job_item_id,campaign_id,user_id,recipient_email,rendered_subject,rendered_body,state,next_attempt_at) VALUES ($1,$2,$2,'z','s','b','queued',NOW())",
		freshItem, uid); err != nil {
		t.Fatalf("module INSERT send_managed.jobs failed (should succeed): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("module commit send_managed.jobs: %v", err)
	}
}

// TestT5b_JobItemIdUnique — a second send_managed.jobs row for the same job_item_id fails with 23505.
func TestT5b_JobItemIdUnique(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "t5bunique")
	defer cleanup()

	c := Connect(t, cloneDSN)
	uid := SeedUser(t, c, "t5b@example.com")
	itemID := SeedJobItem(t, c, uid, uid, "rcpt-t5b@example.com")

	insert := func() error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor','system',true)"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO send_managed.jobs (job_item_id,campaign_id,user_id,recipient_email,rendered_subject,rendered_body,state,next_attempt_at) VALUES ($1,$2,$2,'r','s','b','queued',NOW())",
			itemID, uid); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	if err := insert(); err != nil {
		t.Fatalf("first insert should succeed: %v", err)
	}
	err := insert()
	if err == nil {
		t.Fatal("second insert with same job_item_id succeeded, want 23505 unique_violation")
	}
	if !strings.Contains(err.Error(), "jobs_job_item_id_unique") && !strings.Contains(err.Error(), "23505") && !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		t.Fatalf("second insert failed but not with a unique violation: %v", err)
	}

	var cnt int
	if err := c.QueryRow(ctx, "SELECT count(*) FROM send_managed.jobs WHERE job_item_id=$1", itemID).Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("row count = %d after duplicate insert, want 1", cnt)
	}
}

// TestT6_AppendOnlyJobEvents — as send_managed_app: UPDATE and direct DELETE on send_managed.job_events
// FAIL; INSERT/SELECT succeed; deleting the PARENT send_managed.jobs row cascades away its events
// (cascade cleanup must NOT be blocked). Proves append-only + the deliberate no-DELETE-trigger.
func TestT6_AppendOnlyJobEvents(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "t6append")
	defer cleanup()

	admin := Connect(t, cloneDSN)
	uid := SeedUser(t, admin, "t6@example.com")
	jobID := InsertQueuedJob(t, admin, uid, uid, "rcpt-t6@example.com") // genesis event exists

	var eventID string
	if err := admin.QueryRow(ctx, "SELECT id FROM send_managed.job_events WHERE job_id=$1 LIMIT 1", jobID).Scan(&eventID); err != nil {
		t.Fatalf("read event id: %v", err)
	}

	m := moduleConn(t, cloneDSN)

	// UPDATE fails (privilege 42501 or trigger integrity_constraint_violation — either is valid).
	if _, err := m.Exec(ctx, "UPDATE send_managed.job_events SET actor='admin' WHERE id=$1", eventID); err == nil {
		t.Fatal("UPDATE send_managed.job_events succeeded, want rejection")
	}
	// Direct DELETE fails with insufficient_privilege (no delete-trigger involved).
	if _, err := m.Exec(ctx, "DELETE FROM send_managed.job_events WHERE id=$1", eventID); err == nil {
		t.Fatal("direct DELETE send_managed.job_events succeeded, want 42501 insufficient_privilege")
	}

	// Anti-vacuity positive: INSERT + SELECT succeed for the module. Insert a standalone event
	// row for a fresh job (genesis path already proved INSERT via triggers; here direct INSERT).
	freshJob := InsertQueuedJob(t, admin, uid, uid, "rcpt-t6b@example.com")
	if _, err := m.Exec(ctx,
		"INSERT INTO send_managed.job_events (job_id,user_id,from_state,to_state,actor) VALUES ($1,$2,'queued','assigned','dispatcher')",
		freshJob, uid); err != nil {
		t.Fatalf("module INSERT send_managed.job_events failed (should succeed): %v", err)
	}
	var seen int
	if err := m.QueryRow(ctx, "SELECT count(*) FROM send_managed.job_events WHERE job_id=$1", jobID).Scan(&seen); err != nil {
		t.Fatalf("module SELECT job_events failed: %v", err)
	}
	if seen < 1 {
		t.Fatalf("module SELECT returned %d, want >=1", seen)
	}

	// Cascade cleanup: deleting the PARENT job (module has DELETE on send_managed.jobs) removes its
	// events — proving cascade is NOT blocked by the append-only enforcement.
	before := eventCount(t, admin, jobID)
	if before < 1 {
		t.Fatalf("anti-vacuity: job %s had %d events before delete, want >=1", jobID, before)
	}
	if _, err := m.Exec(ctx, "DELETE FROM send_managed.jobs WHERE id=$1", jobID); err != nil {
		t.Fatalf("module DELETE send_managed.jobs failed (should succeed): %v", err)
	}
	if after := eventCount(t, admin, jobID); after != 0 {
		t.Fatalf("cascade cleanup left %d events for deleted job, want 0", after)
	}
}

// TestT3e_RLSDefaultDenyBackstopsMisgrant — even if a platform role is mistakenly granted
// USAGE+SELECT on send_managed.accounts, RLS (enabled, no policy for that role) default-denies =>
// zero rows. Proves FORCE RLS on the crown-jewel table backstops a future misgrant. Anti-
// vacuity: the same rows ARE visible to send_managed_app (which has a permissive policy), so the
// zero-count for the misgranted role is meaningful, not an empty table.
func TestT3e_RLSDefaultDenyBackstopsMisgrant(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "t3erls")
	defer cleanup()

	admin := Connect(t, cloneDSN)
	uid := SeedUser(t, admin, "t3e@example.com")
	poolID := SeedSharedPool(t, admin, uid, "t3e-pool")
	// Seed one accounts row (real committed) so the table is non-empty.
	insertAccount(t, admin, uid, poolID, []byte("0123456789abcdef0"), "test-key-v1", make([]byte, 12))

	// Anti-vacuity: send_managed_app sees the row (permissive policy admits it).
	m := moduleConn(t, cloneDSN)
	var modSees int
	if err := m.QueryRow(ctx, "SELECT count(*) FROM send_managed.accounts").Scan(&modSees); err != nil {
		t.Fatalf("module count accounts: %v", err)
	}
	if modSees != 1 {
		t.Fatalf("anti-vacuity: send_managed_app sees %d accounts, want 1", modSees)
	}

	// Create a stand-in platform role and MISTAKENLY grant it USAGE + SELECT on accounts.
	roleName := "t3e_platform_role"
	_, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+roleName)
	if _, err := admin.Exec(ctx, "CREATE ROLE "+roleName+" NOLOGIN"); err != nil {
		t.Fatalf("create stand-in role: %v", err)
	}
	defer admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+roleName)
	if _, err := admin.Exec(ctx, "GRANT USAGE ON SCHEMA send_managed TO "+roleName); err != nil {
		t.Fatalf("grant usage: %v", err)
	}
	if _, err := admin.Exec(ctx, "GRANT SELECT ON send_managed.accounts TO "+roleName); err != nil {
		t.Fatalf("grant select: %v", err)
	}

	// SET ROLE to the misgranted platform role and SELECT: RLS default-deny => 0 rows.
	if _, err := admin.Exec(ctx, "SET ROLE "+roleName); err != nil {
		t.Fatalf("set role: %v", err)
	}
	var roleSees int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM send_managed.accounts").Scan(&roleSees); err != nil {
		t.Fatalf("misgranted role count: %v", err)
	}
	if _, err := admin.Exec(ctx, "RESET ROLE"); err != nil {
		t.Fatalf("reset role: %v", err)
	}
	if roleSees != 0 {
		t.Fatalf("RLS default-deny broken: misgranted role sees %d rows, want 0", roleSees)
	}
}

// TestT3f_SuperuserBypassesRLSButReadsOnlyCiphertext — the documented residual (03-encryption-
// and-rls.md §2.5 / T3f): a superuser-class principal BYPASSES RLS and CAN read send_managed.accounts
// rows, but what it reads in credential_ciphertext is ciphertext, never the plaintext secret.
// This turns the "uncomfortable truth" (RLS can't stop a superuser) into a passing assertion:
// secrecy holds because the key lives outside the DB (proven here — the harness holds the key,
// the DB never does). Anti-vacuity: the superuser DOES see the row (count=1), so the
// no-plaintext assertion is meaningful, not an empty-table artifact.
func TestT3f_SuperuserBypassesRLSButReadsOnlyCiphertext(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "t3fsuper")
	defer cleanup()

	// The admin/superuser-class connection (dev superuser) — the strongest DB-plane actor.
	su := Connect(t, cloneDSN)
	uid := SeedUser(t, su, "t3f@example.com")
	poolID := SeedSharedPool(t, su, uid, "t3f-pool")

	// Encrypt a known secret with a key held ONLY in this harness process (never the DB).
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	const keyID = "test-key-v1"
	secret := []byte("t3f-super-secret-smtp-pw")
	accID := insertAccount(t, su, uid, poolID, []byte("placeholder-ct-16b"), keyID, make([]byte, 12))
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("rand nonce: %v", err)
	}
	ct := aesGCMEncrypt(t, key, secret, nonce, aadFor(uuidBytes(t, accID), keyID))
	updateCredential(t, su, accID, ct, keyID, nonce)

	// Superuser bypasses RLS — the row IS visible (anti-vacuity control).
	var seen int
	if err := su.QueryRow(ctx, "SELECT count(*) FROM send_managed.accounts WHERE id=$1", accID).Scan(&seen); err != nil {
		t.Fatalf("superuser count accounts: %v", err)
	}
	if seen != 1 {
		t.Fatalf("anti-vacuity: superuser sees %d rows, want 1 (RLS should NOT hide from superuser)", seen)
	}

	// ...but what it reads is ciphertext, not the plaintext secret.
	var rawHex string
	if err := su.QueryRow(ctx, "SELECT encode(credential_ciphertext,'hex') FROM send_managed.accounts WHERE id=$1", accID).Scan(&rawHex); err != nil {
		t.Fatalf("superuser read ciphertext: %v", err)
	}
	if strings.Contains(rawHex, hex.EncodeToString(secret)) {
		t.Fatal("superuser read recovered the plaintext secret from credential_ciphertext (secrecy broken)")
	}
	// And the superuser has NO key (it's not in the DB), so it cannot decrypt. Prove the DB
	// holds no key material by asserting the secret's plaintext appears nowhere in the row's
	// text columns either.
	var host, uname, kid string
	if err := su.QueryRow(ctx, "SELECT smtp_host, smtp_username, credential_key_id FROM send_managed.accounts WHERE id=$1", accID).Scan(&host, &uname, &kid); err != nil {
		t.Fatalf("superuser read row: %v", err)
	}
	if strings.Contains(host, string(secret)) || strings.Contains(uname, string(secret)) || strings.Contains(kid, string(secret)) {
		t.Fatal("plaintext secret leaked into a plaintext column")
	}
	if kid != keyID {
		t.Fatalf("credential_key_id = %q, want %q (label only, not key material)", kid, keyID)
	}
}

// TestT7_CheckConstraintsRejectInvalid — DB-level guard test for the hand-written Phase 1 CHECK
// constraints that are testable against the schema alone (no dispatcher logic): the pause_reason
// invariant, the 2000-char error-message cap, and the pool self-fallback ban. Each invalid value
// must be rejected with 23514 check_violation; each is PAIRED with a valid positive control that
// succeeds (anti-vacuity — proves the constraint is the thing rejecting, not some unrelated
// error). This is the DB-level verification proposed in phase1-drafts/07-test-plan.md §6.
func TestT7_CheckConstraintsRejectInvalid(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "t7checks")
	defer cleanup()

	c := Connect(t, cloneDSN)
	uid := SeedUser(t, c, "t7@example.com")
	poolID := SeedSharedPool(t, c, uid, "t7-pool")

	isCheckViolation := func(err error) bool {
		if err == nil {
			return false
		}
		m := err.Error()
		return strings.Contains(m, "23514") || strings.Contains(strings.ToLower(m), "check constraint") || strings.Contains(strings.ToLower(m), "violates check")
	}

	// (1) accounts_pause_reason_check: paused=TRUE with NULL pause_reason is INVALID.
	acctBad := `INSERT INTO send_managed.accounts
	  (user_id,pool_id,smtp_host,smtp_port,smtp_username,credential_ciphertext,credential_key_id,credential_nonce,from_address,paused,pause_reason)
	  VALUES ($1,$2,'h',587,'u','\x00','k','\x00','a@b.co',TRUE,NULL)`
	if _, err := c.Exec(ctx, acctBad, uid, poolID); !isCheckViolation(err) {
		t.Fatalf("pause_reason: paused=TRUE, reason=NULL should be 23514, got: %v", err)
	}
	// positive control: paused=TRUE with a reason succeeds.
	acctOK := `INSERT INTO send_managed.accounts
	  (user_id,pool_id,smtp_host,smtp_port,smtp_username,credential_ciphertext,credential_key_id,credential_nonce,from_address,paused,pause_reason)
	  VALUES ($1,$2,'h',587,'u','\x00','k','\x00','a@b.co',TRUE,'daily_limit')`
	if _, err := c.Exec(ctx, acctOK, uid, poolID); err != nil {
		t.Fatalf("pause_reason positive control (paused=TRUE, reason=daily_limit) failed: %v", err)
	}

	// (2) jobs_last_error_message_len_check: 2001 chars INVALID, 2000 chars valid.
	itemID := SeedJobItem(t, c, uid, uid, "rcpt-t7@example.com")
	over := strings.Repeat("x", 2001)
	jobOver := `INSERT INTO send_managed.jobs (job_item_id,campaign_id,user_id,recipient_email,rendered_subject,rendered_body,state,next_attempt_at,last_error_message)
	            VALUES ($1,$2,$2,'r','s','b','queued',NOW(),$3)`
	// Set actor GUC so the genesis trigger doesn't mask the CHECK with an actor error path
	// (INSERT genesis is lenient, but set it anyway for clarity).
	if _, err := c.Exec(ctx, "SELECT set_config('send_managed.actor','system',false)"); err != nil {
		t.Fatalf("set actor: %v", err)
	}
	if _, err := c.Exec(ctx, jobOver, itemID, uid, over); !isCheckViolation(err) {
		t.Fatalf("last_error_message 2001 chars should be 23514, got: %v", err)
	}
	// positive control: 2000 chars succeeds (fresh job_item — job_item_id is UNIQUE).
	item2 := SeedJobItem(t, c, uid, uid, "rcpt-t7b@example.com")
	atCap := strings.Repeat("x", 2000)
	if _, err := c.Exec(ctx, jobOver, item2, uid, atCap); err != nil {
		t.Fatalf("last_error_message 2000-char positive control failed: %v", err)
	}

	// (3) pools_fallback_not_self_check: a pool cannot be its own fallback.
	// Insert a pool, then UPDATE it to point fallback at itself => 23514.
	var p2 string
	if err := c.QueryRow(ctx,
		"INSERT INTO send_managed.pools (user_id,kind,name) VALUES ($1,'shared','t7-fb') RETURNING id", uid).Scan(&p2); err != nil {
		t.Fatalf("seed pool2: %v", err)
	}
	if _, err := c.Exec(ctx, "UPDATE send_managed.pools SET fallback_pool_id=id WHERE id=$1", p2); !isCheckViolation(err) {
		t.Fatalf("self-fallback should be 23514, got: %v", err)
	}
	// positive control: pointing fallback at a DIFFERENT pool succeeds.
	if _, err := c.Exec(ctx, "UPDATE send_managed.pools SET fallback_pool_id=$2 WHERE id=$1", p2, poolID); err != nil {
		t.Fatalf("cross-pool fallback positive control failed: %v", err)
	}
}

// --- helpers (crypto/roles) ---

func insertAccount(t *testing.T, c *pgx.Conn, userID, poolID string, ct []byte, keyID string, nonce []byte) string {
	t.Helper()
	return insertAccountRow(t, c, userID, poolID, ct, keyID, nonce)
}

func updateCredential(t *testing.T, c *pgx.Conn, accID string, ct []byte, keyID string, nonce []byte) {
	t.Helper()
	if _, err := c.Exec(context.Background(),
		"UPDATE send_managed.accounts SET credential_ciphertext=$2, credential_key_id=$3, credential_nonce=$4 WHERE id=$1",
		accID, ct, keyID, nonce); err != nil {
		t.Fatalf("update credential: %v", err)
	}
}

func readCredential(t *testing.T, c *pgx.Conn, accID string) (ct []byte, nonce []byte, keyID string) {
	t.Helper()
	if err := c.QueryRow(context.Background(),
		"SELECT credential_ciphertext, credential_nonce, credential_key_id FROM send_managed.accounts WHERE id=$1", accID).
		Scan(&ct, &nonce, &keyID); err != nil {
		t.Fatalf("read credential: %v", err)
	}
	return ct, nonce, keyID
}

// uuidBytes parses a canonical UUID string into its 16 raw bytes (for AAD binding).
func uuidBytes(t *testing.T, u string) [16]byte {
	t.Helper()
	h := strings.ReplaceAll(u, "-", "")
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 16 {
		t.Fatalf("bad uuid %q: %v", u, err)
	}
	var out [16]byte
	copy(out[:], b)
	return out
}

func assertInsufficientPrivilege(t *testing.T, c *pgx.Conn, label, sql string, arg any) {
	t.Helper()
	_, err := c.Exec(context.Background(), sql, arg)
	if err == nil {
		t.Fatalf("%s: expected permission-denied (42501), got nil error", label)
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "permission denied") && !strings.Contains(err.Error(), "42501") {
		t.Fatalf("%s: expected 42501 permission denied, got: %v", label, err)
	}
}

// withUserPass rewrites the userinfo of a postgres URL DSN.
func withUserPass(dsn, user, pass string) string {
	// dsn like postgres://postgres:pw@host:port/db?... — replace the userinfo segment.
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
