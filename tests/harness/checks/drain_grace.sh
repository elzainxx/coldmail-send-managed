#!/usr/bin/env bash
# drain_grace — BLOCKING CI gate. Asserts the Control's SIGTERM→SIGKILL grace CLEARS the seeded
# shutdown_drain_timeout_sec, in every place that bound is expressed.
#
# WHY THIS EXISTS. Phase 7 §4's graceful shutdown gives in-flight sends up to
# shutdown_drain_timeout_sec (seeded 30s) to report before leaving them for the next boot's rescan.
# If the supervisor's stop grace is SHORTER, it SIGKILLs a Control that is draining correctly and the
# drain branch becomes unreachable — silently. That is exactly what shipped: docker-compose.yml
# declared no stop_grace_period, so Docker's 10s default truncated every in-flight drain
# (`docker compose stop control` exited 137 after ~11s), and Stage 11 step 5 of the local test guide
# is what finally caught it. The invariant was already documented in three places
# (config-reference.md, the install-and-deploy.md systemd unit's TimeoutStopSec, the
# operator-runbook.md row naming this exact failure) — only the compose file contradicted them, and
# nothing enforced agreement between the key and the grace.
#
# A truncated drain is NOT unsafe — §4.2 leave-in-place requeues nothing and marks nothing, and the
# next boot's rescan recovers the job. That is precisely why it went unnoticed for the whole build:
# the only symptoms are exit code 137 and avoidable reclaim work, never lost or duplicated mail. A
# defect whose symptom is invisible needs a gate, not a comment.
#
# The two bounds are INDEPENDENT and both must clear the key: stop_grace_period governs Docker,
# TimeoutStopSec governs systemd. Setting only one leaves the shorter in charge.
set -euo pipefail
cd "$(dirname "$0")/../../.."

DC="${COMPOSE_INVOCATION:-$(bash scripts/dev.sh compose)}"  # the invocation dev.sh uses (RAM Postgres only under DEV_RAM_PG=1)

fail=0

# 1) The seeded drain window is the authority (config-not-code, Rule 1): read it from the live DB,
#    never from a literal here. If send_managed.config is not up yet, fall back to the migration's seed so
#    the gate still runs standalone.
drain="$($DC exec -T postgres psql -U postgres -d platform -tAc \
  "SELECT value->>'v' FROM send_managed.config WHERE key='shutdown_drain_timeout_sec'" 2>/dev/null \
  | tr -d '[:space:]' || true)"
if [[ -z "$drain" ]]; then
  drain="$(grep -rhoE "'shutdown_drain_timeout_sec'[^)]*\{\"v\": *[0-9]+\}" db/migrations/ 2>/dev/null \
    | grep -oE '[0-9]+\}' | tr -d '}' | head -1 || true)"
fi
if [[ -z "$drain" ]]; then
  echo "drain_grace FAIL: could not determine shutdown_drain_timeout_sec (DB down and no seed found)"
  exit 1
fi

# 2) Docker: the control service's stop_grace_period, in seconds. Absent => Docker's 10s default,
#    which is the bug this gate exists to catch, so absence is a FAILURE, not a skip.
grace_raw="$(awk '
  /^  [a-z0-9_-]+:/ { svc = $1; sub(":", "", svc) }
  svc == "control" && /^[[:space:]]*stop_grace_period:/ { print $2; exit }
' docker-compose.yml | tr -d '[:space:]')"
if [[ -z "$grace_raw" ]]; then
  echo "drain_grace FAIL: docker-compose.yml control service has NO stop_grace_period."
  echo "  Docker then defaults to 10s and SIGKILLs a Control draining for ${drain}s (exit 137)."
  fail=1
else
  # Accept 90s / 90 / 2m / 1m30s.
  g="$grace_raw"; secs=0
  if [[ "$g" =~ ^([0-9]+)m([0-9]+)s?$ ]]; then
    secs=$(( ${BASH_REMATCH[1]} * 60 + ${BASH_REMATCH[2]} ))
  elif [[ "$g" =~ ^([0-9]+)m$ ]]; then
    secs=$(( ${BASH_REMATCH[1]} * 60 ))
  elif [[ "$g" =~ ^([0-9]+)s?$ ]]; then
    secs="${BASH_REMATCH[1]}"
  else
    echo "drain_grace FAIL: cannot parse control stop_grace_period '$grace_raw'"
    fail=1
  fi
  if [[ "$fail" -eq 0 && "$secs" -le "$drain" ]]; then
    echo "drain_grace FAIL: control stop_grace_period ${grace_raw} (${secs}s) <= shutdown_drain_timeout_sec ${drain}s."
    echo "  Docker will SIGKILL a Control that is draining correctly. Raise stop_grace_period."
    fail=1
  fi
fi

# 3) systemd: the documented Control unit's TimeoutStopSec must clear the same key. The unit lives in
#    the install guide (no prod compose file is shipped — see install-and-deploy.md), so the doc IS
#    the artifact under test here.
tss="$(grep -oE '^TimeoutStopSec=[0-9]+' Docs/install-and-deploy.md 2>/dev/null \
  | head -1 | cut -d= -f2 || true)"
if [[ -z "$tss" ]]; then
  echo "drain_grace FAIL: no TimeoutStopSec= found in Docs/install-and-deploy.md's systemd unit"
  fail=1
elif [[ "$tss" -le "$drain" ]]; then
  echo "drain_grace FAIL: documented TimeoutStopSec=${tss} <= shutdown_drain_timeout_sec ${drain}s"
  fail=1
fi

if [[ "$fail" -eq 0 ]]; then
  echo "drain_grace OK: drain ${drain}s < docker stop_grace_period ${grace_raw} and systemd TimeoutStopSec=${tss}"
fi
exit "$fail"
