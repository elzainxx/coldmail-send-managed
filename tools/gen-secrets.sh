#!/usr/bin/env bash
# gen-secrets — generate local-dev, non-cert secrets:
#   1) the dev KMS master key (Control is the SOLE decryptor; worker never gets this). In Phase 2 the
#      admin credential-intake path ALSO reads this file, but Encrypt-only (OD-7): the admin
#      service can wrap new credentials, never Decrypt — Decrypt stays Control-only (the send path).
# DEV ONLY. Nothing here is committed (.gitignore excludes secrets/, *.key). Prod sources
# these from a real KMS + secret store; the IAM-separation precondition (only the Control
# holds KMS Decrypt; the admin service holds Encrypt-only) is a deployment sign-off (S15/G4).
#
# NOTE (Phase 2 reconciliation): the operator admin (Domain B) bearer key is now minted with a
# BCRYPT hash by `provision-worker admin-key` and written to .env by `scripts/dev.sh certs`
# (ADMIN_API_KEY / ADMIN_API_KEY_HASHES). That matches the spec's "same discipline as
# send_managed.boxes.token_hash" (bcrypt, constant-time — services/control/internal/adminapi/auth.go).
# The earlier SHA-256 PLATFORM_ADMIN_KEY_HASH scaffold printed here has been REMOVED to avoid two
# competing admin-key mechanisms; there is one operator key, bcrypt-hashed.
set -euo pipefail

KMS_DIR="${KMS_DIR:-secrets/dev/kms}"
mkdir -p "$KMS_DIR"

# 1) Dev KMS master key: 32 random bytes (AES-256-class). Control-only mount in compose.
if [[ ! -f "$KMS_DIR/dev-master.key" ]]; then
  openssl rand -out "$KMS_DIR/dev-master.key" 32
  chmod 600 "$KMS_DIR/dev-master.key" 2>/dev/null || true
  echo "gen-secrets: wrote dev KMS master key -> $KMS_DIR/dev-master.key (Control-only; admin Encrypt-only)"
else
  echo "gen-secrets: dev KMS master key already present ($KMS_DIR/dev-master.key)"
fi

echo "gen-secrets: operator admin (Domain B) key is minted (bcrypt) by 'provision-worker admin-key'"
echo "             and written to .env by 'scripts/dev.sh certs' (ADMIN_API_KEY / ADMIN_API_KEY_HASHES)."
echo "gen-secrets: done."
