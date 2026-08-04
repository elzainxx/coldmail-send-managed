package adminapi

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"testing"

	"github.com/jackc/pgx/v5"
)

// seedUser inserts a real public.users row (as the admin/superuser connection — the module has
// SELECT-only) and returns its id.
func (e *testEnv) seedUser(t *testing.T, email string) string {
	t.Helper()
	var id string
	if err := e.admin.QueryRow(context.Background(),
		"INSERT INTO public.users (email, is_active) VALUES ($1, true) RETURNING id", email).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedSystemUser inserts the reserved SEND_MANAGED_SYSTEM_USER_ID row into public.users (the platform's
// coordination item, §0.1 — done here by the admin connection to simulate the platform seeding).
func (e *testEnv) seedSystemUser(t *testing.T) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(),
		"INSERT INTO public.users (id, email, is_active) VALUES ($1,'system@sender.local',true) ON CONFLICT (id) DO NOTHING",
		SystemUserID); err != nil {
		t.Fatalf("seed system user: %v", err)
	}
}

// rawCredential reads the stored credential columns directly (admin connection = ground truth).
func (e *testEnv) rawCredential(t *testing.T, accID string) (ct, nonce []byte, keyID string) {
	t.Helper()
	if err := e.admin.QueryRow(context.Background(),
		"SELECT credential_ciphertext, credential_nonce, credential_key_id FROM send_managed.accounts WHERE id=$1", accID).
		Scan(&ct, &nonce, &keyID); err != nil {
		t.Fatalf("read raw credential: %v", err)
	}
	return ct, nonce, keyID
}

// decrypt reverses the server's Encrypt using the harness-held key + the given AAD. Returns
// (plaintext, nil) or (nil, err) on GCM auth failure (fails closed).
func (e *testEnv) decrypt(accID [16]byte, keyID string, ct, nonce []byte) ([]byte, error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ct, aadFor(accID, keyID))
}

// setDailySentCount forces daily_sent_count directly (admin) for the §3.1 lowering test.
func (e *testEnv) setDailySentCount(t *testing.T, accID string, n int) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(),
		"UPDATE send_managed.accounts SET daily_sent_count=$2 WHERE id=$1", accID, n); err != nil {
		t.Fatalf("set daily_sent_count: %v", err)
	}
}

// setPauseDaily forces paused=true, pause_reason='daily_limit' directly (for the T8b unpause guard).
func (e *testEnv) setPauseDaily(t *testing.T, accID string) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(),
		"UPDATE send_managed.accounts SET paused=true, pause_reason='daily_limit' WHERE id=$1", accID); err != nil {
		t.Fatalf("set pause daily_limit: %v", err)
	}
}

// eventCount returns the number of account_events rows for an account id (admin truth).
func (e *testEnv) eventCount(t *testing.T, accID string) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(),
		"SELECT count(*) FROM send_managed.account_events WHERE account_id=$1", accID).Scan(&n); err != nil {
		t.Fatalf("event count: %v", err)
	}
	return n
}

// latestEvent returns the (action, actor, from_flags, to_flags, request_id) of the most recent
// account_events row for an account.
func (e *testEnv) latestEvent(t *testing.T, accID string) (action, actor string, fromFlags, toFlags []byte, requestID *string) {
	t.Helper()
	if err := e.admin.QueryRow(context.Background(),
		`SELECT action, actor, from_flags, to_flags, request_id FROM send_managed.account_events
		 WHERE account_id=$1 ORDER BY created_at DESC, id DESC LIMIT 1`, accID).
		Scan(&action, &actor, &fromFlags, &toFlags, &requestID); err != nil {
		t.Fatalf("latest event: %v", err)
	}
	return
}

// moduleConn opens a send_managed_app connection to the clone (for the append-only T9 checks).
func (e *testEnv) moduleConn(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := withUserPass(e.cloneDSN, "send_managed_app", modulePassword())
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("module connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}
