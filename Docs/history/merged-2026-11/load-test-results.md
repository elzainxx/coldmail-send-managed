# Load test results — E11 single-Control throughput ceiling

Recorded, versioned results for **E11** (`Docs/history/phase-9_5-operability-and-control-plane-safety.md`
§7 / `Docs/history/phase-0-groundwork.md` §4.7). This is a **before-you-SCALE gate, not a launch
blocker** — the numbers below are a measured ceiling on THIS run's hardware/config, not a
pass/fail. Re-run `tests/harness/load_e11.sh` before any material capacity increase and append a
new dated entry (OD-7 ruling: committed as a versioned doc artifact, never overwritten).

## How to reproduce

```bash
COMPOSE_INVOCATION="docker compose -f docker-compose.yml -f tests/harness/compose.test.yml" \
  bash tests/harness/load_e11.sh [N_WORKERES] [MAX_LANES]
```

The script provisions `N_WORKERES` real, distinctly-identified worker processes (OD-15 option b — a
shared worker identity would make the workers NOT independent, defeating "the worker side is never the
bottleneck"), over-provisions accounts against the real Mailpit sink, zeroes the Phase 6 per-lane
pacing delay (`send_delay_min_sec`/`send_delay_max_sec` — otherwise that dial, not the Control, sets
the observed ceiling) and shrinks `health_metrics_window_sec` to the measurement window so
`send_rate_per_min` isn't diluted into "per hour". Both are restored on exit. It ramps
`concurrency_per_user_default` across `1, 2, 4, 8, MAX_LANES` steps, seeding `C` synthetic
users × 200 queued jobs per step (`C == lanes`, so per-user concurrency growth and total fan-out
grow together), and polls `GET /v1/admin/health` after a fixed window each step.

## 2026-07-26 — dev host, N_WORKERES=6, MAX_LANES=12

**Hardware.** Docker Desktop dev host, 4 vCPU / 8 GiB RAM allocated to the Docker VM. RAM-backed
Postgres (`tests/harness/compose.test.yml`'s `fsync=off`). Real Control, real worker processes, real
Mailpit sink — no mocks.

**Config held at seeded defaults** except the two temporarily zeroed for the run (`send_delay_min_sec`,
`send_delay_max_sec` — restored after): `dispatch_tick_interval_ms=1000`, `reclaim_tick_interval_ms=15000`,
`job_max_processing_time_sec` at its seeded default.

| lanes (`concurrency_per_user_default`) | users (C) | `send_rate_per_min` | `ticks_per_min` | `queue.queued` at measurement | `boxes.active` |
|---|---|---|---|---|---|
| 1  | 1  | 46  | 50 | 177  | 6 |
| 2  | 2  | 194 | 46 | 455  | 6 |
| 4  | 4  | 238 | 50 | 882  | 6 |
| 8  | 8  | 370 | 54 | 1820 | 6 |
| 12 | 12 | 322 | 40 | 3459 | 6 |

**Max observed send rate: 370 sends/min**, at lanes=8/users=8/workers=6.

**Saturation observed at lanes=12, users=12, workers=6** — throughput regressed from 370/min to
322/min despite MORE concurrency and workers still reporting active, the §7-documented saturation
signal ("backlog growth despite idle workers... Control-bound, not capacity/account-bound"). At only
6 workers, worker-side send throughput itself becomes a co-bottleneck before 12 lanes, so this run's
322-370/min band should be read as **this hardware's practical ceiling with 6 workers**, not a firm
upper bound on the Control's own per-tick capacity — a re-run with more workers (the method's own N
parameter) would separate the two more cleanly. Recorded as the honest result of THIS run rather
than extrapolated.

**Backlog drain note.** At lanes≥4 the seeded backlog did not fully drain within the script's
120s bound before the next ramp step began (each step seeds 200×C fresh jobs on top of whatever
the previous step left in flight) — expected at this worker count and not itself a defect; it is
consistent with, and part of the evidence for, the saturation point recorded above.

**Input to the HA/sharding decision (§7's stated purpose):** at this hardware/worker-count, the
single Control sustains on the order of **300-370 sends/min** before the ramp's own concurrency
stops buying more throughput. A future capacity decision should re-run this test with a
materially larger `N_WORKERES` (the worker side was very likely still a partial co-bottleneck here)
before treating this number as the Control's true isolated ceiling.
