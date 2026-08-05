#!/usr/bin/env bash
# test_enumeration_selftest — the PAIRED CONTROL for test_enumeration.sh.
#
# WHY. test_enumeration.sh is the mechanism that replaced a prose warning, and on the real tree it
# reports OK — which means every one of its failure branches is unexercised there. A gate whose
# failure path never runs is a gate nobody has checked: it could be broken to accept everything
# (a typo in the grep, an empty `enumerated` list, a `find` that matches nothing) and the real tree
# would still print OK on every run, forever. That vacuity is the whole reason this file exists, and
# it matters MORE here than in module 3: this repo is already clean on both rules, so the gate's only
# demonstrated behaviour is printing OK.
#
# It also closes the one hole the meta-gate structurally cannot cover: test_enumeration.sh checks
# that other gates are wired, but it cannot detect its OWN absence from the workflow. This control
# fails if either script is missing from CI, so the pair covers each other.
#
# HOW, AND WHY NOT ENV OVERRIDES. Making the real gate's paths overridable by environment variables
# would hand anyone a way to point a blocking gate at empty fixtures and have it pass having checked
# nothing. Instead this builds a THROWAWAY REPO SKELETON with the same relative layout, copies the
# REAL gate in, and md5-verifies the copy so a future refactor cannot leave this control exercising
# a stale file.
#
# CASES (each asserts an EXIT CODE and a MESSAGE — a gate that fails for the wrong reason is not a
# passing control):
#   1. fully wired                      -> OK.               The case that makes the rest meaningful.
#   2. a test package not in run.sh     -> FAIL, names the package.
#   3. a gate script not in the workflow-> FAIL, names the script.
#   4. both violated at once            -> FAIL, names BOTH (one rule must not mask the other).
#   5. the go_suite() DEFINITION line only, with a package present -> FAIL, not a false pass.
#      (Regression guard: `grep '^go_suite'` alone also matches `go_suite() {`, which would put the
#      literal string "go_suite()" into the enumerated list. That would not mask a real package, but
#      case 5 pins the parse so a future "simplification" of that grep cannot go unnoticed.)
#   6. a *_test.go under vendor/        -> OK. The exclusion holds; vendored tests are not ours.
#   7. a gate step DELETED, with a comment naming its path left behind -> FAIL. The 2026-08-05
#      defect, found in module 3 on identical code: rule 2 grepped the whole workflow, so the TODO
#      satisfied it. A commented-out step is the likeliest way to disable a gate.
#   8. the gate invoked inside a `run: |` BLOCK SCALAR -> OK. The load-bearing partner to case 7:
#      the fix must count real multi-line run bodies, or it would reject correct workflows and the
#      next person would "fix" the gate by loosening it back to grepping the file.
#   9. a `#` in a step NAME does not wire a gate -> FAIL. Narrows the fix: only `run:` counts, not
#      any line that happens to survive comment-stripping.
#
# Cases 1, 6 and 8 are load-bearing: without them, a gate broken to reject EVERYTHING would pass
# cases 2-5, 7 and 9 and this script would call that correct.
set -euo pipefail
cd "$(dirname "$0")/../../.."
REPO="$(pwd)"
GATE="tests/harness/checks/test_enumeration.sh"
SELF="tests/harness/checks/test_enumeration_selftest.sh"
WORKFLOW=".github/workflows/real-infra.yml"

# --- 0) The pair must both be wired. test_enumeration.sh cannot assert its own presence. ---------
for s in "$GATE" "$SELF"; do
  if ! grep -qF "$s" "$WORKFLOW"; then
    echo "test_enumeration_selftest FAIL: $s is not named in $WORKFLOW."
    echo "  The meta-gate cannot detect its own absence from CI — that is this check's job, and the"
    echo "  reason the two scripts are wired as a pair in the same commit."
    exit 1
  fi
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/tests/harness/checks" "$TMP/.github/workflows"
cp "$REPO/$GATE" "$TMP/$GATE"
if [[ "$(md5sum <"$REPO/$GATE" | cut -d' ' -f1)" != "$(md5sum <"$TMP/$GATE" | cut -d' ' -f1)" ]]; then
  echo "test_enumeration_selftest FAIL: the copied gate does not match $GATE — this control would be testing a stale file"
  exit 1
fi

fail=0

# reset_fixture — a minimal skeleton that the real gate should call OK: one test package, named in
# run.sh; one gate script, named in the workflow.
reset_fixture() {
  rm -rf "$TMP/services" "$TMP/vendor"
  # Remove any orphan gate a previous case dropped in. Without this the cases LEAK: case 3's
  # orphan_gate.sh and case 4's orphan2.sh survive into case 6, which then fails for rule 2 while
  # claiming to test rule 1's vendor exclusion — a control reporting the wrong reason, the very
  # thing expect() exists to catch. It caught exactly that in module 3 on the first run.
  find "$TMP/tests/harness/checks" -name '*.sh' ! -name "$(basename "$GATE")" -delete
  mkdir -p "$TMP/services/control/internal/alpha"
  : >"$TMP/services/control/internal/alpha/alpha_test.go"
  # The real run.sh defines go_suite() before calling it; the fixture reproduces that shape so the
  # definition line is present in every case, not just case 5.
  cat >"$TMP/tests/harness/run.sh" <<'EOF'
#!/usr/bin/env bash
go_suite() { # $1=dir $2=label
  :
}
go_suite services/control/internal/alpha "alpha"
EOF
  # Only the gate under test lives in checks/, so the workflow needs exactly one gate line. The
  # selftest itself is not copied in: the fixture exercises the gate's rules, not this file.
  cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  behavioral:
    steps:
      - run: bash tests/harness/checks/test_enumeration.sh
EOF
}

# expect <case-name> <want-rc> <substring...>  (every substring must appear)
expect() {
  local name="$1" want_rc="$2"; shift 2
  local out rc
  set +e
  out="$(cd "$TMP" && bash "$GATE" 2>&1)"
  rc=$?
  set -e
  if [[ "$rc" -ne "$want_rc" ]]; then
    echo "test_enumeration_selftest FAIL [$name]: exit $rc, want $want_rc"
    echo "--- gate output ---"; echo "$out"
    fail=1
    return
  fi
  local want
  for want in "$@"; do
    if ! grep -qF -- "$want" <<<"$out"; then
      echo "test_enumeration_selftest FAIL [$name]: exit code was right ($rc) but the message did not"
      echo "  contain: $want"
      echo "  A gate that fails for the WRONG REASON is not a control. Full output:"
      echo "--- gate output ---"; echo "$out"
      fail=1
      return
    fi
  done
  echo "  ok: $name"
}

echo "test_enumeration_selftest: exercising the failure branches the real tree never reaches"

# 1. Fully wired. Without this case, a gate broken to reject everything would pass 2-5.
reset_fixture
expect "a fully wired skeleton passes" 0 "test_enumeration OK"

# 2. A test package that no go_suite line names — the httpapi/workerlink defect.
reset_fixture
mkdir -p "$TMP/services/control/internal/beta"
: >"$TMP/services/control/internal/beta/beta_test.go"
expect "an unenumerated test package fails" 1 \
  "named on NO go_suite line" "services/control/internal/beta"

# 3. A gate script the workflow never names — module 3's config_consumption_selftest defect.
reset_fixture
: >"$TMP/tests/harness/checks/orphan_gate.sh"
expect "an unenumerated gate script fails" 1 \
  "named nowhere in" "orphan_gate.sh"

# 4. Both at once. Rule 1 must not short-circuit rule 2: a run that reports only the first defect
#    sends someone back for a second round trip, and worse, implies the other rule was satisfied.
reset_fixture
mkdir -p "$TMP/services/control/internal/gamma"
: >"$TMP/services/control/internal/gamma/gamma_test.go"
: >"$TMP/tests/harness/checks/orphan2.sh"
expect "both violations are reported in ONE run" 1 \
  "services/control/internal/gamma" "orphan2.sh"

# 5. Parse guard: the go_suite() DEFINITION must not be mistaken for an enumeration entry.
reset_fixture
cat >"$TMP/tests/harness/run.sh" <<'EOF'
#!/usr/bin/env bash
go_suite() { # $1=dir $2=label
  :
}
EOF
expect "the go_suite() definition alone is not an enumeration" 1 \
  "named on NO go_suite line" "services/control/internal/alpha"

# 6. The vendor/ exclusion holds — vendored test files are not this repo's to run. A gate that
#    demanded they be enumerated would be unusable, and someone would add an exemption list.
reset_fixture
mkdir -p "$TMP/vendor/github.com/someone/lib"
: >"$TMP/vendor/github.com/someone/lib/lib_test.go"
expect "a vendored *_test.go is correctly ignored" 0 "test_enumeration OK"

# --- THE PROSE-SATISFACTION DEFECT (cases 7-9, added 2026-08-05) ---------------------------------
# Rule 2 used to grep the whole workflow FILE for the gate's path, and asserted in its own comment
# that using the path rather than the bare filename made a comment unable to satisfy it. Wrong, and
# break-verified in module 3 on this identical code: deleting the drain_grace.sh step and leaving
# `# TODO: re-enable bash tests/harness/checks/drain_grace.sh` behind left the gate GREEN, reporting
# all 10 gate scripts wired about a workflow running nine.
#
# 7. The defect itself. The orphan gate is REAL and on disk; only its step is gone.
reset_fixture
: >"$TMP/tests/harness/checks/disabled_gate.sh"
cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  behavioral:
    steps:
      - run: bash tests/harness/checks/test_enumeration.sh
      # TODO: re-enable bash tests/harness/checks/disabled_gate.sh — flaky on the CI runner
EOF
expect "a COMMENTED-OUT gate step does not count as wired" 1 \
  "named nowhere in" "disabled_gate.sh"

# 8. A real `run: |` block scalar must still count. Load-bearing in the strict sense: the obvious
#    narrow fix (require the path on a line matching `^\s*(-\s+)?run:`) would REJECT this correct
#    workflow — and this repo's own workflow uses exactly that form for its gofmt/vet step, so the
#    narrow fix would have broken a live tree. A gate that fails on valid input is how exemption
#    lists get born.
reset_fixture
: >"$TMP/tests/harness/checks/block_gate.sh"
cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  behavioral:
    steps:
      - name: gates
        run: |
          bash tests/harness/checks/test_enumeration.sh
          bash tests/harness/checks/block_gate.sh
EOF
expect "a gate invoked inside a 'run: |' block is wired" 0 "test_enumeration OK"

# 9. Narrowing: comment-stripping alone is not the rule — the text must be a RUN command. A step
#    name mentioning the path is prose, exactly like a comment, and must not wire anything.
reset_fixture
: >"$TMP/tests/harness/checks/named_only_gate.sh"
cat >"$TMP/$WORKFLOW" <<'EOF'
jobs:
  behavioral:
    steps:
      - run: bash tests/harness/checks/test_enumeration.sh
      - name: formerly ran tests/harness/checks/named_only_gate.sh
        run: echo skipped
EOF
expect "a gate named only in a step NAME does not count as wired" 1 \
  "named nowhere in" "named_only_gate.sh"

if [[ "$fail" -eq 0 ]]; then
  echo "test_enumeration_selftest OK: 9 cases (3 accept, 6 reject) + both scripts confirmed wired in"
  echo "  CI — rule 1's enumeration (2,4,5), the vendor exclusion (6), and rule 2's run-command"
  echo "  matching incl. the comment-satisfied defect found 2026-08-05 (7-9)"
fi
exit "$fail"
