#!/usr/bin/env bash
# gen-certs — generate local-dev mTLS material for the worker<->Control channel + the SMTP sink.
#
# DEV ONLY: the dev CA never leaves dev and is NOT the prod trust root (prod uses a real
# CA / secret store; a formal cert PKI with rotate/revoke is post-V1). token=authority,
# cert=transport (P9-A). ca.key never leaves the certs dir and is gitignored.
#
# Least-privilege layout (mirrors the compose mounts): worker mounts only worker.crt/worker.key/
# ca.crt; Control mounts only control.crt/control.key/ca.crt + the KMS key; sink mounts only
# sink.crt/sink.key.
#
# Portability: all subjects/SANs are declared in an openssl CONFIG FILE written into $OUT
# (a relative path both native-Windows openssl.exe and Linux openssl can open), avoiding the
# Git-Bash/MSYS "-subj //CN=" mangling and the MSYS-vs-Windows /tmp path mismatch entirely.
set -euo pipefail

OUT="${CERT_DIR:-secrets/dev/certs}"
DAYS="${CERT_DAYS:-3650}"
FORCE="${1:-}"

export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

mkdir -p "$OUT"

if [[ -f "$OUT/ca.crt" && "$FORCE" != "--force" ]]; then
  echo "gen-certs: $OUT/ca.crt already exists; pass --force to regenerate"
  exit 0
fi
echo "gen-certs: writing dev mTLS material to $OUT"

# write_cnf(file, CN, sanCsv) — an openssl config with a prompt-less DN + SAN extension.
write_cnf() {
  local file="$1" cn="$2" san="$3"
  cat > "$file" <<EOF
[req]
distinguished_name = dn
prompt = no
[dn]
CN = $cn
[v3]
subjectAltName = $san
extendedKeyUsage = serverAuth, clientAuth
basicConstraints = CA:FALSE
EOF
}

# 1) Dev CA (self-signed), config-driven so the CN lands reliably.
cat > "$OUT/ca.cnf" <<EOF
[req]
distinguished_name = dn
prompt = no
x509_extensions = v3ca
[dn]
CN = send-managed-dev-ca
[v3ca]
basicConstraints = critical, CA:TRUE
keyUsage = critical, keyCertSign, cRLSign
EOF
openssl genrsa -out "$OUT/ca.key" 2048 2>/dev/null
openssl req -x509 -new -nodes -key "$OUT/ca.key" -sha256 -days "$DAYS" \
  -config "$OUT/ca.cnf" -out "$OUT/ca.crt" 2>/dev/null

# leaf(name, CN, sanCsv) — a CA-signed leaf with SANs + client/server EKU.
leaf() {
  local name="$1" cn="$2" san="$3"
  write_cnf "$OUT/$name.cnf" "$cn" "$san"
  openssl genrsa -out "$OUT/$name.key" 2048 2>/dev/null
  openssl req -new -key "$OUT/$name.key" -config "$OUT/$name.cnf" -out "$OUT/$name.csr" 2>/dev/null
  openssl x509 -req -in "$OUT/$name.csr" -CA "$OUT/ca.crt" -CAkey "$OUT/ca.key" \
    -CAcreateserial -days "$DAYS" -sha256 \
    -extfile "$OUT/$name.cnf" -extensions v3 -out "$OUT/$name.crt" 2>/dev/null
  rm -f "$OUT/$name.csr" "$OUT/$name.cnf"
}

# 2) Control server cert (reachable as "control" in compose, "localhost" on host).
leaf control "control" "DNS:control,DNS:localhost,IP:127.0.0.1"
# 3) Worker client cert (CN carries the worker id).
leaf worker "dev-worker-1" "DNS:worker,DNS:localhost"
# 4) SMTP sink server cert (its own leaf — sink NEVER gets ca.key/control.key).
leaf sink "smtp-sink" "DNS:smtp-sink,DNS:localhost"

rm -f "$OUT/ca.cnf"
chmod 600 "$OUT"/*.key 2>/dev/null || true
echo "gen-certs: done. CA + control/worker/sink leaves in $OUT (ca.key stays here, gitignored)."
