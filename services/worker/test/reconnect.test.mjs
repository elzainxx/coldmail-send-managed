// Phase 3 close-out B1 — THE WORKER NEVER GIVES UP. Behavioural tests of src/reconnect.js (the supervisor and
// the backoff) over the REAL src/stream.js WorkerStream, a REAL local "fake Control" (a net.Server) and, for the
// last group, the REAL src/index.js entrypoint as a child process. The clock, the random source and the dialer are
// injected where the property under test is about THEM (the backoff sequence, 200 failed dials); nothing about the
// worker's own logic is replaced. Only the TLS wrap is substituted (the `dial` seam buffer.test.mjs documents):
// mutual TLS is proven by the live-stack suites.
//
// What each group pins, and the paired control that makes it able to fail:
//   1  the backoff: exponential, capped, jittered, never above cap*(1+jitter)   <- the same checker is run on three
//      deliberately WRONG implementations (linear, uncapped, jitter past the cap) and must reject each, for its reason
//   2  the attempt counter resets after a success
//   3  a dialer that fails 200 times, then succeeds: the worker is still running and then WORKS
//      <- the old ~30-attempt give-up is re-created (legacySupervise) and must be seen to exit, so "worker exited"
//         is a failure this test can actually report; with a limit back in reconnect.js this test fails with it
//   4  a shutdown during a backoff wait exits promptly                          <- a sleep that ignores the signal is measured too
//   5  a REFUSED register is retried, not fatal                                 <- stream.stop() (shutdown) still stops
//   6  a Control that accepts but never answers is a failed attempt, not a hang <- a prompt Control is not timed out
//   7  the Control-delivered dials are adopted when legal and ignored when not; the minimum/maximum legal values
//   8  index.js itself: a bad dial in the environment is a startup error; with no Control the process is still
//      running after more than 30 failed attempts
//
// Run: node --test services/worker/test/reconnect.test.mjs   (Node 24)

import { test } from "node:test";
import assert from "node:assert/strict";
import net from "node:net";
import crypto from "node:crypto";
import path from "node:path";
import { spawn } from "node:child_process";
import { EventEmitter } from "node:events";
import { fileURLToPath } from "node:url";
import { CERTS, failingSocket, instantSleep, makeStream, startFakeControl, waitFor } from "./control-helpers.mjs";
import {
  RECONNECT_DEFAULTS, RECONNECT_LIMITS, backoffDelayMs, isLegalDial, resolveDials, shouldLogFailure,
  superviseConnection,
} from "../src/reconnect.js";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const INDEX_JS = path.resolve(__dirname, "../src/index.js");

const PROD = { baseMs: 1000, capMs: 30000, jitterPct: 20 };
const HARNESS = { baseMs: 50, capMs: 200, jitterPct: 20 };
const MIN_LEGAL = { baseMs: 10, capMs: 10, jitterPct: 0 };

// A push_job whose lease is already expired: the worker answers with an error report at once, no SMTP.
const expiredPushJob = (jobId) => ({
  type: "push_job", request_id: "req-" + jobId, job_id: jobId,
  send: { recipient: "x@example.com", from_address: "u@example.org", subject: "s", body: "b", headers: {} },
  smtp: { host: "127.0.0.1", port: 1, username: "u", secret: "SECRET", security: "starttls" },
  lease_expires_at: new Date(Date.now() - 60_000).toISOString(),
});

// ===== 1. the backoff sequence ====================================================================================

// assertBackoffProperties is the checker for property 1. fn(n, rand) -> ms.
function assertBackoffProperties(fn, dials, label) {
  const j = dials.jitterPct / 100;
  let reachedCap = false;
  for (let n = 1; n <= 80; n++) {
    const d = Math.min(dials.capMs, dials.baseMs * 2 ** (n - 1));
    const lo = fn(n, () => 0);
    const hi = fn(n, () => 0.999999);
    assert.equal(lo, d, `${label}: attempt ${n} without jitter must be exactly min(cap, base*2^(n-1)) = ${d}, got ${lo} (not exponential/capped)`);
    assert.ok(hi >= lo, `${label}: attempt ${n}: jitter made the wait SHORTER than ${lo} (got ${hi})`);
    assert.ok(hi <= Math.ceil(d * (1 + j)), `${label}: attempt ${n}: the wait ${hi} ms exceeds d*(1+jitter) = ${d * (1 + j)}`);
    assert.ok(hi <= dials.capMs * (1 + j), `${label}: attempt ${n}: the wait ${hi} ms exceeds cap*(1+jitter) = ${dials.capMs * (1 + j)}`);
    if (d * j >= 2) assert.ok(hi > lo, `${label}: attempt ${n}: jitter is configured (${dials.jitterPct}%) but the wait never varies`);
    if (d === dials.capMs) reachedCap = true;
  }
  assert.ok(reachedCap, `${label}: the sequence never reached the cap`);
  // exponential: while the next rung is still below the cap, each wait is exactly twice the one before
  for (let n = 1; dials.baseMs * 2 ** n <= dials.capMs; n++) {
    assert.equal(fn(n + 1, () => 0), 2 * fn(n, () => 0), `${label}: attempt ${n + 1} is not double attempt ${n}`);
  }
}

const realBackoff = (dials) => (n, rand) => backoffDelayMs(n, dials, rand);

test("B1 backoff: exponential, capped, jittered, never above cap*(1+jitter) — production, harness and minimum-legal dials", () => {
  for (const [label, dials] of [["production", PROD], ["harness", HARNESS], ["defaults", RECONNECT_DEFAULTS]]) {
    assertBackoffProperties(realBackoff(dials), dials, label);
  }
  // The documented production sequence, spelled out (1 s base, 30 s cap): 1, 2, 4, 8, 16, then 30 for ever.
  assert.deepEqual([1, 2, 3, 4, 5, 6, 7, 50, 5000].map((n) => backoffDelayMs(n, PROD, () => 0)), [1000, 2000, 4000, 8000, 16000, 30000, 30000, 30000, 30000]);
  // ...and the harness sequence (50 ms base, 200 ms cap): 50, 100, 200, 200, ...
  assert.deepEqual([1, 2, 3, 4, 40].map((n) => backoffDelayMs(n, HARNESS, () => 0)), [50, 100, 200, 200, 200]);
  // The minimum legal dials (10 ms / 10 ms / 0 %): a constant 10 ms, whatever the random source says.
  for (const u of [0, 0.5, 0.999999]) assert.equal(backoffDelayMs(7, MIN_LEGAL, () => u), 10);
  // The documented ceiling: with production dials no draw ever exceeds 30 s * 1.2 = 36 s.
  for (let n = 1; n <= 200; n++) assert.ok(backoffDelayMs(n, PROD, () => 0.9999999) <= 36000);
  // With the real random source too (a statistical check of the same floor and the same bound).
  for (let n = 1; n <= 500; n++) {
    const v = backoffDelayMs(n, PROD);
    const d = Math.min(30000, 1000 * 2 ** Math.min(n - 1, 60));
    assert.ok(v >= d && v <= Math.ceil(d * 1.2), `attempt ${n}: ${v} ms is outside [${d}, ${d * 1.2}]`);
  }
});

test("B1 backoff — paired control: the SAME checker rejects a linear, an uncapped and a jitter-past-the-cap implementation, each for its own reason", () => {
  const linear = (n, rand) => Math.round(Math.min(PROD.capMs, PROD.baseMs * n) * (1 + 0.2 * rand()));
  const uncapped = (n, rand) => Math.round(PROD.baseMs * 2 ** Math.min(n - 1, 60) * (1 + 0.2 * rand()));
  const jitterPastCap = (n, rand) => Math.round(Math.min(PROD.capMs, PROD.baseMs * 2 ** (n - 1)) * (1 + 2 * 0.2 * rand()));
  const noJitter = (n) => Math.min(PROD.capMs, PROD.baseMs * 2 ** (n - 1));
  assert.throws(() => assertBackoffProperties(linear, PROD, "linear"), /not exponential\/capped/);
  assert.throws(() => assertBackoffProperties(uncapped, PROD, "uncapped"), /not exponential\/capped/);
  assert.throws(() => assertBackoffProperties(jitterPastCap, PROD, "jitterPastCap"), /exceeds d\*\(1\+jitter\)/);
  assert.throws(() => assertBackoffProperties(noJitter, PROD, "noJitter"), /the wait never varies/);
});

test("B1 log rate: the first failed attempt and then every 10th — and no other", () => {
  const logged = [];
  for (let n = 1; n <= 200; n++) if (shouldLogFailure(n)) logged.push(n);
  assert.deepEqual(logged.slice(0, 4), [1, 10, 20, 30]);
  assert.equal(logged.length, 21, "200 consecutive failures must log 21 lines, not 200");
});

// ===== 2. the attempt counter resets after a success ================================================================

class FakeStream {
  constructor(script) {
    this.script = script.slice();
    this.stopped = false;
    this.sock = null;
    this.starts = 0;
    this.whenStopped = new Promise((r) => { this._resolveStopped = r; });
  }
  start() {
    this.starts++;
    const step = this.script.shift();
    if (step === "ok") {
      const sock = new EventEmitter();
      sock.destroyed = false;
      this.sock = sock;
      setImmediate(() => { sock.destroyed = true; sock.emit("close"); }); // the connection drops
      return Promise.resolve(true);
    }
    if (step === "stop") { this.stop(); return Promise.reject(new Error("stopped")); }
    return Promise.reject(new Error("dial failed (test)"));
  }
  stop() { this.stopped = true; this._resolveStopped(); }
}

test("B1 reset: after a success the next outage starts again at the base delay", async () => {
  const stream = new FakeStream(["fail", "fail", "fail", "fail", "fail", "ok", "fail", "fail", "fail", "ok", "stop"]);
  const sleeps = [];
  const dials = { baseMs: 100, capMs: 1600, jitterPct: 0 };
  await superviseConnection({ stream, getDials: () => dials, sleep: async (ms) => { sleeps.push(ms); }, rand: () => 0, log: () => {} });
  // 5 failures: 100 200 400 800 1600 | success, drop: one base wait | 3 failures: 100 200 400 (NOT 1600 1600 1600) | success, drop: base | stop
  assert.deepEqual(sleeps, [100, 200, 400, 800, 1600, 100, 100, 200, 400, 100]);
  assert.equal(stream.starts, 11);
});

// ===== 3. 200 failed dials, then success: still running, and it works ===============================================

// The OLD supervise loop, re-created: ~30 attempts, then give up (the worker exited 1 for its supervisor to restart).
async function legacySupervise(stream) {
  for (let attempt = 1; ; attempt++) {
    if (stream.stopped) return "stopped";
    try { await stream.start(); return "registered"; } catch {
      if (attempt >= 30 && stream.pendingOutcomeCount() === 0) return "gave up (worker exited 1)";
      await instantSleep();
    }
  }
}

test("B1 paired control: the OLD behaviour (a limit of ~30 attempts) IS seen as 'worker exited' by this suite's detector", async () => {
  const control = await startFakeControl();
  let dials = 0;
  const stream = makeStream(control.port, { dial: (o, t, onSecure) => { dials++; return failingSocket(); } });
  const verdict = await legacySupervise(stream);
  await control.close();
  assert.match(verdict, /worker exited/);
  assert.equal(dials, 30, "the legacy loop must stop at exactly its 30th failed attempt");
});

test("B1: a dialer that fails 200 times then succeeds — the worker is STILL RUNNING, then registers, then WORKS (and logs a bounded number of lines, never the token)", async () => {
  const FAILS = 200;
  const token = crypto.randomBytes(12).toString("hex"); // generated at run time; must never reach a log line
  const reports = [];
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      if (msg.type === "register") {
        conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true, outcome_buffer_ttl_sec: 900 });
        setImmediate(() => conn.reply(expiredPushJob("job-after-outage")));
      }
      if (msg.type === "report") reports.push(msg);
    },
  });
  let dials = 0;
  const stream = makeStream(control.port, {
    token,
    dial: (o, t, onSecure) => {
      dials++;
      return dials <= FAILS ? failingSocket() : net.connect({ host: o.host, port: o.port }, onSecure);
    },
  });
  const logs = [];
  let exited = false;
  const supervised = superviseConnection({ stream, getDials: () => MIN_LEGAL, sleep: instantSleep, log: (l) => logs.push(l) }).then(() => { exited = true; });

  // Wait until every failed attempt has happened (or the supervisor has returned, which is the failure).
  await waitFor(() => dials > FAILS || exited, 15000);
  assert.equal(exited, false, `worker exited after ${dials} dial attempts — a reconnect limit is back (the old worker gave up at ~30)`);
  assert.ok(dials >= FAILS, `only ${dials} attempts were made`);

  // It then registers (attempt 201) and does its job: the Control pushes a job, the worker reports on it.
  assert.ok(await waitFor(() => reports.length === 1), "after 200 failed dials the worker never registered and answered a job");
  assert.equal(reports[0].job_id, "job-after-outage");
  assert.equal(dials, FAILS + 1, "exactly 200 failures and then one successful dial");
  assert.equal(exited, false, "the worker must still be running after it reconnected");

  // Bounded log rate (21 failure lines for 200 failures, not 200) and a recovery line; no secret anywhere.
  const failureLines = logs.filter((l) => /consecutive failed attempt/.test(l));
  assert.equal(failureLines.length, 21, `expected 21 failure log lines for 200 failures, got ${failureLines.length}`);
  assert.match(failureLines[0], /^control unreachable or register refused \(.*\); 1 consecutive failed attempt\(s\)/);
  assert.ok(logs.some((l) => /reconnected after 200 failed attempt\(s\)/.test(l)), "no 'reconnected after 200 failed attempt(s)' line");
  assert.ok(!logs.join("\n").includes(token), "a log line contained the registration token");

  stream.stop();
  await supervised;
  await control.close();
});

// ===== 4. shutdown during a backoff wait is prompt ==================================================================

async function shutdownLatency({ sleep, stopVia }) {
  const stream = makeStream(1, { dial: () => failingSocket() });
  const controller = new AbortController();
  const logs = [];
  const dials = { baseMs: 60000, capMs: 600000, jitterPct: 0 }; // the first backoff wait is a full minute
  const done = superviseConnection({ stream, getDials: () => dials, sleep, rand: () => 0, signal: controller.signal, log: (l) => logs.push(l) });
  assert.ok(await waitFor(() => logs.length === 1, 3000), "the first failed attempt was never logged, so the worker is not in a backoff wait");
  const t0 = Date.now();
  if (stopVia === "signal") controller.abort(); else stream.stop();
  await done;
  return Date.now() - t0;
}

test("B1 shutdown: a SIGTERM-style abort during a 60 s backoff wait ends the worker promptly", async () => {
  const ms = await shutdownLatency({ sleep: undefined, stopVia: "signal" }); // the real timer sleep
  assert.ok(ms < 1000, `the supervisor took ${ms} ms to stop during a backoff wait`);
});

test("B1 shutdown: stream.stop() during a 60 s backoff wait ends the worker promptly too", async () => {
  const ms = await shutdownLatency({ sleep: undefined, stopVia: "stop" });
  assert.ok(ms < 1000, `the supervisor took ${ms} ms to stop during a backoff wait`);
});

test("B1 shutdown — paired control: a sleep that IGNORES the signal is measured as slow by the same instrument", async () => {
  const ignoring = (ms) => new Promise((r) => setTimeout(r, 700));
  const ms = await shutdownLatency({ sleep: ignoring, stopVia: "signal" });
  assert.ok(ms >= 500, `the instrument saw only ${ms} ms for a 700 ms uninterruptible wait — it cannot detect a slow shutdown`);
});

// ===== 5. a refused register is retried, not fatal ==================================================================

test("B1: a REFUSED register is a failed attempt like any other — the worker keeps retrying and registers once accepted; stop() is still a stop", async () => {
  let registers = 0;
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      if (msg.type !== "register") return;
      registers++;
      if (registers <= 3) conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: false, code: "unauthenticated" });
      else conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true });
    },
  });
  const stream = makeStream(control.port);
  const logs = [];
  let exited = false;
  const supervised = superviseConnection({ stream, getDials: () => MIN_LEGAL, sleep: instantSleep, log: (l) => logs.push(l) }).then(() => { exited = true; });
  assert.ok(await waitFor(() => stream.registered), "the worker never registered after three refusals");
  assert.equal(registers, 4, "three refusals then one acceptance");
  assert.equal(exited, false, "worker exited because its register was refused (the old behaviour: stop(), exit 1)");
  assert.equal(stream.stopped, false, "a refused register must not mark the stream stopped");
  assert.ok(logs.some((l) => /register rejected: unauthenticated/.test(l)), "the refusal was not logged with its clean code");
  // control: a real shutdown still stops it
  stream.stop();
  await supervised;
  assert.equal(stream.stopped, true);
  await control.close();
});

// ===== 6. a Control that accepts but never answers is a failed attempt =============================================

test("B1: a Control that accepts the connection but never answers `register` is a failed attempt (register timeout), and the next attempt can succeed", async () => {
  let connections = 0;
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      if (msg.type !== "register") return;
      connections++;
      if (connections >= 2) conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true });
      // the first register gets NO reply: a wedged Control
    },
  });
  const stream = makeStream(control.port, { registerTimeoutMs: 150 });
  const t0 = Date.now();
  const logs = [];
  const supervised = superviseConnection({ stream, getDials: () => MIN_LEGAL, sleep: instantSleep, log: (l) => logs.push(l) });
  assert.ok(await waitFor(() => stream.registered, 8000), "the worker never recovered from a Control that did not answer register");
  assert.ok(Date.now() - t0 < 5000, "recovery from a silent Control took too long");
  assert.ok(logs.some((l) => /register timed out/.test(l)), "the timed-out attempt was not logged");
  stream.stop();
  await supervised;
  await control.close();
});

test("B1 register timeout — paired control: a Control that answers promptly is NOT timed out afterwards (the timer is cleared)", async () => {
  const control = await startFakeControl({
    onFrame: (msg, conn) => { if (msg.type === "register") conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true }); },
  });
  const stream = makeStream(control.port, { registerTimeoutMs: 150 });
  await stream.start();
  await new Promise((r) => setTimeout(r, 500)); // well past the timeout
  assert.equal(stream.registered, true);
  assert.equal(stream.sock.destroyed, false, "a registered connection was destroyed by a stale register timer");
  stream.stop();
  await control.close();
});

// ===== 7. the dials: delivered, validated, never clamped ============================================================

test("B1 dials: legal range edges — the minimum legal values are legal, one below/above is not", () => {
  assert.deepEqual(RECONNECT_LIMITS.baseMs, [10, 60000]);
  assert.deepEqual(RECONNECT_LIMITS.capMs, [10, 600000]);
  assert.deepEqual(RECONNECT_LIMITS.jitterPct, [0, 100]);
  assert.deepEqual(RECONNECT_DEFAULTS, { baseMs: 1000, capMs: 30000, jitterPct: 20 });
  for (const [k, lo, hi] of [["baseMs", 10, 60000], ["capMs", 10, 600000], ["jitterPct", 0, 100]]) {
    assert.equal(isLegalDial(k, lo), true, `${k}=${lo} must be legal (the minimum)`);
    assert.equal(isLegalDial(k, hi), true, `${k}=${hi} must be legal (the maximum)`);
    assert.equal(isLegalDial(k, lo - 1), false, `${k}=${lo - 1} must be illegal`);
    assert.equal(isLegalDial(k, hi + 1), false, `${k}=${hi + 1} must be illegal`);
    assert.equal(isLegalDial(k, "100"), false, `${k}: a string is not a dial`);
    assert.equal(isLegalDial(k, lo + 0.5), false, `${k}: a fraction is not a dial`);
  }
});

test("B1 dials: an illegal candidate is skipped, never clamped; precedence is environment > Control-delivered > built-in default", () => {
  assert.deepEqual(resolveDials(), RECONNECT_DEFAULTS);
  assert.deepEqual(resolveDials({ baseMs: 5, capMs: 99999999, jitterPct: -1 }), RECONNECT_DEFAULTS, "out-of-range values must fall through to the default, not be clamped to the nearest legal value");
  assert.deepEqual(resolveDials({ baseMs: 50 }, { baseMs: 70, capMs: 300 }), { baseMs: 50, capMs: 300, jitterPct: 20 });
  assert.deepEqual(resolveDials({ baseMs: undefined, capMs: undefined, jitterPct: undefined }, { baseMs: 70, capMs: 300, jitterPct: 0 }), { baseMs: 70, capMs: 300, jitterPct: 0 });
});

test("B1 dials: register_ack delivers the dials; a legal field is adopted, an illegal or absent one is not (and a later ack replaces the earlier set)", async () => {
  let ack = { reconnect_base_ms: 50, reconnect_cap_ms: 200, reconnect_jitter_pct: 0 };
  const control = await startFakeControl({
    onFrame: (msg, conn) => { if (msg.type === "register") conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true, ...ack }); },
  });
  const stream = makeStream(control.port);
  assert.deepEqual(stream.deliveredReconnectDials, {}, "before any register_ack nothing is delivered");
  await stream.start();
  assert.deepEqual(stream.deliveredReconnectDials, { baseMs: 50, capMs: 200, jitterPct: 0 }, "a jitter of 0 is a real value and must be adopted");
  stream.sock.destroy();
  await new Promise((r) => setTimeout(r, 30));
  ack = { reconnect_base_ms: 5, reconnect_cap_ms: 200, reconnect_jitter_pct: 101 }; // base and jitter out of range, cap legal
  await stream.start();
  assert.deepEqual(stream.deliveredReconnectDials, { capMs: 200 });
  stream.sock.destroy();
  await new Promise((r) => setTimeout(r, 30));
  ack = {}; // an older Control: nothing delivered
  await stream.start();
  assert.deepEqual(stream.deliveredReconnectDials, {});
  stream.stop();
  await control.close();
});

// ===== 8. index.js as a process ======================================================================================

function runIndex(extraEnv) {
  const env = {
    ...process.env,
    WORKER_ID: "11111111-2222-4333-8444-555555555555",
    WORKER_TOKEN: crypto.randomBytes(12).toString("hex"),
    MTLS_CLIENT_CERT: path.join(CERTS, "harness-client.crt"),
    MTLS_CLIENT_KEY: path.join(CERTS, "harness-client.key"),
    MTLS_CA_CERT: path.join(CERTS, "ca.crt"),
    ...extraEnv,
  };
  const child = spawn(process.execPath, [INDEX_JS], { env, stdio: ["ignore", "pipe", "pipe"] });
  const out = { stdout: "", stderr: "", exit: null };
  child.stdout.on("data", (d) => { out.stdout += d; });
  child.stderr.on("data", (d) => { out.stderr += d; });
  child.on("exit", (code, sig) => { out.exit = { code, sig }; });
  return { child, out, token: env.WORKER_TOKEN };
}

test("B1 index.js: a dial outside the legal range in the environment is a startup error (exit 2, named), never a silent clamp — and a legal one starts", async () => {
  const bad = runIndex({ WORKER_RECONNECT_BASE_MS: "5", CONTROL_HOST: "127.0.0.1", CONTROL_STREAM_PORT: "1" });
  assert.ok(await waitFor(() => bad.out.exit !== null, 10000), "index.js with WORKER_RECONNECT_BASE_MS=5 did not exit");
  assert.equal(bad.out.exit.code, 2);
  assert.match(bad.out.stderr, /WORKER_RECONNECT_BASE_MS must be an integer from 10 to 60000 \(got "5"\)/);
  // control: the same process with a legal value does NOT exit by itself
  const good = runIndex({ WORKER_RECONNECT_BASE_MS: "10", WORKER_RECONNECT_CAP_MS: "10", WORKER_RECONNECT_JITTER_PCT: "0", CONTROL_HOST: "127.0.0.1", CONTROL_STREAM_PORT: "1" });
  await new Promise((r) => setTimeout(r, 1500));
  const aliveAfter = good.out.exit === null;
  good.child.kill();
  assert.ok(aliveAfter, `index.js with legal dials exited by itself: ${JSON.stringify(good.out.exit)} stderr=${good.out.stderr}`);
});

test("B1 index.js: with NO Control the process is still running after more than 30 failed attempts (it never gives up) and logs at a bounded rate, never the token", async () => {
  // A "Control" that accepts every connection and drops it at once: every dial fails fast, on every platform.
  const dropper = net.createServer((s) => s.destroy());
  await new Promise((r) => dropper.listen(0, "127.0.0.1", r));
  const { child, out, token } = runIndex({
    WORKER_RECONNECT_BASE_MS: "10", WORKER_RECONNECT_CAP_MS: "10", WORKER_RECONNECT_JITTER_PCT: "0",
    CONTROL_HOST: "127.0.0.1", CONTROL_STREAM_PORT: String(dropper.address().port),
  });
  try {
    const reached40 = await waitFor(() => /40 consecutive failed attempt\(s\)/.test(out.stdout) || out.exit !== null, 30000);
    assert.ok(reached40, "the worker never logged its 40th consecutive failed attempt");
    assert.equal(out.exit, null, `worker exited (${JSON.stringify(out.exit)}) after fewer than 40 failed attempts — a reconnect limit is back. stderr: ${out.stderr}`);
    const counts = [...out.stdout.matchAll(/; (\d+) consecutive failed attempt\(s\)/g)].map((m) => Number(m[1]));
    assert.ok(counts.length >= 5, `too few failure lines logged: ${counts}`);
    assert.equal(counts[0], 1, "the first failed attempt must be logged");
    assert.ok(counts.every((c) => c === 1 || c % 10 === 0), `a non-multiple-of-10 attempt was logged (the log rate is not bounded): ${counts}`);
    assert.ok(!out.stdout.includes(token) && !out.stderr.includes(token), "the registration token reached the log");
  } finally {
    child.kill();
    await new Promise((r) => dropper.close(r));
  }
});
