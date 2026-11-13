# Security posture

One page, written to be handed to a reviewer. It states what is protected, the mechanism that
protects it, and — the part that matters — the **residual risks that remain after all of it**. Where
a control depends on something outside this codebase, that dependency is named rather than assumed.

Nothing here restates config defaults or endpoint shapes; those are in
[`config-reference.md`](./config-reference.md) and [`api-contract.md`](./api-contract.md). Procedures
live in [`operator-runbook.md`](./operator-runbook.md#disaster-recovery-key-backup-and-rotation).

---

## 1. What this system holds that is worth attacking

| Asset | Where it lives | Sensitivity |
|---|---|---|
| SMTP credentials | `send_managed.accounts.credential_ciphertext` (AES-256-GCM); plaintext transiently in Control and worker memory | **Highest.** Grants the ability to send as the customer's domain. |
| The credential master key | Outside the DB, Control host only | **Highest.** Decrypts all of the above. |
| Message content and recipients | `send_managed.send_logs` (`body`, `subject`, `headers`, `recipient_email`) — **plaintext at rest** | High. Retention-bounded, not encrypted. |
| Operator admin key (Domain B) | bcrypt hashes in the Control env; plaintext never stored | High. Full control-plane authority. |
| Platform service token (Domain A) | bcrypt hashes in the Control env | High. Can queue sends and read campaign reports. |
| Worker registration tokens | bcrypt hash in `send_managed.boxes.token_hash`; plaintext only in the worker env | High. A worker's actual authority to receive credentialed work. |
| mTLS CA key | The CA host — **never** on a worker, never on the Control in a proper deploy | High. Mints worker identities. |

---

## 2. Controls, and the mechanism behind each

Every row is a property the build treats as load-bearing (CLAUDE.md Rule 6) and enforces
structurally where it can, rather than by convention.

| Property | Mechanism | Enforced by |
|---|---|---|
| Credentials are never stored in plaintext | AES-256-GCM, key held outside the DB | Schema + `internal/adminapi/crypto.go` |
| A ciphertext cannot be moved between rows | AAD = `account.id ‖ credential_key_id`; a transplanted or re-labelled ciphertext fails authentication and releases nothing | `internal/credential`; harness test |
| **Control is the sole decryptor** | Decrypt exists in exactly one package (`internal/credential`), wired only into the dispatch path — never into `adminapi.Server` | Package split, not a flag. No `/v1/admin/*` route can reach plaintext. |
| No `/v1/admin/*` **response** ever contains a plaintext credential | `adminapi.Encryptor` has **no Decrypt method** | Type surface; behavioral test asserts no admin route yields a credential. This bounds the API response surface, **not** a rogue operator key — see §3.11 |
| The worker has no DB and no KMS access | It ships **no database driver and no KMS SDK** (`services/worker/package.json` `dependencies: {}`), and mounts neither DB credentials nor key material | Dependency manifest + container mounts; a harness test asserts the worker cannot read the key file |
| The worker holds credentials in memory only | Zeroed on **every send-path exit** — accept, SMTP error, thrown exception — by a `finally` the send cannot skip. Not on the SIGTERM or uncaught-exception paths: see §3.12 | `services/worker/src/creds.js` + `stream.js`'s `finally`; behavioral tests cover all three send-path exits |
| Credentials never appear in logs | Two scans. The `no_secret_leak` CI gate covers the **worker registration token and its hash**; the SMTP-credential canary scan lives in the send-path harness (`phase3_send.sh`, `phase8_variation.sh`), which also checks `send_logs`, `jobs.last_error_message`, the delivered message, and the ciphertext column. The credential scan looks for the canary in **plaintext and in three base64 forms** — SMTP AUTH transmits the secret encoded, so a logged AUTH exchange leaks a *working* credential while a plaintext grep still reads clean (AUTH LOGIN base64s the secret alone, padded or trimmed; AUTH PLAIN base64s `\0user\0secret` as one blob in which the secret's own base64 is **not** a substring) | Blocking CI gate + harness on every push. Neither runs against a production log stream — see §5 |
| **The database password never appears in a log line** | Scrubbed at the **producer**, not at the log calls: every place shipped code hands a connection string to pgx replaces the error text when it embeds the DSN (`internal/dsnscrub`). Fixed 2026-08-07 — before that this was **not true**, and the row above claimed it: MEASURED at the pinned pgx v5.7.4, a rejected connection string is echoed **whole** into the driver's error, and pgx's own redaction covers only `password=` unspaced and URL userinfo — it misses `?password=`, `?sslpassword=` and the honored spaced `password = SECRET`. The leak reached `log.Fatalf` at boot **and** `singleton`'s re-acquire failure, which repeats for as long as the lock stays lost | `internal/dsnscrub` (9 tests, incl. `TestDSNScrub_PgxStillLeaksAtThisPin`, which re-measures the premise on every run) + the blocking `dsn_scrub_wiring` CI gate. The gate exists because the unit package **cannot** prove the wiring: with the wrap removed, it stayed green and so did `go build` and `go vet`. Scope is `services/` and `tools/`; `tests/harness` is an acknowledged exclusion — see future-work §4a |
| Worker↔Control is mutually authenticated and confidential | mTLS on both the register/heartbeat and the send-stream listeners | `internal/tlsutil`; no non-TLS path exists |
| A decommissioned worker is refused even with a valid cert | Authority is the **registration token** (`send_managed.boxes.token_hash`), not the certificate; the cert is transport auth only | Clear/rotate the hash → refused on **next connect**. No CRL/OCSP needed to evict a worker. The check runs at register only — it does not tear down a stream already open (§3.12). The legacy `:8443` register/heartbeat listener authorizes against `DEV_WORKER_ID`/`DEV_WORKER_TOKEN_HASH` from the **environment**, not `send_managed.boxes`, so clearing the hash does not affect it; it carries no job stream and no credential (`internal/workerlink/workerlink.go`) |
| AUTH never precedes TLS | STARTTLS-before-AUTH on 587/25; implicit TLS on 465. TLS is always established first — but the **certificate is verified only for public hosts**; a loopback/RFC-1918/link-local `smtp_host` is trusted without validation (a test affordance, §3.13) | `services/worker/src/smtp.js` |
| The two auth domains never share a credential | Domain A platform service token vs Domain B operator admin key, separate env vars, separate hash lists | `internal/config`; distinct middleware |
| Both API surfaces fail **closed** | No configured hash ⇒ the surface is **not mounted** at all (logged as `admin API NOT mounted`) | `internal/config` + `cmd/control` |
| Bearer keys are not brute-forceable from the DB | Stored bcrypt-hashed, compared in constant time | `internal/adminapi/auth.go` |
| Row-level security actually enforces | `send_managed.accounts` is `FORCE ROW LEVEL SECURITY`, and the schema is owned by the **non-login, non-superuser** `send_managed_owner` — a table owner would otherwise bypass its own policies | `send_managed_0000`, `send_managed_0012` |
| The app role cannot alter the schema | `send_managed_app` is login-only with enumerated grants, never ownership | `send_managed_0000`/`send_managed_0002` |
| Audit trails cannot be rewritten | `job_events`/`account_events` are append-only; `send_logs` is write-once (reject-trigger + `REVOKE UPDATE`) | Triggers + grants |
| No internal detail crosses a client boundary | Two-vocabulary rule: client envelopes carry only `{code, message, request_id}`; internal reasons go to logs keyed by that request id | `internal/adminapi/errors.go`; contract tests |
| The alert webhook's **payload** leaks nothing | Treated as untrusted outbound egress: payload is condition code, severity, subject `{type,id}`, detail, timestamp, module — no content, credentials, reasons, SQL, or hostnames. Delivery failure is logged internally and dropped | `internal/adminapi/health_alerts.go`. The **destination** is unrestricted — §3.14 |
| The Control never invents an outcome | If the worker did not confirm a send/failure/bounce, the Control does not record one. A Control-declared timeout requeues; it never fabricates | Dispatch/reclaim design; behavioral tests. This constrains the **Control**, not a compromised worker reporting on its own job — §3.15 |
| Exactly one Control is ever dispatching | Session-level Postgres advisory lock, bounded acquire then exit. SIGKILL releases it — no stale lock | `cmd/control` + harness test |
| Every autonomous subsystem has an off switch | `dispatch_enabled`, `autoquarantine_mode` (defaults to **shadow**, not enforce), `autopause_enabled`, `variation_enabled`, `alert_webhook_url` | Config-driven, runtime-changeable |

---

## 3. Residual risks — stated honestly

These are **not** hypotheticals we have mitigated. They are what remains true after everything in §2,
and they are the section a reviewer should read first.

### 3.1 A compromised worker exposes the credentials of accounts actively sending through it

Bounded, but real. A worker necessarily receives a decrypted SMTP credential to make a send — that is
what a worker is for. So an attacker with code execution on a worker host can read the credentials of the
sends flowing through it while they hold that access.

What bounds the exposure:

- **Only accounts actually dispatched to that worker, only while it is running.** A worker holds one job in
  flight; credentials are scoped to a single send and zeroed when that send ends. There is no
  credential cache, no store, and no on-disk copy.
- **Not the whole pool.** A worker cannot enumerate accounts, query the database, or ask the Control for a
  credential it was not sent — it has no DB access and no such request exists in the protocol.
- **Not the master key.** The worker holds no key material and cannot decrypt anything.
- **Eviction needs no certificate revocation:** clear `send_managed.boxes.token_hash` (or set
  `status='disabled'`) and the worker is refused on its **next connect**. No CRL, no OCSP.

What does **not** bound it:

- **Eviction is not immediate, and this is the most important correction to make before an
  incident.** The token/status check runs **only in the register handshake**
  (`internal/workerlink/stream.go`). Once a stream is registered, job pushes are served from an
  in-memory worker map with no re-read of `send_managed.boxes`, and the heartbeat path will even return a
  `stale` worker to `active` without re-checking the token. So an attacker who keeps the (worker-initiated,
  outbound) stream open **continues to receive one fresh plaintext credential per job** after the
  operator has run the revocation SQL and believes the worker is evicted. **Kill the host or cut its
  network path first; the SQL is what stops it coming back, not what stops it now.** See
  [`operator-runbook.md`](./operator-runbook.md#disaster-recovery-key-backup-and-rotation) §4.
- **Persistence.** An attacker with root on a worker can keep harvesting for as long as the compromise
  goes unnoticed, and a long-lived worker that dispatch favors will see many accounts over time. There is
  no per-send attestation and no way to detect credential exfiltration from a worker from the Control side.

Treat worker hosts as holding live credentials, and keep them off the public internet (§4).

### 3.2 `send_logs` holds plaintext message bodies and recipient addresses at rest

`send_managed.send_logs.body`, `subject`, `headers`, and `recipient_email` are stored **unencrypted**. This
was a deliberate V1 decision: retention (`ttl_expires_at`, driven by `debug_log_retention_days`) is
the *sole* privacy control, and the sweep drops raw content once it expires.

Consequences to be explicit about:

- **A database dump within the retention window exposes message content and recipient lists** — even
  though it exposes no usable credentials. For many deployments the recipient list is itself the
  sensitive asset.
- Retention is a **config row**, so an operator can silently widen it. A large
  `debug_log_retention_days` is a privacy decision, not a tuning decision.
- The table is write-once and DELETE-able only by the sweep, so content cannot be tampered with —
  but nor can a specific recipient's content be redacted on request ahead of expiry without a manual
  DELETE.
- At-rest encryption for `send_logs` is registered in [`future-work.md`](./future-work.md), not built.
- **The retention promise is not being kept on a freshly migrated deployment.** Since
  `send_managed_0026` the sweep is gated by `retention_mode`, seeded **`shadow`** — at which it counts
  the expired rows and logs the count, deleting nothing. So `debug_log_retention_days` describes an
  intention, not an outcome, until an operator promotes `retention_mode` to `enforce`. Because
  retention is the *sole* privacy control on this table, that promotion is not a tuning step: it is
  the moment the privacy control starts existing. It is a REQUIRED go-live gate
  ([`go-live-checklist.md`](./go-live-checklist.md) §12) precisely so it cannot be missed, and the
  shadow count is what tells you how much content accumulated before you got there. The default is
  `shadow` rather than `enforce` on purpose (ADR-0014): a sweep that deletes on first boot after an
  upgrade is not something an operator can preview, and this table's deletions are irreversible.

Mitigations available today: promote `retention_mode` to `enforce`; keep `debug_log_retention_days`
short; rely on the database's own at-rest encryption; restrict who can read the database at all.

### 3.3 "The platform cannot read credentials" depends on KMS/IAM separation being configured

This is the most important caveat on this page, because it is the one that can be *believed* while
being false.

The code splits the capabilities correctly and unconditionally: Encrypt-only in the admin surface,
Decrypt-only in the send path. But in the dev/default configuration, **both entry points read the
same local key file** (`KMS_DEV_KEY_FILE`). The separation is real at the code level and only
becomes real at the *infrastructure* level when you point them at a genuine KMS in which:

- the Decrypt principal is the **Control role only**, and
- the platform, operators, and CI hold **no** Decrypt capability.

Nothing in this repo can verify that for you — it is a deployment sign-off, not a test. Until it is
done, the honest statement is "anyone who can read the Control's key file can decrypt every
credential," and on a single-VPS install that is whoever has root on the Control host. Do not tell a
reviewer otherwise, and treat this as a go-live checklist item:
[`go-live-checklist.md`](./go-live-checklist.md).

### 3.4 Losing the master key is unrecoverable, by design

The property that makes a stolen DB dump useless also means a lost key destroys every credential
permanently. This is an accepted trade, not a defect — but it makes key backup a **security control
in its own right**, and one whose failure mode is total. Procedure and the recovery drill:
[`operator-runbook.md`](./operator-runbook.md#disaster-recovery-key-backup-and-rotation) §1.

### 3.5 Master-key rotation requires re-entering every password by hand

Because no operator-facing route can decrypt, re-encryption is re-entry. `credential_key_id` makes
rotation *possible and safe* (no silent wrong-key decrypt), but there is no server-side bulk
re-encrypt. In practice this raises the cost of rotation enough that it may be deferred — which is
itself a risk worth naming. See [`operator-runbook.md`](./operator-runbook.md#disaster-recovery-key-backup-and-rotation) §3.

### 3.6 The operator admin key is a single, very powerful credential

Domain B has no roles, scopes, or per-operator identities: one bearer key grants full control-plane
authority — create accounts, write credentials, change any config key, unquarantine anything. There
is no per-operator attribution beyond the `request_id` recorded in the audit trail, so two operators
sharing a key are indistinguishable in `account_events`. Multiple hashes can be configured, which
makes per-operator keys and clean rotation possible, and is the recommended practice; it is not
enforced. RBAC is not built.

Two gaps in what that audit trail actually captures, both worth knowing before an incident review:

- **Config writes leave no DB audit row.** There is no `config_events` table. A `PUT
  /v1/admin/config/{key}` is recorded **only** as a Control process-log line keyed by `request_id`,
  with no before value and no after value. So "when was `debug_log_retention_days` widened?" — the
  question §3.2 makes matter — is answerable only from shipped logs, for as long as their retention
  holds. Log shipping is therefore a security control here, not just an ops convenience
  ([`operator-runbook.md`](./operator-runbook.md#monitoring-and-alerting)).
- **`account_events` records that an account was updated, not what changed.** An `updated` row
  carries identical before/after flag snapshots (`paused`, `pause_reason`, `quarantined`, `disabled`)
  and no changed-field list, so a `PATCH` that rewrote `smtp_host` is indistinguishable from a no-op
  one. This is what makes §3.11 hard to detect after the fact.

### 3.7 There is no rate limiting or lockout on the API surfaces

Neither domain limits authentication attempts or request volume. bcrypt comparison makes online
brute-force expensive rather than impossible, and both surfaces should sit behind a private network
or reverse proxy that does provide limiting. As shipped, an attacker who can reach `:8080` can try
keys as fast as bcrypt allows, and nothing alerts on it.

### 3.8 A compromised Control host is total compromise

The Control is the sole decryptor, the only DB client, and the holder of every credential in turn.
There is no compartment above it. All the worker-side hardening is irrelevant to this case, and it is
the reason the Control host should have the smallest possible attack surface, and the reason the
admin API mounts only when explicitly given a key.

### 3.9 No bounce, complaint, or delivery feedback exists in V1

A security-adjacent gap because it limits *detection*: a provider `250` is recorded as `sent`, and
this system will never learn that a message bounced, was spam-filtered, or was silently dropped. If
credentials were stolen and used elsewhere, or an account's reputation collapsed, this module would
not surface it. Detection depends on provider-side monitoring. See the runbook's deliverability
section and [`future-work.md`](./future-work.md).

### 3.10 The platform tables are trusted

`public.users` and `public.job_items` belong to the platform; this module reads them (SELECT only)
and derives tenancy from them. A compromised or buggy platform can therefore direct sends and bind
jobs to users. The trust boundary is at the module's edge, and it is intentional — but a reviewer
should know that `user_id` is an input this module validates for existence, not for authority.

### 3.11 A Domain B operator can have any stored credential delivered in plaintext to a host they choose

The §2 row "no `/v1/admin/*` response ever contains a plaintext credential" is structurally true and
still worth having — but it bounds the **API response surface**, not the holder of the admin key.

`PATCH /v1/admin/accounts/{id}` can set `smtp_host` on its own, and the credential columns are
touched only when `smtp_secret` is present (the deliberate omit-means-unchanged design). So an
operator — or anyone holding a leaked admin key — can repoint an existing account at a host they
control while leaving the stored ciphertext untouched. On the next dispatch the Control decrypts as
normal and hands the plaintext to a worker, which AUTHs to whatever host the account now names. No DB
access is needed, no key-file access, and no Domain A token: ordinary campaign traffic through that
account is the trigger. TLS is no obstacle — the attacker's host presents a valid certificate for its
own name.

Combined with §3.6's second bullet (`account_events` records no field-level diff), the redirect is
also not visible in the audit trail. Mitigations available today are entirely operational: treat the
admin key as equivalent to the credentials themselves, issue one hash per operator so a leak is
attributable and revocable, and keep the key list short. A host allowlist and a rule requiring
`smtp_secret` re-entry on an `smtp_host` change are registered in
[`future-work.md`](./future-work.md), not built.

### 3.12 Worker credential zeroing covers the send path, not shutdown or an uncaught exception

The worker zeroes its in-memory credential in a `finally` around the SMTP session, so all three
send-path exits — accept, SMTP error, thrown exception — are covered, and there are behavioral tests
for each. Two exits are **not** covered:

- **SIGTERM/SIGINT.** The shutdown handler stops the stream and exits; it holds no reference to the
  in-flight credential and does not zero it.
- **An uncaught exception or unhandled rejection outside the send path.** No handler is registered
  for either, so there is no zeroing step on that exit.

Practical meaning: a memory image captured at that instant — core dump, container checkpoint,
hypervisor snapshot, `/proc/<pid>/mem` read by local root — may still contain the plaintext of one
in-flight send. So `systemctl stop send_managed-worker` on a host you suspect is compromised is **not** a
credential scrub; rotate that provider password rather than assuming it was cleared. Closing this is
a behavior change and is registered in [`future-work.md`](./future-work.md).

### 3.13 Certificate verification is skipped for private-IP SMTP hosts

`AUTH never precedes TLS` (§2) holds unconditionally: TLS is always established before AUTH, on 465
by implicit TLS and on 587/25 by STARTTLS, and the worker aborts rather than AUTHing if STARTTLS is not
offered. But certificate *verification* is host-conditional. The worker disables it for `localhost`, the
compose sink, `127.0.0.0/8`, `::1`, `10/8`, `172.16/12`, `192.168/16`, and `169.254/16` — a test
affordance so the harness can run against a local sink. The Control-side validator accepts any bare IP
literal as `smtp_host`, so nothing stops a private address being stored on a real account.

Consequence: an account pointed at a private-IP SMTP host AUTHs over an **unverified** TLS
connection, so an attacker positioned on that network path (or one who arrived via §3.11's redirect)
can MITM with a self-signed certificate and capture the plaintext credential. A public hostname is
unaffected — verification is enforced there. Deploy-time check in §5; rejecting private-range
`smtp_host` at write time is registered in [`future-work.md`](./future-work.md).

### 3.14 The alert webhook's destination is unrestricted — blind SSRF from inside the trust boundary

§2's webhook row is about the payload, and the payload is clean. The **destination** is validated for
scheme prefix only: `alert_webhook_url` must be empty or start with `http://` or `https://`. There is
no host, port, IP, or private-range restriction, so loopback, RFC-1918, and cloud link-local metadata
addresses (`169.254.169.254`) are all accepted, and the Control will POST to them from inside the trust
boundary on every rising alert edge.

Bounded: only a Domain B holder (or someone with DB write access) can set it, and both are already
fully privileged by §3.6/§3.8. It is also **blind** — the response body is never returned to a caller
and never logged as content — so exploitation is limited to reachability probing and side-effecting
`POST`s. Still: treat the value as privileged config, and constrain the Control's egress at the network
layer rather than relying on the value being sensible. A destination allowlist is registered in
[`future-work.md`](./future-work.md).

### 3.15 A compromised worker can fabricate the outcome of the job it legitimately holds

§2's "the Control never invents an outcome" constrains the Control, and it holds. The converse is not
constrained. The outcome guards check that the reporting worker matches `assigned_box_id` and that the
`attempt_token` is current — which prevents replay across attempts and across workers, **not** a lie
about the current job. A report from the worker that genuinely holds the job, with the correct token, is
applied as-is:

- `accepted` marks the job `sent` with an attacker-chosen `provider_message_id`, so mail that was
  never sent is recorded as delivered.
- `error`/`bounced` feed account scoring, so fabricated `auth_failure`s can drive real accounts —
  potentially a whole pool — into quarantine.

Neither is detectable from this system, because §3.9 means there is no independent delivery feedback
that could contradict the worker. A per-send attestation is what would close it; see §3.1's note that
there is none, and [`future-work.md`](./future-work.md).

### 3.16 Known Go module vulnerabilities — cleared 2026-11-07, re-check before a pilot

This was a current, dated risk and is recorded here so a reviewer reading only §3 can see its history:

- **`GO-2026-5004`** — SQL injection via placeholder confusion in `pgx/v5` (the only database driver in
  the system, in the process that holds every credential). Cleared: `pgx/v5` v5.7.4 → v5.10.0.
- **`GO-2026-5970`** — infinite loop in `golang.org/x/text`. Cleared: v0.21.0 → v0.43.0.

Both had live call paths. Remediation had been attempted in Phase 10 and reverted because the bumps
broke the pinned `golang:1.23-alpine` builder; it was done properly on 2026-11-07 by upgrading the
builder to `golang:1.26-alpine` (go1.26.10). `govulncheck` now reports no vulnerabilities in any of the
four Go modules. The history and numbers are in [`future-work.md`](./future-work.md) §5; the pre-pilot
re-check is [`go-live-checklist.md`](./go-live-checklist.md) item 9. **The result is dated** — the
advisory database moves, so re-run `govulncheck` before a pilot carries real data.

---

## 4. Network exposure — what should be reachable, and from where

| Listener | Should be reachable from | Never from |
|---|---|---|
| Control `:8080` (health, `/readyz`, Domain A, Domain B) | the platform and operators, over a private network or an authenticating proxy | the public internet, unproxied |
| Control `:8443` (worker register/heartbeat, mTLS) | worker hosts only, private network | anywhere else |
| Control `:8444` (worker send stream, mTLS — **carries decrypted credentials**) | worker hosts only, private network | anywhere else |
| **Any worker listener** | — | **Nothing. A worker exposes no inbound service at all and must never be publicly reachable.** It dials out to the Control; there is nothing to connect *to*. |

`/healthz` and `/readyz` are unauthenticated by design (they carry no operational detail — liveness
and "DB reachable and singleton lock held"). Everything with real content is behind a bearer key.
`GET /v1/admin/health` is authenticated Domain B and does expose operational state; do not wire it
into an unauthenticated uptime check. See [`operator-runbook.md`](./operator-runbook.md#monitoring-and-alerting).

---

## 5. What a reviewer should verify at deploy time

The codebase cannot check these; they are the deployment's half of the posture. Every row except the advisory one is **⏳ owner-only** — it describes the real deployment, which does not exist yet; the matching items in [`go-live-checklist.md`](./go-live-checklist.md) carry the evidence for the part the repository already proves.

- ⏳ KMS Decrypt is granted to the **Control role only** (§3.3). The platform, operators, and CI have none.
- ⏳ The master key is backed up off-host, encrypted, in two locations, and the **recovery drill has been run** ([`operator-runbook.md`](./operator-runbook.md#disaster-recovery-key-backup-and-rotation) §1).
- ⏳ No worker host has `DATABASE_URL`, any KMS variable, `ca.key`, or `control.key`. Presence is an incident, not a misconfiguration.
- ⏳ Workers have **no** public ingress; worker↔Control traffic is on a private network.
- ⏳ `ADMIN_API_KEY_HASHES` and `PLATFORM_TOKEN_HASHES` are **different** credentials, each rotatable, each held by the smallest possible set.
- ⏳ `send_managed` schema owner is `send_managed_owner` (non-login, non-superuser) and `send_managed.accounts` shows `relforcerowsecurity = t`.
- ⏳ `debug_log_retention_days` reflects a deliberate privacy decision (§3.2), not a default nobody looked at.
- ⏳ **`retention_mode` is `enforce`** (§3.2). Until it is, `debug_log_retention_days` is an intention and nothing is ever deleted — the sole privacy control on `send_logs` is not running. Verify the value, do not assume the default: `GET /v1/admin/config` and read it.
- ⏳ All three `off`/`shadow`/`enforce` dials are understood: `autoquarantine_mode`, `autopause_mode` and `retention_mode` all default to **shadow** — each logs the action it would take and takes none. Two of the three are safe to leave there; `retention_mode` is not (the row above).
- ⏳ The database's own at-rest encryption is enabled (the only control over `send_logs` content besides retention).
- ⏳ No account has a **private-range or link-local `smtp_host`** (§3.13) — certificate verification is skipped for those, so the credential would cross an unverified connection. `SELECT id, smtp_host FROM send_managed.accounts;` and eyeball it.
- ⏳ `alert_webhook_url` points where you think it does (§3.14) — the value is validated for scheme prefix only, and the Control will POST to a private or link-local address just as readily as to your alerting service.
- ✅ Both Go module advisories are cleared (§3.16) on 2026-11-07 (`4640dfb`), **⏳ re-run on pilot day** — `go-live-checklist.md` item 9. Do this before a pilot carries real data.
- ⏳ Your **own** log stream has been grepped for a known account's password. The shipped `no_secret_leak.sh` gate reads `.env` and compares only the **dev worker token/hash** against the *dev compose* logs — against a production deployment it either aborts on the missing `.env` or prints `OK` having compared nothing. There is no production-ready credential-leak scan; write the grep yourself, or treat this worker as unchecked.

---

## 6. Related

| Doc | For |
|---|---|
| [`operator-runbook.md`](./operator-runbook.md#disaster-recovery-key-backup-and-rotation) | Key backup, the recovery drill, rotation, compromise response. |
| [`go-live-checklist.md`](./go-live-checklist.md) | The pre-launch gate, including the IAM-separation sign-off. |
| [`architecture.md`](./architecture.md) | Boundaries and the send path in detail. |
| [`../CLAUDE.md`](../CLAUDE.md) | The build rules these properties come from — canonical. |
| [`future-work.md`](./future-work.md) | `send_logs` at-rest encryption, bounce ingestion, and the rest of what is not built. |
