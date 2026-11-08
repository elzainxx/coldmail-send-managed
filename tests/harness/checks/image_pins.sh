#!/usr/bin/env bash
# image_pins — BLOCKING gate: no floating image reference, and the images this harness depends on are pinned by DIGEST.
#
#   image_pins.sh [file...]    default: every docker-compose*.yml and tests/harness/compose*.yml, plus services/*/Dockerfile
#
# THE DEFECT CLASS. A floating tag is a moving target. A CI runner pulls cold and gets whatever the registry serves that day;
# a developer's machine keeps an old cached copy for months and never notices. Measured here (2026-11-08): the CI replica held
# CoreDNS 1.14.6 while the runner pulled 1.15.0, and the Ruby base image that "worked for months" could not be built from
# scratch at all (ADR-0018 in module 1). `:latest` — or no tag, which means the same thing — is the worst case: not even a
# version to read off. This gate keeps it from coming back.
#
# WHAT IT ASSERTS
#   1. no `image:` / `FROM` reference is `:latest` or untagged (a `FROM <earlier stage>` and `FROM scratch` are exempt);
#   2. these images carry `@sha256:<64 hex>`: axllent/mailpit, coredns/coredns, python (the DNS black-hole fixture).
# It deliberately does NOT demand a digest on every image (postgres, the Go builder, node, alpine): pinning a builder freezes its
# security patches until someone bumps it, which is a decision for the owner, not a side effect of a hygiene gate. They are
# reported, not failed, so the gap is visible instead of silent.
# Paired control: image_pins_selftest.sh feeds it each violation and checks the exit code AND the message.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."

files=("$@")
if [ ${#files[@]} -eq 0 ]; then
  while IFS= read -r f; do files+=("$f"); done < <(ls docker-compose*.yml tests/harness/compose*.yml services/*/Dockerfile 2>/dev/null || true)
fi
[ ${#files[@]} -gt 0 ] || { echo "image_pins FAIL: no compose/Dockerfile found to check — the gate would be vacuous" >&2; exit 1; }

PINNED_REPOS='^(axllent/mailpit|coredns/coredns|python)$'
fail=0; checked=0; floating_unpinned=()
bad() { echo "image_pins FAIL: $*" >&2; fail=1; }

check_ref() { # <file> <ref>
  local file="$1" ref="$2" repo tag digest
  checked=$((checked + 1))
  digest=""; case "$ref" in *@sha256:*) digest="${ref#*@}"; ref="${ref%%@*}";; esac
  repo="${ref%%:*}"; tag=""; case "$ref" in *:*) tag="${ref#*:}";; esac
  # a registry port (host:5000/img) contains a colon in the repo part; take the tag from after the last path segment only
  case "$ref" in */*) last="${ref##*/}"; case "$last" in *:*) tag="${last#*:}"; repo="${ref%:*}";; *) tag=""; repo="$ref";; esac;; esac
  if [ -z "$tag" ] && [ -z "$digest" ]; then bad "$file: image '$repo' has no tag (that means :latest — a moving target)"; return; fi
  if [ "$tag" = latest ]; then bad "$file: image '$repo:latest' is floating — pin a version AND a digest"; return; fi
  if [[ "$repo" =~ $PINNED_REPOS ]] && ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    bad "$file: image '$repo${tag:+:$tag}' must be pinned by digest (@sha256:<64 hex>), not by tag alone"; return
  fi
  [ -n "$digest" ] || floating_unpinned+=("$repo:$tag")
}

for f in "${files[@]}"; do
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
          case "$ref" in *'${'*) continue;; esac   # interpolated: nothing to read statically
          check_ref "$f" "$ref"
        fi
      done <"$f";;
  esac
done

[ "$checked" -gt 0 ] || bad "found no image reference in ${files[*]} — the gate would be vacuous"
[ "$fail" -eq 0 ] || exit 1
unp="$(printf '%s\n' "${floating_unpinned[@]:-}" | sed '/^$/d' | sort -u | tr '\n' ' ')"
echo "image_pins OK: $checked image reference(s) in ${#files[@]} file(s); none floating, and the pinned set carries digests."
[ -z "$unp" ] || echo "  (tagged but NOT digest-pinned, by decision, not by omission: $unp)"
