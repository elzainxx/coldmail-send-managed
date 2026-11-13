# Capacity planning

How many sending accounts you need for a given daily volume, how many workers, and — the question
that actually decides a deployment — **which limit binds first**. The short answer is almost always
*per-account daily limits*, not the Control.

[`config-reference.md`](./config-reference.md) is the authoritative source for every key named
below, and the only file the `config_drift` CI gate checks. **Sizing arithmetic cannot be done
without concrete numbers**, so this page does use three of them — declared once, here, rather than
scattered through the prose:

| Key | Value used in this page's arithmetic | Used for |
|---|---|---|
| `daily_limit_default` | 500 sends/account/day | §2's account-count table |
| `send_delay_min_sec` / `send_delay_max_sec` | 3–7s | §2's per-lane rate note |
| `debug_log_retention_days` | 7 days | §5's storage estimate |

These were the seeded defaults when this page was written. They are **not** gate-checked here — only
`config-reference.md` is — so if a number above disagrees with `config-reference.md`, that file wins
and this table is stale. Re-do the arithmetic with your own configured values; the *shape* of each
result (which ceiling binds) is what this page is for, not the specific figures.

---

## 1. The two ceilings, and why they are not the same kind of number

| Ceiling | What sets it | Character |
|---|---|---|
| **Per-account daily volume** | `daily_limit` per account (default from `daily_limit_default`), enforced against a per-account daily counter that resets at `daily_reset_hour` in `daily_reset_timezone` | A **hard, deliberate** cap. Reached in the normal course of business. Raising it is a reputation decision, not a scaling one. |
| **Single-Control send rate** | The Control's own dispatch throughput: **~370 sends/min measured**, ~300–370/min sustained band | A **soft, measured** ceiling on specific hardware. Not a designed limit. |

The measured figure is the 2026-07-26 run in [`load-test-results.md`](./load-test-results.md): 4 vCPU
/ 8 GiB Docker host, 6 real workers, RAM-backed Postgres. Read it with that run's own caveat — the worker
side was likely still a partial co-bottleneck at 6 workers, so ~370/min is *this hardware's practical
ceiling*, not the Control's isolated maximum. Re-run `tests/harness/load_e11.sh` before any material
capacity increase and append a dated entry; do not extrapolate from the table below.

**370 sends/min is ~533,000 sends/day** if you could sustain it for 24 hours. You cannot, and you
would not want to — but it establishes the scale: the Control is not the constraint for any realistic
V1 volume. Accounts are.

---

## 2. How many accounts do I need?

Accounts needed ≈ **daily volume ÷ per-account daily limit**, then add headroom, because an account
that is paused, quarantined, or throttled contributes nothing that day.

At the 500/account/day figure declared above (`daily_limit_default`):

| Target sends/day | Accounts at 500/day (bare minimum) | With ~30% headroom (**deploy this**) | Peak rate needed (over an 8h window) | Which limit binds first |
|---|---|---|---|---|
| 1,000 | 2 | 3 | ~2/min | **Account daily limits.** Control is idle. |
| 5,000 | 10 | 13 | ~10/min | **Account daily limits.** |
| 10,000 | 20 | 26 | ~21/min | **Account daily limits.** |
| 25,000 | 50 | 65 | ~52/min | **Account daily limits.** |
| 50,000 | 100 | 130 | ~104/min | **Account daily limits.** Control at ~30% of measured ceiling. |
| 100,000 | 200 | 260 | ~208/min | **Account daily limits** — but the Control is now at ~55% of measured ceiling. Re-run the load test. |
| 200,000 | 400 | 520 | ~417/min | **The Control binds.** ~417/min exceeds the measured ~370/min. Spread the window, or re-run the load test with more workers before committing. |

The peak-rate column assumes the volume is delivered inside an 8-hour window (`volume ÷ 480 min`),
which is the realistic case for business-hours sending — and it is what turns the Control into a
constraint far earlier than the 533k/day arithmetic suggests. If you send around the clock, divide by
1440 instead and the Control constraint moves out by ~3×.

Two things this table does **not** account for, both of which push the account count up:

- **Pacing.** `send_delay_min_sec`/`send_delay_max_sec` impose a random delay *per lane*, and
  `concurrency_per_user_default` caps lanes per user. With the 3–7s delay declared above, a single lane
  sustains roughly 6–20 sends/min; a user's ceiling is about `lanes × that`. If your measured rate is
  well below the table's peak-rate column, pacing is the reason, not capacity —
  [`operator-runbook.md`](./operator-runbook.md) covers telling those apart.
- **A single-account pool has no rotation.** Account selection walks accounts in a deterministic
  order (exhaust-then-move-on) or round-robin per pool. With one account there is nowhere to move on
  to: one throttle pauses the whole pool.

---

## 3. Why account daily limits almost always bind first

For any volume where a small team would deploy this module, the arithmetic is one-sided: 26 accounts
at the default limit cover 10k/day, and 10k/day is ~21 sends/min — under 6% of the measured Control
ceiling. You will add accounts long, long before you add a second Control (which you cannot do anyway:
the singleton is a locked invariant, and horizontal Control scaling is
[future work](./future-work.md)).

The practical consequence: **treat "the pool is exhausted" as the normal scaling signal, and
"send_rate_per_min has plateaued while workers sit idle and the queue grows" as the rare one.** The
first means add accounts. Only the second means you have hit the Control, and it is the specific
saturation signature the load test documents.

There is one important asymmetry: a pool at its daily limit reports `headroom: "exhausted"` and its
jobs sit in `waiting_for_capacity`, which is **not an error** — those jobs read as clean pending
`queued` work and resume at the daily reset. Do not size to eliminate `waiting_for_capacity`
entirely; size so it clears within the same day.

---

## 4. How many workers?

A worker holds **one job in flight**. That makes worker count a function of send *rate* and per-send
duration, not of daily volume:

```
workers ≈ peak sends/min × (average seconds per send ÷ 60)
```

An SMTP send is typically 1–3 seconds including connect, TLS, AUTH, and DATA. At 100 sends/min with
2s sends, that is ~3.3 concurrently busy workers; round up generously, because a worker waiting on a slow
provider is a worker doing nothing else.

| Peak sends/min | Workers (2s/send, rounded up with headroom) |
|---|---|
| 20 | 2 (minimum for redundancy, not throughput) |
| 50 | 3 |
| 100 | 4–6 |
| 200 | 8–10 |
| 370 (measured ceiling) | 12+ — and at this point re-run the load test |

Floor of **two workers regardless of volume**: one worker is a single point of failure for the entire send
path. Overflow is handled gracefully — when no worker is free, the job is cleanly requeued as
`no_worker_available`, which is backpressure, not a stall — so under-provisioning workers costs throughput,
never correctness.

Workers are cheap and stateless. Over-provision them; they are the least risky thing to add.

---

## 5. Database and storage

Postgres is not a capacity concern at these volumes, but storage growth is worth a moment because one
table dominates it.

- **`send_logs` is the growth driver**, holding the full rendered body per send. At ~10 KB/message
  and 10k sends/day that is ~100 MB/day *while within retention* — steady-state ≈
  `daily volume × message size × debug_log_retention_days`. At the 7 days declared above and those numbers,
  ~700 MB. Content is dropped by the retention sweep after `debug_log_retention_days`; metadata rows
  remain.
- **`job_events` and final job states are permanent by design** and never swept. They are small rows,
  but they grow forever — plan for slow, unbounded growth of a narrow table.
- Retention is a config row, so this is a dial you can turn at runtime with no redeploy. It is also a
  **privacy** decision, not only a storage one — see
  [`security-posture.md`](./security-posture.md) §3.2.

---

## 6. What to measure before committing to a number

Do not size from this page alone. Two checks first:

1. **Re-run the load test on your hardware.** `bash tests/harness/load_e11.sh [N_WORKERES] [MAX_LANES]`,
   with a materially larger `N_WORKERES` than 6 if you are anywhere near 200/min. Append a dated entry to
   [`load-test-results.md`](./load-test-results.md) — it is a versioned artifact, never overwritten.
2. **Confirm your providers' real per-mailbox limits.** The seeded `daily_limit_default` is a
   conservative stand-in.
   The provider's actual limit — and its tolerance for the rate you send at — is the number that
   matters, and exceeding it produces throttling and reputation damage that this module can pause
   around but cannot undo. Note also that a `250` acceptance is all this system can observe: there is
   no bounce data in V1, so provider-side monitoring is what tells you a limit was too aggressive.

## 7. Related

| Doc | For |
|---|---|
| [`load-test-results.md`](./load-test-results.md) | The measured ceiling, the method, and how to re-run it. |
| [`operator-runbook.md`](./operator-runbook.md) | Pool exhausted, `waiting_for_capacity`, and reading the health endpoint. |
| [`config-reference.md`](./config-reference.md) | Every key named on this page. |
| [`future-work.md`](./future-work.md) | Control horizontal scaling and the rest of what is not built. |
