// Command provision-worker is the out-of-band worker provisioning CLI (P9-A / ADR-0005).
//
// token = authorization AUTHORITY; the mTLS client cert is transport. Flow:
//
//	new    — mint a worker id + a one-time token; print the plaintext token ONCE and the
//	         bcrypt HASH to seed. Plaintext is never stored.
//	seed   — emit the SQL to store the token HASH (Phase 1: into send_managed.boxes.token_hash;
//	         Phase 0: into the Control's DEV_WORKER_TOKEN_HASH env — the worker table arrives in
//	         Phase 1's send_managed_0006). Never stores the plaintext.
//	revoke — emit the SQL to clear/rotate token_hash; a decommissioned worker is refused on
//	         next connect regardless of a still-valid cert.
//	hash   — bcrypt-hash a provided token (used by tooling/tests to compute a hash).
//
// The bcrypt hash format matches exactly what the Control verifies with
// bcrypt.CompareHashAndPassword (services/control/internal/workerlink).
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "new":
		id := flagVal("--id", "dev-worker-1")
		token := randToken()
		hash := mustHash(token)
		fmt.Printf("# worker provisioned (P9-A). token=authority, cert=transport.\n")
		fmt.Printf("WORKER_ID=%s\n", id)
		fmt.Printf("WORKER_TOKEN=%s        # SHOWN ONCE — copy to the worker's env; never stored\n", token)
		fmt.Printf("DEV_WORKER_TOKEN_HASH=%s   # seed into the Control env (Phase 0) / send_managed.boxes (Phase 1)\n", hash)
	case "hash":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: provision-worker hash <token>")
			os.Exit(2)
		}
		fmt.Println(mustHash(os.Args[2]))
	case "admin-key":
		// Mint an operator admin API key (Domain B, Phase 2). Same discipline as the worker token:
		// print the plaintext key ONCE (copy to the operator's client) and the bcrypt HASH to
		// configure in ADMIN_API_KEY_HASHES. Plaintext is never stored.
		key := randToken()
		hash := mustHash(key)
		fmt.Printf("# operator admin key provisioned (Domain B). Hashed at rest; constant-time compared.\n")
		fmt.Printf("ADMIN_API_KEY=%s        # SHOWN ONCE — the operator's bearer key; never stored\n", key)
		fmt.Printf("ADMIN_API_KEY_HASH=%s   # configure in ADMIN_API_KEY_HASHES (comma-separated for a set)\n", hash)
	case "seed":
		id := flagVal("--id", "")
		h := flagVal("--token-hash", "")
		if id == "" || h == "" {
			fmt.Fprintln(os.Stderr, "usage: provision-worker seed --id <id> --token-hash <bcrypt-hash>")
			os.Exit(2)
		}
		// Phase 1 form (send_managed.boxes exists then). Emitted for reference/CI. Single quotes in
		// id/hash are escaped ('') so a stray quote in --id can't produce broken/dangerous SQL.
		fmt.Printf("-- Phase 1+ (requires send_managed.boxes from send_managed_0006):\n")
		fmt.Printf("UPDATE send_managed.boxes SET token_hash='%s' WHERE id='%s';\n", sqlLit(h), sqlLit(id))
		fmt.Printf("-- Phase 0: instead set DEV_WORKER_ID=%s and DEV_WORKER_TOKEN_HASH=%s in the Control env.\n", id, h)
	case "revoke":
		id := flagVal("--id", "")
		if id == "" {
			fmt.Fprintln(os.Stderr, "usage: provision-worker revoke --id <id>")
			os.Exit(2)
		}
		fmt.Printf("-- decommission: refused on next connect regardless of cert.\n")
		fmt.Printf("UPDATE send_managed.boxes SET token_hash=NULL, status='disabled' WHERE id='%s';\n", sqlLit(id))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `provision-worker — out-of-band worker provisioning (token=authority, cert=transport)

usage:
  provision-worker new       [--id <id>]              mint id + one-time token + bcrypt hash
  provision-worker hash      <token>                  bcrypt-hash a token
  provision-worker admin-key                          mint an operator admin API key + bcrypt hash
  provision-worker seed      --id <id> --token-hash <h>  emit seed SQL / env line
  provision-worker revoke    --id <id>                emit revocation SQL
`)
}

func randToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		fmt.Fprintf(os.Stderr, "provision-worker: rand: %v\n", err)
		os.Exit(1)
	}
	return hex.EncodeToString(b)
}

func mustHash(token string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provision-worker: bcrypt: %v\n", err)
		os.Exit(1)
	}
	return string(h)
}

// sqlLit escapes single quotes for safe interpolation into an emitted SQL string literal.
// (The SQL is printed for a human/CI to review and apply, not executed here — but a stray
// quote in an operator-supplied --id must not produce broken or dangerous SQL.)
func sqlLit(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func flagVal(name, def string) string {
	for i, a := range os.Args {
		if a == name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return def
}
