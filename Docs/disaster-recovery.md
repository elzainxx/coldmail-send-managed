# Disaster recovery, key backup, and rotation

Read the **key backup** section before you go live. It is the one procedure in this repo whose
absence is unrecoverable: everything else on this page can be redone from a backup or a fresh
deploy, and the master key cannot.

This document does not restate config defaults or request shapes — those are in
[`config-reference.md`](./config-reference.md) and [`api-contract.md`](./api-contract.md).
Diagnosing a live incident is [`operator-runbook.md`](./operator-runbook.md); this page is for the
losses those trees cannot fix.

---

## 1. The single biggest risk: losing the master key

### Why it is fatal

SMTP credentials are stored in `send_managed.accounts.credential_ciphertext`, encrypted AES-256-GCM. The
key lives **outside the database**, held only by the Control (`KMS_DEV_KEY_FILE`, or your KMS wiring).
That separation is the point — a stolen database dump yields no credentials — and it is exactly why
losing the key is terminal:

- **A database backup does not contain the key.** Restoring the DB restores ciphertext only.
- **There is no escrow, no recovery share, no second copy inside the system.** Nothing in this repo
  can reconstruct the key from anything else it holds.
- **Decrypt is Control-only by design** (CLAUDE.md Rule 6). The platform holds no Decrypt capability
  that could be used to rescue the plaintext, and adding one is forbidden.
- **The AAD binds each ciphertext to its row** (`account.id ‖ credential_key_id`), so ciphertext
  cannot be moved, re-labelled, or brute-forced across accounts.

If the key is gone, **every SMTP credential in the system is permanently unrecoverable.** The
accounts, pools, jobs, and history all survive; the secrets do not. Recovery is manual re-entry of
every account's SMTP password via `PATCH /v1/admin/accounts/{id}` with `smtp_secret`. For a hundred
accounts, that is a hundred password resets at the provider — assume most originals are not written
down anywhere else.

The failure is also **silent until send time**. A Control that starts with the wrong key starts
cleanly (the key is only validated for length and cipher init at startup); the failure surfaces as
`credential decrypt failed (authentication)` on the first send attempt per account. Confirm a
restore with a real send, not with a clean startup log.

### What to back up

Exactly one file (or, with a cloud KMS, the key's own backup/replica arrangement):

| Item | Where it is in a default install | Notes |
|---|---|---|
| The master key material | the path in `KMS_DEV_KEY_FILE` (install guide: `/etc/send_managed/kms/master.key`; local dev: `secrets/dev/kms/dev-master.key`) | 32 raw bytes, AES-256. This is the whole risk. |
| `CREDENTIAL_KEY_ID` | the Control's environment | Not secret, but a restore with the wrong label fails AAD authentication. Back up the *pairing* of key file to label. |

The mTLS CA key (local dev: `secrets/dev/certs/ca.key`) is a *separate* and much less severe
concern — and note it belongs on the CA host, not on the Control host, in a real deploy. Losing it
means re-issuing a CA and re-deploying leaf certs to every worker, which is laborious but fully
recoverable. Losing the master key is not. Do not conflate them, and do not store them together
(a single compromised backup should not yield both the transport identity and the credential key).

### Backup procedure

Do this once, before the first real credential is entered, and again after every rotation.

```bash
# On the Control host, as the operator. Never over a shared terminal, never in a chat transcript.
umask 077

# Read the key path out of the Control's env file. The trailing sed/tr are NOT decoration: if you
# copied the install guide's annotated block, the line has an inline "# or your KMS wiring"
# comment, and a bare `cut -d= -f2-` hands you a path with the comment glued on. Verify, don't
# assume — an unreadable path here means you back up nothing and find out during a restore.
KEY_PATH="$(grep -E '^KMS_DEV_KEY_FILE=' /etc/send_managed/control.env \
             | cut -d= -f2- | sed 's/[[:space:]]*#.*$//' | tr -d '"'\''\r')"
test -s "$KEY_PATH" || { echo "FATAL: no key at [$KEY_PATH]"; exit 1; }
test "$(wc -c < "$KEY_PATH")" -eq 32 || echo "WARNING: key is not 32 bytes — check before trusting this backup"

# 1. Wrap it in something you can actually restore from, with a passphrase you have separately.
gpg --symmetric --cipher-algo AES256 --output /tmp/send_managed-master-key.$(hostname).gpg "$KEY_PATH"

# 2. Record what it is, so a stranger restoring it in two years knows what they hold.
cat > /tmp/send_managed-master-key.README <<'EOF'
SendManaged module credential master key (AES-256, 32 raw bytes).
Pairs with CREDENTIAL_KEY_ID=<value>  <-- fill this in; a mismatch fails AAD authentication.
Restore to the path named by KMS_DEV_KEY_FILE on the Control host, mode 0400, owned by the control user.
Losing this file makes every SMTP credential in send_managed.accounts permanently unrecoverable.
EOF

# 3. Move both OFF this host, then shred the temporary copies.
#    (scp/rclone/your secrets manager — see "Where the backup lives" below.)
shred -u /tmp/send_managed-master-key.*.gpg /tmp/send_managed-master-key.README
```

Non-negotiables:

- **Off-host.** A backup on the Control host does not survive the loss of the Control host.
- **Encrypted at rest with a passphrase held separately from the backup.** Otherwise the backup is
  a second copy of the key with weaker access control than the original.
- **At least two independent locations**, so one failed location is not the incident.
- **Never in the repo, never in the DB, never in a ticket, chat, or CI variable.** CI has no reason
  to hold it — nothing in `tests/harness/` needs the production key.

### Where the backup lives, and who can reach it

Fill this table in for your deployment and keep it current. An unfilled table is the same as no
backup: at 3am nobody knows where to look.

| Copy | Location | Access controlled by | Last verified |
|---|---|---|---|
| Primary | *(e.g. the org password manager, "SendManaged module — master key" item)* | *(who)* | *(date)* |
| Secondary | *(e.g. offline encrypted media in a physical safe)* | *(who)* | *(date)* |
| Passphrase | *(separate from both of the above)* | *(who)* | *(date)* |

Access rules that follow from the security model:

- The set of people who can reach a backup copy is the set of people who can decrypt every SMTP
  credential in the system. Keep it small, name them explicitly, and review it when someone leaves.
- **At least two people** must be able to reach it, or a single unavailable person is a disaster.
- The platform team does not need it. Only whoever can deploy the Control does.

### The recovery drill — run it, do not assume it

An untested backup is a belief, not a backup. Run this on a **staging** Control against a **copy** of
production data (or at minimum a staging install with real-shaped credentials). It is safe: it
touches nothing in production and sends nothing to a real recipient.

1. Retrieve the backup and the passphrase *by the documented route only*. If you had to ask someone
   where it was, or use knowledge not in the table above, the drill has already found its bug — fix
   the table.
2. `gpg --decrypt` to a scratch path; confirm the size is exactly 32 bytes:
   `wc -c < /tmp/restored.key` → `32`.
3. Install it as the staging Control's `KMS_DEV_KEY_FILE` (mode `0400`, owned by the Control's user) and
   set `CREDENTIAL_KEY_ID` to the paired label from the README.
4. Restart the Control. A clean start proves only that the key is 32 bytes — **it does not prove the
   key is right.**
5. **Prove it by decrypting.** Queue one job for an account whose credential predates the drill and
   watch it reach `sent` (send into a sink such as Mailpit, never a real recipient). A wrong key
   fails here with an authentication error and no plaintext.
6. Record the date in the table above.

Re-run the drill after every rotation, every change of custodian, and at least annually.

---

## 2. Database loss and restore

### What is recoverable, and what is not

| State | Recoverable from a DB backup? | Notes |
|---|---|---|
| Accounts, pools, config, workers | Yes | Config rows restore to their backed-up values, not to `default_value`. |
| Credential ciphertext | Yes — **but only useful with the master key** | See §1. |
| Job rows and final states | Yes | |
| `job_events` / `account_events` audit trail | Yes | Permanent by design, never swept. |
| `send_logs` rendered content | Only within retention | The sweep drops raw content after `debug_log_retention_days`; older rows keep metadata, not bodies. |
| Jobs in flight at the moment of loss | **No** — and this is safe | See below. |
| The worker outcome buffer | **No** | A worker's buffer survives a *connection* loss, not a *process* restart. See the runbook's "A worker died". |

### In-flight jobs after a restore

A restore rewinds to the backup's point in time. Jobs that were `assigned`/`sending` then are still
`assigned`/`sending` in the restored DB, but the workers that held them are long gone. This resolves
itself: the reclaim loop finds leases past `job_max_processing_time_sec` and requeues them. No
manual reconciliation, and nothing marks a job `sent` that a worker did not confirm (CLAUDE.md Rule 5 —
the system never invents an outcome).

The real risk in the other direction: **a job the backup shows as `queued` may already have been
sent** before the loss. Restoring an older snapshot can therefore re-send. That is a duplicate-mail
risk, not a corruption risk — decide deliberately whether to hold dispatch after a restore:

```bash
# Before bringing the Control back up on restored data, if re-sends are unacceptable:
#   set dispatch_enabled=false, bring the Control up, reconcile against provider-side records,
#   then re-enable. Exact call: api-contract.md (PUT /v1/admin/config/{key}).
```

### Restore procedure

1. **Stop the Control first — actually stop it, do not rely on it pausing itself.** Exactly one Control is
   ever active (advisory-lock singleton); a Control running against a database being restored under it is
   undefined. Stopping it also releases the lock — the lock is session-level, so there is never a stale
   lock to clear.

   This step matters *more* since G11-OD-1 / ADR-0011, not less. A Control left running will lose its
   lock when the database goes away and correctly **pause** — but the keeper then re-attempts
   acquisition every `brain_singleton_reacquire_interval_sec` (default 5s), so the moment the restored
   database accepts connections it re-acquires and **resumes dispatching against the restored data on
   its own**, mid-procedure and before you have completed step 3's ownership re-verification. Pausing
   is a safety property for a *transient* outage; it is not a substitute for stopping the process
   during a deliberate restore.
2. Restore the database by your provider's procedure (`pg_restore`, Supabase PITR, snapshot
   rollback). This module does not prescribe one.
3. **Re-verify roles and ownership.** A restore into a fresh cluster can land the `send_managed` schema
   under the wrong owner, which silently breaks a load-bearing property: `FORCE ROW LEVEL SECURITY`
   on `send_managed.accounts` only enforces because the schema is owned by the non-login, non-superuser
   `send_managed_owner` role. Confirm it (read-only):

   ```sql
   SELECT n.nspname, pg_get_userbyid(n.nspowner) AS owner
     FROM pg_namespace n WHERE n.nspname = 'send_managed';                 -- expect send_managed_owner
   SELECT relname, relrowsecurity, relforcerowsecurity
     FROM pg_class WHERE relname = 'accounts';                       -- expect t, t
   SELECT rolcanlogin, rolsuper FROM pg_roles WHERE rolname = 'send_managed_owner';  -- expect f, f
   ```

   If any of these are wrong, fix them before accepting traffic. The install guide's roles/grants
   step is the reference: [`install-and-deploy.md`](./install-and-deploy.md).
4. Run migrations. The runner is self-tracking and idempotent — on an up-to-date restore it applies
   nothing, which is the expected and desired outcome.
5. Confirm the master key is present and paired (§1), then start the Control.
6. Verify with `/readyz` (DB reachable **and** singleton lock held) and then a real send. Reading
   the health endpoint: [`operator-runbook.md`](./operator-runbook.md).

### Total-loss rebuild

Losing the host but not the database or the key is an ordinary redeploy — follow
[`install-and-deploy.md`](./install-and-deploy.md) and skip the parts that create state. Workers are
stateless and disposable: rebuild one from its cert + `WORKER_ID` + `WORKER_TOKEN` and it re-registers.
The order that matters is **key present → migrations current → Control up → workers up**, because a worker
with nothing to connect to simply retries, whereas a Control without its key fails at send time.

---

## 3. Credential and key rotation

`credential_key_id` exists for exactly this. Every ciphertext carries the label of the key that
wrote it, and the Control refuses to decrypt a row labelled with a key it does not resolve — an
explicit rotation error, never a silent wrong-key attempt.

### What can and cannot be rotated concurrently

The constraint that shapes every procedure below: **a Control resolves exactly one
`credential_key_id`** — the value of its `CREDENTIAL_KEY_ID` env var. There is no multi-key
keyring. So there is no window in which rows under two labels are both sendable. That rules out the
obvious "flip the writer, drain the readers" approach and makes the safe ordering the one below.

### Rotating an individual account's SMTP password (routine, no key involved)

This is the common case and needs no procedure beyond the API: `PATCH /v1/admin/accounts/{id}` with
`smtp_secret` re-encrypts under the current key with a fresh nonce, in one transaction, and writes
an audit row. In-flight sends already hold a decrypted copy for their one send and are unaffected;
the next dispatch picks up the new secret. No restart, no downtime.

Do this whenever a provider password changes, and always after an `auth_failure` quarantine — see
the runbook's "An account got quarantined".

### Rotating the master key (re-encrypting every credential)

**Prerequisite that has no workaround: you must have every account's plaintext SMTP password.** The
system cannot help you here, and this is not an oversight — the admin surface holds an *Encrypt-only*
primitive with no Decrypt method, so no operator-facing route can read a credential back out
(CLAUDE.md Rule 6). Re-encryption is therefore **re-entry**, not a server-side transform.

That is a real limitation, and it is the honest state of V1: a server-side bulk re-encrypt would
require a Control-side rotation path that can decrypt-under-old and encrypt-under-new. That capability
does not exist and is registered in [`future-work.md`](./future-work.md); building it here would be a
new feature, not polish.

Safe ordering — the sequence that never leaves a row unreadable while dispatch is live:

1. **Prepare.** Generate the new key (32 random bytes) and choose the next label:
   `cred-dek-v1` → `cred-dek-v2`. Back the new key up **before** using it (§1) — a key you rotate to
   and then lose is the §1 disaster with extra steps. Collect every account's plaintext password.
2. **Announce a send freeze.** Set `dispatch_enabled=false`. It halts dispatch cleanly with zero job
   loss and is resumable; queued jobs wait. Confirm `dispatch.dispatch_paused: true` on the health
   endpoint before continuing.
3. **Cut the key over.** Install the new key file, set `CREDENTIAL_KEY_ID` to the new label, restart
   the Control. Every existing row is now labelled with the *old* key and is unreadable — this is
   precisely why step 2 came first. Do not skip it and do not reorder these two.
4. **Re-enter every credential.** `PATCH /v1/admin/accounts/{id}` with `smtp_secret` for each
   account; each write re-encrypts under the new key and re-stamps `credential_key_id`. Track
   progress against the DB, not against your list (read-only):

   ```sql
   SELECT credential_key_id, COUNT(*) FROM send_managed.accounts GROUP BY 1 ORDER BY 1;
   ```

5. **Verify before unfreezing.** The count under the old label must be **zero**. Any row still on the
   old label will fail at send time, so check, do not assume:

   ```sql
   SELECT id, smtp_host, smtp_username FROM send_managed.accounts
    WHERE credential_key_id <> '<new label>';       -- expect no rows
   ```

6. **Prove one real decrypt.** Re-enable dispatch and watch a single job reach `sent`. A clean
   startup proves nothing about the key (§1) — only a completed send does.
7. **Retire the old key.** Keep it until step 5 returns zero rows *and* step 6 passes, then destroy
   it and remove its backups. Keeping a superseded key indefinitely widens the blast radius of a
   backup compromise for no benefit.

If the rotation must be abandoned mid-way, roll **back**: restore the old key file and old
`CREDENTIAL_KEY_ID`, restart, and re-enter the credentials of any accounts already moved to the new
label (they are now the unreadable ones). This is symmetric to step 4 — which is the argument for
doing the whole thing inside one freeze window rather than incrementally.

### Rotating the operator admin key or platform service token

Both are stored as bcrypt hashes, comma-separated — which makes rotation zero-downtime, because the
Control accepts *any* configured hash:

1. Mint a new key (`provision-worker admin-key`, which prints the plaintext and its bcrypt hash) and
   **append** the hash to `ADMIN_API_KEY_HASHES`
   (or `PLATFORM_TOKEN_HASHES`), keeping the old one. Restart.
2. Move every caller to the new key.
3. Remove the old hash. Restart.

Never let the list go empty by accident: an empty `ADMIN_API_KEY_HASHES` **unmounts the admin API**
(fail-closed by design) and you will lose the surface you are using to fix things. The two domains
never share a credential — rotate them independently.

### Rotating the mTLS CA or a worker cert

A worker's authority is its **registration token**, not its certificate; the cert is transport
auth/confidentiality. That splits the work:

- **One worker's leaf cert:** re-issue from the existing CA, deploy to that worker, restart it. It
  re-registers. No Control change.
- **The CA:** every leaf must be re-issued and every worker restarted, plus the Control's own
  `MTLS_CA_CERT`. Do the Control last, so workers are already carrying certs the new root can verify.
  Plan a maintenance window; there is no dual-trust-root mode.
- **Decommissioning a worker:** clear or rotate `send_managed.boxes.token_hash`. It is refused on its next
  connect *regardless of a still-valid cert* — that is the intended kill path, and it is why cert
  revocation infrastructure is not required to remove a worker. **Then stop the worker process**, because
  the SQL alone does not disconnect a worker that is currently connected — see §4a. For a routine
  decommission you are stopping it anyway, so the order barely matters; for a *compromise* it is the
  whole procedure.

---

## 4. What to do in the first five minutes of a suspected key compromise

Not a drill — the case where you believe the master key or a backup copy leaked. For a compromised
**worker** the first move is different and the order matters — see §4a.

1. **Assume every SMTP credential is exposed.** The key decrypts all of them. This is a provider-side
   incident before it is a this-system incident.
2. **Stop the bleeding at the providers**, not here. Change the SMTP passwords at each provider. That
   invalidates the leaked plaintext regardless of what happens to the key.
3. `dispatch_enabled=false` while credentials are stale, so the system is not hammering providers
   with passwords that no longer work (which would otherwise drive `auth_failure` quarantines
   across the pool).
4. Rotate the master key and re-enter the new passwords in one pass — §3's procedure does both,
   since re-entry is required either way.
5. Review who could reach the backups (§1's table), and treat the audit trail as evidence:
   `send_managed.account_events` records every credential write with a request id, and it is never swept.

---

## 4a. What to do in the first five minutes of a suspected WORKER compromise

A different incident from §4, with a different first move — and the one place where doing the obvious
thing first does **not** stop the bleeding.

**The trap:** clearing `send_managed.boxes.token_hash` is checked only during the register handshake. A worker
that is *already* connected keeps its stream, and the Control keeps pushing jobs to it from its
in-memory registration — each push carrying one freshly decrypted SMTP credential. The revocation SQL
stops the worker coming *back*; it does not stop it *now*. Run it and walk away and the attacker keeps
harvesting a new credential per job while your ticket says "worker evicted".

1. **Cut the worker off the network, or kill the host.** Firewall its route to the Control's `:8444`, or
   stop the instance. This is the step that actually ends credential flow. The stream is the worker's own
   outbound connection, so nothing the Control does terminates it.
   - If you cannot reach the host, `dispatch_enabled=false` is the blunt Control-side stop — it halts
     dispatch to *every* worker, which is the right trade for a few minutes.
2. **Then revoke:** clear/rotate `send_managed.boxes.token_hash` and set `status='disabled'`, so it is
   refused if it ever reconnects.
3. **Rotate the SMTP passwords of every account that dispatched to that worker** while it may have been
   compromised. `SELECT DISTINCT assigned_account_id FROM send_managed.jobs WHERE assigned_box_id = '<worker>'
   AND updated_at > '<suspected start>';` gives you the list. Do not skip this because the worker was
   stopped: shutdown does **not** zero the in-flight credential (`security-posture.md` §3.12), and a
   worker that ran for a while saw many accounts.
4. **Distrust what that worker reported.** A compromised worker can report any outcome for a job it
   legitimately held — including `accepted` for mail never sent, and fabricated `auth_failure`s that
   may already have quarantined healthy accounts (`security-posture.md` §3.15). Review
   `send_managed.account_events` for quarantines in the window and un-quarantine what the worker lied about;
   treat `sent` rows from that worker as unverified.
5. **The master key is not exposed.** A worker holds no key material and cannot decrypt anything, so this
   is not a §4 incident unless you have separate evidence. Do not rotate the master key reflexively —
   it costs re-entry of every password for no gain here.
6. Re-issue that worker's leaf cert before reusing the host, and mint a **new** registration token; never
   redeploy the old token.

---

## 5. Related

| Doc | For |
|---|---|
| [`security-posture.md`](./security-posture.md) | What is protected, how, and the residual risks — including what a compromised worker actually exposes. |
| [`operator-runbook.md`](./operator-runbook.md) | Diagnosing a live incident. |
| [`install-and-deploy.md`](./install-and-deploy.md) | Roles/grants, env vars, the deploy ordering. |
| [`api-contract.md`](./api-contract.md) | The exact shape of every call referenced above. |
| [`future-work.md`](./future-work.md) | Server-side bulk re-encryption, and the rest of what is deliberately not built. |
