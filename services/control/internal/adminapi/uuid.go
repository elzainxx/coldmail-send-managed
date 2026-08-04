package adminapi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// newUUIDv4 generates a random RFC 4122 v4 UUID application-side (OD-4). Generating the id
// before the INSERT lets the credential AAD (account.id ‖ credential_key_id) be computed at
// encrypt time, so the ciphertext is bound to the exact row in a SINGLE insert (no
// insert-then-update dance). Returns the canonical string form and the raw 16 bytes.
func newUUIDv4() (string, [16]byte, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", b, fmt.Errorf("uuid rand: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return formatUUID(b), b, nil
}

// uuidToBytes parses a canonical UUID string into its 16 raw bytes (for AAD binding on update,
// where the id already exists). Returns an error on a malformed string.
func uuidToBytes(s string) ([16]byte, error) {
	var out [16]byte
	if !isUUID(s) {
		return out, fmt.Errorf("not a uuid: %q", s)
	}
	clean := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	raw, err := hex.DecodeString(clean)
	if err != nil || len(raw) != 16 {
		return out, fmt.Errorf("uuid decode: %q", s)
	}
	copy(out[:], raw)
	return out, nil
}

// formatUUID renders 16 bytes as canonical 8-4-4-4-12 hex.
func formatUUID(b [16]byte) string {
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
