#!/usr/bin/env bash
# image_pins_selftest — BLOCKING paired control for image_pins.sh. On a good tree the gate prints `OK`, which means every one of
# its failure branches is unexercised: it could be broken into accepting everything and CI would stay green forever. This
# builds throwaway compose/Dockerfile fixtures that trip each assertion in turn and checks BOTH the exit code and the message
# (a gate that fails for the wrong reason is not a control).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$HERE/image_pins.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
D=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa   # 64 hex

pass=0; failn=0
compose() { printf 'services:\n  x:\n    image: %s\n' "$1" >"$TMP/docker-compose.yml"; }
dockerfile() { printf '%b' "$1" >"$TMP/Dockerfile"; }
run() { (cd "$TMP" && bash "$GATE" "$@" 2>&1); }
expect_ok()   { local out rc=0; out="$(run "$@")" || rc=$?; if [ $rc -eq 0 ]; then pass=$((pass+1)); echo "  ok   $LABEL"; else failn=$((failn+1)); echo "  BAD  $LABEL — expected exit 0, got $rc: $out"; fi; }
expect_fail() { local msg="$1"; shift; local out rc=0; out="$(run "$@")" || rc=$?
  if [ $rc -ne 0 ] && grep -q -- "$msg" <<<"$out"; then pass=$((pass+1)); echo "  ok   $LABEL (rejected: $msg)"
  else failn=$((failn+1)); echo "  BAD  $LABEL — expected a failure containing '$msg', got rc=$rc: $out"; fi; }

LABEL="a digest-pinned mailpit passes";                     compose "axllent/mailpit:v1.33.1@sha256:$D"; expect_ok "$TMP/docker-compose.yml"
LABEL="a digest-pinned builder (postgres) passes";          compose "postgres:16-alpine@sha256:$D"; expect_ok "$TMP/docker-compose.yml"
LABEL="postgres by tag alone is REJECTED (was: reported, not failed)"; compose "postgres:16-alpine"; expect_fail "must be pinned by digest" "$TMP/docker-compose.yml"
LABEL=":latest is rejected";                                compose "axllent/mailpit:latest"; expect_fail "is floating" "$TMP/docker-compose.yml"
LABEL="an untagged image (= :latest) is rejected";          compose "somevendor/thing"; expect_fail "has no tag" "$TMP/docker-compose.yml"
LABEL="mailpit by tag alone (no digest) is rejected";       compose "axllent/mailpit:v1.33.1"; expect_fail "must be pinned by digest" "$TMP/docker-compose.yml"
LABEL="coredns by tag alone is rejected";                   compose "coredns/coredns:1.15.0"; expect_fail "must be pinned by digest" "$TMP/docker-compose.yml"
LABEL="the python black-hole fixture by tag alone is rejected"; compose "python:3.15.2-alpine"; expect_fail "must be pinned by digest" "$TMP/docker-compose.yml"
LABEL="a truncated digest is rejected";                     compose "coredns/coredns:1.15.0@sha256:abc"; expect_fail "must be pinned by digest" "$TMP/docker-compose.yml"
LABEL="a registry host:port in the repo is not mistaken for a tag"; compose "registry.local:5000/team/thing:1.0@sha256:$D"; expect_ok "$TMP/docker-compose.yml"
LABEL="a Dockerfile FROM :latest is rejected";              dockerfile 'FROM golang:latest AS build\nRUN true\n'; expect_fail "is floating" "$TMP/Dockerfile"
LABEL="a later stage that FROMs an earlier stage is not an image"; dockerfile "FROM golang:1.26-alpine@sha256:$D AS build\nRUN true\nFROM build AS test\nFROM alpine:3.20@sha256:$D\n"; expect_ok "$TMP/Dockerfile"
LABEL="FROM scratch is exempt (checked beside a real pinned image, so the run is not vacuous)"; dockerfile 'FROM scratch
COPY x /x
'; compose "coredns/coredns:1.15.0@sha256:$D"; expect_ok "$TMP/docker-compose.yml" "$TMP/Dockerfile"
LABEL="a file with no image reference is a vacuous check, not a pass"; printf 'services: {}\n' >"$TMP/docker-compose.yml"; expect_fail "vacuous" "$TMP/docker-compose.yml"

LABEL="a Dockerfile FROM golang by tag alone is rejected";  dockerfile 'FROM golang:1.26-alpine AS build\nRUN true\n'; expect_fail "must be pinned by digest" "$TMP/Dockerfile"
LABEL="a digest-pinned multi-stage Dockerfile passes";      dockerfile "FROM golang:1.26-alpine@sha256:$D AS build\nRUN true\nFROM alpine:3.20@sha256:$D\n"; expect_ok "$TMP/Dockerfile"
LABEL="node by tag alone is rejected";                      dockerfile 'FROM node:20-alpine\n'; expect_fail "must be pinned by digest" "$TMP/Dockerfile"
LABEL="a workflow service image by tag alone is rejected";  printf 'jobs:\n  j:\n    services:\n      pg:\n        image: postgres:16-alpine\n' >"$TMP/ci.yml"; expect_fail "must be pinned by digest" "$TMP/ci.yml"
LABEL="a workflow service image with a digest passes";      printf 'jobs:\n  j:\n    services:\n      pg:\n        image: postgres:16-alpine@sha256:%s\n' "$D" >"$TMP/ci.yml"; expect_ok "$TMP/ci.yml"

# The SCOPE of the default run — the gap that let `alpine:3` sit in tools/own-secrets.sh while the gate said OK. A copy of the gate in a
# throwaway repo tree, run with NO arguments, must find and reject a tag-only image in tools/*/Dockerfile and in tools/*.sh.
mkscope() { rm -rf "$TMP/scope"; mkdir -p "$TMP/scope/tests/harness/checks" "$TMP/scope/tools/x" "$TMP/scope/services/s"; cp "$GATE" "$TMP/scope/tests/harness/checks/image_pins.sh"
  printf 'FROM alpine:3.20@sha256:%s\n' "$D" >"$TMP/scope/services/s/Dockerfile"; printf 'FROM alpine:3.20@sha256:%s\n' "$D" >"$TMP/scope/tools/x/Dockerfile"; }
scoped() { local out rc=0; out="$(cd "$TMP/scope" && bash tests/harness/checks/image_pins.sh 2>&1)" || rc=$?; echo "$rc|$out"; }
LABEL="default scope: a clean tree passes"; mkscope; r="$(scoped)"; if [ "${r%%|*}" = 0 ]; then pass=$((pass+1)); echo "  ok   $LABEL"; else failn=$((failn+1)); echo "  BAD  $LABEL — $r"; fi
LABEL="default scope covers tools/*/Dockerfile (a tag-only builder there is rejected)"; mkscope; printf 'FROM golang:1.26-alpine AS build\n' >"$TMP/scope/tools/x/Dockerfile"
r="$(scoped)"; if [ "${r%%|*}" != 0 ] && grep -q "tools/x/Dockerfile" <<<"$r" && grep -q "must be pinned by digest" <<<"$r"; then pass=$((pass+1)); echo "  ok   $LABEL"; else failn=$((failn+1)); echo "  BAD  $LABEL — $r"; fi
LABEL="default scope covers shell tooling (docker run ... alpine:3 in tools/*.sh is rejected)"; mkscope; printf '#!/usr/bin/env bash\ndock() { docker run --rm "$@"; }\ndock -v /a:/s alpine:3 chown 1:1 /s\n' >"$TMP/scope/tools/own.sh"
r="$(scoped)"; if [ "${r%%|*}" != 0 ] && grep -q "tools/own.sh" <<<"$r" && grep -q "must be pinned by digest" <<<"$r"; then pass=$((pass+1)); echo "  ok   $LABEL"; else failn=$((failn+1)); echo "  BAD  $LABEL — $r"; fi
LABEL="shell tooling: a digest-pinned image and a \$IMG variable both pass"; mkscope; printf '#!/usr/bin/env bash\ndock() { docker run --rm "$@"; }\ndock -v /a:/s alpine:3.22@sha256:%s chown 1:1 /s\ndocker run --rm "$IMG" true\n# docker run alpine:3 (a comment)\n' "$D" >"$TMP/scope/tools/own.sh"
r="$(scoped)"; if [ "${r%%|*}" = 0 ]; then pass=$((pass+1)); echo "  ok   $LABEL"; else failn=$((failn+1)); echo "  BAD  $LABEL — $r"; fi

echo "image_pins_selftest: $pass passed, $failn failed"
[ "$failn" -eq 0 ]
