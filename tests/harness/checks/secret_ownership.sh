#!/usr/bin/env bash
# secret_ownership — BLOCKING CI gate. Each dev key is readable by the uid that must read it, stays mode 600, and (on Linux)
# is NOT readable by the others.
#
# THE DEFECT this guards: the dev keys were written 600 and owned by the CI user; the Control (uid 10001) and workers
# (uid 10002) are different uids, so on a real Linux runner the Control died at boot with
# `FATAL kms load: ... dev-master.key: permission denied`. Docker Desktop bind mounts do not enforce POSIX ownership,
# so every local run was green and the first CI run was red. The fix is OWNERSHIP (tools/own-secrets.sh), never looser modes.
#
# It READS each key as the real container uid, so it fails for exactly the reason production would — and the negative
# probes (the worker uid must NOT read the KMS key; nobody may read worker.key) are the paired control that stops the
# positive probes passing for the wrong reason (e.g. keys that had simply been made world-readable). On hosts that do not
# enforce ownership (Docker Desktop on Windows/macOS) the negatives and the mode check are skipped, loudly.
set -euo pipefail
cd "$(dirname "$0")/../../.."
[[ -f secrets/dev/certs/control.key ]] || { echo "FAIL (secret_ownership): secrets/dev not generated — run scripts/dev.sh certs first" >&2; exit 1; }
exec bash tools/own-secrets.sh --check
