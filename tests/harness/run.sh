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
DC="${COMPOSE_INVOCATION:-$(bash scripts/dev.sh compose)}"  # the invocation dev.sh uses (RAM Postgres only under DEV_RAM_PG=1)

PASS=0; FAIL=0
ok()   { echo "  PASS: $1"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL: $1"; FAIL=$((FAIL+1)); }
hr()   { echo "──────────────────────────────────────────────────────────────"; }

# Build the migrate runner binary this script calls three times below. NOTHING BUILT IT before
# 2026-08-07: bin/ is gitignored, so on a fresh checkout — which is every CI run — the path simply
# did not exist, and each up-and-set-password call is `>/dev/null 2>&1`-guarded inside an `if`. So
# TEST_TEMPLATE_DB ended up "" three times and every test gated on it t.Skip()ed.
#
# MEASURED here by moving bin/ aside, 2026-08-07: 269 of 359 declared tests degraded to skips —
# 16 of 16 in the Phase 1 data model, 67 of 72 in adminapi, 141 of 168 in dispatch, 45 of 54 in
# platformapi. `go test` exits 0 on an all-skip package and go_suite() judges by grepping for a FAIL
# line, so on the pre-C4.1 harness that read as "58 passed, 0 failed" with the append-only, RLS,
# credential-AAD and tenancy suites all unexecuted.
#
# It is red now, and that is C4.1's nonzero floor doing its job: the declared==ran check reports
# "EXECUTED 0 OF 16 DECLARED TESTS" and prints "go test exited without a FAIL line, so this would
# have been reported as PASSING". This build line is still the actual fix — the floor turns a silent
# false green into a loud red, it does not make the tests run.
#
# Module 3's tests/harness/run.sh has carried this same build line, and a comment describing this
# same failure, since M3-P0-OD-12. It was never ported here. Mirrors scripts/dev.sh's existing
# inline build of tools/provision-worker/bin/provision-worker.
( cd tools/send-managed-migrate && GOTOOLCHAIN=local go build -o bin/send-managed-migrate . )

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
export WORKER_MTLS_CERT="$(pwd)/secrets/dev/certs/harness-client.crt"
export WORKER_MTLS_KEY="$(pwd)/secrets/dev/certs/harness-client.key"   # the HOST-side identity: worker.key is owned by the worker container uid
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
#
# GO_SUITE_TIMEOUT: every package here drives REAL Postgres, and several deliberately spend real
# elapsed time (CLAUDE.md Rule 10 — a lease expiry test must actually wait). Go's DEFAULT test
# timeout is 600s per package, and `go_suite` had never overridden it. The sibling module
# (coldmail-send-byo) walked into it on 2026-08-05: its internal/dispatch was killed at exactly
# 600.007s with EVERY test PASSED and not one `--- FAIL` line, and its harness reported
# "FAIL: Phase 3 dispatch" for a suite in which nothing was wrong.
#
# The cause was measured, not assumed, and it is not test count: that package needed 646s on an
# accumulated dev database and **113s on a clean slate** minutes later — a ~5.7x swing from
# dev-DB state sitting behind every per-test template clone. Wall-clock here is a function of
# environment state, so a fixed invisible 600s converts a dirty database or a loaded CI runner
# into a phantom test failure. Made explicit here PRE-emptively: this module's suite is the same
# shape on the same kind of dev database. Set per-run so CI and a laptop share one number.
GO_SUITE_TIMEOUT="${GO_SUITE_TIMEOUT:-40m}"

# --- THE COUNT ASSERTION -----------------------------------------------------------------------
#
# A raised timeout alone fixes the WRONG HALF of the finding that produced it. A per-package cutoff
# fails in BOTH directions at once: it reports a phantom failure for a package in which everything
# passed, AND it silently discards every test after the cutoff. Raising the ceiling makes the loud
# half go quiet while the silent half survives — worse than before, because the noisy symptom that
# led anyone to look is gone.
#
# So the ceiling is not the control. THIS is: every package must EXECUTE AS MANY TOP-LEVEL TESTS AS
# IT DECLARES. The expected number is read per run from `go test -list` — the compiler's own view of
# the roster — never hardcoded, so it cannot drift, needs no maintenance when a test is added, and
# cannot be quietly lowered to match a broken run the way a literal in this file could.
#
# One control, three failure modes, all red on a shortfall regardless of cause:
#
#   1. TRUNCATED RUN — a timeout, panic or early exit stops the package mid-way. The declared
#      roster still lists everything; only the executed count drops.
#   2. SKIPPED TESTS — measured in THIS repo, not inherited: with TEST_TEMPLATE_DB unset,
#      internal/platformapi skips 44 of its 49 tests, `go test` exits 0, and before this control
#      the harness reported the package as PASSING. (The sibling module's equivalent figure is 143
#      of 152 — different numbers, same defect, which is why each repo measures its own.)
#   3. AN UNWIRED TEST FILE — partially covered by `test_enumeration` at the PACKAGE level; this
#      covers it at the TEST level, inside a package that is already wired.
#
# A SKIP IS A SHORTFALL, NOT A PASS. The first version of this control counted PASS+FAIL+SKIP as
# "executed" and merely PRINTED the skip count — which reported 143 skipped tests as green in the
# sibling module. A tolerated skip is how a harness prints "61 passed, 0 failed" over tests that
# never ran. `executed` counts PASS+FAIL only. If a skip is ever genuinely intended, the answer is
# to make it not-a-skip (give the package what it needs, or delete the test), never to widen this
# arithmetic.
count_ran() { # stdin: `go test -v` output. Counts EXECUTED top-level results only (PASS/FAIL).
  grep -cE '^(--- (PASS|FAIL)): ' || true
}

go_suite() { # $1=dir  $2=label
  local out declared ran skipped
  # The trailing `|| true` is load-bearing under `set -euo pipefail`: on Windows `go test` can
  # exit non-zero purely from the post-test temp-.exe cleanup ("unlinkat ... being used by
  # another process") even when EVERY test PASSES, and with pipefail that non-zero would
  # propagate through the command substitution and `set -e` would kill this script mid-run
  # (observed: the larger control binary triggers the cleanup artifact intermittently). We
  # deliberately judge pass/fail from the Go verdict LINE below, never the exit code — so
  # swallowing the substitution's exit status here is correct, not a masked failure. On Linux
  # CI the artifact does not occur, so `|| true` is a no-op there.
  # The declared roster, from the compiler rather than from a number kept by hand. If this fails to
  # build, `declared` is 0 and the shortfall check below is inert — it must never be able to mask
  # the verdict-line failure that a compile error already produces.
  declared="$( { cd "$1" && GOTOOLCHAIN=local go test -list '.*' . 2>/dev/null | grep -cE '^(Test|Example|Fuzz)'; } || true )"
  out="$( { cd "$1" && GOTOOLCHAIN=local go test -count=1 -v -timeout "$GO_SUITE_TIMEOUT" . 2>&1 | grep -vE "unlinkat|being used by another process"; } || true )"
  echo "$out" | grep -E "^(ok|FAIL|--- (PASS|FAIL))" | tail -40
  ran="$(echo "$out" | count_ran)"
  skipped="$(echo "$out" | grep -cE '^--- SKIP: ' || true)"
  echo "  counts: declared=$declared ran=$ran skipped=$skipped"
  # A timeout is reported SEPARATELY from a test failure, because they call for opposite responses:
  # a `--- FAIL` means fix the code, a timeout means the package outgrew its budget and no verdict
  # was reached at all. Conflating them is what made the sibling module's 600s kill read as a
  # dispatch bug when every one of its tests had passed.
  #
  # Branch ORDER is deliberate: timeout and FAIL come first because they are the more specific
  # diagnosis of the same shortfall, and the shortfall branch comes BEFORE ok() so that a package
  # exiting 0 with tests missing can never be reported as passing.
  if echo "$out" | grep -qE "^panic: test timed out"; then
    echo "  NOTE: package exceeded GO_SUITE_TIMEOUT=$GO_SUITE_TIMEOUT — no verdict was reached; this is a BUDGET failure, not a test failure. Raise the budget or split the package."
    bad "$2 (TIMED OUT after $GO_SUITE_TIMEOUT — ran $ran of $declared declared, no verdict)"
  elif echo "$out" | grep -qE "^--- FAIL|^FAIL\b"; then bad "$2"
  elif [ "$declared" -gt 0 ] && [ "$ran" -lt "$declared" ]; then
    echo "  NOTE: $((declared - ran)) declared test(s) never EXECUTED ($skipped skipped, $((declared - ran - skipped)) with no result at all). go test exited without a FAIL line, so this would have been reported as PASSING. Causes seen here: tests gated on an env var that was empty (TEST_TEMPLATE_DB=\"\" skips 44 of platformapi's 49), or a truncated run (timeout/panic). NOT satisfied by raising GO_SUITE_TIMEOUT, and NOT satisfied by declaring the skips intended."
    bad "$2 (EXECUTED $ran OF $declared DECLARED TESTS — $skipped skipped, $((declared - ran)) unaccounted for)"
  else
    ok "$2"
  fi
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

# N1/D12 — the DSN scrub. Needs NO database and no stack: every DSN in it is rejected at the pgx
# PARSE stage (before a socket opens) or points at 127.0.0.1:1, so it is listed here with the other
# substrate-free packages. Its load-bearing test is not the leak assertion but
# TestDSNScrub_PgxStillLeaksAtThisPin, which re-measures on every run that the pinned pgx v5.7.4
# genuinely leaks the credential — if a pgx bump fixes redaction upstream, or changes which trigger
# carries the DSN, the premise fails loudly instead of the whole file going quietly vacuous.
go_suite services/control/internal/dsnscrub    "DSN credential scrub (N1/D12: pgx echoes the whole connection string; 9 tests, no DB)"

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
# in). The SHELL expands the glob, NOT node: this used to pass the pattern quoted so that node would
# expand it, which only works on Node >= 21. On Node 20 (the worker image's own runtime, and what the
# CI replica runs) the quoted pattern is looked up as a literal file name, nothing runs, and this suite
# reported "ZERO tests" — invisible on a dev host whose Node is newer. A bare `test/` directory arg is
# no alternative: some Node versions treat it as a module and fail to resolve. Shell expansion works on
# every version; if the glob matches nothing the literal pattern is passed through, node runs zero
# tests, and the anti-vacuity guard below fails the suite. Judged on its own PASS/FAIL summary.
if command -v node >/dev/null 2>&1; then
  echo "  (node $(node --version))"
  WORKER_TEST_OUT="$( { cd services/worker && node --test test/*.test.mjs 2>&1; } || true )"
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
