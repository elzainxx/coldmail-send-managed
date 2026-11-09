#!/usr/bin/env bash
# image_pins — BLOCKING gate: EVERY image this repository pulls is pinned by DIGEST.
#
#   image_pins.sh [file...]    default scope (see SCOPE below)
#
# THE DEFECT CLASS. A floating tag is a moving target. A CI runner pulls cold and gets whatever the registry serves that day;
# a developer's machine keeps an old cached copy for months and never notices. Measured here (2026-11-08): the CI replica held
# CoreDNS 1.14.6 while the runner pulled 1.15.0, and the Ruby base image that "worked for months" could not be built from
# scratch at all (ADR-0018 in module 1). `:latest` — or no tag, which means the same thing — is the worst case: not even a
# version to read off. A version tag without a digest is the same defect one step quieter: the tag moves.
#
# WHAT IT ASSERTS (changed 2026-11-09 — it used to demand digests only for mailpit, CoreDNS and the DNS black-hole python, and
# merely REPORTED the builders as "pinned by decision, not by omission"; the owner ruled that every tag is pinned):
#   1. no reference is `:latest` or untagged;
#   2. EVERY reference carries `@sha256:<64 hex>` — postgres, the Go builder, node and alpine included.
# The cost is real and is the point of a decision, not a side effect: a pinned builder stops receiving its base-image security
# patches until someone bumps the digest. That bump is now a reviewed commit instead of something that happens to you on a
# runner on some Tuesday.
#
# SCOPE. A gate that reads only the files it was first written for is the shape that let `alpine:3` sit in tools/own-secrets.sh
# while this gate reported "OK". So:
#   * structural (definitive): `image:` in docker-compose*.yml, tests/harness/compose*.yml and .github/workflows/*.yml, and every
#     `FROM` in services/*/Dockerfile AND tools/*/Dockerfile  (a `FROM <earlier stage>` and `FROM scratch` are exempt);
#   * heuristic: shell tooling (tools/*.sh, tests/harness/**/*.sh) — any `docker run|pull|create` / `dock` line whose arguments
#     include a well-known base-image name (alpine, golang, node, postgres, python, ruby, ubuntu, debian, busybox, ...) must carry a
#     digest. This one cannot be definitive: an image from a repository name it does not know is invisible to it. It exists because
#     the alternative — not looking — already missed a real one. `"$IMG"`-style variables are fine (their value is a pinned constant
#     the same file defines, and is itself checked where it is written).
# A `${...}`-interpolated `image:` cannot be read statically; it is counted and reported, not silently skipped.
#
# Paired control: image_pins_selftest.sh feeds it each violation — including the tools/ Dockerfile and shell shapes — and checks
# the exit code AND the message.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."

structural=("$@"); scripts=()
if [ ${#structural[@]} -eq 0 ]; then
  while IFS= read -r f; do structural+=("$f"); done < <(ls docker-compose*.yml tests/harness/compose*.yml .github/workflows/*.yml \
                                                          services/*/Dockerfile tools/*/Dockerfile 2>/dev/null || true)
  # *_selftest.sh are excluded: their fixtures contain violations on purpose.
  while IFS= read -r f; do case "$f" in *_selftest.sh) ;; *) scripts+=("$f");; esac; done < <(ls tools/*.sh tests/harness/*.sh tests/harness/*/*.sh 2>/dev/null || true)
fi
[ ${#structural[@]} -gt 0 ] || { echo "image_pins FAIL: no compose/Dockerfile found to check — the gate would be vacuous" >&2; exit 1; }

# Names a shell line is recognised by (the heuristic half). Extend when a new base image enters the tooling.
SCRIPT_IMAGES='alpine|golang|node|postgres|python|ruby|ubuntu|debian|busybox|redis|nginx|mikefarah/yq|curlimages/curl|axllent/mailpit|coredns/coredns|zricethezav/gitleaks'
SCRIPT_REF_RE="^(docker\\.io/)?(library/)?($SCRIPT_IMAGES)(:[A-Za-z0-9._-]+)?(@sha256:[0-9a-fA-F]+)?\$"

fail=0; checked=0; interpolated=0
bad() { echo "image_pins FAIL: $*" >&2; fail=1; }

check_ref() { # <file> <ref>
  local file="$1" ref="$2" repo tag digest last
  checked=$((checked + 1))
  digest=""; case "$ref" in *@sha256:*) digest="${ref#*@}"; ref="${ref%%@*}";; esac
  repo="${ref%%:*}"; tag=""; case "$ref" in *:*) tag="${ref#*:}";; esac
  # a registry port (host:5000/img) contains a colon in the repo part; take the tag from after the last path segment only
  case "$ref" in */*) last="${ref##*/}"; case "$last" in *:*) tag="${last#*:}"; repo="${ref%:*}";; *) tag=""; repo="$ref";; esac;; esac
  if [ -z "$tag" ] && [ -z "$digest" ]; then bad "$file: image '$repo' has no tag (that means :latest — a moving target)"; return; fi
  if [ "$tag" = latest ]; then bad "$file: image '$repo:latest' is floating — pin a version AND a digest"; return; fi
  if ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    bad "$file: image '$repo${tag:+:$tag}' must be pinned by digest (@sha256:<64 hex>), not by tag alone"; return
  fi
}

for f in "${structural[@]}"; do
  [ -f "$f" ] || { bad "no such file: $f"; continue; }
  case "$f" in
    *Dockerfile*)
      stages=""
      while IFS= read -r line; do
        line="${line%$'\r'}"
        if [[ "$line" =~ ^FROM[[:space:]]+(--platform=[^[:space:]]+[[:space:]]+)?([^[:space:]]+)([[:space:]]+[Aa][Ss][[:space:]]+([^[:space:]]+))? ]]; then
          ref="${BASH_REMATCH[2]}"; stage="${BASH_REMATCH[4]:-}"
          if [ "$ref" != scratch ] && ! grep -qiw -- "$ref" <<<"$stages"; then check_ref "$f" "$ref"; fi
          [ -n "$stage" ] && stages="$stages $stage"
        fi
      done <"$f";;
    *)
      while IFS= read -r line; do
        line="${line%$'\r'}"
        if [[ "$line" =~ ^[[:space:]]*image:[[:space:]]*([^[:space:]#]+) ]]; then
          ref="${BASH_REMATCH[1]}"; ref="${ref//\"/}"; ref="${ref//\'/}"
          case "$ref" in *'${'*) interpolated=$((interpolated + 1)); continue;; esac   # interpolated: nothing to read statically
          check_ref "$f" "$ref"
        fi
      done <"$f";;
  esac
done

# The heuristic half: shell tooling that runs an image by name.
script_refs=0
for f in "${scripts[@]:-}"; do
  [ -n "$f" ] || continue
  [ -f "$f" ] || { bad "no such file: $f"; continue; }
  while IFS= read -r line; do
    line="${line%$'\r'}"
    [[ "$line" =~ ^[[:space:]]*# ]] && continue
    [[ "$line" =~ (docker[[:space:]]+(run|pull|create)|(^|[^a-zA-Z0-9_])dock[[:space:]]) ]] || continue
    read -ra toks <<<"$line"
    for tok in "${toks[@]}"; do
      tok="${tok//\"/}"; tok="${tok//\'/}"
      if [[ "$tok" =~ $SCRIPT_REF_RE ]]; then check_ref "$f" "$tok"; script_refs=$((script_refs + 1)); fi
    done
  done <"$f"
done

[ "$checked" -gt 0 ] || bad "found no image reference in ${structural[*]} — the gate would be vacuous"
[ "$fail" -eq 0 ] || exit 1
echo "image_pins OK: $checked image reference(s) in $((${#structural[@]} + ${#scripts[@]})) file(s) ($script_refs in shell tooling); none floating, and every one carries a digest."
[ "$interpolated" -eq 0 ] || echo "  ($interpolated \${...}-interpolated image: reference(s) cannot be read statically and were not checked)"
