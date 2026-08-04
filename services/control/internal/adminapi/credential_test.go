package adminapi

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// createSharedPoolAndAccount is a helper: seed the system user, create a shared pool, create an
// account with the given secret. Returns (poolID, accountID).
func (e *testEnv) createSharedPoolAndAccount(t *testing.T, secret string) (string, string) {
	t.Helper()
	e.seedSystemUser(t)
	pr := e.do(t, "POST", "/v1/admin/pools", map[string]any{
		"kind": "shared", "name": "pool-" + randHex(4),
	}).requireStatus(t, 201)
	poolID := pr.str("id")
	ar := e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id":       poolID,
		"smtp_host":     "smtp.example.com",
		"smtp_username": "mailer-" + randHex(3) + "@example.com",
		"smtp_secret":   secret,
		"from_address":  "sender@example.com",
	}).requireStatus(t, 201)
	return poolID, ar.str("id")
}

// TestT1_CredentialRoundTripNeverLeaks — the credential round-trips (decryptable with the test
// key + AAD) but NEVER appears in the create response, reads/lists, logs, or as plaintext in the
// DB.
func TestT1_CredentialRoundTripNeverLeaks(t *testing.T) {
	e := newTestEnv(t, "t1")
	const secret = "hunter2-smtp-secret"
	_, accID := e.createSharedPoolAndAccount(t, secret)

	// 1) Round-trip: read raw row, decrypt with the harness key + AAD (id ‖ key_id) => secret.
	ct, nonce, keyID := e.rawCredential(t, accID)
	accBytes, err := uuidToBytes(accID)
	if err != nil {
		t.Fatalf("uuid parse: %v", err)
	}
	pt, derr := e.decrypt(accBytes, keyID, ct, nonce)
	if derr != nil {
		t.Fatalf("round-trip decrypt failed: %v", derr)
	}
	if string(pt) != secret {
		t.Fatalf("round-trip mismatch: got %q want %q", pt, secret)
	}
	if keyID != testKeyID {
		t.Fatalf("credential_key_id = %q, want %q", keyID, testKeyID)
	}

	// 2) Never in the create response: re-create and inspect the body.
	cr := e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "shared", "name": "p2-" + randHex(4)}).requireStatus(t, 201)
	createResp := e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": cr.str("id"), "smtp_host": "smtp.example.com",
		"smtp_username": "u2@example.com", "smtp_secret": secret, "from_address": "s@example.com",
	}).requireStatus(t, 201)
	assertNoCredentialFields(t, createResp.raw)
	if hc, _ := createResp.body["has_credential"].(bool); !hc {
		t.Fatal("create response has_credential != true")
	}

	// 3) Never in read/list.
	getResp := e.do(t, "GET", "/v1/admin/accounts/"+accID, nil).requireStatus(t, 200)
	assertNoCredentialFields(t, getResp.raw)
	listResp := e.do(t, "GET", "/v1/admin/accounts", nil).requireStatus(t, 200)
	assertNoCredentialFields(t, listResp.raw)
	if !bytes.Contains(listResp.raw, []byte("has_credential")) {
		t.Fatal("list did not include has_credential (sanity)")
	}

	// 4) Never in logs: neither the plaintext nor the ciphertext hex appears.
	logs := e.logBuf.String()
	if strings.Contains(logs, secret) {
		t.Fatal("plaintext secret found in captured logs")
	}
	if strings.Contains(logs, hex.EncodeToString(ct)) {
		t.Fatal("ciphertext hex found in captured logs")
	}

	// 5) Not plaintext in DB: raw ciphertext bytes never contain the plaintext.
	if bytes.Contains(ct, []byte(secret)) {
		t.Fatal("plaintext found inside credential_ciphertext")
	}
}

// TestT2_UpdateSecretOmitVsReplace — omit leaves ciphertext byte-identical; a new secret replaces
// it (fresh nonce); null/empty rejected.
func TestT2_UpdateSecretOmitVsReplace(t *testing.T) {
	e := newTestEnv(t, "t2")
	_, accID := e.createSharedPoolAndAccount(t, "orig-secret")
	ct0, nonce0, kid0 := e.rawCredential(t, accID)

	// A) PATCH changing only from_name (NO smtp_secret) => credential bytes byte-identical.
	e.do(t, "PATCH", "/v1/admin/accounts/"+accID, map[string]any{"from_name": "Renamed"}).requireStatus(t, 200)
	ctA, nonceA, kidA := e.rawCredential(t, accID)
	if !bytes.Equal(ct0, ctA) || !bytes.Equal(nonce0, nonceA) || kid0 != kidA {
		t.Fatal("omit=unchanged violated: credential bytes changed on a no-secret PATCH")
	}

	// B) PATCH with a new secret => ciphertext + nonce change; decrypt yields the new secret.
	const newSecret = "newpass"
	e.do(t, "PATCH", "/v1/admin/accounts/"+accID, map[string]any{"smtp_secret": newSecret}).requireStatus(t, 200)
	ctB, nonceB, kidB := e.rawCredential(t, accID)
	if bytes.Equal(ct0, ctB) {
		t.Fatal("new secret did not change ciphertext")
	}
	if bytes.Equal(nonce0, nonceB) {
		t.Fatal("new secret did not change nonce (fresh nonce required)")
	}
	if kidB != testKeyID {
		t.Fatalf("key_id after replace = %q, want %q", kidB, testKeyID)
	}
	accBytes, _ := uuidToBytes(accID)
	pt, derr := e.decrypt(accBytes, kidB, ctB, nonceB)
	if derr != nil || string(pt) != newSecret {
		t.Fatalf("new secret decrypt: pt=%q err=%v want %q", pt, derr, newSecret)
	}

	// C) smtp_secret: "" and null both rejected 400; bytes unchanged.
	for _, raw := range []string{`{"smtp_secret":""}`, `{"smtp_secret":null}`} {
		r := e.doRawBody(t, "PATCH", "/v1/admin/accounts/"+accID, raw).requireStatus(t, 400)
		if r.errorDetailIssue("smtp_secret") != issueMalformed {
			t.Fatalf("smtp_secret %s: issue = %q, want malformed", raw, r.errorDetailIssue("smtp_secret"))
		}
	}
	ctC, nonceC, _ := e.rawCredential(t, accID)
	if !bytes.Equal(ctB, ctC) || !bytes.Equal(nonceB, nonceC) {
		t.Fatal("rejected secret update mutated the credential bytes")
	}
}

// TestT2b_AADRowBindingRejectsTransplant — copy account A's ciphertext+nonce into account B's row
// (direct DB write), then decrypt with B's AAD => fails closed (OD-3 adopted).
func TestT2b_AADRowBindingRejectsTransplant(t *testing.T) {
	e := newTestEnv(t, "t2b")
	_, accA := e.createSharedPoolAndAccount(t, "secret-A")

	// Create B in the same pool with a distinct username.
	prA, _, _ := e.rawCredential(t, accA)
	_ = prA
	poolID := e.do(t, "GET", "/v1/admin/accounts/"+accA, nil).requireStatus(t, 200).str("pool_id")
	brA := e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": poolID, "smtp_host": "smtp.example.com",
		"smtp_username": "B-" + randHex(3) + "@example.com", "smtp_secret": "secret-B", "from_address": "s@example.com",
	}).requireStatus(t, 201)
	accB := brA.str("id")

	ctA, nonceA, kidA := e.rawCredential(t, accA)

	// Transplant A's ciphertext+nonce into B's row (direct admin write).
	if _, err := e.admin.Exec(bgCtx(),
		"UPDATE send_managed.accounts SET credential_ciphertext=$2, credential_nonce=$3, credential_key_id=$4 WHERE id=$1",
		accB, ctA, nonceA, kidA); err != nil {
		t.Fatalf("transplant: %v", err)
	}
	// Decrypt with B's AAD => must fail closed.
	bBytes, _ := uuidToBytes(accB)
	if _, err := e.decrypt(bBytes, kidA, ctA, nonceA); err == nil {
		t.Fatal("transplanted ciphertext decrypted under B's AAD (row-binding broken)")
	}
	// Sanity: still decrypts under A's AAD (proves the failure is AAD-binding, not an unrelated break).
	aBytes, _ := uuidToBytes(accA)
	if _, err := e.decrypt(aBytes, kidA, ctA, nonceA); err != nil {
		t.Fatalf("A-AAD control decrypt failed: %v", err)
	}
}

// TestT3_SecretNeverLoggedOnFailure — a create that fails validation AFTER parsing (bad
// from_address) while carrying a real secret leaves NO occurrence of the secret in logs.
func TestT3_SecretNeverLoggedOnFailure(t *testing.T) {
	e := newTestEnv(t, "t3")
	e.seedSystemUser(t)
	poolID := e.do(t, "POST", "/v1/admin/pools", map[string]any{"kind": "shared", "name": "p-" + randHex(4)}).requireStatus(t, 201).str("id")

	const secret = "leak-me-if-you-can-9987"
	r := e.do(t, "POST", "/v1/admin/accounts", map[string]any{
		"pool_id": poolID, "smtp_host": "smtp.example.com", "smtp_username": "u@example.com",
		"smtp_secret": secret, "from_address": "NOT-AN-EMAIL",
	}).requireStatus(t, 400)
	if r.errorDetailIssue("from_address") != issueMalformed {
		t.Fatalf("expected from_address malformed, got detail issue %q (code %s)", r.errorDetailIssue("from_address"), r.errorCode())
	}
	// The request_id is present (bridge) and the secret is absent from logs AND the response.
	if r.errorRequestID() == "" {
		t.Fatal("no request_id in error envelope")
	}
	if strings.Contains(e.logBuf.String(), secret) {
		t.Fatal("secret found in logs on a failure path")
	}
	if bytes.Contains(r.raw, []byte(secret)) {
		t.Fatal("secret echoed in the error response body")
	}
}

// assertNoCredentialFields fails if any credential-related field name appears in a response body.
func assertNoCredentialFields(t *testing.T, raw []byte) {
	t.Helper()
	for _, banned := range []string{"smtp_secret", "credential_ciphertext", "credential_nonce", "credential_key_id"} {
		if bytes.Contains(raw, []byte(banned)) {
			t.Fatalf("response leaked credential field %q: %s", banned, string(raw))
		}
	}
}
