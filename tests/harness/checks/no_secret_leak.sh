#!/usr/bin/env bash
# no_secret_leak — BLOCKING CI gate + harness check. Greps the running services' logs for the
# known dev secrets (the worker registration token and its bcrypt hash) and FAILS if any appears.
# Enforces CLAUDE.md Rule 5/6 "a credential is never logged" as an automated regression guard:
# today workerlink logs only the worker id + cert CN (safe), but a future log.Printf(req.Token) would
# be caught here rather than shipping. SCOPE: this gate covers the WORKER TOKEN + its hash only,
# because those are the credentials that flow on every stack, with no send required. The SMTP
# credential is a different credential on a different path (it only exists once an account is
# provisioned and only reaches a worker during a send), so its leak scan lives with the send harness —
# tests/harness/phase3_send.sh S1, which greps plaintext AND base64 in logs and in the DB.
set -euo pipefail
cd "$(dirname "$0")/../../.."
DC="${COMPOSE_INVOCATION:-$(bash scripts/dev.sh compose)}"  # the invocation dev.sh uses (RAM Postgres only under DEV_RAM_PG=1)

[[ -f .env ]] || { echo "no_secret_leak: .env missing (run scripts/dev.sh certs first)"; exit 1; }
TOKEN="$(grep -E '^DEV_WORKER_TOKEN=' .env | cut -d= -f2-)"
HASH="$(grep -E '^DEV_WORKER_TOKEN_HASH=' .env | cut -d= -f2- | sed 's/\$\$/$/g')"  # un-escape compose $$
# Worker 2's token is a DISTINCT credential, so it needs its own scan — a gate that checks only worker 1
# would call a worker-2 token leak clean. Empty when worker 2 was never provisioned; the -n guards skip it.
# `grep -E '^DEV_WORKER2_TOKEN='` is anchored so it cannot match DEV_WORKER2_TOKEN_HASH.
TOKEN2="$(grep -E '^DEV_WORKER2_TOKEN=' .env | cut -d= -f2- || true)"
HASH2="$(grep -E '^DEV_WORKER2_TOKEN_HASH=' .env | cut -d= -f2- | sed 's/\$\$/$/g' || true)"

# --profile multiworker so worker2's logs are included when it is running (a leak there must not hide).
logs="$($DC --profile multiworker logs 2>/dev/null || true)"

fail=0
if [[ -n "$TOKEN" ]] && grep -qF "$TOKEN" <<<"$logs"; then
  echo "no_secret_leak FAIL: the plaintext worker token appears in service logs"; fail=1
fi
if [[ -n "$HASH" ]] && grep -qF "$HASH" <<<"$logs"; then
  echo "no_secret_leak FAIL: the worker token bcrypt hash appears in service logs"; fail=1
fi
if [[ -n "$TOKEN2" ]] && grep -qF "$TOKEN2" <<<"$logs"; then
  echo "no_secret_leak FAIL: worker 2's plaintext token appears in service logs"; fail=1
fi
if [[ -n "$HASH2" ]] && grep -qF "$HASH2" <<<"$logs"; then
  echo "no_secret_leak FAIL: worker 2's token bcrypt hash appears in service logs"; fail=1
fi

[[ "$fail" -eq 0 ]] && echo "no_secret_leak OK: no dev token/hash found in service logs."
exit "$fail"
