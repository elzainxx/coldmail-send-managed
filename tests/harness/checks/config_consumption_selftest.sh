#!/usr/bin/env bash
# config_consumption_selftest — the PAIRED CONTROL for config_consumption.sh.
#
# WHY THIS EXISTS. config_consumption.sh is the gate that decides whether a seeded-but-unconsumed
# config key may exist, and on the real tree it reports OK for all 54 keys — so every one of its
# failure branches is unexercised there. A gate whose failure path never runs is a gate nobody has
# checked: it could be broken to accept everything and the real tree would still print OK on every
# run, forever.
#
# That is not hypothetical here. On 2026-08-05 the consumer branch was found to accept a COMMENT as a
# consumer: with runner.go:118's real `cfg.DispatchTickIntervalMS` read replaced by a hardcoded 2000ms
# the gate correctly went RED, but adding `// TODO: restore cfg.DispatchTickIntervalMS` to that same
# line turned it GREEN — reporting "all 54 config key(s) have a verified consumer" about a tree where
# the dispatch tick ignored its dial. The gate was satisfied by PROSE about the thing it guards. The
# fix is in the gate; case 3 below is what keeps it fixed.
#
# Module 3 has the same pair (its gate additionally enforces RESERVED-marker EXPIRY, a rule this
# module's gate does not have — see the note at the end of this file).
#
# HOW IT WORKS, AND WHY NOT ENV OVERRIDES. config_consumption.sh resolves its inputs from the repo
# root with hardcoded doc paths. Making those overridable by environment variables is REJECTED: it
# would hand anyone a way to point the blocking gate at empty files and have it pass having checked
# nothing — the vacuity class these rules exist to forbid. Instead this builds a THROWAWAY REPO
# SKELETON with the same relative layout, copies the REAL gate in unmodified, and md5-verifies the
# copy so a future refactor cannot leave this control exercising a stale file.
#
# CASES (each asserts an EXIT CODE and a MESSAGE, because a gate that fails for the wrong reason is
# not a passing control):
#   1. a real code consumer            -> OK. Load-bearing: without it, a gate broken to reject
#      everything would pass 2-5 and this script would call that correct.
#   2. RESERVED with the refdoc marker -> OK. The documented escape hatch still works.
#   3. pattern ONLY in a comment       -> FAIL, "ONLY INSIDE COMMENTS". The 2026-08-05 defect.
#   4. pattern absent from code        -> FAIL, the OTHER message. The remedies are opposite (never
#      written vs. removed with a note left behind), so the two must stay distinguishable.
#   5. RESERVED with no refdoc marker  -> FAIL. Operator visibility: a dial that does nothing must
#      say so where an operator reads, not only in an internal ledger.
#   6. a manifest key with no ledger line -> FAIL. The bookkeeping rule that makes the rest reachable:
#      a key omitted from the ledger would otherwise never be evaluated by any branch above.
set -euo pipefail
cd "$(dirname "$0")/../../.."
REPO="$(pwd)"
GATE="tests/harness/checks/config_consumption.sh"
SELF="tests/harness/checks/config_consumption_selftest.sh"
WORKFLOW=".github/workflows/real-infra.yml"

# --- 0) Both must be wired. A blocking control the workflow never names is the defect that put the
# enumeration meta-gate in this repo; this pair does not get to repeat it. -------------------------
for s in "$GATE" "$SELF"; do
  if ! grep -qF "$s" "$WORKFLOW"; then
    echo "config_consumption_selftest FAIL: $s is not named in $WORKFLOW."
    echo "  An unenumerated gate never runs in CI, so it is a green signal that proves nothing."
    exit 1
  fi
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/tests/harness/checks" "$TMP/Docs"
cp "$REPO/$GATE" "$TMP/$GATE"
if [[ "$(md5sum <"$REPO/$GATE" | cut -d' ' -f1)" != "$(md5sum <"$TMP/$GATE" | cut -d' ' -f1)" ]]; then
  echo "config_consumption_selftest FAIL: the copied gate does not match $GATE — this control would be testing a stale file"
  exit 1
fi

fail=0

# fixture <ledger-line> <refdoc-marker: yes|no> [go-body]
# The fixture universe is ONE key, deliberately: the manifest/ledger set-equality checks then pass
# trivially, so every failure below is attributable to the branch under test rather than bookkeeping.
# Passing an empty go-body means "no source file at all".
fixture() {
  local ledger_line="$1" marker="$2" body="${3:-}"
  printf 'selftest_key\n' >"$TMP/Docs/config-manifest.txt"
  printf '# fixture ledger\n%s\n' "$ledger_line" >"$TMP/Docs/config-consumers.txt"
  if [[ "$marker" == "yes" ]]; then
    printf '| `selftest_key` | `{"v":1}` | integer | fixture | **RESERVED / NOT-YET-CONSUMED** fixture row |\n' \
      >"$TMP/Docs/config-reference.md"
  else
    printf '| `selftest_key` | `{"v":1}` | integer | fixture | an ordinary documented row, no marker |\n' \
      >"$TMP/Docs/config-reference.md"
  fi
  # Wipe the previous case's source so a consumer cannot leak forward and make one case pass on
  # another's file — the leak that broke test_enumeration_selftest's case 6 on its first run.
  rm -rf "$TMP/services"
  if [[ -n "$body" ]]; then
    mkdir -p "$TMP/services/control/internal/dispatch"
    printf '%s' "$body" >"$TMP/services/control/internal/dispatch/consumer.go"
  fi
}

# expect <case-name> <want-rc: 0|1> <substring-that-must-appear>
expect() {
  local name="$1" want_rc="$2" want_msg="$3" out rc
  set +e
  out="$(cd "$TMP" && bash "$GATE" 2>&1)"
  rc=$?
  set -e
  if [[ "$rc" -ne "$want_rc" ]]; then
    echo "config_consumption_selftest FAIL [$name]: exit $rc, want $want_rc"
    echo "--- gate output ---"; echo "$out"
    fail=1
    return
  fi
  if ! grep -qF -- "$want_msg" <<<"$out"; then
    echo "config_consumption_selftest FAIL [$name]: exit code was right ($rc) but the message did not"
    echo "  contain: $want_msg"
    echo "  A gate that fails for the WRONG REASON is not a control. Full output:"
    echo "--- gate output ---"; echo "$out"
    fail=1
    return
  fi
  echo "  ok: $name"
}

echo "config_consumption_selftest: exercising the failure branches the real 54-key ledger never reaches"

# 1. A real consumer in code. Without this case, a gate broken to reject everything passes 2-6.
fixture 'selftest_key|FixtureDial' no 'package dispatch

func use(cfg Config) int { return cfg.FixtureDial * 2 }
'
expect "a real code consumer is accepted" 0 "config_consumption OK"

# 2. The RESERVED hatch, correctly documented. Also an accept case, for the same reason.
fixture 'selftest_key|RESERVED' yes
expect "RESERVED with the operator-visible marker is accepted" 0 "config_consumption OK"

# 3. THE DEFECT (found 2026-08-05). The pattern appears ONLY in a comment: the dial is inert and the
#    note is not the thing. Asserts the specific message, so a regression that failed for the
#    generic "no real consumer" reason could not pass here while losing the distinction.
fixture 'selftest_key|FixtureDial' no 'package dispatch

func use(cfg Config) int {
	return 2000 // TODO: restore cfg.FixtureDial * 2
}
'
expect "a pattern found ONLY in a comment is rejected" 1 "ONLY INSIDE COMMENTS"

# 4. Absent from the tree entirely -> the OTHER message.
fixture 'selftest_key|FixtureDial' no 'package dispatch

func use(cfg Config) int { return 2000 }
'
expect "a pattern absent from code is rejected with the OTHER message" 1 \
  "has no real consumer matching pattern"

# 5. Operator visibility: RESERVED in the ledger must be readable in the operator-facing doc.
fixture 'selftest_key|RESERVED' no
expect "RESERVED with no refdoc marker is rejected" 1 \
  "no matching RESERVED / NOT-YET-CONSUMED marker"

# 6. Bookkeeping: a manifest key with no ledger line at all. This is what makes every branch above
#    reachable — a key missing from the ledger is never evaluated by any of them.
fixture 'selftest_key|FixtureDial' no 'package dispatch

func use(cfg Config) int { return cfg.FixtureDial }
'
printf '# fixture ledger with the key omitted\n' >"$TMP/Docs/config-consumers.txt"
expect "a manifest key with no ledger line is rejected" 1 "MISSING from"

if [[ "$fail" -eq 0 ]]; then
  echo "config_consumption_selftest OK: 6 cases (2 accept, 4 reject) — consumer branch incl. the"
  echo "  comment-satisfied defect found 2026-08-05, RESERVED visibility, and ledger bookkeeping"
fi

# NOTE FOR A FUTURE READER — a real gap this control cannot close, recorded rather than silently
# left. This module's gate has NO RESERVED-marker EXPIRY rule: the ledger syntax is `key|RESERVED`
# with no third field, so a marker here never comes due. Module 3 added exactly that rule
# (`key|RESERVED|<gate-or-phase>`) after finding retry_count_4xx and variation_zero_width_rate_pct
# RESERVED "by accident" with no expiry, and DROPPED both keys when it fired. This module still
# carries those same two keys as unexpiring RESERVED entries. Adopting the expiry rule here is a
# behavioural change to a blocking gate (a new REQUIRED ledger field) plus a disposition decision on
# two live seeded keys, which is not this commit's scope — it is RECORDED, with the full reasoning and
# module 3's precedent, at Docs/future-work.md §5 ("A RESERVED-marker EXPIRY rule for
# config_consumption"). Deferred, not forgotten: this NOTE and that row point at each other.
exit "$fail"
