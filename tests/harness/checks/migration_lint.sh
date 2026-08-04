#!/usr/bin/env bash
# migration_lint — BLOCKING CI gate. Enforces the locked migration convention:
#   - every migration is a send_managed_NNNN_<desc>.up.sql + matching .down.sql pair
#   - versions are contiguous ascending (no gaps, no dupes)
#   - every UP self-records: INSERT INTO send_managed.schema_migrations(version) VALUES('NNNN')
#   - every DOWN self-removes: DELETE ... WHERE version='NNNN' (or documented no-op for
#     enum ADD VALUE, which is forward-only)
set -euo pipefail
cd "$(dirname "$0")/../../.."

DIR="db/migrations"
fail=0
prev=""

ups=$(ls "$DIR"/send_managed_*.up.sql 2>/dev/null | sort || true)
if [[ -z "$ups" ]]; then
  echo "migration_lint: no migration files yet (Phase 0 ships send_managed_0000_bootstrap)."
fi

for up in $ups; do
  base="$(basename "$up")"
  if [[ ! "$base" =~ ^send_managed_([0-9]{4})_(.+)\.up\.sql$ ]]; then
    echo "migration_lint FAIL: bad name $base (want send_managed_NNNN_desc.up.sql)"; fail=1; continue
  fi
  ver="${BASH_REMATCH[1]}"; desc="${BASH_REMATCH[2]}"
  down="$DIR/send_managed_${ver}_${desc}.down.sql"

  # paired down
  [[ -f "$down" ]] || { echo "migration_lint FAIL: $base has no matching .down.sql"; fail=1; }

  # contiguity — the chain starts at 0000 (Phase 0 pre-feature bootstrap) so the LOCKED
  # feature chain stays 0001..0020; a bootstrap-free chain may legitimately start at 0001.
  if [[ -n "$prev" ]]; then
    expect=$(printf '%04d' $((10#$prev + 1)))
    [[ "$ver" == "$expect" ]] || { echo "migration_lint FAIL: gap/dup — after $prev expected $expect, got $ver"; fail=1; }
  else
    [[ "$ver" == "0000" || "$ver" == "0001" ]] || { echo "migration_lint FAIL: chain must start at 0000 (bootstrap) or 0001, got $ver"; fail=1; }
  fi
  prev="$ver"

  # self-record in UP
  grep -Eq "INSERT +INTO +send_managed\.schema_migrations.*'${ver}'" "$up" \
    || { echo "migration_lint FAIL: $base does not self-record version '$ver'"; fail=1; }

  # self-remove in DOWN (allow a documented forward-only no-op comment for enum ADD VALUE)
  if [[ -f "$down" ]]; then
    if ! grep -Eq "DELETE +FROM +send_managed\.schema_migrations.*'${ver}'" "$down"; then
      grep -Eiq "forward-only|no-op|cannot safely remove" "$down" \
        || { echo "migration_lint FAIL: $(basename "$down") neither DELETEs version '$ver' nor documents a forward-only no-op"; fail=1; }
    fi
  fi
done

# The lifecycle test's EXPECTED chain must include the newest migration. TestRunnerLifecycle
# hardcodes wantChain (deliberately: it is the executable expectation of the chain, and a test that
# derives its expectation from the same directory it is checking cannot catch a missing file). The
# cost is that adding a migration without extending wantChain breaks it — which is exactly what
# send_managed_0022 did, and nothing here caught it: the chain on disk was perfectly contiguous, so this
# gate passed while the suite failed. Assert the coupling the two files have rather than leaving it
# to whoever remembers.
#
# Scoped to the wantChain LITERAL, not the whole file. A bare `grep '"0022"'` passes on any
# mention anywhere — including the version named in this gate's own failure message, in a comment,
# or in the `if v == "0022"` down-check — so the first version of this check reported OK with
# wantChain reverted to its pre-fix state. Caught by its own negative control; the extraction below
# reads only the bracketed wantChain value.
LIFECYCLE_TEST="tools/send-managed-migrate/runner_integration_test.go"
if [[ -n "$prev" && -f "$LIFECYCLE_TEST" ]]; then
  want_literal="$(sed -n '/wantChain := \[\]string{/,/}/p' "$LIFECYCLE_TEST")"
  if [[ -z "$want_literal" ]]; then
    echo "migration_lint FAIL: could not find the wantChain literal in $LIFECYCLE_TEST"
    echo "  -> this gate reads 'wantChain := []string{...}'; if that declaration moved or was"
    echo "     renamed, update this check rather than deleting it."
    fail=1
  elif ! grep -Eq "\"${prev}\"" <<<"$want_literal"; then
    echo "migration_lint FAIL: newest migration '$prev' is absent from wantChain in $LIFECYCLE_TEST"
    echo "  -> add \"$prev\" to wantChain, plus an anti-vacuity assertion that send_managed_${prev}'s"
    echo "     object/seed/behavior landed (and the paired check that its DOWN removed it)."
    fail=1
  fi
fi

[[ "$fail" -eq 0 ]] && echo "migration_lint OK: chain contiguous, paired, self-tracking; lifecycle test covers $prev."
exit "$fail"
