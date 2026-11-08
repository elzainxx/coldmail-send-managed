#!/usr/bin/env bash
# postgres_durability — BLOCKING CI gate (#131). The DEFAULT dev stack's Postgres must survive a
# `docker stop`; the RAM-backed one must exist only when asked for.
#
# THE DEFECT. scripts/dev.sh merged tests/harness/compose.test.yml unconditionally, and that file puts
# Postgres' data directory on a tmpfs with fsync=off. A `docker stop` frees a tmpfs, so the next
# `docker start` brought up an EMPTY database: the test guide's own outage drill ("docker stop Postgres
# for a few minutes, then start it") DESTROYED the database and read as a catastrophic recovery bug in
# the system. dev.sh now merges that file only under DEV_RAM_PG=1 (which CI sets, for speed).
#
# WHAT THIS ASSERTS, from the RENDERED compose config (`docker compose config`) — the same thing
# docker would run, not a grep of the source files, so a future edit that re-merges the override by any
# route (a new -f, an `include:`, a profile) is seen:
#   1. DEFAULT mode: postgres has NO tmpfs and NO fsync=off.
#   2. RAM mode (DEV_RAM_PG=1): postgres HAS the tmpfs data dir and fsync=off.   <- the CONTROL.
#      Without it, check 1 would pass on an EMPTY render (a `config` that failed, a service that was
#      renamed) — a green that proves nothing. The control makes the parser prove it can see a tmpfs.
#   3. scripts/dev.sh's printed invocation names compose.test.yml in RAM mode and ONLY then.
#
# Needs the docker CLI and compose, no running stack. In CI it runs after `dev.sh certs`, so .env exists.
set -euo pipefail
cd "$(dirname "$0")/../../.."

pg_block() { # $1 = DEV_RAM_PG value → the rendered `postgres:` service, or nothing if config failed
  local inv; inv="$(DEV_RAM_PG="$1" bash scripts/dev.sh compose)"
  $inv config 2>/dev/null | awk '/^  postgres:$/{p=1; print; next} p && /^  [a-zA-Z0-9_-]+:$/{exit} p{print}' || true   # a failed render must reach the explicit FAIL below, not kill the script silently under set -e
}

fail=0
bad() { echo "FAIL (postgres_durability): $*" >&2; fail=1; }

default_block="$(pg_block "")"
ram_block="$(pg_block "1")"

# A render that did not happen must be a failure, not a pass-by-absence.
grep -q 'image: postgres' <<<"$default_block" || bad "DEFAULT mode: could not render the postgres service from 'docker compose config' (is .env present? run scripts/dev.sh certs)"
grep -q 'image: postgres' <<<"$ram_block"     || bad "RAM mode: could not render the postgres service from 'docker compose config'"

# 1. default mode is durable
if grep -q 'tmpfs' <<<"$default_block"; then
  bad "DEFAULT mode puts postgres on a tmpfs — 'docker stop postgres' would destroy the database (#131). scripts/dev.sh must merge tests/harness/compose.test.yml only under DEV_RAM_PG=1."
fi
if grep -q 'fsync=off' <<<"$default_block"; then
  bad "DEFAULT mode runs postgres with fsync=off — that tuning belongs to the opt-in RAM mode only."
fi

# 2. the control: RAM mode really does render the tmpfs, so the check above can see one
grep -q 'tmpfs' <<<"$ram_block"                   || bad "CONTROL: DEV_RAM_PG=1 rendered no tmpfs for postgres — check 1 is blind and proves nothing"
grep -q '/var/lib/postgresql/data' <<<"$ram_block" || bad "CONTROL: DEV_RAM_PG=1 did not put /var/lib/postgresql/data on the tmpfs"
grep -q 'fsync=off' <<<"$ram_block"               || bad "CONTROL: DEV_RAM_PG=1 rendered no fsync=off"

# 3. the printed invocation
if DEV_RAM_PG= bash scripts/dev.sh compose | grep -q 'compose.test.yml'; then
  bad "DEFAULT 'scripts/dev.sh compose' names compose.test.yml"
fi
DEV_RAM_PG=1 bash scripts/dev.sh compose | grep -q 'compose.test.yml' \
  || bad "CONTROL: 'DEV_RAM_PG=1 scripts/dev.sh compose' does not name compose.test.yml"

if [[ "$fail" -ne 0 ]]; then exit 1; fi
echo "postgres_durability: ok — default stack: no tmpfs, no fsync=off; DEV_RAM_PG=1: tmpfs on /var/lib/postgresql/data + fsync=off (control)."
