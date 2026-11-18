#!/usr/bin/env bash
# consume_contention — BLOCKING gate (FW-M3-22, ADR-0015): the daily-limit ceiling is defended under FORCED contention, on BOTH paths a peer can be refused by,
# not hoped-for contention.
#
#   consume_contention.sh                  runs the REAL code once. Both tests must pass AND each must have logged its evidence line — a green that never forced
#                                          the interleave is not a green:
#                                            TestAccountConsume_NeverExceedsDailyLimit   "FORCED CONTENTION: 1 holder parked ..."   (peers refused by SKIP LOCKED)
#                                            TestAccountConsume_StaleSnapshotPeerIsRefused               "MVCC RE-CHECK FORCED: ..."                (peer refused by the re-check)
#   consume_contention.sh --repeat N       both tests N times (go test -count=N): N passes each, zero failures, N evidence lines each. This is what the
#                                          consume-contention-repeat workflow runs with N=50; a flake would show here as a failure count.
#   consume_contention.sh --mutant NAME    the PAIRED CONTROL, one mutant of the production code (or of the test's own gate) that its TARGET test MUST turn red,
#                                          for the RIGHT reason. NAME is one of:
#                                            noskip      (DailyLimit)  FOR UPDATE SKIP LOCKED -> FOR UPDATE: a peer waits on the locked row instead of being refused
#                                            mutex       (DailyLimit)  the consume is serialised behind one Go mutex: no two consumes can ever overlap
#                                            noceiling   (DailyLimit)  the daily-limit and paused predicates are removed: a consume past the limit is possible
#                                            nogate      (DailyLimit)  the gate trigger can never fire: the interleave is not forced (the test's own vacuity guard)
#                                            norecheck   (StaleSnapshot)    the row lock is removed, and with it the re-check: only the CHECK constraint stands between the peer and a double booking
#                                            nopolicy    (StaleSnapshot)    the MVCC gate policy can never call its function: the peer never parks, the interleave is not forced
#                                          consume_contention_selftest.sh runs every mutant.
#
# WHY IT EXISTS. The first test used to *hope* its eight workers contended and then assert that two of them had succeeded; under SKIP LOCKED one worker can take
# every slot before a peer is scheduled, and it did (run 43445719147, "only 1 worker(s) ever succeeded", on a commit with no executable change). It now FORCES the
# contention (consume_contention_test.go: a consume parked on a test-held advisory lock while it holds the last slot, observed in pg_stat_activity, with seven peers
# that must all be refused). The second test forces the OTHER refusal path, the MVCC re-check (a peer whose snapshot predates the holder's commit), with a test-only
# row-level-security policy that parks only that peer between its snapshot and its row lock; the production SQL is untouched. This gate keeps both true on every run:
# it runs the tests against the real code, and against mutants that remove exactly the properties they claim to prove.
#
# Needs: Go, the compose Postgres up (dev.sh reset or `compose up -d --wait postgres`), tools/send-managed-migrate buildable.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."

T1=TestAccountConsume_NeverExceedsDailyLimit
T2=TestAccountConsume_StaleSnapshotPeerIsRefused
MODE=real; MUTANT=""; COUNT=1
while [ $# -gt 0 ]; do
  case "$1" in
    --mutant) MODE=mutant; MUTANT="${2:?--mutant needs a name: noskip|mutex|noceiling|nogate|norecheck|nopolicy}"; shift 2;;
    --repeat) COUNT="${2:?--repeat needs a count}"; shift 2;;
    *) echo "consume_contention: unknown argument '$1'" >&2; exit 2;;
  esac
done
case "$MUTANT" in ""|noskip|mutex|noceiling|nogate|norecheck|nopolicy) ;; *) echo "consume_contention: unknown mutant '$MUTANT'" >&2; exit 2;; esac
LABEL="$MODE${MUTANT:+:$MUTANT}"
fail() { echo "consume_contention ($LABEL) FAIL: $*" >&2; exit 1; }

# Which test a mutant is aimed at: only that test runs, so a mutant of one path cannot be "caught" by the other test failing first.
TARGET=""
case "$MUTANT" in
  noskip|mutex|noceiling|nogate) TARGET="$T1";;
  norecheck|nopolicy)            TARGET="$T2";;
esac
if [ "$MODE" = real ]; then RUN="^($T1|$T2)\$"; else RUN="^$TARGET\$"; fi

DC="${COMPOSE_INVOCATION:-$(bash scripts/dev.sh compose)}"
SUPER="${POSTGRES_SUPERPASS:-devsuperpass}"
TPL=send_managed_template_contention
export TEST_ADMIN_DATABASE_URL="postgres://postgres:${SUPER}@localhost:5432/platform"
export TEST_TEMPLATE_DB="$TPL"
export SEND_MANAGED_MODULE_PASSWORD="${SEND_MANAGED_MODULE_PASSWORD:-devmodulepass}"

# A migrated template of its own — never the harness's, so this gate runs in any order and leaves nothing behind that another suite reads.
[ -x tools/send-managed-migrate/bin/send-managed-migrate ] || ( cd tools/send-managed-migrate && GOTOOLCHAIN=local go build -o bin/send-managed-migrate . )
$DC exec -T postgres psql -U postgres -d postgres -v ON_ERROR_STOP=1 -q -c "DROP DATABASE IF EXISTS $TPL" -c "CREATE DATABASE $TPL" >/dev/null \
  || fail "could not create the template database (is the compose postgres up?)"
$DC exec -T postgres psql -U postgres -d "$TPL" -v ON_ERROR_STOP=1 -q \
  -c "CREATE TABLE IF NOT EXISTS public.users(id uuid primary key default gen_random_uuid(), email text, is_active boolean default true, created_at timestamptz default now())" \
  -c "CREATE TABLE IF NOT EXISTS public.job_items(id uuid primary key default gen_random_uuid(), campaign_id uuid, user_id uuid, recipient_email text, rendered_subject text, rendered_body text, rendered_text text, reply_to text, status text, created_at timestamptz default now())" >/dev/null \
  || fail "could not seed the platform fixture tables"
MIGRATE_DATABASE_URL="postgres://postgres:${SUPER}@localhost:5432/$TPL" SEND_MANAGED_MODULE_PASSWORD="$SEND_MANAGED_MODULE_PASSWORD" \
  tools/send-managed-migrate/bin/send-managed-migrate up-and-set-password >/dev/null 2>&1 \
  || fail "could not migrate the template database to head"

# Under the checkout, not /tmp: the same path must be valid wherever this runs (a runner, or the CI replica).
W="$PWD/.consume-contention-work"
rm -rf "$W"; mkdir -p "$W"
trap 'rm -rf "$W"; $DC exec -T postgres psql -U postgres -d postgres -q -c "DROP DATABASE IF EXISTS '"$TPL"'" >/dev/null 2>&1 || true' EXIT
mkdir -p "$W/control"; cp -r services/control/. "$W/control/"
D="$W/control/internal/dispatch"

# Every mutation is verified to have LANDED, not assumed: a line that was renamed would turn the "mutant" into the real code, and a mutant that is the real
# code would then "fail to go red" — or, worse, be reported as a control that passed for the wrong reason.
landed() { grep -q "$1" "$2" || fail "the '$MUTANT' mutation did not apply ($2 no longer has the line this control rewrites) — it would be testing the real code"; }
case "$MUTANT" in
  noskip)
    sed -i 's/^    FOR UPDATE SKIP LOCKED$/    FOR UPDATE -- MUTANT-noskip: SKIP LOCKED removed/' "$D/eligibility.go"
    landed 'MUTANT-noskip: SKIP LOCKED removed' "$D/eligibility.go";;
  mutex)
    printf 'package dispatch\n\nimport "sync"\n\nvar mutantConsumeMu sync.Mutex\n' >"$D/mutant_mutex.go"
    sed -i 's|^\tquery := accountConsumeQuery(orderBy)$|\tquery := accountConsumeQuery(orderBy); mutantConsumeMu.Lock(); defer mutantConsumeMu.Unlock() // MUTANT-mutex: consume serialised|' "$D/eligibility.go"
    landed 'MUTANT-mutex: consume serialised' "$D/eligibility.go";;
  noceiling)
    sed -i 's/^                AND (a\.paused = FALSE$/                AND (TRUE -- MUTANT-noceiling: paused ignored/' "$D/eligibility.go"
    sed -i 's/^                AND a\.daily_sent_count < a\.daily_limit)$/                AND TRUE) -- MUTANT-noceiling: limit ignored/' "$D/eligibility.go"
    landed 'MUTANT-noceiling: paused ignored' "$D/eligibility.go"
    landed 'MUTANT-noceiling: limit ignored' "$D/eligibility.go";;
  nogate)
    sed -i "s|WHEN (OLD.id = '%s'::uuid)|WHEN (OLD.id = '%s'::uuid AND false /*MUTANT-nogate*/)|" "$D/consume_contention_test.go"
    landed 'MUTANT-nogate' "$D/consume_contention_test.go";;
  norecheck)
    # No row lock => no LockRows => no re-check of the predicate against the committed version. The UPDATE that follows has no guard of its own, so only the
    # accounts_daily_sent_check CONSTRAINT is left between a stale-snapshot peer and a double booking.
    sed -i 's/^    FOR UPDATE SKIP LOCKED$/    -- MUTANT-norecheck: no row lock, so no re-check/' "$D/eligibility.go"
    landed 'MUTANT-norecheck: no row lock, so no re-check' "$D/eligibility.go";;
  nopolicy)
    sed -i 's|USING (public.zz_mvcc_gate(id, daily_sent_count, daily_limit))|USING (true OR public.zz_mvcc_gate(id, daily_sent_count, daily_limit)) /*MUTANT-nopolicy*/|' "$D/consume_contention_test.go"
    landed 'MUTANT-nopolicy' "$D/consume_contention_test.go";;
esac

rc=0
out="$(cd "$W/control" && GOTOOLCHAIN=local go test -count="$COUNT" -run "$RUN" -v -timeout 900s ./internal/dispatch 2>&1)" || rc=$?

# Verdicts read $out through here-strings, never `echo "$out" | grep -q` (an early-exit pipe under pipefail reports "no match" for a real match).
n() { grep -c -- "$1" <<<"$out" || true; }
p1="$(n "--- PASS: $T1")"; f1="$(n "--- FAIL: $T1")"; s1="$(n "--- SKIP: $T1")"
p2="$(n "--- PASS: $T2")"; f2="$(n "--- FAIL: $T2")"; s2="$(n "--- SKIP: $T2")"
ev1="$(n "FORCED CONTENTION: 1 holder parked")"
ev2="$(n "MVCC RE-CHECK FORCED: peer was shown")"
echo "consume_contention ($LABEL): count=$COUNT | $T1 passed=$p1 failed=$f1 skipped=$s1 evidence=$ev1 | $T2 passed=$p2 failed=$f2 skipped=$s2 evidence=$ev2 | rc=$rc"
grep -E "_test.go:[0-9]+:|FORCED CONTENTION|MVCC|^--- (FAIL|SKIP)" <<<"$out" | cut -c1-300 | head -14 || true

if [ "$MODE" = real ]; then
  [ "$rc" -eq 0 ]            || { echo "$out" | tail -30 >&2; fail "the tests failed (rc=$rc)"; }
  [ $((f1 + f2)) -eq 0 ]     || fail "$((f1 + f2)) run(s) failed"
  [ $((s1 + s2)) -eq 0 ]     || fail "$((s1 + s2)) run(s) were skipped — a skip is a shortfall, not a pass (is TEST_ADMIN_DATABASE_URL / the template set?)"
  [ "$p1" -eq "$COUNT" ]     || fail "only $p1 of $COUNT runs of $T1 passed"
  [ "$p2" -eq "$COUNT" ]     || fail "only $p2 of $COUNT runs of $T2 passed"
  [ "$ev1" -eq "$COUNT" ]    || fail "only $ev1 of $COUNT runs of $T1 logged the FORCED CONTENTION evidence line — a pass that did not force the interleave proves nothing"
  [ "$ev2" -eq "$COUNT" ]    || fail "only $ev2 of $COUNT runs of $T2 logged the MVCC RE-CHECK FORCED evidence line — a pass that did not force the interleave proves nothing"
  echo "consume_contention OK: $COUNT of $COUNT run(s) of each test passed, each with its interleave forced (last-slot holder parked and peers refused with capacity remaining; stale-snapshot peer parked, holder committed, peer refused by the re-check)"
else
  case "$MUTANT" in
    noskip|mutex) sig='FORCED CONTENTION BROKEN';;
    noceiling)    sig='OVER-CONSUMPTION|errored once capacity was gone|accounts_daily_sent_check FIRED';;
    nogate)       sig='forced interleave did not engage';;
    norecheck)    sig='MVCC RE-CHECK PATH FAILED';;
    nopolicy)     sig='MVCC forced interleave did not engage';;
  esac
  [ "$rc" -ne 0 ]            || fail "the '$MUTANT' mutant PASSED: $TARGET cannot detect the defect it exists for"
  grep -q -- "--- FAIL: $TARGET" <<<"$out" \
    || fail "the '$MUTANT' mutant failed, but not in the test under control, $TARGET (build error? $(grep -m1 -E 'build failed|cannot|undefined|syntax error' <<<"$out" || true)) — that proves nothing"
  grep -qE -- "$sig" <<<"$out" \
    || fail "the '$MUTANT' mutant went red, but without its signature /$sig/ — it failed for some other reason, which proves nothing about this defect"
  echo "consume_contention --mutant $MUTANT OK: $TARGET goes RED with the signature /$sig/"
fi
