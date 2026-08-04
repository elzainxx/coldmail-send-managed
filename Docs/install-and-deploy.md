# Install and deploy

Standing this module up on real servers, from a bare VPS to a verified first send — then keeping it
running through worker changes, redeploys, and rollbacks.

**Target:** a generic Ubuntu-style VPS. Nothing here is cloud-provider-specific. Where a managed
service is a reasonable substitute (Postgres, KMS), that is called out.

**This document does not restate config keys or endpoint shapes.** Every config key's meaning and
default is in [`config-reference.md`](./config-reference.md); every request/response shape is in
[`api-contract.md`](./api-contract.md). When a step here says "set X", the reference is where you
look up what X means.

**Before a first LIVE deployment**, also work through [`go-live-checklist.md`](./go-live-checklist.md)
— it covers the platform-coordination sign-offs (KMS/IAM separation, advisory-lock namespace,
platform table shapes) that this guide assumes are settled.

---

## Part 1 — Prerequisites

### What you need before starting

| Requirement | Notes |
|---|---|
| **A VPS for the Control** | 2 vCPU / 2 GB RAM is comfortable for V1 volumes. The Control is a single process and is not memory-hungry; see [`capacity-planning.md`](./capacity-planning.md) before sizing for scale. |
| **One or more VPSes for workers** | 1 vCPU / 512 MB each is sufficient — a worker is a thin SMTP relay. Start with one. |
| **A private network between them** | Essential. See Part 4 — the worker↔Control channel must not traverse the public internet, and no worker port is ever publicly reachable. |
| **PostgreSQL 16+** | Managed (Supabase, RDS, etc.) or self-hosted. The module owns the `send_managed` schema; the platform owns `public`. |
| **A 32-byte master encryption key** | Wraps SMTP credentials. In production this should be a real KMS key. See Part 3 and the key-backup section of [`disaster-recovery.md`](./disaster-recovery.md) **before you create it** — losing this key is unrecoverable. |
| **SMTP account credentials** | At least one working account (host, port, username, password). |
| **`public.users` and `public.job_items`** | Platform-owned tables the module reads. They must exist before migrating; the preflight fails loudly if they do not. |

### Software on each host

```bash
# Both Control and worker hosts:
sudo apt-get update
sudo apt-get install -y ca-certificates curl gnupg git jq
# jq is not optional in practice: the admin/health API returns compact single-line JSON, and every
# diagnostic in operator-runbook.md pipes through it. Install it before you need it at 3am.

# Docker (the supported deployment shape — same images CI tests):
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"   # log out and back in for this to take effect

# Go toolchain — CONTROL HOST ONLY, and it is not optional:
sudo apt-get install -y golang-go
```

Verify: `docker --version && docker compose version && go version`.

> **Why Go is a prerequisite even though everything runs in containers.** `tools/provision-worker`
> ships no Dockerfile, and it is what mints the operator admin key, the platform service token, and
> every worker token. You need it at Step 6 — *before* the Control can be configured at all — so
> `go: command not found` there blocks the entire install. It is a build-once tool: run it, keep the
> binary, and Go is not needed again. Only the Control host needs it; worker hosts do not.

> **Running without Docker** is possible — the Control is a static Go binary, the worker is Node.js —
> but the containers are what CI exercises, so they are what this guide covers.

### Decide your topology first

The smallest sane production shape:

```
              ┌───────────────────────────────┐
  platform ──►│  CONTROL VPS                    │
  (Domain A)  │   :8080  HTTP  API + health    │◄── operator (Domain B)
              │   :8443  mTLS  register/hb     │
              │   :8444  mTLS  send stream     │
              └───────┬───────────────────────┘
                      │  private network only
        ┌─────────────┼─────────────┐
        ▼             ▼             ▼
   ┌─────────┐  ┌─────────┐  ┌─────────┐
   │ WORKER VPS │  │ WORKER VPS │  │ WORKER VPS │   workers open OUTBOUND connections only.
   │  (no    │  │  (no    │  │  (no    │   No inbound port. Not publicly routable.
   │ inbound)│  │ inbound)│  │ inbound)│
   └────┬────┘  └────┬────┘  └────┬────┘
        └────────────┴────────────┘
                     ▼
              SMTP providers (:587 / :465)

        Postgres: reachable from the CONTROL ONLY. Workers have no DB access, ever.
```

---

## Part 2 — First-time setup, in order

Each step depends on the one before it. Do not skip ahead; the preflight and healthchecks exist to
catch out-of-order setup, but some ordering (KMS key before any credential is stored) cannot be
recovered from afterward.

### Step 1 — Get the code onto the Control host

```bash
git clone <your-repo-url> coldmail-send-managed
cd coldmail-send-managed
```

### Step 2 — Confirm the database connection

You need two distinct Postgres roles. This separation is load-bearing, not a style preference:

| Role | Used by | Capability |
|---|---|---|
| **migration admin** | `tools/send-managed-migrate` only | admin/owner-capable: must `CREATE ROLE`, `CREATE SCHEMA`, and assign ownership to `send_managed_owner` |
| **`send_managed_app`** | the Control, at runtime | login-only, non-superuser, holds only enumerated grants. Never runs DDL. |

The migration admin credential comes from your secret store and is used only during migration runs.
Confirm which real role it is before going live (go-live checklist item 3).

Verify the platform tables exist and you can reach them:

```bash
psql "$MIGRATE_DATABASE_URL" -c "\d public.users" -c "\d public.job_items"
```

If either is missing, stop — deploy the platform schema first. The preflight will refuse to migrate
without them.

> **Also confirm the column shapes**, not just existence. The preflight deliberately checks
> existence only (a column-level check would false-fail every time the platform adds an unrelated
> column). The shapes the module assumes are in go-live checklist item 2. A mismatch found after
> migrating is much more expensive than one found now.

### Step 3 — Create the master encryption key

This key wraps every SMTP credential. **Read the key-backup section of
[`disaster-recovery.md`](./disaster-recovery.md) before you create it** — if it is lost, every
stored credential is permanently unrecoverable and must be re-entered by hand.

**Production (recommended): a real KMS key.** Create a key whose IAM policy grants `Decrypt` to the
Control's runtime identity **and to nothing else** — not to operators, not to the platform, not to
your CI. That separation is what makes "the platform cannot read credentials" true, and it is not
checkable from code (go-live checklist item 4).

**Self-hosted / file-backed key** (what the dev stack uses, viable for a small deployment where you
control the host):

```bash
sudo mkdir -p /etc/send_managed/kms
sudo openssl rand -out /etc/send_managed/kms/master.key 32
sudo chmod 400 /etc/send_managed/kms/master.key
sudo chown <control-user>:<control-user> /etc/send_managed/kms/master.key
```

Then **back it up off-host immediately**, before storing any credential. See
[`disaster-recovery.md`](./disaster-recovery.md).

Set `CREDENTIAL_KEY_ID` to a version label for this key (e.g. `cred-dek-v1`). It is stamped into
every encrypted row and is what makes rotation possible later — see the rotation procedure in
[`disaster-recovery.md`](./disaster-recovery.md).

### Step 4 — Generate mTLS material

The worker↔Control channel is mutual TLS in both directions. You need a CA, a server leaf for the Control,
and a client leaf per worker.

```bash
bash tools/gen-certs.sh
# writes secrets/dev/certs/: ca.crt ca.key control.crt control.key worker.crt worker.key sink.crt sink.key
#
# NOTE: if secrets/dev/certs/ca.crt already exists the script prints a message and EXITS 0 without
# regenerating anything. That reads like success. Pass --force to actually regenerate — and be aware
# that a new CA invalidates every existing worker leaf, so every worker needs its new leaf before it can
# reconnect.
```

> **`tools/gen-certs.sh` is explicitly DEV-ONLY** — it says so in its own header. It self-signs a
> CA that never leaves the machine and issues 10-year leaves with no revocation path. For
> production, issue the leaves from a CA you actually manage (your internal PKI, `step-ca`, Vault,
> or your cloud's private CA). A managed cert PKI with rotation and revocation is deferred work,
> not built — see [`future-work.md`](./future-work.md).

Whatever issues them, the requirements are fixed:

- The Control needs its server cert + key + the CA cert.
- Each worker needs its own client cert + key + the CA cert.
- The Control's cert must carry a SAN matching the hostname workers connect to, and the worker must be
  configured with that same name (`CONTROL_SERVER_NAME`).
- **A worker host never receives `ca.key`, `control.key`, or the KMS key.** Distribute only that worker's
  own leaf. This is enforced in the compose mounts and worth preserving in any other deployment
  shape.

Remember: **the cert is transport, not authority.** A valid cert alone gets a worker nothing — the
registration token is what authorizes it (Step 7).

### Step 5 — Run the migrations

Migrations are centrally owned, contiguous, self-recording, and applied in ascending order. The
runner takes an advisory lock so two migrators cannot race.

**Do not run the migrator through `docker-compose.yml`.** That file is the *dev* stack, and two
things about it make it the wrong tool against a real database — both verified, not theoretical:

1. Its `migrate` service **hardcodes** `MIGRATE_DATABASE_URL` to the in-compose dev Postgres
   (`docker-compose.yml`, the `migrate:` `environment:` block). A `MIGRATE_DATABASE_URL` you export
   in your shell is silently **overridden** — you would point the runner at a database that does not
   exist on a production host, or worse, at the wrong one.
2. Compose interpolates the **entire file** before running any service, so a `docker compose run
   migrate` on a fresh clone with no `.env` aborts on `DEV_WORKER_TOKEN_HASH is missing a value` — a
   variable the migrator does not use. (Reproduce: `docker compose config` in a fresh clone.)

Build the runner image once and run it directly. This is the whole of Step 5:

```bash
# Build the migration-runner image (context is the repo root — the Dockerfile copies the module):
docker build -f tools/send-managed-migrate/Dockerfile -t send-managed-migrate:local .

MIGRATE_DATABASE_URL='postgres://<admin-role>:<pw>@<host>:5432/<db>'
SEND_MANAGED_MODULE_PASSWORD='<a strong password for the app role>'

# Inspect first — read-only, shows applied vs pending, changes nothing:
docker run --rm \
  -e MIGRATE_DATABASE_URL="$MIGRATE_DATABASE_URL" \
  -e MIGRATIONS_DIR=/db/migrations \
  -e PREFLIGHT_SQL=/db/preflight/check_platform_tables.sql \
  -v "$PWD/db:/db:ro" \
  send-managed-migrate:local status

# Apply the chain, then set the app role's password out-of-band:
docker run --rm \
  -e MIGRATE_DATABASE_URL="$MIGRATE_DATABASE_URL" \
  -e SEND_MANAGED_MODULE_PASSWORD="$SEND_MANAGED_MODULE_PASSWORD" \
  -e MIGRATIONS_DIR=/db/migrations \
  -e PREFLIGHT_SQL=/db/preflight/check_platform_tables.sql \
  -v "$PWD/db:/db:ro" \
  send-managed-migrate:local up-and-set-password
```

`--network` may be needed depending on where your database lives: add `--network host` for a
Postgres on the same host, or `--network <your-net>` to reach one on a Docker network. A managed
database reachable over the public internet needs neither.

> **If you nonetheless use the dev compose file locally, `--no-deps` is not optional and omitting it
> is destructive.** The `migrate` service `depends_on` the dev `postgres`, so without `--no-deps`,
> `docker compose run` will start — and can **recreate** — that container, silently destroying the
> data volume. This was found by running the command as first written while drafting this guide: it
> recreated the postgres container and wiped the dev database.
>
> Either way: `status` is read-only, so run it first, always.

What that does: runs the preflight (platform tables exist?), applies every pending
`send_managed_NNNN_*.up.sql` in order, records each in `send_managed.schema_migrations`, creates the
non-login `send_managed_owner` (schema owner, so `FORCE ROW LEVEL SECURITY` actually enforces) and the
login-only `send_managed_app` app role, and seeds every config key with its documented default
(the authoritative key set is [`config-reference.md`](./config-reference.md) — the one file the
`config_drift` gate checks; a count restated here would be a number nothing verifies).

The role password is set via `set-app-password` rather than in SQL specifically so it never lands
in a migration file or the DB's statement logs.

**Migrations are forward-only in production.** `down` refuses to run unless you explicitly set
`SEND_MANAGED_MIGRATE_ALLOW_DOWN=true`. Do not casually override that — see the rollback section (Part 5).

Confirm: re-run the `status` command above — every version applied, none pending.

### Step 6 — Start the Control

Set the Control's environment (Part 4 has a complete systemd unit; the full variable list is in
`.env.example`). The essentials:

```bash
DATABASE_URL=postgres://send_managed_app:<pw>@<host>:5432/<db>   # the app role, NOT the admin role
KMS_DEV_KEY_FILE=/etc/send_managed/kms/master.key                   # or your KMS wiring
CREDENTIAL_KEY_ID=cred-dek-v1
MTLS_SERVER_CERT=/etc/send_managed/certs/control.crt
MTLS_SERVER_KEY=/etc/send_managed/certs/control.key
MTLS_CA_CERT=/etc/send_managed/certs/ca.crt
CONTROL_HEALTH_ADDR=:8080
CONTROL_WORKERLINK_ADDR=:8443
CONTROL_WORKERLINK_STREAM_ADDR=:8444
ADMIN_API_KEY_HASHES=<bcrypt hash(es), comma-separated>        # Domain B; empty = admin API OFF
PLATFORM_TOKEN_HASHES=<bcrypt hash(es), comma-separated>       # Domain A; empty = platform API OFF
DEV_WORKER_TOKEN_HASH=<bcrypt hash>                               # legacy register/heartbeat route
```

**Both API surfaces fail closed**: leave a hash list empty and that surface stays off entirely.
That is deliberate — a misconfigured deploy serves nothing rather than serving unauthenticated.

The two auth domains never share a credential. Domain A is the platform's service token
(`quote`/`run`/`report`/`describe`); Domain B is the operator's admin key (`/v1/admin/*`). Mint each
separately:

```bash
cd tools/provision-worker && go build -o bin/provision-worker .

./bin/provision-worker admin-key    # run 1 -> the OPERATOR key   (Domain B)
./bin/provision-worker admin-key    # run 2 -> the PLATFORM token (Domain A) — a distinct credential
```

**Both runs print lines named `ADMIN_API_KEY=` and `ADMIN_API_KEY_HASH=`.** The tool has one
key-minting subcommand and does not know which domain you are minting for, so the variable names it
prints are only right for the first run. Map them yourself:

| Run | `ADMIN_API_KEY=` (the plaintext) goes to | `ADMIN_API_KEY_HASH=` goes into |
|---|---|---|
| 1 | your secret store, as the **operator admin key** — the `Authorization: Bearer` value for `/v1/admin/*` | `ADMIN_API_KEY_HASHES` |
| 2 | your secret store, as the **platform service token** — the bearer value the platform uses for `quote`/`run`/`report`/`describe` | `PLATFORM_TOKEN_HASHES` |

Pasting run 2's output verbatim into your env file overwrites `ADMIN_API_KEY_HASHES` and leaves
`PLATFORM_TOKEN_HASHES` empty — which fails closed, so Domain A silently stays off and every
platform call returns `401`. (`scripts/dev.sh` greps the two runs apart for exactly this reason.)

The Control stores only hashes and compares in constant time. Record the plaintexts in your secret
store; they are not recoverable from the hash.

Start it, then verify:

```bash
curl -fsS http://localhost:8080/readyz     # expect db:true, singleton_lock:true
```

`singleton_lock:true` means this Control holds the advisory lock and is the active one. **Exactly one
Control is ever active** — a second instance waits for the lock (bounded by
`brain_singleton_acquire_timeout_sec`) and then exits. Never run two, and never add a second
dispatch path.

That guarantee covers **two** events, not just startup. A running Control can also *lose* the lock (the
lock is session-level, so a Postgres restart or a killed backend drops it); when that happens it
**pauses** dispatch/reclaim/retention rather than continuing without the lock, and re-attempts
acquisition every `brain_singleton_reacquire_interval_sec` (default 5s) — see
[`operator-runbook.md`](./operator-runbook.md#readyz-says-singleton_lockfalse-on-a-control-that-was-ready-mid-life-lock-loss).
So `singleton_lock:false` on a live process means "running but dispatching nothing," which is a normal
transient state during a DB restart and not by itself a reason to intervene.

### Step 7 — Provision the first worker

Worker lifecycle is an out-of-band operator procedure; there is deliberately no `/v1/admin/workers` REST
surface in V1 (see [`future-work.md`](./future-work.md)).

**On the Control host** (where the tool and DB access live):

```bash
cd tools/provision-worker && go build -o bin/provision-worker .

# 1. Mint an id + token + bcrypt hash:
./bin/provision-worker new --id <worker-id>
#    -> prints WORKER_TOKEN=... and DEV_WORKER_TOKEN_HASH=...
```

**Record `WORKER_TOKEN` now.** It is stored nowhere — only its bcrypt hash goes to the database — and
cannot be recovered after this step. It goes into that worker's own secret config.

```bash
# 2. Emit the SQL that seeds the send_managed.boxes row:
./bin/provision-worker seed --id <worker-uuid> --token-hash '<hash>'
```

**Read the emitted SQL before you run it — for a worker that does not exist yet, it is the wrong
statement.** `provision-worker seed` emits an `UPDATE`, which is correct for *rotating* an existing
worker's token but matches **zero rows** on a first install, reports success, and leaves you with a worker
that is refused at registration for a reason nothing logs as an error. For the **first** row for a
given worker id, run an `INSERT` instead, as the migration-admin role:

```bash
psql "$MIGRATE_DATABASE_URL" -v ON_ERROR_STOP=1 -c "
  INSERT INTO send_managed.boxes (id, user_id, token_hash, status)
  VALUES ('<worker-uuid>', '<owning-user-uuid>', '<hash>', 'active')
  ON CONFLICT (id) DO UPDATE SET token_hash = EXCLUDED.token_hash, status = 'active';"
```

Two columns the tool's output does not mention and you cannot omit:

- **`user_id` is `NOT NULL` with an FK to `public.users(id)`** (`send_managed_0006`). It must be a real
  platform user id or the `INSERT` fails on the FK. Use the platform user that owns this worker's
  sending — for a single-tenant install, the same user id you use elsewhere.
- **`status` must be `'active'`.** A worker row with `status='disabled'` is refused at registration
  exactly like a revoked one.

Confirm the row landed before starting the worker — this is the check that turns a silent failure into
an obvious one:

```bash
psql "$MIGRATE_DATABASE_URL" -tAc \
  "SELECT id, status, token_hash IS NOT NULL FROM send_managed.boxes WHERE id='<worker-uuid>';"
# expect exactly one row:  <worker-uuid>|active|t
```

Note `send_managed.boxes.id` is a **UUID**. The token is the authority (verified against `token_hash`);
the cert CN is transport only.

For **rotating** an already-seeded worker's token later, `provision-worker seed`'s `UPDATE` is exactly
right — see [Adding a worker](#adding-a-worker) and [Removing a worker](#removing-a-worker) in Part 5. The
distinction is first-insert vs rotate, not a defect in the tool.

### Step 8 — Start the worker

On the worker host, with its own leaf certs and its token:

```bash
CONTROL_HOST=<control-private-hostname>
CONTROL_STREAM_PORT=8444          # the worker connects on the send stream only
CONTROL_SERVER_NAME=<the name in the Control cert's SAN>
WORKER_ID=<the worker uuid you seeded>
WORKER_TOKEN=<the plaintext token from step 7>
MTLS_CLIENT_CERT=/etc/send_managed/certs/worker.crt
MTLS_CLIENT_KEY=/etc/send_managed/certs/worker.key
MTLS_CA_CERT=/etc/send_managed/certs/ca.crt
```

**There is no `DATABASE_URL` and no KMS variable here, and there never will be.** The worker ships no
DB driver and no KMS SDK — its `package.json` dependency set is empty. That is structural, not
configuration.

Confirm registration in the Control's log: `worker registered id="<id>"`.

If the worker exits instead: it has a **bounded** register-retry budget and deliberately exits for the
supervisor to restart rather than hammering a Control that is not ready. A worker that exits during
first-time setup usually means the Control was not up yet, or the `send_managed.boxes` row was not seeded.
See the runbook.

### Step 9 — Create a pool, add an account, set the default pool

Nothing sends until a pool exists, has an eligible account, and is designated the default.
Request/response shapes are in [`api-contract.md`](./api-contract.md).

**Two secrets get typed in this step — keep them out of your shell history and out of `ps`.** Both
the admin key and the mailbox password below would otherwise land in `~/.bash_history`, and an inline
`curl -d '{"smtp_secret":…}'` is visible in the process list to every local user for the duration of
the request. The pattern used here: `read -rs` instead of an assignment, `--data @file` instead of an
inline body, and a `shred` at the end. (This is the same discipline as `provision-worker`'s `--pass-env`,
which exists so a password never becomes a command-line argument.)

```bash
ADMIN=http://localhost:8080
read -rs -p 'ADMIN_API_KEY: ' KEY; echo    # not an assignment: keeps it out of history

# 1. A shared pool:
curl -fsS -X POST $ADMIN/v1/admin/pools -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' -d '{"kind":"shared","name":"primary"}'

# 2. An SMTP account in it (the credential is encrypted on arrival; the Control never returns it).
#    The admin surface decodes STRICTLY — a misspelled field name is a 400, not an ignored key.
#    Full field table incl. the optional ones: api-contract.md.
#    Written to a file first so the password never appears in `ps` or in shell history.
umask 077
cat > /tmp/acct.json <<'JSON'
{
  "pool_id":       "<pool-uuid from step 1>",
  "smtp_host":     "smtp.example.com",
  "smtp_port":     587,
  "smtp_username": "<mailbox username>",
  "smtp_secret":   "<mailbox password>",
  "from_address":  "you@example.com",
  "from_name":     "Your Name"
}
JSON
$EDITOR /tmp/acct.json    # fill in the real values
curl -fsS -X POST $ADMIN/v1/admin/accounts -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' --data @/tmp/acct.json
shred -u /tmp/acct.json

# 3. Designate the default shared pool — base sending does NOT work until this is set:
curl -fsS -X PUT $ADMIN/v1/admin/config/default_shared_pool_id -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' -d '{"value":"<pool-uuid>"}'

unset KEY   # when you are done with this shell session
```

**Keep `smtp_host` a public hostname.** A private-range or link-local address (`10.*`, `192.168.*`,
`172.16–31.*`, `127.*`, `169.254.*`, `localhost`) is accepted by the API, but the worker **skips
certificate verification** for those — a test affordance for the local sink — so the credential would
cross an unverified TLS connection. See [`security-posture.md`](./security-posture.md) §3.13.

> **On the local dev stack, the account above points at the bundled sink: `"smtp_host": "smtp-sink"`,
> `"smtp_port": 1025`** (Mailpit, reachable from the worker over the compose network; its web UI is
> <http://localhost:8025>). `smtp_username`/`smtp_secret` can be anything — Mailpit accepts any
> credential, but only over STARTTLS, so the worker's STARTTLS-before-AUTH path is genuinely exercised.
> This is stated here because it is the one value in Step 9 a local reader cannot derive from this
> doc: every other field has a self-explanatory placeholder, but `smtp.example.com` does not resolve
> and there is no hint of what to use instead. Found by walking this doc verbatim on a clean slate
> (2026-07-29) — it was the first hard stop. The public-hostname warning above still applies in
> production; `smtp-sink` is exactly the "test affordance" it refers to.

`default_shared_pool_id` seeds `NULL` and stays `NULL` until you set it. Until then, base-campaign
jobs stay cleanly `queued` — they are not lost and not failed, but nothing sends. This is the single
most common "why is nothing happening" cause; the runbook's flagship tree starts there.

### Step 10 — Verify a real send end to end

```bash
curl -fsS $ADMIN/v1/admin/health -H "Authorization: Bearer $KEY" \
  | jq '{dispatch, pools, workers, accounts}'
```

Expect: your pool present with headroom that is **not** `exhausted`, `dispatch_paused:false`, and at
least one connected worker. What each field means and what warrants action is in the runbook.

Then run one real campaign through `quote` → `run` → `report` (Domain A, **platform service token**,
not the admin key) with a single recipient you control, and confirm the job reaches `sent`. The three
bodies, because this is where a first-time reader stops (full field tables:
[`api-contract.md`](./api-contract.md)):

```bash
PLAT=<platform service token>          # Domain A. NOT the admin key from Step 9.
OWNER=<the owning tenant's uuid>       # a public.users row.  NOT `UID=` — see below.
ITEM=<a public.job_items uuid owned by $OWNER>

# quote — recipient_count ONLY. It is pure, side-effect-free, and takes no tenant:
curl -fsS -X POST $ADMIN/v1/quote -H "Authorization: Bearer $PLAT" \
  -H 'Content-Type: application/json' -d '{"recipient_count":1}'

# run — job_item_id is SINGULAR, and there is no campaign_id field (that lives on the job_items row):
curl -fsS -X POST $ADMIN/v1/run -H "Authorization: Bearer $PLAT" \
  -H 'Content-Type: application/json' -d "{\"user_id\":\"$OWNER\",\"job_item_id\":\"$ITEM\"}"
# -> 202 {"job_id":"...","state":"queued"}   (async kickoff; it does NOT block on the send)

# report — QUERY parameters, not a path segment. /v1/report/<id> is a bare router 404:
curl -fsS "$ADMIN/v1/report?job_id=<job_id>&user_id=$OWNER" -H "Authorization: Bearer $PLAT"
```

Expect `report` to reach `{"state":"sent", "provider_message_id":"...", "completed_at":"..."}`. A
`provider_message_id` is the proof: the worker reported a real accepted send, and the Control never invents
one.

> **The three shapes above are each a mistake made while walking this doc verbatim** on a clean slate
> (2026-07-29), when this step said only "run one real campaign through `quote` → `run` → `report`"
> with no bodies and — unlike Step 9 — no link to the contract. `quote` rejected `user_id` as an
> **unknown field** (the admin surface and the platform surface both decode strictly), `run` was
> guessed as `{campaign_id, job_item_ids[]}`, and `report` was tried as a path segment. All three are
> `400`/`404` with correct, spec-compliant envelopes — the product was right; the doc was silent.

> **`UID=` is a readonly variable in bash** — `UID=<uuid>` fails with `bash: UID: readonly variable`
> and the assignment silently does not happen, so `run` then posts an empty `user_id`. This snippet
> used `UID` until 2026-07-29, when a verbatim re-walk of this step hit exactly that. Hence `OWNER`.
> (`zsh` allows the assignment, which is why this can survive a review: it depends on the reader's
> shell.)

**On the local dev stack, the three values above come from `scripts/dev.sh reset`, not from a secret
store.** It mints both credentials into `.env` and seeds the platform side:

```bash
PLAT=$(grep -E '^PLATFORM_TOKEN=' .env | cut -d= -f2)   # NOT ADMIN_API_KEY= — different domain
OWNER=00000000-0000-0000-0000-000000000000              # the reserved system user dev.sh seeds

# public.job_items is EMPTY after a reset — the platform owns that table and the module only
# reads it, so nothing in this repo creates a row for you. Insert one as the admin role:
ITEM=$(docker compose exec -T postgres psql -U postgres -d platform -tAc \
  "INSERT INTO public.job_items (campaign_id,user_id,recipient_email,rendered_subject,rendered_body,status)
   VALUES (gen_random_uuid(),'$OWNER','you@example.com','Local smoke test','<p>hello</p>','pending')
   RETURNING id" | head -1 | tr -d '\r\n')
```

**Both filters on that capture are load-bearing, and neither is obvious.** `psql -tA` suppresses the
header and the row count but still prints the **command tag** on its own line, so the raw output of a
`RETURNING` insert is two lines: `<uuid>\nINSERT 0 1\n`. `head -1` drops the tag; `tr -d '\r\n'` drops
the newline (and the `\r` Git Bash adds on Windows). Omit either and `$ITEM` becomes
`e5ca20e2-…-2b385e4271a7INSERT 0 1` or keeps a trailing newline — in both cases the JSON body below is
malformed, and `run` answers `400 validation_failed` naming **`job_item_id: required`**, which reads
like a wrong field name and is not one. Confirm with `echo "[$ITEM]"` (expect exactly 36 characters)
before blaming the body. Both variants were hit while re-walking this step on 2026-07-29.

> **The platform-side seeding above is dev-stack only.** In production the platform inserts
> `public.job_items` as part of its own campaign flow and passes you the id; the module never writes
> that table (it holds `SELECT` only — Step 4's grant check). Do not carry this `INSERT` into a
> production runbook.

> **Before pointing this at a real SMTP provider for the first time**, use
> [`smoke-test-real-provider.md`](./smoke-test-real-provider.md). Everything up to this point has
> been exercised against a local SMTP sink; first contact with a real provider surfaces
> provider-specific quirks (auth mechanisms, TLS behavior, rate-limit responses, send_managed identity
> verification) and it touches real sending reputation.

---

## Part 3 — Environment variables by service

Full annotated list: `.env.example`. Config *keys* (runtime dials, changeable without a redeploy)
are a different thing entirely and live in the database — see
[`config-reference.md`](./config-reference.md).

### Control

| Variable | Purpose |
|---|---|
| `DATABASE_URL` | The **`send_managed_app`** app role. Not the migration admin role. |
| `KMS_DEV_KEY_FILE` | Path to the master key file (or your KMS wiring). **Control only.** |
| `CREDENTIAL_KEY_ID` | Write-key version label, stamped into `credential_key_id`. Rotation depends on this. |
| `MTLS_SERVER_CERT` / `MTLS_SERVER_KEY` / `MTLS_CA_CERT` | The Control's mTLS server identity + trust root. |
| `CONTROL_HEALTH_ADDR` | HTTP: health, readiness, Domain A, Domain B. Default `:8080`. |
| `CONTROL_WORKERLINK_ADDR` | mTLS register/heartbeat. Default `:8443`. |
| `CONTROL_WORKERLINK_STREAM_ADDR` | mTLS persistent send stream (carries credentials). Default `:8444`. |
| `CONTROL_SINGLETON_ACQUIRE_TIMEOUT_SEC` | Bounded wait for the singleton lock, then exit. |
| `ADMIN_API_KEY_HASHES` | Domain B operator key hashes, comma-separated. **Empty = admin API off.** |
| `PLATFORM_TOKEN_HASHES` | Domain A platform service token hashes. **Empty = platform API off.** |
| `DEV_WORKER_TOKEN_HASH` | Token hash for the register/heartbeat route. |

**Not in this table on purpose:** `SEND_MANAGED_ALERT_WEBHOOK_URL` appears in `.env.example` but **no code
reads it** — it is inert. The webhook destination comes only from the `alert_webhook_url` config row.
Setting the env var and expecting alerts is a silent no-op; see [`monitoring.md`](./monitoring.md).

### Worker

| Variable | Purpose |
|---|---|
| `CONTROL_HOST`, `CONTROL_STREAM_PORT` | Where to connect (private network). `CONTROL_STREAM_PORT` defaults to `8444`. |
| `CONTROL_SERVER_NAME` | Must match the Control cert's SAN, or TLS verification fails. |
| `WORKER_ID` | The UUID seeded into `send_managed.boxes`. |
| `WORKER_TOKEN` | The plaintext registration token — **the worker's actual authority**. |
| `MTLS_CLIENT_CERT` / `MTLS_CLIENT_KEY` / `MTLS_CA_CERT` | This worker's own leaf + trust root. Never the CA key. |
| `WORKER_HEARTBEAT_INTERVAL_SEC`, `WORKER_SMTP_TIMEOUT_MS` | Liveness cadence; SMTP socket timeout. Both are **bootstrap defaults only** — the Control overrides them from seeded config on `register_ack`. |

A worker connects on the **send stream** (`:8444`) only; it does not dial the register/heartbeat port
itself, so there is no `CONTROL_WORKERLINK_PORT` for the worker to set. `docker-compose.yml` passes one to
the dev worker container and the worker ignores it — it reads only the variables above
(`services/worker/src/index.js`).

**Never set on a worker:** `DATABASE_URL`, any KMS variable, `ca.key`, `control.key`. If a worker host has
any of these, that is a security incident, not a misconfiguration —
[`security-posture.md`](./security-posture.md) explains what it costs.

### Migration runner

`MIGRATE_DATABASE_URL` (admin role), `MIGRATIONS_DIR`, `PREFLIGHT_SQL`, `SEND_MANAGED_MODULE_PASSWORD`,
and optionally `MIGRATE_LOCK_TIMEOUT` / `MIGRATE_STATEMENT_TIMEOUT` (the one documented exception to
config-not-code: `send_managed.config` does not exist yet when the bootstrap migration runs) and
`SEND_MANAGED_MIGRATE_ALLOW_DOWN`.

---

## Part 4 — Production deployment

### Network layout and why no worker endpoint is public

**Workers are outbound-only.** A worker opens the connection to the Control and holds it; the Control never
dials a worker. So a worker needs **no inbound port at all** — no listener, no public IP, nothing to
firewall correctly. That is the design, and it is worth not undermining:

- The Control↔worker channel carries **decrypted SMTP credentials**, one job at a time. It is mutual TLS
  in both directions, but the correct posture is that this traffic never touches the public
  internet — put it on a private network / VPC / WireGuard mesh.
- A worker that accepted inbound connections would be an attack surface holding live credentials in
  memory. There is no reason to give it one.
- **Postgres is reachable from the Control only.** Workers have no DB access by construction; make the
  network agree with the code.

Minimum firewall shape on the Control host:

```bash
# The worker-facing mTLS ports: private network only.
sudo ufw allow from <private-subnet> to any port 8443 proto tcp
sudo ufw allow from <private-subnet> to any port 8444 proto tcp

# :8080 serves the platform API, the operator admin API, AND health.
# Expose it only to the platform and your operator network — never 0.0.0.0.
sudo ufw allow from <platform-ip> to any port 8080 proto tcp
sudo ufw allow from <operator-ip> to any port 8080 proto tcp

sudo ufw default deny incoming
sudo ufw enable
```

On worker hosts: **deny all inbound.** Outbound needs the Control's private address plus your SMTP
providers on 587/465.

> `:8080` is plain HTTP and serves both authenticated API domains. Terminate TLS in front of it
> (nginx/Caddy) if it crosses any untrusted network. The bearer credentials on that port deserve
> transport encryption.

### A production compose file is not shipped — you write it

> **FLAGGED GAP, read before using the units below.** This repo ships `docker-compose.yml` (the dev
> stack) and `tests/harness/compose.test.yml` (the test override). It does **not** ship a
> `docker-compose.prod.yml`. The units below reference one because a production deployment needs
> one, and it is deliberately not committed: it is site-specific (your hostnames, your cert paths,
> your secret store) and an untested committed one would be worse than none. Registered in
> [`future-work.md`](./future-work.md).
>
> Derive it from `docker-compose.yml`, changing exactly these things:
>
> - **Drop the `postgres` and `smtp-sink` services.** Production Postgres is external; the sink is a
>   dev-only local mail trap. Leaving either in a prod file is how you accidentally send real mail
>   into a black hole.
> - **Drop the `migrate` service's automatic run** (or keep it as a `profiles:`-gated one-shot).
>   Migrations must run as a deliberate step before the Control deploys, not implicitly on boot — see
>   Part 6.
> - **Split control and worker into separate files**, since they run on separate hosts.
> - **Point volume mounts at real paths** (`/etc/send_managed/certs/...`, `/etc/send_managed/kms/...`) instead of
>   `./secrets/dev/...`.
> - **Keep the least-privilege mounts exactly as they are.** The worker mounts only its own leaf + the
>   CA cert; the Control alone mounts the KMS key. Do not "simplify" by mounting a shared secrets
>   directory into both.
> - **Remove the worker's host port mapping** (there is none in the dev file either — keep it that way)
>   and keep the Control's ports bound to interfaces your firewall covers.
> - **Keep the Control's `stop_grace_period` above `shutdown_drain_timeout_sec`** (the dev file sets
>   `90s` for a 30 s drain). Docker's default is **10 s**, which SIGKILLs a Control that is draining
>   correctly — the dev stack shipped without this and every in-flight drain exited 137 until Stage 11
>   step 5 caught it. Note this is a *second* place the same bound must hold: `TimeoutStopSec` below
>   governs systemd, `stop_grace_period` governs Docker, and **both** have to clear the key. Setting
>   only one leaves the shorter of the two in charge.
>
> Adjust the unit file paths to whatever you name yours.

### systemd units

**Control** (`/etc/systemd/system/send_managed-control.service`):

```ini
[Unit]
Description=SendManaged Module — Control
After=network-online.target docker.service
Requires=docker.service

[Service]
Type=simple
WorkingDirectory=/opt/coldmail-send-managed
EnvironmentFile=/etc/send_managed/control.env
ExecStartPre=-/usr/bin/docker compose -f docker-compose.prod.yml rm -f control
ExecStart=/usr/bin/docker compose -f docker-compose.prod.yml up control
ExecStop=/usr/bin/docker compose -f docker-compose.prod.yml stop -t 90 control
Restart=on-failure
RestartSec=5

# The Control drains in-flight jobs on SIGTERM. Give it MORE time than
# shutdown_drain_timeout_sec (see config-reference.md) or systemd will
# SIGKILL a Control that was shutting down cleanly.
TimeoutStopSec=120
KillSignal=SIGTERM

[Install]
WantedBy=multi-user.target
```

`TimeoutStopSec` is the one value here that is easy to get wrong and quietly harmful. On SIGTERM the
Control stops claiming new work, keeps the worker stream up so in-flight jobs can report their outcomes,
and only then exits. Cut that short and you turn a clean shutdown into reclaim work — recoverable,
but pointless. Keep it above `shutdown_drain_timeout_sec`.

A SIGKILL is still safe: the singleton advisory lock is session-level, so Postgres releases it
automatically. There is no stale-lock recovery step.

**Worker** (`/etc/systemd/system/send_managed-worker.service`):

```ini
[Unit]
Description=SendManaged Module — SendManaged Worker
After=network-online.target docker.service
Requires=docker.service

[Service]
Type=simple
WorkingDirectory=/opt/coldmail-send-managed
EnvironmentFile=/etc/send_managed/worker.env
ExecStart=/usr/bin/docker compose -f docker-compose.prod.yml up worker
ExecStop=/usr/bin/docker compose -f docker-compose.prod.yml stop -t 60 worker
# A worker EXITS on purpose when it cannot register within its retry budget.
# Restart=always is correct: the supervisor is the retry mechanism.
Restart=always
RestartSec=10
TimeoutStopSec=90

[Install]
WantedBy=multi-user.target
```

Secure the env files: `sudo chmod 600 /etc/send_managed/*.env`. They hold the DB password, the token, and
the key path.

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now send_managed-control    # on the Control host
sudo systemctl enable --now send_managed-worker      # on each worker host
```

---

## Part 5 — Operating procedures

### Adding a worker

Workers are additive and need no Control restart.

> **Every worker needs its OWN id and token.** Identity is the authorization subject, so running a
> second worker with an existing worker's `WORKER_ID` adds no capacity: the Control closes the previous
> connection whenever an id re-registers (correct for a reconnect), so two same-id workers evict each
> other in a permanent register/disconnect loop while `send_managed.boxes` still shows one row. Adding a
> *replica* is not adding a worker. Certs may be shared — the cert is transport, the token is authority.

1. On the Control host: `provision-worker new --id <id>` → record the token.
2. Insert the `send_managed.boxes` row as the migration admin. **A new worker needs the `INSERT` from
   [Step 7](#step-7--provision-the-first-worker), not `provision-worker seed`'s `UPDATE`** — the `UPDATE`
   matches zero rows for an id that does not exist yet and silently reports success. Verify the row
   with the `SELECT` in Step 7 before continuing.
3. Bring up the worker with its own leaf certs and its token.
4. Confirm `worker registered id="<id>"` in the Control log, and that
   `GET /v1/admin/health` counts the new worker.

A worker coming online kicks the dispatcher, so queued work starts flowing to it immediately.

### Removing a worker

```bash
./bin/provision-worker revoke --id <id>    # emits SQL: clears token_hash, status='disabled'
```

Run that SQL, then stop the worker (`sudo systemctl disable --now send_managed-worker`).

**Both steps are required, and the second is the one that takes effect now.** The token/status check
runs only in the register handshake, so the SQL refuses the worker's *next* connect but does not tear
down a stream it already holds — while it stays connected the Control keeps pushing it jobs, each
carrying a decrypted credential. If you are removing a worker because you suspect it is **compromised**,
reverse the order and isolate the host first: [`disaster-recovery.md`](./disaster-recovery.md) §4a.

On its next connect attempt the worker is refused **regardless of its still-valid mTLS certificate** —
the token is the authority. Any job it held in flight is picked up by the reclaim loop and
re-dispatched to a live worker. No manual requeue.

**Rotating** a worker's token is revoke + re-provision: revoke the old, mint and seed a new one, update
the worker's secret, restart it.

### Redeploying

Ordinary code change, no migration:

```bash
cd /opt/coldmail-send-managed && git pull
docker compose -f docker-compose.prod.yml build control
sudo systemctl restart send_managed-control      # drains, then restarts
curl -fsS http://localhost:8080/readyz
```

Restarting the Control is safe by design: it drains in-flight work, buffered outcomes replay on
reconnect, and anything still in flight is reclaimed. Workers reconnect on their own.

**Config changes are not deploys.** Every dial lives in `send_managed.config` and takes effect on the next
tick — `PUT /v1/admin/config/{key}` and you are done. Restarting the Control to change a timeout means
something is wrong with your mental model, not with the Control.

### Rolling back

**Code rollback** is a normal redeploy of the previous image. Safe, because the schema is
additive-only: an older Control against a newer schema does not see columns it does not know about.

**Schema rollback is not the tool you want.** `down` is refused unless
`SEND_MANAGED_MIGRATE_ALLOW_DOWN=true`, deliberately: contracts here are additive-only, so a forward fix
is nearly always available and nearly always cheaper. Reverting a migration that has been live can
destroy data an older binary cannot see.

If you genuinely must:

1. Take a database backup first. Not optional.
2. Stop the Control (do not leave it running against a schema being changed underneath it).
3. `docker run --rm -e SEND_MANAGED_MIGRATE_ALLOW_DOWN=true -e MIGRATE_DATABASE_URL="$MIGRATE_DATABASE_URL" -e MIGRATIONS_DIR=/db/migrations -v "$PWD/db:/db:ro" send-managed-migrate:local down <version>` — one version at
   a time, verifying between.
4. Deploy the matching older Control **before** starting anything.

#### One named hazard: rolling back past `send_managed_0024` re-wedges account deletion

`send_managed_0024`'s down migration **deliberately restores `send_managed_0008`'s unconditional append-only
trigger body** (ADR-0012). That is correct as a down migration — it reverses what the up did — but the
thing it reverses is a *bug fix*, not a feature, and reversing it re-opens the bug:

> With `send_managed_0024` reverted, `DELETE /v1/admin/accounts/{id}` returns **HTTP 500** for any account
> that has ever been assigned a job. Postgres implements the `job_events.account_id` `ON DELETE SET
> NULL` as an internal `UPDATE`, the unconditional trigger rejects it, and the parent `DELETE` aborts.
> That was the live behavior from Phase 1 through Phase 10.

Two properties make this worth naming rather than leaving to be rediscovered:

- **It does not present as a migration problem.** The `down` succeeds cleanly and `migrate status`
  looks healthy. The symptom appears later, on an unrelated operator action, as a 500 whose internal
  reason is an append-only violation. Nothing links it back to the rollback.
- **Rows already nulled stay nulled.** The down cannot restore the `account_id` values the carve-out
  permitted to be set to NULL. So the rollback is not a clean reversal of observable state — only of
  the trigger body.

**Rule:** rolling back below `0024` means accepting that account deletion is broken until you roll
forward again. Deletion is the mechanism behind any retention or erasure obligation, so treat `0024`
as a floor and prefer a forward fix. This is the general case of why `down` is gated at all: on an
additive-only chain, forward is the tested direction.

---

## Part 6 — Version skew during a rolling deploy

This bites on the *second* deploy, not the first, so it is worth reading before you get there.

### Migrations run BEFORE the Control deploys. Always.

The order is:

```
1. migrate up          (schema + any new config keys are now present)
2. deploy the Control    (the new binary finds everything it expects)
3. deploy workers        (independently, whenever — see below)
```

**Why this direction and not the other:** the Control reads its config keys from `send_managed.config` at
startup and again on every tick, and a key it expects but cannot find is a **hard error** — startup
fails fatally (`FATAL send-path config load`), and a tick that cannot load config does not dispatch.
So a new Control against an old schema does not degrade gracefully; it does not run. Migrating first
makes that impossible.

The reverse skew is the safe one. **An old Control against a new schema is fine**: the schema is
additive-only (columns, enum values, and config keys are only ever added, never renamed or removed),
so the old binary simply ignores what it does not know about. That asymmetry is exactly why
migrations lead.

Practical consequences:

- **Never deploy the Control and the migration concurrently.** Let `migrate up` finish and verify with
  `migrate status` before the new binary starts.
- The window between step 1 and step 2 is safe to sit in. The old Control keeps working.
- If step 2 fails, you are in that safe window, not a broken state. Roll the binary back and leave
  the schema alone.
- Two migrators cannot race — the runner holds an advisory lock — but two *deploy pipelines* both
  restarting the Control can fight over the singleton lock. Serialize your deploys.

### Old workers talking to a new Control

**This works, and it is designed to.** Workers and the Control are independently deployable and you do
not need to restart them together.

The reason it works: the wire protocol is newline-delimited JSON with a `type` discriminator, and
both sides are **additive-tolerant by construction**:

- The worker's frame dispatch has a `default: // Unknown frame: ignore` arm. A new Control sending a
  frame type an old worker has never heard of does not crash it — the worker ignores it.
- New *fields* on an existing frame type are ignored by an older peer's decoder. (Note the
  deliberate contrast with the client-facing HTTP APIs, which use `DisallowUnknownFields` to reject
  typos in operator input. The internal worker channel is lenient precisely so version skew is a
  non-event; the client boundary is strict precisely so a typo is not silently accepted.)
- `Register` carries an optional `box_version`, which the Control records for observability. It is
  **not** a compatibility gate: the Control does not refuse an old worker, and there is no version
  negotiation to get wrong.

What an old worker will do is simply not honor a *behavior* introduced with the new Control — a new
variation toggle, for example, is stamped onto the pushed job but a worker that predates it ignores it.
The job still sends correctly. So during a rolling worker deploy expect **mixed behavior, not
failures**: some sends get the new treatment, some don't, until the roll completes.

**Rolling workers safely**, one at a time:

1. Revoke nothing. Just stop the worker: `sudo systemctl stop send_managed-worker`.
2. In-flight jobs on that worker are reclaimed automatically (dead-worker detection + lease expiry) and
   re-dispatched to the remaining workers. Nothing is lost and the account is not penalized.
3. Deploy and start the new worker; confirm it registers.
4. Move to the next. Keep at least one worker up throughout, or work simply queues until one returns —
   which is also safe, just idle.

### Summary

| Skew | Safe? | Why |
|---|---|---|
| New schema, old Control | **Yes** | Additive-only; the old binary ignores what it doesn't know. This is the window you deploy through. |
| Old schema, new Control | **No** | A missing config key is a fatal startup error. Migrate first — this is why the order is fixed. |
| New Control, old workers | **Yes** | Unknown frames ignored, unknown fields ignored, `box_version` is observability not a gate. Expect mixed behavior mid-roll, not failures. |
| Old Control, new workers | **Yes** | Same tolerance in the other direction. |
| Two Controls **dispatching** at once | **Never** | The singleton advisory lock prevents it. At startup the second exits after a bounded wait; if a running Control *loses* the lock it pauses dispatch and re-attempts acquisition, so at most one process is ever entitled to dispatch. Two Control *processes* can therefore briefly coexist during a lock handover — one of them dispatching nothing. Serialize deploys so they don't fight over it. |
| Two migrators at once | **Prevented** | The runner holds an advisory lock; the second waits. |

---

## Where to go next

- Something is broken → [`operator-runbook.md`](./operator-runbook.md)
- First live deployment → [`go-live-checklist.md`](./go-live-checklist.md)
- Back up the key you just created → [`disaster-recovery.md`](./disaster-recovery.md)
- Hook up monitoring → [`monitoring.md`](./monitoring.md)
- Size the deployment → [`capacity-planning.md`](./capacity-planning.md)
