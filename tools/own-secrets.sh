#!/usr/bin/env bash
# own-secrets.sh [--check] — give each dev key to the uid that must read it, KEEPING mode 600, then PROVE it.
#
# THE DEFECT (found 2026-11-08, the first time these workflows ran on a real Linux runner): gen-certs.sh and
# gen-secrets.sh write the dev keys mode 600, owned by whoever ran them. The Control runs as uid 10001 and the workers
# as uid 10002 (services/*/Dockerfile: `adduser -D -u ...`). On a Linux host those uids are NOT the file's owner, so
# the kernel refuses: `FATAL kms load: ... dev-master.key: permission denied`. Docker Desktop's bind mounts do not
# enforce POSIX ownership, so every local run was green while CI was red.
#
# THE FIX IS OWNERSHIP, NOT LOOSER PERMISSIONS. The keys stay 600; each is chown'd to the one uid that reads it:
#     control.key, dev-master.key  -> the Control uid
#     worker.key                   -> the worker uid
#     ca.key, sink.key, harness-client.key  -> left to the host user (nothing containerised running as another uid reads
#                                  them: Mailpit runs as root; the CA signs on the host; harness-client is the host-side
#                                  tests' OWN identity, so the Node tests no longer share the worker's key)
# This is also strictly STRONGER than before: the worker uid can no longer read the KMS key even if a mount were
# added by mistake, which is the "worker cannot read KMS" property enforced by the kernel and not only by omission.
#
# The uids are READ FROM THE DOCKERFILES, not repeated here, so they cannot drift apart.
#
# chown to another uid needs privilege the CI user does not have, so: root -> chown; passwordless sudo -> sudo chown;
# otherwise a throwaway root container does it. On hosts whose bind mounts do not enforce ownership (Windows/macOS
# Docker Desktop) a failed chown is a warning, and the negative probes are skipped; on Linux both are hard failures.
#
# --check performs the probes only (used by tests/harness/checks/secret_ownership.sh).
set -euo pipefail
cd "$(dirname "$0")/.."
SEC=secrets/dev
MODE=own; [ "${1:-}" = "--check" ] && MODE=check

# `;T;p;q` prints the first match and quits sed ITSELF — no `| head -1`, because under pipefail an early-exiting reader
# makes the writer die of a broken pipe and fails the pipeline (the config_consumption gate was bitten by exactly that).
uid_of() { sed -n "s/.*adduser -D -u \([0-9]\{1,\}\) $2.*/\1/;T;p;q" "$1"; }
CTL_UID="$(uid_of services/control/Dockerfile control)"
WRK_UID="$(uid_of services/worker/Dockerfile worker)"
[ -n "$CTL_UID" ] && [ -n "$WRK_UID" ] || { echo "own-secrets: cannot read the container uids from services/*/Dockerfile" >&2; exit 2; }

case "$(uname -s)" in Linux) ENFORCED=1;; *) ENFORCED=0;; esac
ABS="$(pwd -W 2>/dev/null || pwd)/$SEC"
dock() { MSYS_NO_PATHCONV=1 docker run --rm "$@"; }

fail=0
bad()  { echo "own-secrets: FAIL: $*" >&2; fail=1; }
note() { echo "own-secrets: $*"; }

chown_to() { # chown_to <uid> <path relative to $SEC>...
  local uid="$1"; shift
  if [ "$(id -u)" = 0 ]; then ( cd "$SEC" && chown "$uid:$uid" "$@" ); return; fi
  if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then ( cd "$SEC" && sudo chown "$uid:$uid" "$@" ); return; fi
  dock -v "$ABS:/s" -w /s alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f chown "$uid:$uid" "$@"
}

if [ "$MODE" = own ]; then
  [ -f "$SEC/certs/control.key" ] && [ -f "$SEC/certs/worker.key" ] && [ -f "$SEC/kms/dev-master.key" ] \
    || { echo "own-secrets: run gen-certs.sh and gen-secrets.sh first" >&2; exit 2; }
  chmod a+rx "$SEC" "$SEC/certs" "$SEC/kms" 2>/dev/null || true      # directories only: the uids must be able to traverse
  chmod 600 "$SEC"/certs/*.key "$SEC/kms/dev-master.key" 2>/dev/null || true   # re-assert; a no-op once chown'd away
  if chown_to "$CTL_UID" certs/control.key kms/dev-master.key && chown_to "$WRK_UID" certs/worker.key; then
    note "chown'd control.key + dev-master.key -> uid $CTL_UID, worker.key -> uid $WRK_UID (mode 600 kept)"
  elif [ "$ENFORCED" = 1 ]; then
    bad "could not chown the dev keys to the container uids on a Linux host"
  else
    note "WARNING: chown failed, but this host does not enforce bind-mount ownership ($(uname -s)); continuing"
  fi
fi

# ── the proof: read each key AS THE UID THAT MUST READ IT ──────────────────────────────────────────────────────────
as() { local uid="$1"; shift; dock --user "$uid:$uid" -v "$ABS:/s:ro" alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f sh -c "$*" >/dev/null 2>&1; }

as "$CTL_UID" 'cat /s/certs/control.key /s/kms/dev-master.key' || bad "the Control (uid $CTL_UID) cannot read control.key / dev-master.key — it will die at boot with 'permission denied'"
as "$WRK_UID" 'cat /s/certs/worker.key'                        || bad "the worker (uid $WRK_UID) cannot read worker.key — it will fail to open its mTLS client"
[ -r "$SEC/certs/harness-client.key" ] || bad "the host user cannot read harness-client.key (the host-side tests' own identity)"

if [ "$ENFORCED" = 1 ]; then
  for f in "$SEC"/certs/*.key "$SEC/kms/dev-master.key"; do
    m="$(stat -c %a "$f")"; [ "$m" = 600 ] || bad "$f is mode $m — dev keys must stay 600 (never loosen them to make a mount work)"
  done
  # negatives: ownership must actually be doing the work, or the positives above would pass for the wrong reason
  as "$WRK_UID" 'cat /s/kms/dev-master.key'  && bad "the worker uid CAN read the KMS key — the KMS boundary is no longer enforced by permissions"
  as "$WRK_UID" 'cat /s/certs/control.key'   && bad "the worker uid CAN read control.key"
  as "$CTL_UID" 'cat /s/certs/worker.key'    && bad "the Control uid CAN read worker.key"
  as 65534      'cat /s/certs/worker.key'    && bad "an arbitrary uid (nobody) CAN read worker.key"
else
  note "negative probes + mode check skipped: $(uname -s) bind mounts do not enforce POSIX ownership (they run on Linux/CI)"
fi

[ "$fail" = 0 ] || exit 1
note "ok — Control(uid $CTL_UID) reads control.key+dev-master.key, worker(uid $WRK_UID) reads worker.key, keys are 600$([ "$ENFORCED" = 1 ] && echo ', and no other uid can read them' || true)"
