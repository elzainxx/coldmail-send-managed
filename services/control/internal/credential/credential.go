// Package credential is the Control's send-path credential DECRYPTOR — the sole place in the
// whole system that turns ciphertext back into a plaintext SMTP secret (CLAUDE.md Rule 6;
// Phase 3 §0/§3). It is DELIBERATELY a separate package from internal/adminapi:
//
//   - adminapi holds an Encrypt-ONLY primitive (adminapi.Encryptor) with NO Decrypt method,
//     so the operator surface can write credentials but never read them back (OD-7).
//   - This package holds a Decrypt-ONLY primitive with NO Encrypt method, wired ONLY into the
//     dispatch/send path (cmd/control -> dispatcher). It is never injected into adminapi.Server,
//     so structurally no /v1/admin/* route can reach plaintext even though decrypt now lives in
//     the same process. The behavioral suite proves this (no admin route yields a credential).
//
// The worker has no equivalent: it holds no key material and never decrypts (it receives an
// already-decrypted secret over mTLS, scoped to one send).
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
)

// Decryptor unseals send_managed.accounts credentials. AES-256-GCM, 12-byte nonce, 128-bit tag
// appended inside the ciphertext (the combined-mode layout the schema stores). AAD =
// account.id (raw 16 bytes) ‖ credential_key_id (utf8) — the exact encode contract the admin
// Encryptor and the harness reference implementation share (Phase 2 OD-3, LOCKED-ADOPTED). The
// key lives OUTSIDE the DB (Control-held dev key file in dev; a real KMS Decrypt-scoped key in
// prod whose IAM grants Decrypt to the Control role ONLY — the sole-decryptor precondition).
type Decryptor struct {
	key   []byte // 32 bytes for AES-256; held in-process only, never logged
	keyID string // the write-key alias this Decryptor can resolve (e.g. "cred-dek-v1")
}

// NewDecryptor builds a Decryptor from raw key material + the key-id alias it resolves. key must
// be exactly 32 bytes (AES-256); keyID must be non-empty. Fails fast so a bad key never surfaces
// at send time.
func NewDecryptor(key []byte, keyID string) (*Decryptor, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("credential key must be 32 bytes for AES-256, got %d", len(key))
	}
	if keyID == "" {
		return nil, fmt.Errorf("credential_key_id (write-key alias) must be non-empty")
	}
	if _, err := cipher.NewGCM(mustBlock(key)); err != nil {
		return nil, fmt.Errorf("init gcm: %w", err)
	}
	return &Decryptor{key: append([]byte(nil), key...), keyID: keyID}, nil
}

// Decrypt unseals ciphertext(‖tag) for the row identified by accountID + keyID. It rebuilds the
// AAD from the row's own id + credential_key_id, so a ciphertext transplanted to another row (or
// re-labelled with a different key id) fails authentication and releases NO plaintext (fail
// closed). The returned plaintext is the caller's to zero after use — this package retains none.
func (d *Decryptor) Decrypt(accountID [16]byte, keyID string, ciphertext, nonce []byte) ([]byte, error) {
	// Resolve the key from the row's key-id. In dev there is a single alias; a row labelled with
	// an alias this Control cannot resolve is an operational/rotation error, never a silent
	// wrong-key decrypt attempt.
	if keyID != d.keyID {
		return nil, fmt.Errorf("cannot resolve credential_key_id %q (this Control resolves %q)", keyID, d.keyID)
	}
	block, err := aes.NewCipher(d.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("nonce length %d, want %d", len(nonce), gcm.NonceSize())
	}
	aad := aadFor(accountID, keyID)
	pt, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		// GCM auth failure (tamper / wrong key / wrong AAD). Never leak detail or partial output.
		return nil, fmt.Errorf("credential decrypt failed (authentication)")
	}
	return pt, nil
}

// KeyID reports the alias this Decryptor resolves (diagnostics only; never the key material).
func (d *Decryptor) KeyID() string { return d.keyID }

// aadFor builds the credential AAD = raw 16-byte account UUID ‖ utf8(credential_key_id). Shared
// encode contract with adminapi.aadFor and tests/harness/factories.go aadFor.
func aadFor(accountUUID [16]byte, keyID string) []byte {
	out := make([]byte, 0, 16+len(keyID))
	out = append(out, accountUUID[:]...)
	out = append(out, []byte(keyID)...)
	return out
}

func mustBlock(key []byte) cipher.Block {
	b, _ := aes.NewCipher(key)
	return b
}
