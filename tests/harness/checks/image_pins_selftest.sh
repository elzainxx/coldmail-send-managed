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
LABEL="an unpinned-by-digest builder tag passes (reported, not failed)"; compose "postgres:16-alpine"; expect_ok "$TMP/docker-compose.yml"
LABEL=":latest is rejected";                                compose "axllent/mailpit:latest"; expect_fail "is floating" "$TMP/docker-compose.yml"
LABEL="an untagged image (= :latest) is rejected";          compose "somevendor/thing"; expect_fail "has no tag" "$TMP/docker-compose.yml"
LABEL="mailpit by tag alone (no digest) is rejected";       compose "axllent/mailpit:v1.33.1"; expect_fail "must be pinned by digest" "$TMP/docker-compose.yml"
LABEL="coredns by tag alone is rejected";                   compose "coredns/coredns:1.15.0"; expect_fail "must be pinned by digest" "$TMP/docker-compose.yml"
LABEL="the python black-hole fixture by tag alone is rejected"; compose "python:3.15.2-alpine"; expect_fail "must be pinned by digest" "$TMP/docker-compose.yml"
LABEL="a truncated digest is rejected";                     compose "coredns/coredns:1.15.0@sha256:abc"; expect_fail "must be pinned by digest" "$TMP/docker-compose.yml"
LABEL="a registry host:port in the repo is not mistaken for a tag"; compose "registry.local:5000/team/thing:1.0@sha256:$D"; expect_ok "$TMP/docker-compose.yml"
LABEL="a Dockerfile FROM :latest is rejected";              dockerfile 'FROM golang:latest AS build\nRUN true\n'; expect_fail "is floating" "$TMP/Dockerfile"
LABEL="a later stage that FROMs an earlier stage is not an image"; dockerfile 'FROM golang:1.26-alpine AS build\nRUN true\nFROM build AS test\nFROM alpine:3.20\n'; expect_ok "$TMP/Dockerfile"
LABEL="FROM scratch is exempt (checked beside a real pinned image, so the run is not vacuous)"; dockerfile 'FROM scratch
COPY x /x
'; compose "coredns/coredns:1.15.0@sha256:$D"; expect_ok "$TMP/docker-compose.yml" "$TMP/Dockerfile"
LABEL="a file with no image reference is a vacuous check, not a pass"; printf 'services: {}\n' >"$TMP/docker-compose.yml"; expect_fail "vacuous" "$TMP/docker-compose.yml"

echo "image_pins_selftest: $pass passed, $failn failed"
[ "$failn" -eq 0 ]
