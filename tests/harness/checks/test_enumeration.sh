#!/usr/bin/env bash
# test_enumeration — BLOCKING CI gate. Turns "remember to wire your new test/gate" into "CI fails
# if you didn't."
#
# WHY THIS EXISTS. Work that was written, correct, and passing has been executed by NOTHING twice in
# THIS repo, and twice more in module 3 within the same week:
#
#   * internal/httpapi's first test file landed with item 7b (e3fee99) and no line ran it.
#   * internal/workerlink + internal/workerproto — 3 tests, 0 skips, all passing — were found
#     unwired by a cross-repo audit and wired in 2e8be72.
#   * module 3: config_consumption_selftest.sh (a BLOCKING control the workflow never named), plus
#     four more Go packages — 31 tests — found by the same audit.
#
# There was no signal to miss in any of those cases, which is the important part. tests/harness/
# run.sh names each Go package explicitly (deliberately — it gives each suite its own PASS/FAIL
# line) and CI runs gofmt + `go vet ./...` + this harness + each gate by name. `go vet`
# TYPE-CHECKS test files without running them. So an unwired package compiles, vets clean, and the
# harness prints a green total that silently excludes it. Identical signals, different truth.
#
# run.sh has carried a PROSE WARNING about exactly this since the httpapi incident, and it recurred
# two packages later anyway. **A warning comment is not a control.** The fix for "someone might
# forget X" is never a note reminding them; it is a mechanism that fails when X is missing — the
# same reasoning as the ADR-0008 grants invariant and config_consumption: turn a fact that must be
# remembered into one that cannot be forgotten.
#
# THIS REPO IS CLEAN on both rules (it was 9 packages and 6 gates when this gate was added; the live count is printed on every pass). That is the point of
# adding it now rather than after the next incident: the gate exists to keep a clean tree clean, and
# its paired control (test_enumeration_selftest.sh) is what proves it would still bite if it weren't.
#
# WHAT IT CHECKS. Two rules, one per flavour observed:
#   1. Every directory containing *_test.go files is named on a `go_suite` line in run.sh.
#   2. Every tests/harness/checks/*.sh is named in the CI workflow.
#
# Rule 2 covers this file too: if someone adds a gate and forgets the workflow, this gate says so.
# It cannot, of course, detect its own absence — that is why it is wired in the same commit that
# creates it, and why the paired control exists.
#
# NO EXEMPTION LIST, DELIBERATELY. The obvious "escape hatch for packages that legitimately should
# not run in the harness" is rejected: this project's own history is several instances of an
# exemption quietly becoming permanent. If a package has test files, it runs. A package whose tests
# genuinely cannot run here should not have test files here.
#
# FILESYSTEM DISCOVERY, NOT `go list`. Discovery is by find(1) rather than `go list ./...` for two
# reasons: it needs no Go toolchain (so this gate runs in any environment, including one where the
# module's Go version is not installed), and it sees a directory whose test files do not COMPILE —
# which `go list` reports as an error rather than as a package, exactly when you most want to know.
set -euo pipefail
cd "$(dirname "$0")/../../.."

RUNSH="tests/harness/run.sh"
CHECKSDIR="tests/harness/checks"
WORKFLOW=".github/workflows/real-infra.yml"

fail=0

# --- 1) Every package with test files is enumerated in run.sh ----------------------------------
# The `go_suite() {` function-definition line also starts with go_suite, so it is filtered out by
# requiring a non-'(' character after the whitespace rather than by hoping no definition appears
# at column 0.
#
# The `|| true` is load-bearing, not defensive noise: under `set -euo pipefail`, a run.sh with ZERO
# go_suite CALL lines makes grep exit 1, which kills this script mid-check with NO output at all —
# so the single worst case (every suite un-enumerated) would report as a silent crash rather than as
# the failure it is. Found by the paired control's case 5, which is exactly what it is for.
enumerated="$(grep -E '^go_suite[[:space:]]+[^([:space:]]' "$RUNSH" | awk '{print $2}' | sort -u || true)"

# Directories holding *_test.go, repo-relative, deduplicated. vendor/ is excluded (not this repo's
# tests to run) and so is Docs/, which carries the gitignored reference codebase and its nested .git.
pkgs="$(find . -name '*_test.go' \
          -not -path './.git/*' \
          -not -path '*/vendor/*' \
          -not -path './Docs/*' \
        | sed 's|/[^/]*$||' | sed 's|^\./||' | sort -u)"

missing=""
while IFS= read -r p; do
  [[ -z "$p" ]] && continue
  if ! grep -qxF -- "$p" <<<"$enumerated"; then
    missing+="$p"$'\n'
  fi
done <<<"$pkgs"

if [[ -n "${missing//[$'\n']/}" ]]; then
  echo "test_enumeration FAIL: package(s) contain *_test.go but are named on NO go_suite line in $RUNSH."
  echo "  Their tests do not run — not in the harness, and not in CI (which runs gofmt + 'go vet ./...' +"
  echo "  this harness; go vet type-checks test files WITHOUT running them, so an unwired package looks"
  echo "  identical to a wired one in every signal we have):"
  printf '%s' "$missing" | sed '/^$/d;s/^/    - /'
  echo "  Fix: add a 'go_suite <dir> \"<label>\"' line to $RUNSH. If the package needs a migrated schema"
  echo "  or the live stack, place that line after the relevant template/stack setup."
  fail=1
fi

# --- 2) Every gate script is enumerated in the CI workflow --------------------------------------
# A gate on disk that the workflow never names is indistinguishable from a gate that passes: local
# runs (and any `for g in checks/*.sh` loop) execute it, CI does not. That is how module 3's
# config_consumption_selftest.sh — a BLOCKING control — landed unwired.
#
# THE MATCH IS AGAINST EXECUTED COMMANDS, NOT THE WORKFLOW FILE (fixed 2026-08-05). This rule used to
# grep the whole workflow for the gate's path, and its own comment claimed that matching the path
# rather than the bare filename meant "a passing mention inside a comment cannot satisfy this." That
# claim was FALSE, and break-verification is what showed it — in module 3 first, on the identical code:
# deleting the drain_grace.sh step outright and leaving behind
#     # TODO: re-enable bash tests/harness/checks/drain_grace.sh — flaky on the CI runner
# left the gate GREEN, reporting all 10 gate scripts wired about a workflow that ran nine. A
# commented-out step is the LIKELIEST way to reach this — it is exactly what someone writes while
# disabling a gate they mean to restore — and it is precisely when a blocking gate stops blocking.
#
# This is the third instance of one shape found in a single day: module 1's rule 1 matched
# `go test ./...` inside a comment ABOUT ITSELF, both config_consumption gates accepted a TODO naming
# a config key as that key's consumer, and now this. A gate satisfied by PROSE about the thing it
# guards. In every case the gate's own comments asserted it was immune, and in every case only
# breaking the guarded thing and watching the gate revealed otherwise. Reasoning about a gate cannot
# find this class; that is the standing lesson.
workflow_commands() {
  # Emit ONLY the shell text the workflow actually EXECUTES: the value of every `run:` key, including
  # the body of `run: |` block scalars, with comments stripped. Anything else in the YAML — a `name:`,
  # a full-line comment, a trailing comment — is prose about the workflow, not the workflow.
  #
  # A `#` begins a comment at start-of-line or after whitespace (YAML's rule, and close enough to the
  # shell's inside a run block: a shell comment in a run block does not invoke anything either).
  # Verified against all three repos' workflows before adopting: no `run:` line contains a ` #`, so
  # nothing real is truncated. Block-scalar membership is "indented deeper than the `run` key
  # itself" — measured from `run`, not from the line's leading whitespace, so a `- run: |` list-item
  # form cannot swallow its sibling keys.
  awk '
    function strip(s) { sub(/(^|[ \t])#.*$/, "", s); return s }
    {
      if (inblock) {
        if ($0 ~ /^[ \t]*$/) { next }
        match($0, /^[ \t]*/)
        if (RLENGTH > blockind) { print strip($0); next }
        inblock = 0
      }
      if ($0 ~ /^[ \t]*(-[ \t]+)?run:/) {
        blockind = index($0, "run:") - 1
        rest = $0
        sub(/^[ \t]*(-[ \t]+)?run:[ \t]*/, "", rest)
        if (rest ~ /^[|>]/) { inblock = 1; next }
        print strip(rest)
      }
    }
  ' "$1"
}

# `|| true`: a workflow with no `run:` key at all yields nothing. An empty $CMDS must still be allowed
# to reach the loop below and fail every gate LOUDLY, rather than dying here with no output — the same
# reasoning as rule 1's `|| true`, and as the fix for config_consumption's silent death on an emptied
# ledger, found the same day.
CMDS="$(workflow_commands "$WORKFLOW" || true)"

unwired=""
for g in "$CHECKSDIR"/*.sh; do
  base="$(basename "$g")"
  if ! grep -qF "$CHECKSDIR/$base" <<<"$CMDS"; then
    unwired+="$base"$'\n'
  fi
done

if [[ -n "${unwired//[$'\n']/}" ]]; then
  echo "test_enumeration FAIL: gate script(s) exist in $CHECKSDIR/ but are named nowhere in $WORKFLOW."
  echo "  The workflow enumerates each gate as its own step; it does NOT glob. An unenumerated gate"
  echo "  never runs in CI, so it is a green signal that proves nothing:"
  printf '%s' "$unwired" | sed '/^$/d;s/^/    - /'
  echo "  NOTE: only a 'run:' command counts. A COMMENTED-OUT step naming the gate does not satisfy"
  echo "  this — that was a real defect here until 2026-08-05, and a disabled gate is the whole point."
  echo "  Fix: add a step to $WORKFLOW. Use 'if: always()' so one gate's failure does not mask the rest."
  fail=1
fi

if [[ "$fail" -eq 0 ]]; then
  npkg="$(echo "$pkgs" | grep -c . || true)"
  ngate="$(ls "$CHECKSDIR"/*.sh | wc -l | tr -d ' ')"
  echo "test_enumeration OK: all $npkg package(s) with test files are in $RUNSH, and all $ngate gate script(s) are in $WORKFLOW"
fi
exit "$fail"
