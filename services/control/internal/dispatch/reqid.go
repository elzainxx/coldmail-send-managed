package dispatch

import (
	"crypto/rand"
	"encoding/hex"
)

// newRequestID mints an opaque correlation id for a push_job/report pair (§2.6 — the bridge
// between the internal and external vocabularies). Same shape as the admin surface's request ids.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req_0000000000000000"
	}
	return "req_" + hex.EncodeToString(b[:])
}
