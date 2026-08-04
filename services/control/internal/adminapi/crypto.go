package adminapi

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// Encryptor is the credential-intake encrypt-on-write primitive (§2.6). It is deliberately
// Encrypt-ONLY: there is NO Decrypt method here, mirroring the OD-7 deployment precondition
// that the admin service's IAM grant is Encrypt/DEK-wrap only and Decrypt stays Control-only (the
// send path, Phase 3). Even a compromised admin surface can write new credentials but cannot
// read existing ones back — a structural property, not just an IAM promise.
//
// AES-256-GCM, 12-byte random nonce per encryption, 128-bit tag APPENDED inside the ciphertext
// (combined mode — the exact layout send_managed.accounts.credential_ciphertext stores). AAD =
// account.id (raw 16 bytes) ‖ credential_key_id (utf8) binds the ciphertext to its row so it
// cannot be transplanted to another account (OD-3, LOCKED-ADOPTED). The key lives OUTSIDE the
// DB (Control-held dev key file in Phase 0/2; a real KMS DEK in prod).
type Encryptor struct {
	key   []byte // 32 bytes for AES-256; held in-process only, never logged
	keyID string // namespaced alias e.g. "cred-dek-v3" (OD-6), NOT a raw KMS ARN
}

// NewEncryptor builds an Encryptor from raw key material + the current write-key alias. The key
// must be exactly 32 bytes (AES-256). keyID must be non-empty (it is stored in
// credential_key_id and drives rotation + AAD binding).
func NewEncryptor(key []byte, keyID string) (*Encryptor, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("credential key must be 32 bytes for AES-256, got %d", len(key))
	}
	if keyID == "" {
		return nil, fmt.Errorf("credential_key_id (write-key alias) must be non-empty")
	}
	// Fail fast if the key can't build a cipher (never at request time).
	if _, err := cipher.NewGCM(mustBlock(key)); err != nil {
		return nil, fmt.Errorf("init gcm: %w", err)
	}
	return &Encryptor{key: append([]byte(nil), key...), keyID: keyID}, nil
}

// KeyID returns the current write-key alias stored in credential_key_id.
func (e *Encryptor) KeyID() string { return e.keyID }

// Encrypt seals plaintext under the current key with a FRESH random nonce and AAD =
// accountID(16 raw bytes) ‖ keyID. Returns (ciphertext‖tag, nonce). The plaintext is never
// retained; callers zero their own copy after this returns.
func (e *Encryptor) Encrypt(accountID [16]byte, plaintext []byte) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("nonce: %w", err)
	}
	aad := aadFor(accountID, e.keyID)
	// Seal appends ciphertext‖tag to dst(nil) — the combined-mode layout the schema stores.
	ct := gcm.Seal(nil, nonce, plaintext, aad)
	return ct, nonce, nil
}

// aadFor builds the credential AAD = raw 16-byte account UUID ‖ utf8(credential_key_id). It is
// the encode contract shared with the Control's decrypt path (Phase 3) and the harness reference
// implementation (tests/harness/factories.go aadFor).
func aadFor(accountUUID [16]byte, keyID string) []byte {
	out := make([]byte, 0, 16+len(keyID))
	out = append(out, accountUUID[:]...)
	out = append(out, []byte(keyID)...)
	return out
}

// mustBlock is a NewEncryptor-time helper; a 32-byte key never fails aes.NewCipher.
func mustBlock(key []byte) cipher.Block {
	b, _ := aes.NewCipher(key)
	return b
}
