#!/usr/bin/env bash
# dev.sh — portable dev==CI entrypoint. `make` is not on the Windows dev host (OD-16), so
# this script is the single source of truth for up/seed/test/reset; the Makefile delegates
# here and CI calls the same targets. Dev and CI therefore run the identical stack.
set -euo pipefail
cd "$(dirname "$0")/.."

# Explicit -f flags (portable): COMPOSE_FILE's separator differs by OS (':' Linux, ';'
# Windows), so we pass both files explicitly instead. Base + harness override (§4.1).
#
# THE RAM-BACKED POSTGRES IS OPT-IN (module 3's #131, ported to this module). tests/harness/compose.test.yml puts
# Postgres' data directory on a tmpfs with fsync=off — faster, and exactly right for a throwaway CI run, and exactly
# wrong for anything that stops a container: a `docker stop postgres` frees the tmpfs, so the next start brings up an
# EMPTY database. This script used to merge that file unconditionally, so an outage drill on the default dev stack
# silently DESTROYED the database and read as a catastrophic recovery bug. The default is now the base file alone: the
# data lives in the image's volume, which survives stop/start and a compose recreate. CI sets DEV_RAM_PG=1 and keeps
# its speed. Anything that invokes compose on this stack should take its invocation from `scripts/dev.sh compose`
# rather than re-deriving it, so a default-mode stack is never re-merged with the RAM override (compose would then see
# a changed postgres spec and recreate it — onto a tmpfs).
COMPOSE="docker compose -f docker-compose.yml"
if [[ "${DEV_RAM_PG:-0}" == "1" ]]; then
  COMPOSE="$COMPOSE -f tests/harness/compose.test.yml"
fi

cmd="${1:-help}"

gen_all_secrets() {
  echo "== gen-certs =="; bash tools/gen-certs.sh "${2:-}"
  echo "== gen-secrets =="; bash tools/gen-secrets.sh
  echo "== provision dev worker + operator admin key + write .env =="
  local prov token hash hash_esc
  ( cd tools/provision-worker && GOTOOLCHAIN=local go build -o bin/provision-worker . ) 2>/dev/null || true
  prov="$(tools/provision-worker/bin/provision-worker new --id dev-worker-1)"
  token="$(echo "$prov" | grep '^WORKER_TOKEN=' | sed 's/^WORKER_TOKEN=//; s/ .*//')"
  hash="$(echo "$prov" | grep '^DEV_WORKER_TOKEN_HASH=' | sed 's/^DEV_WORKER_TOKEN_HASH=//; s/ .*//')"
  # SECOND dev worker (dev-worker-2): its OWN identity + its OWN token, because worker identity is the
  # authorization subject. `docker compose up --scale worker=2` does NOT work and must not be
  # suggested: both replicas inherit one WORKER_ID/WORKER_TOKEN, and workerlink's register path closes the
  # previous connection for an id that re-registers (a correct reconnect rule), so two same-id
  # replicas evict each other in a permanent register/disconnect loop. Multi-worker routing, worker
  # death onto a SURVIVING worker, and reclaim are only testable with two DISTINCT identities.
  # The mTLS cert is deliberately SHARED with worker 1: the cert is transport only, and the Control's
  # authorizeWorker() checks (id, token) against send_managed.boxes and never pins the cert CN to a worker id.
  local prov2 token2 hash2 hash2_esc
  prov2="$(tools/provision-worker/bin/provision-worker new --id dev-worker-2)"
  token2="$(echo "$prov2" | grep '^WORKER_TOKEN=' | sed 's/^WORKER_TOKEN=//; s/ .*//')"
  hash2="$(echo "$prov2" | grep '^DEV_WORKER_TOKEN_HASH=' | sed 's/^DEV_WORKER_TOKEN_HASH=//; s/ .*//')"
  # Operator admin key (Domain B, Phase 2): mint plaintext key + bcrypt hash the same way.
  local admprov admkey admhash admhash_esc
  admprov="$(tools/provision-worker/bin/provision-worker admin-key)"
  admkey="$(echo "$admprov" | grep '^ADMIN_API_KEY=' | sed 's/^ADMIN_API_KEY=//; s/ .*//')"
  admhash="$(echo "$admprov" | grep '^ADMIN_API_KEY_HASH=' | sed 's/^ADMIN_API_KEY_HASH=//; s/ .*//')"
  # Platform service token (Domain A, Phase 3): a DISTINCT credential from the admin key (Rule 6).
  local plprov plkey plhash plhash_esc
  plprov="$(tools/provision-worker/bin/provision-worker admin-key)"
  plkey="$(echo "$plprov" | grep '^ADMIN_API_KEY=' | sed 's/^ADMIN_API_KEY=//; s/ .*//')"
  plhash="$(echo "$plprov" | grep '^ADMIN_API_KEY_HASH=' | sed 's/^ADMIN_API_KEY_HASH=//; s/ .*//')"
  # bcrypt hashes contain '$', which docker-compose .env interpolates — escape as '$$'.
  hash_esc="$(printf '%s' "$hash" | sed 's/\$/$$/g')"
  hash2_esc="$(printf '%s' "$hash2" | sed 's/\$/$$/g')"
  admhash_esc="$(printf '%s' "$admhash" | sed 's/\$/$$/g')"
  plhash_esc="$(printf '%s' "$plhash" | sed 's/\$/$$/g')"
  cat > .env <<EOF
POSTGRES_SUPERPASS=devsuperpass
SEND_MANAGED_MODULE_PASSWORD=devmodulepass
DEV_WORKER_ID=dev-worker-1
DEV_WORKER_UUID=11111111-1111-1111-1111-111111111111
DEV_WORKER_TOKEN=$token
DEV_WORKER_TOKEN_HASH=$hash_esc
DEV_WORKER2_ID=dev-worker-2
DEV_WORKER2_UUID=22222222-2222-2222-2222-222222222222
DEV_WORKER2_TOKEN=$token2
DEV_WORKER2_TOKEN_HASH=$hash2_esc
ADMIN_API_KEY=$admkey
ADMIN_API_KEY_HASHES=$admhash_esc
CREDENTIAL_KEY_ID=cred-dek-v1
PLATFORM_TOKEN=$plkey
PLATFORM_TOKEN_HASHES=$plhash_esc
EOF
  echo ".env written (gitignored). TWO worker tokens/hashes + operator admin key + platform service token provisioned."
  # Hand each container-facing key (mode 600) to the uid its container runs as, and PROVE each can read it. On a real
  # Linux host a 600 key owned by the CI user is unreadable by the Control (uid 10001) and the workers (uid 10002);
  # Docker Desktop's bind mounts hide that, which is how it stayed invisible until the first CI run. See the script.
  echo "== own-secrets =="; bash tools/own-secrets.sh
}

# seed_rows — the system user + BOTH dev worker rows. Called by `seed` and, crucially, by `reset`:
# `down -v` drops the volume that HELD these rows, so a reset without a re-seed leaves every worker
# unable to register (`register rejected: unauthenticated`) until it burns its retry budget and
# exits 1. `up --wait` reports the worker "healthy" moments before it dies, so the failure looks like
# a green start. Found walking Docs/coldmail-send-managed-local-test-guide.md Stage 0 as an operator.
seed_rows() {
  # Read the values by grep (NOT `source .env`: bash would expand the '$' in the bcrypt
  # hash). Un-escape the compose '$$' back to '$' for the real hash the Control compares.
  [[ -f .env ]] || { echo "seed: .env missing (run scripts/dev.sh certs first)"; exit 1; }
  local WORKER_UUID WORKER_HASH_UNESC WORKER2_UUID WORKER2_HASH_UNESC
  WORKER_UUID="$(grep -E '^DEV_WORKER_UUID=' .env | cut -d= -f2-)"
  WORKER_UUID="${WORKER_UUID:-11111111-1111-1111-1111-111111111111}"
  WORKER_HASH_UNESC="$(grep -E '^DEV_WORKER_TOKEN_HASH=' .env | cut -d= -f2- | sed 's/\$\$/$/g')"
  # Worker 2 (multi-worker routing / worker-death / reclaim testing). Seeded only if `certs` minted
  # it, so an .env written before worker 2 existed still seeds worker 1 cleanly.
  WORKER2_UUID="$(grep -E '^DEV_WORKER2_UUID=' .env | cut -d= -f2- || true)"
  WORKER2_HASH_UNESC="$(grep -E '^DEV_WORKER2_TOKEN_HASH=' .env | cut -d= -f2- | sed 's/\$\$/$/g' || true)"
  $COMPOSE exec -T postgres psql -U postgres -d platform -v ON_ERROR_STOP=1 -q \
    -c "INSERT INTO public.users (id, email, is_active) VALUES ('00000000-0000-0000-0000-000000000000','system@sender.local', true) ON CONFLICT (id) DO NOTHING" \
    -c "INSERT INTO send_managed.boxes (id, user_id, token_hash, status) VALUES ('${WORKER_UUID}','00000000-0000-0000-0000-000000000000','${WORKER_HASH_UNESC}','active') ON CONFLICT (id) DO UPDATE SET token_hash=EXCLUDED.token_hash, status='active'" \
    && echo "seed: system user + dev worker row (${WORKER_UUID}) seeded; worker can register on the send stream."
  if [[ -n "$WORKER2_UUID" && -n "$WORKER2_HASH_UNESC" ]]; then
    $COMPOSE exec -T postgres psql -U postgres -d platform -v ON_ERROR_STOP=1 -q \
      -c "INSERT INTO send_managed.boxes (id, user_id, token_hash, status) VALUES ('${WORKER2_UUID}','00000000-0000-0000-0000-000000000000','${WORKER2_HASH_UNESC}','active') ON CONFLICT (id) DO UPDATE SET token_hash=EXCLUDED.token_hash, status='active'" \
      && echo "seed: second dev worker row (${WORKER2_UUID}) seeded; run 'scripts/dev.sh up2' to start it."
  fi
}

case "$cmd" in
  certs)   gen_all_secrets "${2:-}";;
  build)   $COMPOSE build;;
  up)      $COMPOSE up -d --wait;;
  up2)     # Bring up the SECOND worker (not in the default `up` set — it is a profile service, so
           # single-worker runs and the harness are unaffected until you ask for it).
           $COMPOSE --profile multiworker up -d --wait worker2;;
  seed)    # Phase 3: seed the system user + the dev worker rows so a worker can register on the
           # send stream (token=authority, verified vs send_managed.boxes.token_hash). Account/send
           # fixtures are seeded programmatically by the behavioral harness (it holds the encrypt
           # helper + AAD), not here — this only makes the live stack's workers connectable.
           seed_rows;;
  test)    COMPOSE_INVOCATION="$COMPOSE" bash tests/harness/run.sh "${@:2}";;
           # --profile multiworker on every DOWN: without it, `down` ignores profile services and worker2
           # SURVIVES the teardown, so a "clean slate" silently keeps a worker from the previous run.
  down)    $COMPOSE --profile multiworker down -v;;
  reset)   # down -v drops the volume, so the worker rows MUST be re-seeded before any worker can register.
           # Order is load-bearing: postgres (so we can connect) -> migrate (send_managed.boxes must EXIST
           # before we insert into it) -> seed -> the rest. Seeding before migrate fails; seeding
           # after `up` is too late, because the worker has already burned its retry budget and exited.
           $COMPOSE --profile multiworker down -v
           $COMPOSE up -d --wait postgres
           $COMPOSE up migrate
           seed_rows
           $COMPOSE up -d --wait;;
  logs)    $COMPOSE logs -f control worker;;
  ps)      $COMPOSE ps -a;;
  compose) echo "$COMPOSE";;   # the one place the invocation is derived; scripts that run compose call this
  help|*)  cat <<EOF
scripts/dev.sh — dev==CI entrypoint (base compose; RAM-backed Postgres only with DEV_RAM_PG=1)

  certs [--force]  generate dev mTLS certs + KMS key + operator key + provision worker -> .env
  build            build all service images
  up               bring up the stack (blocks until healthchecks pass, or fails loud)
  test [args]      run the real-infra harness against the live stack
  reset            down -v then up (clean slate)
  down             tear down (-v drops volumes)
  logs / ps        follow logs / show status
  compose          print the docker compose invocation this script uses

Environment:
  DEV_RAM_PG=1     merge tests/harness/compose.test.yml: Postgres on a tmpfs with fsync=off.
                   Faster, and what CI sets — but a `docker stop postgres` then DESTROYS the
                   database. Leave it unset for anything that stops/starts containers.
EOF
  ;;
esac
