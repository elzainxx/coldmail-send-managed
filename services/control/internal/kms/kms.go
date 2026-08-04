// Package kms is the Control's credential-decryption boundary. The Control is the SOLE
// decryptor (CLAUDE.md Rule 6): only this process loads the key. In Phase 0 the "KMS" is a
// local dev key file (prod is a real cloud KMS whose Decrypt principal is the Control role
// only — the IAM-separation precondition, verified by deployment sign-off, not code).
//
// The worker has no equivalent package and no key material — enforced structurally (no KMS SDK
// in the worker service) and by container mounts (a harness test asserts the worker cannot read
// the key file).
package kms

import (
	"fmt"
	"os"
)

// Decryptor holds the dev key material in-process. Real Decrypt is Phase 2 — Phase 0 only
// proves the key path is Control-only and never logged.
type Decryptor struct {
	key []byte
}

// LoadDev reads the dev key file. It requires a non-empty key and never logs its contents.
func LoadDev(path string) (*Decryptor, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read dev kms key %q: %w", path, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("dev kms key %q is empty", path)
	}
	return &Decryptor{key: b}, nil
}

// KeyLen reports the key length for a startup log line WITHOUT exposing key material.
func (d *Decryptor) KeyLen() int {
	if d == nil {
		return 0
	}
	return len(d.key)
}

// LoadEncryptKey reads raw key material for the Phase 2 admin credential-INTAKE path (Encrypt
// only). It is a DELIBERATELY separate entry point from the Control's decryptor: the admin surface
// encrypts new credentials but never decrypts (OD-7 — Decrypt stays Control-only, the send path).
// In dev/Phase 2 the Encrypt DEK is the same 32-byte dev key file (Phase 1 §0 "single master key
// acceptable V1"); in prod it is an Encrypt/DEK-wrap-only KMS key whose IAM excludes Decrypt.
// Returns exactly the raw bytes; the caller (adminapi.NewEncryptor) validates the 32-byte length.
func LoadEncryptKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credential encrypt key %q: %w", path, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("credential encrypt key %q is empty", path)
	}
	return b, nil
}

// LoadDecryptKey reads raw key material for the Phase 3 SEND-PATH decryptor (the sole decryptor,
// Rule 6). It is a deliberately separate entry point from LoadEncryptKey so the two capabilities
// read as distinct in the code even though dev reuses one key file: in prod the Decrypt key's IAM
// grants Decrypt to the Control role ONLY (sole-decryptor precondition G4). The returned bytes are
// handed to internal/credential.NewDecryptor (validates the 32-byte AES-256 length); they are
// wired ONLY into the dispatch path, never into the admin surface (OD-7, structural).
func LoadDecryptKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credential decrypt key %q: %w", path, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("credential decrypt key %q is empty", path)
	}
	return b, nil
}
