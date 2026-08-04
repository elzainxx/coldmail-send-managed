// Package config loads the Control's runtime configuration.
//
// Config-not-code (CLAUDE.md Rule 1): behavioral tuning lives in send_managed.config, seeded by
// migrations. Phase 0 predates send_managed.config (created in Phase 1 migration 0010), so the
// FEW values needed to bootstrap the process come from env — the same documented bootstrap
// exception as the migration runner's timeouts. These are process-wiring values (addresses,
// paths, credentials), not behavioral dials. See docs/open-decisions.md OD-18.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerlink"
	"github.com/coldmail-send-managed/control/internal/tlsutil"
)

// Config is the resolved Control configuration.
type Config struct {
	DatabaseURL             string
	HealthAddr              string
	WorkerlinkAddr             string
	WorkerlinkStreamAddr       string // Phase 3: the persistent send-stream mTLS listener (§2.1)
	KMSDevKeyFile           string
	SingletonAcquireTimeout time.Duration
	TLS                     tlsutil.ServerConfig
	WorkerRegistry             workerlink.Registry

	// Admin API (Domain B, operator). Phase 2. The bearer-key HASHES (bcrypt) are configured
	// out-of-band (comma-separated for a small key set); the plaintext key is never stored. The
	// credential write-key ALIAS (OD-6, e.g. cred-dek-v3) is stored in credential_key_id. The
	// admin surface reuses the Control's dev key file as the AES-256 credential key material in
	// dev/Phase 2 (prod: an Encrypt-only DEK — the admin IAM gets Encrypt, never Decrypt, OD-7).
	AdminKeyHashes  []string
	CredentialKeyID string
	AdminEnabled    bool // false when no key hashes configured — the surface stays unmounted

	// Platform API (Domain A, Phase 3). The service-token HASHES (bcrypt) authenticate the
	// platform-facing quote/run/report/describe surface. DISTINCT credential from the operator
	// admin key (Rule 6 — the two domains never share). The surface mounts only when at least one
	// hash is configured (fail-closed). The credential Decryptor is loaded separately (send path).
	PlatformTokenHashes []string
	PlatformEnabled     bool
}

// FromEnv resolves configuration from the environment, failing loudly on anything missing
// that has no safe default.
func FromEnv() (*Config, error) {
	c := &Config{
		DatabaseURL:       env("DATABASE_URL", ""),
		HealthAddr:        env("CONTROL_HEALTH_ADDR", ":8080"),
		WorkerlinkAddr:       env("CONTROL_WORKERLINK_ADDR", ":8443"),
		WorkerlinkStreamAddr: env("CONTROL_WORKERLINK_STREAM_ADDR", ":8444"),
		KMSDevKeyFile:     env("KMS_DEV_KEY_FILE", ""),
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if c.KMSDevKeyFile == "" {
		return nil, fmt.Errorf("KMS_DEV_KEY_FILE is required (Control is the sole decryptor)")
	}

	secs, err := envInt("CONTROL_SINGLETON_ACQUIRE_TIMEOUT_SEC", 60)
	if err != nil {
		return nil, err
	}
	if secs < 0 {
		return nil, fmt.Errorf("CONTROL_SINGLETON_ACQUIRE_TIMEOUT_SEC must be >= 0")
	}
	c.SingletonAcquireTimeout = time.Duration(secs) * time.Second

	c.TLS = tlsutil.ServerConfig{
		CertFile: env("MTLS_SERVER_CERT", "/secrets/mtls/control.crt"),
		KeyFile:  env("MTLS_SERVER_KEY", "/secrets/mtls/control.key"),
		CAFile:   env("MTLS_CA_CERT", "/secrets/mtls/ca.crt"),
	}

	// Phase-0 worker registry: a single dev worker id + token HASH from env. Phase 1 moves this
	// to send_managed.boxes.token_hash. token=authority, cert=transport (P9-A). If unset, the
	// registry rejects all registrations (fail-closed).
	c.WorkerRegistry = workerlink.Registry{
		WorkerID:     env("DEV_WORKER_ID", ""),
		TokenHash: env("DEV_WORKER_TOKEN_HASH", ""),
	}

	// Admin API (Phase 2). ADMIN_API_KEY_HASHES is a comma-separated list of bcrypt hashes; the
	// plaintext admin key never appears in config. CREDENTIAL_KEY_ID is the write-key alias
	// stored in credential_key_id (OD-6). The surface only mounts when at least one hash is
	// configured — otherwise it stays off (fail-closed: no key => no operator surface).
	c.AdminKeyHashes = splitNonEmpty(env("ADMIN_API_KEY_HASHES", ""))
	c.CredentialKeyID = env("CREDENTIAL_KEY_ID", "cred-dek-v1")
	c.AdminEnabled = len(c.AdminKeyHashes) > 0

	// Platform API (Phase 3). PLATFORM_TOKEN_HASHES is a comma-separated list of bcrypt hashes;
	// the plaintext service token never appears in config. Fail-closed: no hash => surface off.
	c.PlatformTokenHashes = splitNonEmpty(env("PLATFORM_TOKEN_HASHES", ""))
	c.PlatformEnabled = len(c.PlatformTokenHashes) > 0

	return c, nil
}

// splitNonEmpty splits a comma-separated env value, trimming spaces and dropping empties.
func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: not an integer: %q", key, v)
	}
	return n, nil
}
