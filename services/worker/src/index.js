// SendManaged Worker entrypoint (Phase 3 — the minimal send path; Phase 7 — outcome buffering; Phase 3
// close-out B1 — the worker never gives up). The worker dials the Control over mTLS, holds a persistent
// control stream (src/stream.js), registers (id + token), heartbeats, and — when the Control pushes a job —
// runs the full SMTP session itself (src/smtp.js). The worker has NO DB access and NO KMS: it receives an
// already-decrypted credential per send, holds it in memory only, and zeroes it after the session
// (CLAUDE.md Rule 6; Phase 3 §3.4/§4.4).
//
// Phase 7 §1.3 (P7-4): on a stream drop the worker RECONNECTS IN-PROCESS (preserving its in-memory
// outcome buffer) and REPLAYS any unacked outcomes on reconnect, rather than exiting.
//
// B1: and it does so FOR EVER. There is no attempt limit and no "could not register, exiting for restart"
// path: a failed dial or a refused register is retried with a capped exponential backoff plus jitter
// (src/reconnect.js) until the worker is shut down. A SIGTERM/SIGINT still exits cleanly, at once, even in
// the middle of a backoff wait.

import { WorkerStream } from "./stream.js";
import { superviseConnection, resolveDials, isLegalDial, RECONNECT_LIMITS } from "./reconnect.js";

function env(k, def) {
  const v = process.env[k];
  return v === undefined || v === "" ? def : v;
}

// envDial reads one optional WORKER_RECONNECT_* override. Unset -> undefined (the Control-delivered value,
// then the built-in default, applies). Set but not a legal integer in the dial's range -> a startup
// configuration error (exit 2, like a missing WORKER_ID), never a silent clamp.
function envDial(name, key) {
  const raw = process.env[name];
  if (raw === undefined || raw === "") return undefined;
  const n = Number(raw);
  if (!isLegalDial(key, n)) {
    const [lo, hi] = RECONNECT_LIMITS[key];
    console.error(`worker: ${name} must be an integer from ${lo} to ${hi} (got "${raw}")`);
    process.exit(2);
  }
  return n;
}

async function main() {
  const workerId = env("WORKER_ID", "");
  const token = env("WORKER_TOKEN", "");
  if (!workerId || !token) {
    console.error("worker: WORKER_ID and WORKER_TOKEN are required (out-of-band provisioned)");
    process.exit(2);
  }

  // Optional operator/harness override of the reconnect dials. When set it WINS over what the Control
  // delivers on register_ack (worker_reconnect_base_ms / _cap_ms / _jitter_pct), so a harness can run the
  // worker at 50 ms / 200 ms from its very first attempt without touching the Control's config.
  const envDials = {
    baseMs: envDial("WORKER_RECONNECT_BASE_MS", "baseMs"),
    capMs: envDial("WORKER_RECONNECT_CAP_MS", "capMs"),
    jitterPct: envDial("WORKER_RECONNECT_JITTER_PCT", "jitterPct"),
  };

  const stream = new WorkerStream({
    host: env("CONTROL_HOST", "control"),
    port: Number(env("CONTROL_STREAM_PORT", "8444")),
    workerId,
    token,
    certPath: env("MTLS_CLIENT_CERT", "/secrets/mtls/worker.crt"),
    keyPath: env("MTLS_CLIENT_KEY", "/secrets/mtls/worker.key"),
    caPath: env("MTLS_CA_CERT", "/secrets/mtls/ca.crt"),
    serverName: env("CONTROL_SERVER_NAME", "control"),
    heartbeatSec: Number(env("WORKER_HEARTBEAT_INTERVAL_SEC", "10")),
    // Per-operation SMTP timeout: the Control seeds smtp_operation_timeout_sec (Phase 6) and would
    // deliver it via push_job; until Phase 6 wires it, the worker uses this bootstrap default (ms).
    smtpTimeoutMs: Number(env("WORKER_SMTP_TIMEOUT_MS", "30000")),
    // Bootstrap outcome-buffer TTL (ms). The Control overrides this on register_ack with the seeded
    // box_outcome_buffer_ttl_sec (config-not-code, P7-4); this env is only the pre-register default.
    outcomeBufferTtlMs: Number(env("WORKER_OUTCOME_BUFFER_TTL_MS", "900000")),
    metadata: {
      box_version: env("WORKER_VERSION", "1.0.0"),
      hostname: env("HOSTNAME", ""),
    },
  });

  const shutdownController = new AbortController();
  const shutdown = () => {
    console.log(`worker ${workerId}: shutdown`);
    shutdownController.abort(); // ends a backoff wait at once
    stream.stop();
    process.exit(0);
  };
  process.on("SIGTERM", shutdown);
  process.on("SIGINT", shutdown);

  // The supervise loop (reconnect.js) dials, registers, waits for the connection to close, and redials —
  // for as long as the worker runs. The SAME WorkerStream instance is reused across every reconnect, so its
  // in-memory outcome buffer survives and unacked outcomes are replayed on the next register_ack (P7-4).
  // It returns only when the worker is stopping.
  await superviseConnection({
    stream,
    // Env override, then what the Control delivered on the last register_ack, then the built-in default.
    getDials: () => resolveDials(envDials, stream.deliveredReconnectDials),
    log: (line) => console.log(`worker ${workerId}: ${line}`),
    signal: shutdownController.signal,
  });
  process.exit(0);
}

main().catch((e) => {
  console.error(`worker: fatal: ${e.message}`);
  process.exit(1);
});
