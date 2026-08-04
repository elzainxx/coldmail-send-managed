# API contract (living mirror of Phase 9 §5)

Authoritative source: **Phase 9 §5** + `history/phase-9-contract-and-doc-completions.md`.
This file mirrors the served surface and is verified by the CI `contract_drift` gate
against the Control's registered routes.

## Auth domains (never share a credential)

- **Domain A** — platform-facing (`quote`/`run`/`report`/`describe`): B7 (Phase 9 contract
  completion) — this surface is called **only by the platform**, service-to-service, over the
  **private network**. Authentication is the **platform service token**,
  `Authorization: Bearer <token>`, stored hashed (bcrypt), constant-time compared, issued/rotated
  by the platform's own service-identity mechanism (out of band to this module). It is **not
  exposed to end users or the public internet** — a control-plane channel between the platform and
  the module, distinct from the operator admin key (CLAUDE.md Rule 6). *(Built in Phase 3 — the
  minimal send path; see the Phase 3 routes below. The surface only mounts when
  `PLATFORM_TOKEN_HASHES` is configured, fail-closed.)*
- **Domain B** — operator (`/v1/admin/*`, incl. `GET /v1/admin/health`): the operator
  `platform_admin` bearer key (Phase 2 §4), env-configured, stored hashed, constant-time compared.
  *(Built in Phase 2 — accounts/pools/config + lifecycle; see the Phase 2 routes below. The Phase 0
  health stub (`/healthz`,`/readyz`) remains unauthenticated.)*
- **Separation invariant (already true, now stated per B7):** the two domains never share a
  credential and never cross-serve — a platform service token cannot reach `/v1/admin/*` and an
  operator key cannot drive `quote`/`run`/`report`/`describe`. Both use the identical
  two-vocabulary error envelope; a missing/invalid credential on either → `401 unauthenticated`.

## The error envelope + the full `error.code` enum (both domains)

Every non-2xx response, on EITHER domain, is exactly this shape — never SQL/SQLSTATE, an internal
reason, a credential, ciphertext, a key id, a stack trace, or an internal hostname:

```json
{ "error": { "code": "validation_failed", "message": "One or more fields are invalid.",
             "details": [ { "field": "smtp_port", "issue": "out_of_range" } ],
             "request_id": "req_01J..." } }
```

`details[]` accompanies field-level rejections — every `validation_failed`, and those
`unprocessable` refusals that can name the field or flag responsible (an immutable field on a
`PATCH`, an illegal account lifecycle transition). It is always optional and always additive:
a client must not require it, and its absence never changes the meaning of `code`.
`details[].issue` ∈ `{required, out_of_range, malformed, not_found, conflict, forbidden_value}`.

**One vocabulary across every surface (the rule a client can build generic per-field messaging on).**
The issue slug distinguishes *why* a field was rejected, and the three rejection classes are never
collapsed into one slug:

| The field was… | `issue` | Example |
|---|---|---|
| absent, `null`, or empty | `required` | `POST /v1/run` with no `user_id`; `POST /v1/quote` with no `recipient_count` |
| present but the wrong TYPE or shape | `malformed` | `user_id: "not-a-uuid"`; `recipient_count: "7"` (a string) |
| present, right type, VALUE outside the allowed domain | `out_of_range` | `recipient_count: -5`; `smtp_port: 99999`; `daily_limit` below the live counter |

A caller must be able to tell "you forgot this field" from "the value you sent is not allowed" — so a
field that WAS supplied is never reported as `required`. A wrong-type value inside an otherwise
well-formed body names the offending field (it is the one class where the JSON parses and exactly one
field is at fault); an UNKNOWN field deliberately gets NO `details[]` entry, because its name is
caller-supplied text and echoing it into a structured field a UI renders is the two-vocabulary hazard
(the `code`/`message`/status are unaffected — still `400 validation_failed`).
*(Both `/v1/quote` behaviours above were fixed 2026-07-28 — before that a negative count reported
`required` and a wrong type reported an empty `details[]`. `details[]` being optional and additive
means neither fix changed the contract's meaning, only its diagnosability.)*

The first row applies to a **required** field. An **optional** field that is absent is not a rejection
at all — but if it is SUPPLIED it is still validated, and a bad value gets the same slug it would get
on a required field. `quote`'s `user_id` is the reference case (fix-plan item 5): omitted → `200`,
`"not-a-uuid"` → `400` with `[user_id:malformed]`. Optional means "you need not send it", never
"anything you send is accepted" — a caller that supplied a typo must be told, not silently given an
answer computed without it.

The full, additive-only `error.code` enum:

| HTTP | `error.code` | When |
|---|---|---|
| 400 | `validation_failed` | Body/query failed field validation (see `details[]`). |
| 400 | `config_ordering_violation` | A config `PUT` would break one of the two ordering invariants (`Docs/config-reference.md`) — enforced at the `PUT` boundary itself (rejected before the value lands) AND independently re-checked at config-LOAD as a defense-in-depth backstop (`Docs/history/open-decisions.md` P9-OD-2, RULED-AND-CLOSED). |
| 401 | `unauthenticated` | Missing/invalid API key/token, either domain. |
| 403 | `forbidden` | Reserved for future scoped roles; unused in V1's single-scope model. |
| 404 | `not_found` | Resource id / config key does not exist, OR belongs to another tenant (no existence disclosure — never `403`). |
| 409 | `conflict` | State/RI conflict (delete pool with accounts; duplicate within pool; limit-below-counter; system user unprovisioned; duplicate active campaign for a user). |
| 422 | `unprocessable` | Well-formed but semantically illegal (illegal state transition, fallback cycle, owner-rule violation). |
| 429 | `rate_limited` | Admin rate limit — off by default in V1. |
| 500 | `internal_error` | Unexpected. Generic message only; reason in logs. |

## Phase 0 served routes (SKELETON only)

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/healthz` | none (Phase 0 stub) | liveness |
| GET | `/readyz` | none (Phase 0 stub) | readiness: DB reachable AND singleton lock held (true state) |

Worker-link channel (mTLS, client cert required; token is the authorization authority):

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/v1/worker/register` | mTLS client cert + registration token | worker registers (id + one-time token) |
| POST | `/v1/worker/heartbeat` | mTLS client cert + prior registration | worker heartbeat |

> Two-vocabulary rule: error envelopes carry only `{code}` (Phase 0) — no internal reason,
> SQL, credential, or hostname. Additive-only: these routes never rename; later phases add.

## Phase 2 served routes — operator admin API (Domain B)

All under `/v1/admin/*`; every route requires the operator `platform_admin` bearer key
(`Authorization: Bearer <key>`, hashed + constant-time compared). Missing/invalid key →
`401 unauthenticated`. The surface only mounts when `ADMIN_API_KEY_HASHES` is configured
(fail-closed). Error envelopes carry ONLY `{code, message, details[], request_id}` — never an
internal reason, SQL, SQLSTATE, credential, ciphertext, key-id, or internal hostname (§1.2 of
`history/phase-2-account-management.md`). Additive-only: these routes never rename; later phases add.

**Accounts** (`send_managed.accounts`; `smtp_secret` is write-only — reads expose only `has_credential`):

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/admin/accounts` | create an account (encrypt-on-write credential intake) |
| GET | `/v1/admin/accounts` | list (cursor paginated; filters `pool_id`/`paused`/`quarantined`/`disabled`/`user_id`) |
| GET | `/v1/admin/accounts/{id}` | read one |
| PATCH | `/v1/admin/accounts/{id}` | update (omit `smtp_secret` = unchanged, structural) |
| DELETE | `/v1/admin/accounts/{id}` | hard delete → `204`. The audit row survives: `send_managed.job_events` and `send_managed.account_events` rows for the account are KEPT, with their `account_id` set to NULL by the FK and every other column byte-unchanged (`send_managed_0024`, ADR-0012). Deleting a **worker** behaves the same way (`box_id` → NULL). *Historical note: from Phase 1 through Phase 10 this returned `500` for any account that had ever been assigned a job — `job_events`' two `ON DELETE SET NULL` FKs were blocked by its unconditional append-only trigger. Fixed 2026-07-29 by `send_managed_0024`; nothing was ever dropped or corrupted, the delete simply aborted atomically. If you see a `500` here, check that migration `0024` is applied* |
| GET | `/v1/admin/accounts/{id}/events` | read the account audit trail (cursor paginated) |

### `POST /v1/admin/accounts` request body

The admin surface decodes **strictly** (`DisallowUnknownFields`) — an unrecognised or misspelled
field name is a hard `400 validation_failed`, not a silently ignored key. So the field set matters:

| Field | Required | Type | Notes |
|---|---|---|---|
| `pool_id` | **yes** | uuid | The account's pool. `user_id` is *derived* from the pool — never sent |
| `smtp_host` | **yes** | string | |
| `smtp_username` | **yes** | string | |
| `smtp_secret` | **yes** | string | **Write-only.** Encrypted on write; never returned by any read. Reads expose `has_credential` only |
| `from_address` | **yes** | string | |
| `smtp_port` | no | int | Defaults per the module's port handling (587/25 STARTTLS, 465 implicit TLS) |
| `from_name` | no | string | |
| `daily_limit` | no | int | Defaults to `daily_limit_default`; capped by the `DailyLimitCeiling` bound |

`PATCH /v1/admin/accounts/{id}` takes the same fields, all optional — and **omitting `smtp_secret`
leaves the stored credential unchanged**, which is structural rather than a convenience: there is no
way to read a credential back, so a partial update cannot round-trip one.

`POST /v1/admin/pools` takes `kind` (**required**, `shared`\|`dedicated`) and `name` (**required**),
plus optional `owner_user_id`, `routing_strategy`, `fallback_pool_id`, `enabled`. `kind` and
`owner_user_id` are immutable after creation.

**Account lifecycle transitions** (manual, audited — one `account_events` row each; `actor='admin'`):

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/admin/accounts/{id}/quarantine` | `quarantined = true` |
| POST | `/v1/admin/accounts/{id}/unquarantine` | `quarantined = false` |
| POST | `/v1/admin/accounts/{id}/disable` | `disabled = true` |
| POST | `/v1/admin/accounts/{id}/enable` | `disabled = false` |
| POST | `/v1/admin/accounts/{id}/pause` | `paused = true, pause_reason = 'manual'` |
| POST | `/v1/admin/accounts/{id}/unpause` | clears only a `pause_reason='manual'` pause |

A refused transition is `422 unprocessable` with **no** `account_events` row written (the
precondition is checked before the write). The refusal is diagnosable without a log dive: the
`message` is a curated sentence specific to the precondition that failed, and `details[]` carries
one entry naming the blocking flag (`quarantined`/`disabled`/`paused`, or `pause_reason` when the
pause exists but is not `manual`) with `issue: "conflict"`. The seven refusals are mutually
distinct — asserted behaviorally, not by convention. Flag names are already public in the account
read model's `status` object, so this discloses nothing new and the two-vocabulary rule still holds:
the internal reason (`already_in_state:*`, `not_in_state:*`, `not_manual_pause`) stays log-only.

**Pools** (`send_managed.pools`; `user_id` derived from `kind`, never a request field):

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/admin/pools` | create (shared/dedicated; owner rules; fallback cycle check) |
| GET | `/v1/admin/pools` | list (cursor paginated; filters `kind`/`owner_user_id`/`enabled`/`user_id`) |
| GET | `/v1/admin/pools/{id}` | read one |
| PATCH | `/v1/admin/pools/{id}` | update (`kind`/`owner_user_id` immutable) |
| DELETE | `/v1/admin/pools/{id}` | delete (RESTRICT if it still has accounts → clean 409) |

**Config** (`send_managed.config`; JSONB `{"v": …}` unwrapped/rewrapped; only `value` writable).

The `PUT` request body is `{"value": <v>}` — the bare JSON value at `value`, **not** the stored
`{"v": …}` envelope, which is an internal storage detail the API unwraps on read and re-wraps on
write. `<v>` is typed per the key's `value_type` (`integer` → a JSON number, `boolean` → `true`/
`false`, `string`/`enum` → a JSON string, `json` → an object). A value of the wrong type is
`400 validation_failed`; the key's `value_type` and legal range are in
[`config-reference.md`](./config-reference.md), which is the only place values are documented:

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/admin/config` | read all keys |
| GET | `/v1/admin/config/{key}` | read one (unknown → 404) |
| PUT | `/v1/admin/config/{key}` | update one (`value_type` + semantic validation). **Config-ordering invariants (`Docs/history/open-decisions.md` P9-OD-2, RULED-AND-CLOSED):** for the four keys with a documented ordering counterpart (`smtp_operation_timeout_sec`/`job_max_processing_time_sec`, `box_outcome_buffer_ttl_sec`/`startup_reconnect_window_sec`), this `PUT` validates the RESULTING pair against the counterpart's LIVE current value BEFORE writing — a write that would invert either invariant is rejected with `400 config_ordering_violation`, naming the conflicting counterpart key, its current value, and the required relation; the bad value never lands. Read-check-write happens in ONE transaction under an advisory lock that serializes all config writes, so two *concurrent* PUTs to the two halves of one pair cannot both pass (one succeeds, one is rejected) — see `Docs/config-reference.md`. The config-LOAD path independently re-checks the same invariant on every tick as a defense-in-depth backstop. Widening BOTH keys of a pair upward requires ordering the two `PUT`s (raise the boundary key first). |

## Phase 9.5 served routes — observability (Domain B)

Same operator admin auth as the routes above (`Authorization: Bearer <admin key>`).

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/admin/health` | read-only operability snapshot (§3 B3): dispatch cadence/pause state, queue counts by state, worker liveness, per-account-status summary, per-pool eligibility headroom, error-class counts over a rolling window, the auto-quarantine shadow-mode summary, and active alert conditions (§4 B4) — all derived from existing tables, no new persisted state |

### `GET /v1/admin/health` response fields

The authoritative field list (`internal/adminapi/health_handler.go`'s `healthResponse`). Every
top-level block is always present; `errors_last_window` always carries all six `error_class` keys
even when a class did not fire, so a poller can index it by name unconditionally.
`oldest_active_heartbeat_age_sec` is the one omittable field (absent when no worker is active).
For how to *interpret* these numbers, see [`operator-runbook.md`](./operator-runbook.md).

| Field | Type | Notes |
|---|---|---|
| `generated_at` | string (RFC 3339 UTC) | when the snapshot was computed |
| `window_sec` | int | the rolling window all `*_last_window` fields cover (`health_metrics_window_sec`) |
| `dispatch.dispatch_paused` | bool | live `dispatch_enabled` inverted |
| `dispatch.ticks_per_min` | float | observed dispatch cadence |
| `dispatch.sends_last_window` | int | sends completed inside `window_sec` |
| `dispatch.send_rate_per_min` | float | derived from the two above |
| `queue.{queued,assigned,sending,sent,failed,bounced}` | int | `COUNT(*) GROUP BY state` over `send_managed.jobs` — one field per `job_state` |
| `queue.waiting_for_capacity` | int | queued jobs whose resolved pool chain has ZERO eligible accounts. **Not an error state** |
| `workers.{active,stale,disabled}` | int | counts by `box_status` |
| `workers.oldest_active_heartbeat_age_sec` | int, **omitted** when no active worker | staleness of the least-recently-heard-from active worker |
| `accounts.{healthy,paused,quarantined,disabled,at_daily_limit}` | int | per-status summary; `at_daily_limit` is derived, not a status |
| `pools[]` | array | `{pool_id, kind, eligible_accounts, headroom}` — `headroom` is a word, not a number |
| `errors_last_window` | object | `{bounce_permanent, transport_4xx, auth_failure, throttle, timeout, unknown}` → int. Always all six keys. Counts **error transitions only** — see the note below |
| `shadow.autoquarantine_mode` | string | `off` \| `shadow` \| `enforce` |
| `shadow.would_quarantine_count` | int | shadow-mode decisions inside the window |
| `shadow.recent[]` | array | `{account_id, score, threshold, at}` |
| `active_conditions[]` | array | `{condition, severity, detail{}, since}` — empty array, never null. `severity` is `critical` for all three V1 conditions |

**`errors_last_window` counts one failure once.** It counts `send_managed.job_events` rows in the window
that are *error transitions* — a terminal `failed` or `bounced`, or a **requeue** (a non-genesis
transition back to `queued`). Forward progress (`queued→assigned`, `assigned→sending`) and success
(`sending→sent`) are never counted, so the number is a count of *failures*, not of rows that happen to
carry a class. Read it as incident size: one failing send contributes exactly 1, no matter how many
times that job is later retried and eventually succeeds.

> Until `send_managed_0022` this was not true. `send_managed_0008`'s triggers copied the job row's **sticky**
> `last_error_class` onto every event, so a single requeue tagged four rows — the requeue plus the
> `queued→assigned`, `assigned→sending`, and `sending→sent` that followed — and this field reported
> `{"unknown": 4}` for one failure. `send_managed_0022` gates the trigger; `queryErrorsInWindow` also filters
> on the transition shape, so the count is correct even over rows an older trigger already wrote
> (`job_events` is append-only and was deliberately not rewritten). Note the two-sided consequence: a
> database still carrying pre-`send_managed_0022` rows returns the correct count from this endpoint, but a
> **hand-written** `COUNT(*) WHERE error_class IS NOT NULL` against `job_events` will still over-count
> on those old rows. Filter on the transition shape in ad-hoc queries too. Pinned by
> `TestHealth_ErrorsLastWindowCountsOnlyErrorTransitions` and
> `TestJobEvents_ErrorContextOnlyOnErrorTransitions`.

### Alert webhook payload (outbound, `alert_webhook_url`)

Not a served route — an outbound POST across a trust boundary, listed here because it is a contract
a receiver depends on. Fires on the **rising edge** of a condition, deduped by
`(condition, subject.id)` until it clears; there is no all-clear POST. Shape:
`internal/adminapi/health_alerts.go`'s `webhookPayload`.

| Field | Type | Notes |
|---|---|---|
| `condition` | string | `pool_exhausted` \| `account_quarantined` \| `worker_death` |
| `severity` | string | `critical` |
| `subject` | object | `{type, id}` — the id of the pool / account / worker the condition is about |
| `detail` | object | condition-specific counts and ids only |
| `fired_at` | string (RFC 3339 UTC) | rising-edge timestamp |
| `module` | string | constant module identifier, so a shared receiver can route |

Deliberately absent, per CLAUDE.md Rule 5 (the webhook is untrusted outbound egress): no message
content, no recipient address, no credential, no internal reason string, no SQL/SQLSTATE, no
internal hostname. A receiver gets condition codes, subject ids, and timestamps — nothing else.
Transport bounds (a 5-second client timeout, 3 attempts, then dropped) are code constants, not
config keys: see `config-reference.md`'s hardcoded-bounds note.

## Phase 3 served routes — platform-facing API (Domain A)

The end-user surface for the minimal send path. Every route requires the platform service token
(`Authorization: Bearer <token>`, hashed + constant-time; distinct from the operator key).
Missing/invalid token → `401 unauthenticated`. The surface only mounts when `PLATFORM_TOKEN_HASHES`
is configured (fail-closed). Error envelopes carry ONLY `{code, message, details[], request_id}` —
never SMTP text, the worker's `error_reason`, an internal reason, SQL/SQLSTATE, a credential, or an
internal hostname (two-vocabulary rule). Additive-only: these routes never rename; later phases add.

> **Per-tenant isolation is ENFORCED (OD-25, RULED option b).** Every platform request carries an
> explicit tenant selector — `user_id` (a UUID): `run` in the body, `report` in the query. The
> module scopes every `send_managed.jobs` read/write `WHERE user_id = <tenant>`. A `run` for a
> `job_item_id` **owned by another tenant**, or a `report` for a `job_id`/`job_item_id` belonging to
> another tenant, returns the SAME `404 not_found` as a non-existent id (never `403`) — so a caller
> can neither read nor confirm the existence of another tenant's jobs. "The same" means the same
> `code`, `message`, AND `details[]`: `report`'s not-found names **the selector the caller sent**
> (`job_id` for a `?job_id=` lookup, `job_item_id` for a `?job_item_id=` lookup), which echoes back
> the caller's own input and so discloses nothing about the row — a cross-tenant miss and a wholly
> nonexistent id remain byte-identical apart from `request_id`. Both halves are asserted together
> behaviorally, since making the error more specific is exactly how existence disclosure gets
> introduced. A missing/malformed
> `user_id` → clean `400 validation_failed`. This is module-level defense-in-depth: even a
> platform-side bug that passes the wrong `user_id` cannot cross tenant boundaries here.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/quote` | recipient count → cost (Phase 9 §1, finalized: `cost = recipient_count × unit_price_credits`, unit read LIVE from `send_managed.config`) — response adds the additive `unit_price_credits`/`tier` fields so the platform's preview/confirm-cost step can show the caller why the number is what it is; pure, side-effect-free, idempotent — reserves nothing and creates no job. **`recipient_count` rejections are distinguishable** (see the `details[]` note above): absent/`null` → `required`; present-but-negative → `out_of_range`; wrong JSON type → `malformed` naming the field. `0` is a VALID count (`cost: 0`), not a rejection. **`user_id` is OPTIONAL, ACCEPTED, AND IGNORED** (additive request field, audit finding **D4**/fix-plan item 5): this module's cost is `recipient_count × unit_price_credits` with no tenant term, so `user_id` cannot and must not change the answer — supplying it returns a **byte-identical** response to omitting it, which is pinned by a test rather than merely documented. It exists so ONE request body works against both senders: module 3's `quote` REQUIRES `user_id` (its answer is a per-user feasibility assessment), both modules decode with `DisallowUnknownFields`, and before this field there was no body both accepted. **Validated if present, never required:** a well-formed-but-unknown UUID is accepted (no tenant lookup — `quote` must not become an existence oracle), a malformed one is `400 validation_failed` with `details[]` = `[user_id:malformed]`, the same slug module 3 and this module's own `run`/`report` use; `""` is treated as absent. Making it required would break existing callers, which additive-only forbids |
| POST | `/v1/run` | accept a job — body `{job_item_id, user_id}` (both UUIDs; `user_id` = owning tenant, REQUIRED) → create one `send_managed.jobs` row, return `202` immediately (async kickoff; never blocks on the send). The `job_item_id` must belong to `user_id` (else `404 not_found`). Duplicate `job_item_id` for the same tenant → clean `200` with the existing job (idempotent; never a raw `23505`). **One active campaign per user (Phase 4 §8, P4-7):** if `user_id` already has a DIFFERENT campaign active (any non-terminal job), the run is rejected outright with `409 conflict` (internal reason `user_campaign_active`) and creates ZERO job rows — enforced under a per-user Postgres advisory lock so concurrent attempts are genuinely serialized, not racy |
| GET | `/v1/report` | reflect a job's state — `?user_id=<uuid>` (REQUIRED tenant selector) plus `?job_id=` or `?job_item_id=`: returns `state`, `provider_message_id` (when `sent`), `completed_at`, `attempt_count` — clean external state only. **`provider_message_id`/`completed_at` are OMITTED (key absent, not `null`) until set** — a UI must check for key presence, not for `null`, on this single-job shape. A job owned by another tenant → `404 not_found` (no existence disclosure). **Phase 5 (§5.4):** a job WAITING for capacity — its only accounts self-recovering (throttle/daily-limit paused), or an unconfigured shared-pool chain — reads as clean pending `state:"queued"` (never `failed`, never an error, even if it waits until the next daily reset); a job whose entire resolved pool is truly exhausted (all accounts quarantined/disabled/manual-paused) reads `state:"failed"`. The internal reason (`pool_exhausted`, which accounts, why) is NEVER surfaced — two-vocabulary rule. **Phase 6 (§2):** a job RECLAIMED from a dead/hung worker (heartbeat-miss or lease-expiry) returns to clean pending `state:"queued"` with an incremented `attempt_count` (saturating) — the internal reclaim reason (`worker heartbeat missed`/`lease expired`, `actor='reclaim_loop'`) stays internal and is never surfaced; a persistently-reclaimed job never fails by attempt count (it fails only on true pool exhaustion, §2.5). **B8 (Phase 9 contract completion) — `?campaign_id=<uuid>` mode:** when `campaign_id` is present (instead of `job_id`/`job_item_id`), `report` returns the CAMPAIGN-scoped aggregate shape below rather than the single-job shape. |

**B8 — the campaign-scoped `report` response** (`GET /v1/report?campaign_id=<uuid>&user_id=<uuid>`),
derived ENTIRELY from `send_managed.jobs` state counts + existing per-job fields — no new state, no
schema change:

```jsonc
{
  "campaign_id": "…",
  "dispatch_paused": false,           // the live dispatch_enabled config, negated (B1)

  "totals": {
    "total": 5000,
    "sent": 4100,
    "bounced": 210,                   // terminal, recipient-fault, billable
    "failed": 3,                      // terminal, infra/pool-exhaustion, refundable
    "in_flight": 20,                  // assigned + sending
    "pending": 667,                   // queued and currently dispatchable
    "waiting_for_capacity": 0         // queued but zero eligible account right now
  },

  "progress": {
    "terminal": 4313,                 // sent + bounced + failed
    "fraction_complete": 0.8626,      // terminal / total
    "is_complete": false              // true iff terminal == total
  },

  "items": {                          // paginated per-job detail, cursor-paginated
    "results": [
      { "job_item_id": "…", "state": "sent",
        "provider_message_id": "…", "completed_at": "…", "attempt_count": 1 }
    ],
    "next_cursor": "opaque-cursor-or-null",
    "page_size": 100
  }
}
```

The three-way billing-relevant distinction a UI can render WITHOUT reading internal state-machine
docs: `waiting_for_capacity` (still pending, no eligible account right now — a job here is
`state:"queued"` at the per-item level, **never** shown as failed), `failed` (terminal, true pool
exhaustion — refundable, the platform's own policy), `bounced` (terminal, recipient-permanent 550
— billable). `waiting_for_capacity` is an AGGREGATE count distinction on top of the same
`state:"queued"` per-job value (§6.4's rule that a waiting job renders as clean pending) — never a
new per-job state. A `campaign_id` that belongs to another tenant, or does not exist, returns a
clean EMPTY report (`total:0`) rather than `404` — a `campaign_id` is not a single-resource
identity the module allocates (the platform mints it), so "zero jobs under this id for this
tenant" is the honest answer regardless of whether the id is foreign or simply not yet run; no
cross-tenant existence signal either way (OD-25's principle, extended). Implemented in
`internal/platformapi/campaign_report.go`.

**Items presence semantics (differs from the single-job shape above):** in `items.results`,
`provider_message_id`/`completed_at` are **ALWAYS present as JSON keys**, valued `null` until set
— unlike the single-job `report` shape, where the same two keys are omitted entirely until set. A
UI integrating against both modes must check for `null` here but for key-absence there.

**Pagination request parameters** (`items`, cursor-paginated on `(created_at, id)`, same keyset
pattern as the admin list endpoints, §5.1): `?campaign_id=<uuid>&user_id=<uuid>&cursor=<opaque,
from a previous response's next_cursor>&page_size=<int>`. `page_size` defaults to **100** and
clamps to a max of **200**; a missing, zero, negative, or non-numeric `page_size` silently falls
back to the default (never a `400`). `cursor` is opaque (base64) — pass back exactly what
`next_cursor` returned; a malformed `cursor` returns `400 validation_failed`. `next_cursor:null`
means the last page.
| GET | `/v1/describe` | module self-description (Phase 9 §2, finalized): `capabilities` (honest — `sequences:false`, no tracking, `content_variation:true`) + `pricing` (`unit_price_credits` — the SAME live value `quote` multiplies by — and `tiers` — `shared`/`dedicated`, the one real V1 axis, **identical unit price in V1**; dedicated is isolation, not a premium — plus **`cost_fields_inert`** (bool, additive, audit finding **D22**/fix-plan item 5b): `true` when `unit_price_credits` is `0`, i.e. when `quote`'s `cost` is structurally zero rather than computed, so a cost-preview UI can tell "this module does not charge per recipient" from "the price lookup returned 0" — both are `200` with `cost:0` otherwise. Normally `false` here (this module charges, seeded `1`), but `unit_price_credits` is live-retunable with no redeploy, so an operator CAN put this module in that state — and under a declared `flat_per_recipient` model a `0` contradicts the model, which makes the declaration more load-bearing here, not less. **DERIVED from the live value on every request, never hardcoded**; the `unitprice_test.go` zero-retune block is the control that a literal `false` cannot pass) + **`model`** (`"flat_per_recipient"` — this module's own answer, unchanged; now emitted from the same constant the declared set below is built from, so the reported model and the declared set cannot be edited apart) + **`models[]`** (fix-plan item 6 — the platform's whole declared set of pricing models, `{flat_per_recipient, flat_subscription, free}`, ADDITIVE-ONLY per Rule 5; `model` above is always a MEMBER of it. `free` is declared though neither sender reports it, deliberately: the point of publishing the set is that a client writes its branch once and handles a module it has not met yet, so declaring only today's two values would make a third module's launch a client-side code change) + **`vocabularies{}`/`error_codes[]`** (fix-plan item 8 — see the dedicated note below) |

**`GET /v1/describe`'s `vocabularies{}` and `error_codes[]`** (fix-plan item 8) — `capabilities` and
`pricing` told a caller what this module can DO and what it CHARGES, but never which VALUES its
responses can carry, so every UI hardcoded `job_state`, the tier ids, and the `details[].issue` slugs
out of prose documentation. That is the per-module special-casing the alignment effort exists to
remove, and worse: a module ADDING an enum value broke those clients silently, which is the exact
failure additive-only versioning was meant to prevent.

- **`vocabularies{}`** — one entry per value set a Domain A response can carry, keyed by the
  vocabulary's name, each `{source, values[], fields[]}`. `source:"db_enum"` entries are read from
  `pg_enum` **LIVE on every request**, in the type's own `enumsortorder` (the order is part of the
  contract — a client rendering a legend of `queued→assigned→sending→sent` sees it): an operator who
  runs `ALTER TYPE … ADD VALUE` changes what `describe` publishes with **no code edit and no
  redeploy**. Two are published: `job_state` (the only enum both `report` modes put in a body) and
  `pool_kind` (the tier axis IS the pool-kind axis — `quote.tier` and `pricing.tiers[].id` — so
  `db_enum` is the truthful description of where those ids come from; a test pins both directions
  against the live type, since `describeTiers` writes them as Go literals).
  `source:"module_constant"` entries are the sets with no DB enum behind them (`details[].issue` and
  `pricing.model`), and their `values[]` reference the SAME Go constants the emitting code path uses —
  never a re-typed literal. `fields[]` names the response paths that carry the value; every leaf is
  asserted to be a real JSON field in the production source, so a renamed response field cannot leave
  a dangling path.
  **FIVE of this module's seven schema enums are DELIBERATELY absent**, each with a recorded reason —
  the exclusion list is the bulk of the accounting here, not a footnote on it, because module 2's
  users own neither accounts, pools, nor workers, so most of the schema's vocabularies describe
  infrastructure a tenant can neither see nor act on: `box_status` (worker-fleet state, operator-only),
  `error_class` (internal failure classification; `report` publishes clean external state only, §6.3
  — a deliberate divergence from module 3, which DOES publish it, because its users own their SMTP
  accounts and can act on a class), `event_actor` (no Domain-A events route in this module),
  `pause_reason` (its OUTCOME is published, as the `waiting_for_capacity` count; the reason is not —
  a tenant cannot unpause an account it does not own), `routing_strategy` (operator-owned pools, so
  there is no per-user settings object to carry it). The accounting is gated in both directions —
  every `send_managed` enum type must be either published or excluded, never both and never neither,
  and an exclusion must carry a substantive reason — so a NEW user-facing enum cannot ship undeclared.
  `details[].issue` declares **FOUR** slugs here, not the six in the union table above: `conflict` and
  `forbidden_value` are `internal/adminapi`'s (config `PUT`s), and `describe` publishes what THIS
  surface can emit, pinned in both directions against the production emission sites.
- **`error_codes[]`** — every code this module can emit with the HTTP status it maps to
  (`[{code, http_status}]`, sorted by status then code), **DERIVED by walking the same `codeStatus`
  map `writeError` resolves a status from**. It therefore cannot list a code the module cannot emit,
  nor omit one it can. This surface emits **five** (`validation_failed`, `unauthenticated`,
  `not_found`, `conflict`, `internal_error`) — a subset of the union table above, which also carries
  `adminapi`'s `config_ordering_violation` and the reserved `forbidden`/`unprocessable`/
  `rate_limited`. Module 3 emits seven at the same `/v1`; the two sets differ legitimately (it has a
  batch endpoint and a rate-limited test button), and a client that can read both lists needs no
  per-module knowledge to handle either. The status is what makes the list actionable for a client
  meeting an unrecognised code: it degrades by status. The fixed **message is deliberately NOT
  published** — it is operator-facing English rather than a branchable value, and publishing it would
  invite clients to match on message text, the coupling the coded envelope exists to eliminate.
- **`describe` FAILS CLOSED on a vocabulary error**, returning `internal_error` rather than a partial
  set — the deliberate opposite of `unit_price_credits`, which falls back to its seeded default. A
  tunable integer has an honest default; a vocabulary does not, since the only fallback available is a
  hardcoded list, which is precisely the stale-declaration failure this field exists to remove. A
  client told "these are the values" would be misinformed rather than merely unserved, and `describe`
  is a discovery call, not the send path. Per the two-vocabulary rule the offending type name appears
  only in the internal reason logged against the `request_id`, never on the wire.

> The worker↔Control send channel is a THIRD, internal machine-to-machine channel (mTLS, worker-initiated
> persistent stream on the workerlink stream port) carrying
> `register`/`register_ack`/`heartbeat`/`push_job`/`report`/`report_ack`. It is not a platform-facing
> HTTP route and is not part of this contract surface; its security properties are covered by the
> Phase 3 spec (§2/§3) and the behavioral suite.
>
> **Phase 7 (§1.3, P7-4) — the outcome persist-ack contract (additive frame `report_ack`).** The
> Control sends `report_ack {job_id, request_id, accepted}` back to the worker ONLY AFTER the reported
> outcome is durably committed to the DB (or determined a clean idempotent no-op the Control already
> owns). Until it receives that ack, the worker RETAINS the outcome and replays it on (re)connection,
> bounded by `box_outcome_buffer_ttl_sec`. One mechanism covers stream-drop, Control-restart, and
> DB-outage recovery: no outcome is lost to a blip, and idempotency (`provider_message_id UNIQUE` +
> the pre-persist check) makes a replay a no-op, never a duplicate. Two-vocabulary: the ack carries
> only `{job_id, request_id, accepted}` — never an internal reason. The Control NEVER acks an outcome
> it did not persist (Rule 5).
>
> **Phase 8 Chunk 1 (P7-OD-6) — per-attempt identity (additive wire field `attempt_token`).** Both
> `push_job` and `report` carry an additive integer `attempt_token`. The Control mints it inside the
> guarded `assigned → sending` transition (`send_managed.jobs.attempt_token`, migration `send_managed_0020`:
> monotonic, NON-saturating, one increment per dispatched attempt) and the worker echoes it back
> VERBATIM — the worker never mints, defaults, or increments one. The Control REJECTS a report whose token
> is a PROVABLE mismatch with the job's current attempt (both values non-zero and different): the report
> is acked (so the worker stops replaying a permanently-inapplicable outcome) but applies NOTHING — no
> state change, no `send_logs` row, no `job_events` row, no account scoring. This closes the case the
> Phase 7 worker-attribution guard could not decide: a stale buffered outcome from attempt 1 replayed
> after the job was reclaimed and re-dispatched to the SAME worker. An absent or zero token on either
> side is treated as UNKNOWN, never a mismatch, so a worker or job row that predates the field degrades
> to exactly Phase 7 behavior rather than having legitimate outcomes discarded. Two-vocabulary is
> unchanged: the token is an internal machine-to-machine identifier on the worker channel and is never
> surfaced on any client boundary (`/v1/report` still returns only clean external state).
>
> **Phase 8 Chunk 2 (§2.2, P8-2/P8-3) — the additive `variation` block on `push_job`.** `push_job`
> gains one optional field, `variation`: the Control's RESOLVED content-variation toggle snapshot
> (plain booleans/strings/arrays — no `send_managed.config` shape, no logic) for the worker's MIME-assembly
> pipeline to apply. It is per-job-fresh (the Control builds it fresh for every `push_job`, never
> cached) and GLOBAL in V1 (one operator-owned toggle set applied to every send, §3). The block is
> ABSENT, or present with every field at its documented neutral default, exactly when Phase 3
> behavior is intended: the worker's variation layer is then the IDENTITY FUNCTION and the bytes it
> puts on the wire are byte-for-byte the Phase 3 output (the headline safety property, §4). The worker
> never reads `send_managed.config` and holds no variation state across jobs — it applies exactly the
> values this block carries. Two-vocabulary is unchanged: the block carries no credential, no
> internal reason string, and never appears on any client-facing boundary. Chunk 2 shipped the wire
> shape and the worker-side identity-function pipeline with the Control stamping a literal Phase-3-neutral
> snapshot (no `send_managed.config` row existed yet for these keys); Chunk 5 seeded the toggle/intensity
> keys into `send_managed_0020` and switched the Control to read them for real — this wire contract did not
> change when it did. **Amended 2026-07-26 (P8-OD-1 RULED-WIDEN):** the block gained three additional
> fields (`css_magnitude_px`, `zero_width_rate`, `user_agent_rate`), promoting what were fixed-in-code
> rates/magnitudes to operator-tunable parameters; those three fields deliberately omit `omitempty`
> so an operator-set value of exactly `0` reaches the worker as a real `0` rather than collapsing to the
> worker's own internal default via the omitted-field fallback. Growing `send_managed_0020` from fourteen keys
> to seventeen.
>
> **Amended 2026-07-26 (Phase 9.5 S11, §2.4, P9.5-OD-3 RULED) — two more additive fields:**
> `comment_intensity_rates` (object `{low,medium,high}`, percent 0–100 — the per-bucket comment-
> injection rate table, replacing the worker's own hardcoded `{low:5,medium:15,high:30}`) and
> `css_skip_below_px` (integer, `omitempty` — the px value below which `css_micro` leaves a
> padding/margin declaration untouched, replacing the worker's own hardcoded `<3px`). **P9.5-OD-3**
> (recorded in `Docs/history/open-decisions.md`): `send_managed_0021`'s `variation_zero_width_rate_pct` and
> `variation_css_micro_params.delta_px` are documented DUPLICATES of the already-wired
> `variation_zero_width_rate`/`variation_css_magnitude_px` (P8-OD-1) under a different unit/shape —
> ruled to wire only the two genuinely-new sub-values (the rate table, the skip-below threshold) onto
> the existing keys' already-established delivery mechanism, and leave the two duplicate parameters
> unconsumed rather than introduce a second, competing source of truth for the same worker behavior.

## What is NOT part of the public REST contract

The worker↔Control protocol (mTLS channel, `register`/`register_ack`/`heartbeat`/`push_job`/`report`/
`report_ack`, including the additive `variation`/`attempt_token`/`smtp.operation_timeout_sec`
fields documented above) is internal machine-to-machine wire format. It is **not** part of either
public surface (Domain A or Domain B) and evolves independently of the additive-only REST
versioning rule — a wire field can be added the moment the Control and worker images that use it are
both deployed, with no coordination against `quote`/`run`/`report`/`describe` or `/v1/admin/*`.
None of `quote`/`run`/`report`/`describe`/`/v1/admin/*` ever return a worker-protocol field verbatim.

## S9 — `send_logs` at-rest governance (Phase 9 contract completion, documentation only)

`send_logs` retains full email bodies + recipients in **plaintext** for the retention window
(default 7d, `debug_log_retention_days`) — **accepted for V1**, with rationale: it is the user's
own campaign material (the body/recipient list they themselves supplied), not a secret the module
holds on their behalf (unlike an SMTP credential, which is the module's custody and stays
encrypted). It is retention-bounded (TTL-swept, Phase 7 §3) and reachable only via
**access-controlled direct DB** — **no operator or platform API returns `send_logs` content**;
this document's every endpoint above confirms that (no route selects from `send_logs`). At-rest
encryption of `send_logs.body`/`recipient` is a named **post-V1 hardening item**, not built now.
This is a documentation ruling only — no behavior, schema, or endpoint change.
