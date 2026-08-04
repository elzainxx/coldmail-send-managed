// SendManaged Worker entrypoint (Phase 3 — the minimal send path; Phase 7 — outcome buffering). The worker
// dials the Control over mTLS, holds a persistent control stream (src/stream.js), registers
// (id + token), heartbeats, and — when the Control pushes a job — runs the full SMTP session itself
// (src/smtp.js). The worker has NO DB access and NO KMS: it receives an already-decrypted credential
// per send, holds it in memory only, and zeroes it after the session (CLAUDE.md Rule 6; Phase 3
// §3.4/§4.4).
//
// Phase 7 §1.3 (P7-4): on a stream drop the worker now RECONNECTS IN-PROCESS (preserving its in-memory
// outcome buffer) and REPLAYS any unacked outcomes on reconnect, rather than exiting. This is the
// minimum P7-4 contract — in-memory retention across a STREAM reconnect (the common blip / Control-
// restart case). Surviving a worker PROCESS restart via on-disk buffer persistence is P7-6 (FLAGGED,
// NOT built). A SIGTERM/SIGINT still exits cleanly.

import { WorkerStream } from "./stream.js";

function env(k, def) {
  const v = process.env[k];
  return v === undefined || v === "" ? def : v;
}

async function main() {
  const workerId = env("WORKER_ID", "");
  const token = env("WORKER_TOKEN", "");
  if (!workerId || !token) {
    console.error("worker: WORKER_ID and WORKER_TOKEN are required (out-of-band provisioned)");
    process.exit(2);
  }

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

  const shutdown = () => {
    console.log(`worker ${workerId}: shutdown`);
    stream.stop();
    process.exit(0);
  };
  process.on("SIGTERM", shutdown);
  process.on("SIGINT", shutdown);

  // connectOnce dials + registers with bounded retry. SAFETY BOUND (Phase 4 closeout triage,
  // Docs/history/open-decisions.md): a startup-ordering retry, not a runtime dial — a worker that can't register
  // within ~30 attempts is a deployment-ordering problem, not something an operator tunes.
  // A retry attempt is ~1s, so the budget is ~30s of dialling. That bound is a STARTUP-ORDERING
  // guard, and it must not be allowed to destroy buffered outcomes: exiting throws away the
  // in-memory buffer, and §1.3 says an outcome is dropped only on "failure to reconnect within the
  // TTL" (900s default) — not after 30s. So whenever unacked outcomes are still inside their TTL,
  // connectOnce keeps retrying past the budget instead of returning failure. Still bounded: entries
  // age out via pendingOutcomeCount()'s prune, after which the budget applies again and the worker exits
  // for the orchestrator to restart it.
  const connectOnce = async () => {
    for (let attempt = 1; ; attempt++) {
      if (stream.stopped) return false;
      try {
        await stream.start();
        console.log(`worker ${workerId}: registered over mTLS stream; awaiting pushed jobs (buffered outcomes replayed on connect)`);
        return true;
      } catch (e) {
        if (attempt >= 30) {
          const pending = stream.pendingOutcomeCount();
          if (pending === 0) return false;
          if (attempt % 30 === 0) {
            console.error(`worker ${workerId}: still cannot register after ${attempt} attempts, but ${pending} unacked outcome(s) remain inside the buffer TTL — continuing to retry rather than exiting and losing them (§1.3)`);
          }
        } else {
          console.log(`worker ${workerId}: connect attempt ${attempt} not ready (${e.message}); retrying`);
        }
        await new Promise((r) => setTimeout(r, 1000));
      }
    }
  };

  // Supervise loop (Phase 7 §1.3, P7-4): on a stream drop, RECONNECT IN-PROCESS — the same WorkerStream
  // instance is reused, so its in-memory outcome buffer survives the drop and unacked outcomes are
  // replayed on the next register_ack. We do NOT exit on a mere stream close anymore (that would lose
  // the buffer and defeat the no-duplicate guarantee). Only a failed initial/repeated connect (Control
  // unreachable for the whole retry budget) exits non-zero for the orchestrator to restart us.
  while (!stream.stopped) {
    const ok = await connectOnce();
    if (!ok) {
      console.error(`worker ${workerId}: could not (re)register within the retry budget; exiting for restart`);
      process.exit(1);
    }
    // Wait for THIS connection to close, then loop to reconnect (buffer intact).
    await new Promise((resolve) => {
      if (!stream.sock) return resolve();
      stream.sock.once("close", () => {
        if (!stream.stopped) console.error(`worker ${workerId}: control stream closed; reconnecting (outcome buffer retained)`);
        resolve();
      });
    });
  }
}

main().catch((e) => {
  console.error(`worker: fatal: ${e.message}`);
  process.exit(1);
});
