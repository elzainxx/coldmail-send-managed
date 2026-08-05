#!/usr/bin/env bash
# run.sh — the real-infra behavioral harness. Runs against the LIVE compose stack (real
# Postgres + real Control + real worker + real Mailpit). NEVER mocks infra (CLAUDE.md Rule 3).
#
# Suites:
#   1. migration lifecycle        (tools/send-managed-migrate integration test — real PG)
#   2. DB isolation modes         (template-clone cross-connection + txn-rollback)
#   3. worker<->Control mTLS register/heartbeat + wrong-token negative control (live Control)
#   4. two-Control singleton        (real 2nd Control process exits, no double-lock)
#   5. worker-CANNOT-read-KMS        (the worker container cannot read the Control's KMS key)
set -euo pipefail
cd "$(dirname "$0")/../.."

# Compose invocation with explicit base + override -f flags (portable across OSes; passed
# from scripts/dev.sh, or defaulted here so run.sh works standalone).
DC="${COMPOSE_INVOCATION:-docker compose -f docker-compose.yml -f tests/harness/compose.test.yml}"

PASS=0; FAIL=0
ok()   { echo "  PASS: $1"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL: $1"; FAIL=$((FAIL+1)); }
hr()   { echo "──────────────────────────────────────────────────────────────"; }

# --- Derive connection info from the running stack ---------------------------
# Admin (superuser) DSN via the mapped host port 5432.
SUPER="${POSTGRES_SUPERPASS:-devsuperpass}"
export TEST_ADMIN_DATABASE_URL="postgres://postgres:${SUPER}@localhost:5432/platform"
export MIGRATIONS_DIR="$(pwd)/db/migrations"
export PREFLIGHT_SQL="$(pwd)/db/preflight/check_platform_tables.sql"

# The migration lifecycle test runs the down migration, so it gets its OWN dedicated DB —
# never the live `platform` DB the Control is using (dropping the send_managed schema out from under
# a running Control would disrupt the stack). Create + seed fixtures here.
$DC exec -T postgres psql -U postgres -d postgres -v ON_ERROR_STOP=1 -q \
  -c "DROP DATABASE IF EXISTS send_managed_migtest" -c "CREATE DATABASE send_managed_migtest" >/dev/null 2>&1 || true
$DC exec -T postgres psql -U postgres -d send_managed_migtest -v ON_ERROR_STOP=1 -q \
  -c "CREATE TABLE IF NOT EXISTS public.users(id uuid primary key default gen_random_uuid(), email text, is_active boolean default true, created_at timestamptz default now())" \
  -c "CREATE TABLE IF NOT EXISTS public.job_items(id uuid primary key default gen_random_uuid(), campaign_id uuid, user_id uuid, recipient_email text, rendered_subject text, rendered_body text, reply_to text, status text, created_at timestamptz default now())" >/dev/null 2>&1
export TEST_DATABASE_URL="postgres://postgres:${SUPER}@localhost:5432/send_managed_migtest"

# Worker token for the live mTLS test (from .env, written by scripts/dev.sh certs).
if [[ -f .env ]]; then
  # shellcheck disable=SC1091
  export DEV_WORKER_ID="$(grep -E '^DEV_WORKER_ID=' .env | cut -d= -f2-)"
  export WORKER_TOKEN="$(grep -E '^DEV_WORKER_TOKEN=' .env | cut -d= -f2-)"
fi
export CONTROL_WORKERLINK_URL="https://localhost:8443"
export WORKER_MTLS_CERT="$(pwd)/secrets/dev/certs/worker.crt"
export WORKER_MTLS_KEY="$(pwd)/secrets/dev/certs/worker.key"
export WORKER_MTLS_CA="$(pwd)/secrets/dev/certs/ca.crt"

# --- Build a migrated TEMPLATE db for the isolation suite --------------------
# Create send_managed_template as a FRESH empty DB (NOT a clone of the live `platform`). Cloning
# platform would require pg_terminate_backend on its connections — which would kill the
# Control's singleton-lock connection (SQLSTATE 57P01) and release the lock. The isolation
# tests only need a database with the platform fixture tables + the migrated send_managed schema,
# so we build one from scratch and never touch platform's live connections.
echo "== preparing template DB (send_managed_template) =="
$DC exec -T postgres psql -U postgres -d postgres -v ON_ERROR_STOP=1 -q \
  -c "DROP DATABASE IF EXISTS send_managed_template" \
  -c "CREATE DATABASE send_managed_template" >/dev/null 2>&1 || true
# Seed the platform fixture tables + apply the send_managed migration chain into the template,
# using an admin DSN pointed at send_managed_template (never platform).
TPL_DSN="postgres://postgres:${SUPER}@localhost:5432/send_managed_template"
$DC exec -T postgres psql -U postgres -d send_managed_template -v ON_ERROR_STOP=1 -q \
  -c "CREATE TABLE IF NOT EXISTS public.users(id uuid primary key default gen_random_uuid(), email text, is_active boolean default true, created_at timestamptz default now())" \
  -c "CREATE TABLE IF NOT EXISTS public.job_items(id uuid primary key default gen_random_uuid(), campaign_id uuid, user_id uuid, recipient_email text, rendered_subject text, rendered_body text, reply_to text, status text, created_at timestamptz default now())" >/dev/null 2>&1
# up-and-set-password: apply the full chain AND set send_managed_app's password out-of-band, so
# the Phase 1 role-isolation tests (T5/T6/T3e) can connect as send_managed_app against clones.
if MIGRATE_DATABASE_URL="$TPL_DSN" SEND_MANAGED_MODULE_PASSWORD="${SEND_MANAGED_MODULE_PASSWORD:-devmodulepass}" \
     tools/send-managed-migrate/bin/send-managed-migrate up-and-set-password >/dev/null 2>&1; then
  export TEST_TEMPLATE_DB="send_managed_template"; echo "  template ready (fresh DB, migrated + app password; platform untouched)"
else
  export TEST_TEMPLATE_DB=""; echo "  template create failed"
fi
# Phase 1 behavioral tests connect as send_managed_app; hand them the dev password.
export SEND_MANAGED_MODULE_PASSWORD="${SEND_MANAGED_MODULE_PASSWORD:-devmodulepass}"

hr; echo "SUITE 1+2: Go real-infra tests (migration lifecycle + DB isolation + Phase 1 data model)"; hr
# Judge on the Go test verdict line ("^ok"/"^FAIL"), NOT the raw exit code: on Windows the
# post-test temp-.exe cleanup ("unlinkat ... being used by another process") makes `go test`
# exit non-zero even when every test PASSES. That artifact does not occur on Linux CI. We run
# the tests, strip the cleanup noise, and decide pass/fail from whether Go printed a FAIL line.
go_suite() { # $1=dir  $2=label
  local out
  # The trailing `|| true` is load-bearing under `set -euo pipefail`: on Windows `go test` can
  # exit non-zero purely from the post-test temp-.exe cleanup ("unlinkat ... being used by
  # another process") even when EVERY test PASSES, and with pipefail that non-zero would
  # propagate through the command substitution and `set -e` would kill this script mid-run
  # (observed: the larger control binary triggers the cleanup artifact intermittently). We
  # deliberately judge pass/fail from the Go verdict LINE below, never the exit code — so
  # swallowing the substitution's exit status here is correct, not a masked failure. On Linux
  # CI the artifact does not occur, so `|| true` is a no-op there.
  out="$( { cd "$1" && GOTOOLCHAIN=local go test -count=1 -v . 2>&1 | grep -vE "unlinkat|being used by another process"; } || true )"
  echo "$out" | grep -E "^(ok|FAIL|--- (PASS|FAIL))" | tail -40
  if echo "$out" | grep -qE "^--- FAIL|^FAIL\b"; then bad "$2"; else ok "$2"; fi
}
go_suite tools/send-managed-migrate "migration lifecycle + contiguity guard (apply/idempotent/down/gap)"
# The harness package now holds BOTH the isolation-mode tests AND the Phase 1 behavioral suite
# (T1 concurrent claim, T1b N-workers, T2 event-per-transition, T3 no-silent-state-change,
# T3e RLS default-deny, T3f superuser-reads-only-ciphertext, T4 credential round-trip+tamper+
# AAD, T5 role-denied-platform-write, T5b job_item_id UNIQUE, T6 append-only job_events). All
# run against real Postgres clones.
go_suite tests/harness "DB isolation + Phase 1 data model (claim/events/RLS/crypto/append-only)"
# The ROOT mux's own routing (item 7b's third surface) and /readyz's uniform envelope (item 10).
# Listed HERE, before any TEMPLATE is built, because nothing in this package needs a migrated
# schema: the 7b tests drive the real NewHealthServer wiring with nil db/guard (what they assert is
# what that constructor REGISTERS), and the item-10 tests need only a reachable Postgres — Ping needs
# no tables and pg_try_advisory_lock needs no schema, so they create a plain throwaway database off
# TEST_ADMIN_DATABASE_URL, which is exported at the top of this script.
#
# It is listed at all because it was missed once: internal/httpapi's first-ever test file landed
# with item 7b (e3fee99) and nothing ran it. This script names each package explicitly (never
# `go test ./...`), so a new test package is invisible to the harness AND to CI — which runs only
# gofmt + vet over ./... and this script — until a line is added here. A written test that nothing
# executes is worse than no test: it reports coverage it does not provide.
go_suite services/control/internal/httpapi "root-mux coded 404 + log-forgery neutralization (item 7b) + readyz uniform checks{} envelope (item 10)"

# --- Two packages that were WRITTEN AND PASSING BUT RUN BY NOTHING (found 2026-08-05) ----------
# Exactly the failure the paragraph above records, still live two packages later. A cross-repo
# audit of every *_test.go directory against this script's go_suite list found these two here (and
# four in module 3): 3 tests, 0 skips, all passing, executed by nothing. Neither this script nor CI
# would ever have said so — CI runs gofmt + `go vet ./...` + this script, and `go vet` type-checks
# test files WITHOUT running them, so an unwired package is indistinguishable from a wired one in
# every signal we had. Both need no database and no stack (verified with every TEST_* variable
# unset), so they belong here rather than after a template build.
#
# Standing rule from this finding: a test that EXISTS is not a test that RUNS. When adding either a
# test package or a CI gate, verify the wiring — ideally by breaking it once and watching red.
go_suite services/control/internal/workerlink  "workerlink stream in-flight accounting (2 tests)"
go_suite services/control/internal/workerproto "variation wire-format round-trip (1 test)"

# --- Phase 2 admin API behavioral suite (real Server over httptest against real PG clones) -----
# These tests live in the control module (internal/adminapi) and drive the ACTUAL admin handlers
# against clones of a template migrated 0000..0014. Build a dedicated template so the clones have
# send_managed.account_events (0014) + send_managed_app's password. Never touches the live `platform` DB.
echo "== preparing Phase 2 admin template (send_managed_template_p2, migrated 0000..0014) =="
$DC exec -T postgres psql -U postgres -d postgres -v ON_ERROR_STOP=1 -q \
  -c "DROP DATABASE IF EXISTS send_managed_template_p2" \
  -c "CREATE DATABASE send_managed_template_p2" >/dev/null 2>&1 || true
P2_DSN="postgres://postgres:${SUPER}@localhost:5432/send_managed_template_p2"
$DC exec -T postgres psql -U postgres -d send_managed_template_p2 -v ON_ERROR_STOP=1 -q \
  -c "CREATE TABLE IF NOT EXISTS public.users(id uuid primary key default gen_random_uuid(), email text, is_active boolean default true, created_at timestamptz default now())" \
  -c "CREATE TABLE IF NOT EXISTS public.job_items(id uuid primary key default gen_random_uuid(), campaign_id uuid, user_id uuid, recipient_email text, rendered_subject text, rendered_body text, reply_to text, status text, created_at timestamptz default now())" >/dev/null 2>&1
if MIGRATE_DATABASE_URL="$P2_DSN" SEND_MANAGED_MODULE_PASSWORD="${SEND_MANAGED_MODULE_PASSWORD:-devmodulepass}" \
     tools/send-managed-migrate/bin/send-managed-migrate up-and-set-password >/dev/null 2>&1; then
  export TEST_TEMPLATE_DB="send_managed_template_p2"; echo "  Phase 2 template ready (migrated 0000..0014 + app password)"
else
  export TEST_TEMPLATE_DB=""; echo "  Phase 2 template create failed"
fi
# The adminapi tests share the same admin DSN + module password + template env as the harness.
( cd services/control && GOTOOLCHAIN=local go build -o /dev/null ./... ) >/dev/null 2>&1 || true
go_suite services/control/internal/adminapi "Phase 2 admin API (credential intake/omit/AAD, pools/owner/cycle/RESTRICT, lifecycle+audit, config, auth 401)"
# Restore the Phase 1 isolation template env for any later suite that expects it.
export TEST_TEMPLATE_DB="send_managed_template"

hr; echo "SUITE 3: worker<->Control mTLS register/heartbeat (probed IN-NETWORK, from the worker container)"; hr
# Probe from INSIDE the worker container: real mTLS on the compose network, using the worker's
# own mounted client cert. (Host->localhost is intercepted by host antivirus TLS-inspection,
# so an in-network probe is the correct, real test — see Docs/history/open-decisions.md OD-19.)
WORKERTOK="$(grep -E '^DEV_WORKER_TOKEN=' .env | cut -d= -f2-)"
reg_code() { # $1=token
  $DC exec -T worker sh -c "curl -s -o /dev/null -w '%{http_code}' --cert /secrets/mtls/worker.crt --key /secrets/mtls/worker.key --cacert /secrets/mtls/ca.crt -X POST https://control:8443/v1/worker/register -H 'content-type: application/json' -d '{\"box_id\":\"dev-worker-1\",\"token\":\"$1\"}'"
}
hb_code() {
  $DC exec -T worker sh -c "curl -s -o /dev/null -w '%{http_code}' --cert /secrets/mtls/worker.crt --key /secrets/mtls/worker.key --cacert /secrets/mtls/ca.crt -X POST https://control:8443/v1/worker/heartbeat -H 'content-type: application/json' -d '{\"box_id\":\"dev-worker-1\"}'"
}
RC_OK="$(reg_code "$WORKERTOK" 2>/dev/null || true)"
[[ "$RC_OK" == "200" ]] && ok "register with correct token over mTLS -> 200" || bad "register(correct token) got '$RC_OK', want 200"
RC_HB="$(hb_code 2>/dev/null || true)"
[[ "$RC_HB" == "200" ]] && ok "heartbeat after register -> 200" || bad "heartbeat got '$RC_HB', want 200"
# NEGATIVE CONTROL (anti-vacuity): same mTLS cert, WRONG token MUST be 401 — proves the
# token is a real second authorization gate, not just mTLS being open.
RC_BAD="$(reg_code "definitely-the-wrong-token" 2>/dev/null || true)"
[[ "$RC_BAD" == "401" ]] && ok "register with WRONG token -> 401 (token gate enforced)" || bad "wrong-token register got '$RC_BAD', want 401"

hr; echo "SUITE 4: two-Control singleton (real second Control process must exit, no double-lock)"; hr
# Start a SECOND control against the SAME Postgres. It must fail to acquire the singleton lock
# and exit non-zero WITHOUT issuing work. acquire timeout kept short so the test is fast.
# -T (no TTY): this control is expected to EXIT fast. It races the primary for the lock and
# must lose. --entrypoint keeps it from inheriting anything unexpected; --rm cleans up.
SECOND_OUT="$($DC run --rm --no-deps -T \
  -e CONTROL_SINGLETON_ACQUIRE_TIMEOUT_SEC=3 control 2>&1 || true)"
if echo "$SECOND_OUT" | grep -q "another Control is active; exiting cleanly"; then
  ok "second Control exited cleanly (singleton lock held by the first)"
else
  bad "second Control did NOT report clean singleton exit; output: $(echo "$SECOND_OUT" | tail -3)"
fi
# Anti-vacuity: prove the FIRST control is still healthy (the lock is genuinely held, not free).
#
# The `sed` is REQUIRED, not tidying. Item 10 added the contract's checks{} map, which carries a
# SECOND "singleton_lock":true inside it, so a bare grep would match the nested copy and keep
# passing even if the top-level field — the one Docs/monitoring.md tells an uptime monitor to alert
# on — were deleted. That would make this anti-vacuity check itself vacuous. Strip the checks object
# first, then grep what remains, so this asserts the TOP-LEVEL field specifically.
# `[^}]*` is safe because checks{} is a flat object of name->bool; readyz_test.go's
# TestReadyz_TopLevelFieldsAreDistinguishableFromTheNestedCopy pins that flatness AND pins that
# stripping it leaves a top-level match behind, so this sed cannot silently stop matching.
READYZ_TOP="$(curl -fsS http://localhost:8080/readyz | sed 's/"checks":{[^}]*}//')"
if echo "$READYZ_TOP" | grep -q '"singleton_lock":true'; then
  ok "first Control still holds the singleton lock (readyz top-level singleton_lock=true)"
else
  bad "first Control readyz does not report a TOP-LEVEL singleton_lock=true; body minus checks{}: $READYZ_TOP"
fi

hr; echo "SUITE 5: worker CANNOT read KMS key material (least-privilege container boundary)"; hr
# The worker container must not have the Control's KMS key on any mount. Assert the path is absent
# AND that the whole kms dir is unreadable from the worker.
if $DC exec -T worker sh -c 'test -e /secrets/kms/dev-master.key' 2>/dev/null; then
  bad "worker CAN see /secrets/kms/dev-master.key — KMS boundary BROKEN"
else
  ok "worker cannot see /secrets/kms/dev-master.key (path absent from worker mounts)"
fi
# Anti-vacuity: prove the worker CAN read its OWN cert (so the negative above isn't just an empty FS).
if $DC exec -T worker sh -c 'test -r /secrets/mtls/worker.crt' 2>/dev/null; then
  ok "worker can read its own mTLS cert (proves the worker FS is mounted; KMS absence is meaningful)"
else
  bad "worker cannot read its own cert — mount wiring wrong, KMS test would be vacuous"
fi
# And prove the Control CAN read the KMS key (the key exists; the worker simply isn't given it).
if $DC exec -T control sh -c 'test -r /secrets/kms/dev-master.key' 2>/dev/null; then
  ok "Control CAN read the KMS key (sole decryptor); the worker cannot — boundary is real"
else
  bad "Control cannot read the KMS key — sole-decryptor wiring wrong"
fi

hr; echo "SUITE 6: no credential leak in service logs (Rule 5/6 regression guard)"; hr
# The worker token + hash flow through registration today; assert neither appears in any log.
if COMPOSE_INVOCATION="$DC" bash tests/harness/checks/no_secret_leak.sh >/dev/null 2>&1; then
  ok "no dev token/hash found in control/worker/migrate/postgres/sink logs"
else
  bad "a dev credential (token or hash) leaked into service logs"
fi

hr; echo "SUITE 7: worker unit tests (real local sink / real stream; credential + outcome-buffer paths)"; hr
# The REAL worker sources run against REAL local servers (no mocks):
#   smtp.test.mjs   — Phase 3 S1 failed-AUTH scrub, S3 zeroing on every exit path incl. exception,
#                     S5 abort-before-AUTH when STARTTLS is unavailable (real controllable SMTP sink).
#   buffer.test.mjs — Phase 7 P7-4 outcome buffering: drop-only-on-report_ack, replay on reconnect,
#                     TTL-bounded expiry (real stream server, real reconnect).
# Matches every *.test.mjs by GLOB rather than hand-listing files, so a new worker unit test cannot be
# silently left out of CI (buffer.test.mjs was added in Phase 7 Chunk 2b and this is what wires it
# in). Quoted so node expands the pattern itself — a bare `test/` directory arg is treated as a
# module by some Node versions and fails to resolve. Judged on its own PASS/FAIL summary.
if command -v node >/dev/null 2>&1; then
  WORKER_TEST_OUT="$( { cd services/worker && node --test "test/*.test.mjs" 2>&1; } || true )"
  # Node's built-in runner prints "ℹ pass N" / "ℹ fail N" and "✔"/"✖" lines. Judge on fail count.
  ( echo "$WORKER_TEST_OUT" | grep -E "(pass|fail) [0-9]+" | tail -4 ) || true
  fails="$(echo "$WORKER_TEST_OUT" | grep -oE "fail [0-9]+" | grep -oE "[0-9]+" | tail -1 || true)"
  passes="$(echo "$WORKER_TEST_OUT" | grep -oE "pass [0-9]+" | grep -oE "[0-9]+" | tail -1 || true)"
  # Anti-vacuity: a run that executed ZERO tests must not be reported as green (a mistyped path or a
  # renamed directory would otherwise print "fail 0" and pass this gate).
  if [[ "${fails:-1}" == "0" && "${passes:-0}" -ge 1 ]]; then
    ok "worker units: ${passes} passed (S1 scrub / S3 zeroing / S5 STARTTLS-before-AUTH + P7-4 buffer ack/replay/TTL)"
  elif [[ "${passes:-0}" -lt 1 ]]; then
    bad "worker units ran ZERO tests — the test path or runner invocation is wrong (vacuous green)"
  else
    bad "worker units failed (fail=${fails:-?}): $(echo "$WORKER_TEST_OUT" | grep -iE 'not ok|✖' | head -3 | tr '\n' ' ')"
  fi
else
  bad "node not found on PATH — cannot run worker unit tests"
fi

hr; echo "SUITE 8: Phase 3 dispatch + credential Go tests (real Postgres clones)"; hr
# Build a template migrated 0000..0014 (same shape as the Phase 2 template) and run the real
# Dispatcher + OutcomeHandler + credential.Decryptor against clones. Reuses the Phase 2 template
# if present, else builds send_managed_template_p3.
echo "== preparing Phase 3 dispatch template (send_managed_template_p3, migrated 0000..0014) =="
$DC exec -T postgres psql -U postgres -d postgres -v ON_ERROR_STOP=1 -q \
  -c "DROP DATABASE IF EXISTS send_managed_template_p3" -c "CREATE DATABASE send_managed_template_p3" >/dev/null 2>&1 || true
P3_DSN="postgres://postgres:${SUPER}@localhost:5432/send_managed_template_p3"
$DC exec -T postgres psql -U postgres -d send_managed_template_p3 -v ON_ERROR_STOP=1 -q \
  -c "CREATE TABLE IF NOT EXISTS public.users(id uuid primary key default gen_random_uuid(), email text, is_active boolean default true, created_at timestamptz default now())" \
  -c "CREATE TABLE IF NOT EXISTS public.job_items(id uuid primary key default gen_random_uuid(), campaign_id uuid, user_id uuid, recipient_email text, rendered_subject text, rendered_body text, reply_to text, status text, created_at timestamptz default now())" >/dev/null 2>&1
if MIGRATE_DATABASE_URL="$P3_DSN" SEND_MANAGED_MODULE_PASSWORD="${SEND_MANAGED_MODULE_PASSWORD:-devmodulepass}" \
     tools/send-managed-migrate/bin/send-managed-migrate up-and-set-password >/dev/null 2>&1; then
  export TEST_TEMPLATE_DB="send_managed_template_p3"; echo "  Phase 3 template ready"
else
  export TEST_TEMPLATE_DB=""; echo "  Phase 3 template create failed"
fi
go_suite services/control/internal/dispatch "Phase 3 dispatch (claim/decrypt/push, idempotency, requeue, bounced, never-invent)"
go_suite services/control/internal/credential "Phase 3 credential decryptor (round-trip/tamper/transplant/key-id)"
go_suite services/control/internal/platformapi "Platform API tenancy (OD-25) + one-campaign-per-user (Phase 4 P4-7, incl. concurrent-attempt serialization)"
export TEST_TEMPLATE_DB="send_managed_template"

hr; echo "SUITE 9: Phase 3 minimal send path — LIVE end-to-end (real worker + real Mailpit)"; hr
# T1 real email accepted, T2 async run + report, T4 no double-send, T6 register auth, S1 no
# credential leak, OD-7 no admin route decrypts. Sourced so it shares PASS/FAIL/ok/bad + $DC.
# shellcheck disable=SC1091
source tests/harness/phase3_send.sh

hr; echo "SUITE 10: Phase 8 content variation — LIVE end-to-end (real worker + real Mailpit)"; hr
# T1 all-toggles-off delivered bytes match the Phase 3 shape; T3/T7 variation-on delivered bytes
# differ, still parse, and the unique-URL-param toggle introduces no tracking surface; S1 re-checked
# with variation ON. Reuses phase3_send.sh's pool/account/$SYSUSER fixtures + $CANARY + $DC/$PSQL.
# shellcheck disable=SC1091
source tests/harness/phase8_variation.sh

hr
echo "HARNESS RESULT: $PASS passed, $FAIL failed"
hr
[[ "$FAIL" -eq 0 ]]
