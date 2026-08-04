package credential

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/coldmail-send-managed/control/internal/adminapi"
)

// TestDecrypt_RoundTrip — the send-path Decryptor reverses exactly what the admin Encryptor
// sealed (the two share the AES-256-GCM + AAD=account.id‖key_id contract). Real crypto, no mocks.
func TestDecrypt_RoundTrip(t *testing.T) {
	key := randKey(t)
	enc, err := adminapi.NewEncryptor(key, "cred-dek-v1")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	dec, err := NewDecryptor(key, "cred-dek-v1")
	if err != nil {
		t.Fatalf("decryptor: %v", err)
	}

	var acct [16]byte
	rand.Read(acct[:])
	secret := []byte("smtp-password-hunter2")
	ct, nonce, err := enc.Encrypt(acct, secret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := dec.Decrypt(acct, "cred-dek-v1", ct, nonce)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, secret)
	}
}

// TestDecrypt_TamperFailsClosed — flipping a ciphertext byte fails GCM auth and releases NO
// plaintext (fail closed). Anti-vacuity: the untampered value decrypts.
func TestDecrypt_TamperFailsClosed(t *testing.T) {
	key := randKey(t)
	enc, _ := adminapi.NewEncryptor(key, "k")
	dec, _ := NewDecryptor(key, "k")
	var acct [16]byte
	rand.Read(acct[:])
	ct, nonce, _ := enc.Encrypt(acct, []byte("secret"))

	// Positive control: untampered decrypts.
	if _, err := dec.Decrypt(acct, "k", ct, nonce); err != nil {
		t.Fatalf("control decrypt failed: %v", err)
	}
	// Tamper one byte.
	bad := append([]byte(nil), ct...)
	bad[0] ^= 0xff
	if pt, err := dec.Decrypt(acct, "k", bad, nonce); err == nil {
		t.Fatalf("tampered ciphertext decrypted (released %q) — must fail closed", pt)
	}
}

// TestDecrypt_TransplantFailsClosed — a ciphertext bound to account A cannot be decrypted under
// account B's id (the AAD binds it to its row, OD-3). This is the anti-transplant guarantee.
func TestDecrypt_TransplantFailsClosed(t *testing.T) {
	key := randKey(t)
	enc, _ := adminapi.NewEncryptor(key, "k")
	dec, _ := NewDecryptor(key, "k")
	var acctA, acctB [16]byte
	rand.Read(acctA[:])
	rand.Read(acctB[:])
	ct, nonce, _ := enc.Encrypt(acctA, []byte("secret"))

	if _, err := dec.Decrypt(acctA, "k", ct, nonce); err != nil {
		t.Fatalf("control (correct account) failed: %v", err)
	}
	if pt, err := dec.Decrypt(acctB, "k", ct, nonce); err == nil {
		t.Fatalf("transplant to account B decrypted (released %q) — AAD binding broken", pt)
	}
}

// TestDecrypt_WrongKeyIDRejected — a row labelled with an unresolvable key-id is rejected, never
// a silent wrong-key attempt.
func TestDecrypt_WrongKeyIDRejected(t *testing.T) {
	key := randKey(t)
	dec, _ := NewDecryptor(key, "cred-dek-v1")
	var acct [16]byte
	if _, err := dec.Decrypt(acct, "cred-dek-v99", []byte("x"), make([]byte, 12)); err == nil {
		t.Fatal("decrypt with unresolvable key-id succeeded — must reject")
	}
}

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	return k
}
