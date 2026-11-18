// reconnect.js — the worker's NEVER-GIVE-UP reconnect supervisor (Phase 3 close-out B1).
//
// THE RULE. The worker dials the Control; when the dial or the register handshake fails, it waits and tries
// again, FOREVER — until its own shutdown (SIGTERM/SIGINT, an aborted signal, stream.stop()). There is no
// attempt limit and no "give up" exit: a Control that is down for a minute, an hour or a weekend is
// reconnected to, with no restart of the worker. (Until this file the worker made about thirty one-second
// attempts, then exited 1 for its supervisor to restart; an outage longer than that needed the worker
// restarted by hand. Buffered outcomes were kept past the 30 only while still inside their TTL.)
//
// THE DELAY. Capped exponential backoff with additive jitter:
//
//     d(n)     = min(cap, base * 2^(n-1))            n = 1 for the first failed attempt since the last success
//     sleep(n) = d(n) * (1 + jitter * u)             u uniform in [0, 1), jitter = jitter_pct / 100
//
// so every sleep is in [d(n), d(n) * (1 + jitter)) and NEVER exceeds cap * (1 + jitter). The jitter spreads
// a fleet of workers that lost the same Control at the same instant so they do not all redial on the same
// tick. The attempt counter RESETS to zero the moment a connection registers (a success), so the next outage
// starts again at `base`. A connection that registers and then drops is followed by one `base`-sized wait
// before the redial, so a Control that accepts and immediately drops cannot be hammered in a tight loop.
//
// THE DIALS (config-not-code). base / cap / jitter are the seeded send_managed.config keys
// worker_reconnect_base_ms / worker_reconnect_cap_ms / worker_reconnect_jitter_pct. The worker has no
// database, so the Control delivers them on register_ack (the same way box_outcome_buffer_ttl_sec reaches
// it) and the worker adopts them for every wait that follows. A worker that has never registered uses the
// built-in defaults below (which are the seeded defaults) or its WORKER_RECONNECT_* environment variables,
// which, when set, win over what the Control delivers (an operator/harness override; see index.js).
//
// Everything with a side effect is injectable (sleep, random, log, the stream), so the behaviour is tested
// without a real clock or a real Control (test/reconnect.test.mjs).

// The seeded defaults: production-suited. (The Control's migration seeds the same three numbers.)
export const RECONNECT_DEFAULTS = Object.freeze({ baseMs: 1000, capMs: 30000, jitterPct: 20 });

// The legal range of each dial, inclusive. The MINIMUM LEGAL values (10 ms, 10 ms, 0 %) are what makes the
// platform harness possible: with a 50 ms base and a 200 ms cap the worker makes more than thirty attempts
// in about six seconds. The Control validates the same ranges on a config PUT (adminapi/config_validate.go)
// and a Go test (workerproto/reconnect_dials_test.go) fails if these literals and the Go ones differ.
export const RECONNECT_LIMITS = Object.freeze({
  baseMs: Object.freeze([10, 60000]),
  capMs: Object.freeze([10, 600000]),
  jitterPct: Object.freeze([0, 100]),
});

const KEYS = ["baseMs", "capMs", "jitterPct"];

// isLegal: a finite integer inside the dial's inclusive range.
export function isLegalDial(key, v) {
  const range = RECONNECT_LIMITS[key];
  return Boolean(range) && Number.isInteger(v) && v >= range[0] && v <= range[1];
}

// resolveDials builds a complete, legal dial set: for each dial the first legal value among the
// candidates, in order, else the built-in default. A candidate is an object with any of baseMs / capMs /
// jitterPct (an illegal or missing field is skipped, never clamped: a value outside the range is
// ignored, not "corrected" into a different number).
export function resolveDials(...candidates) {
  const out = {};
  for (const k of KEYS) {
    out[k] = RECONNECT_DEFAULTS[k];
    for (const c of candidates) {
      if (c && isLegalDial(k, c[k])) { out[k] = c[k]; break; }
    }
  }
  return out;
}

// backoffDelayMs is the wait after the n-th consecutive failed attempt (n >= 1). `rand` returns u in [0, 1).
export function backoffDelayMs(failures, dials, rand = Math.random) {
  const n = Math.max(1, Math.floor(failures));
  // 2^(n-1) overflows to Infinity for very large n; Math.min against the cap keeps it finite and exact.
  const exp = Math.min(dials.capMs, dials.baseMs * 2 ** Math.min(n - 1, 60));
  const jitter = dials.jitterPct / 100;
  return Math.round(exp * (1 + jitter * rand()));
}

// shouldLogFailure bounds the log rate: the first failed attempt, then every 10th. At the production cap
// (30 s) a Control that is down all night logs a line about every five minutes; it can never flood the log.
export function shouldLogFailure(failures) {
  return failures === 1 || failures % 10 === 0;
}

// defaultSleep waits ms, or less if the signal aborts (shutdown): it resolves either way, never rejects, so
// a shutdown during a backoff wait ends the wait at once instead of after the full (up to 30 s+) delay.
export function defaultSleep(ms, signal) {
  return new Promise((resolve) => {
    if (signal && signal.aborted) return resolve();
    const t = setTimeout(done, ms);
    function done() {
      clearTimeout(t);
      if (signal) signal.removeEventListener("abort", done);
      resolve();
    }
    if (signal) signal.addEventListener("abort", done, { once: true });
  });
}

// waitForClose resolves when the stream's CURRENT socket closes (immediately if it already has).
function waitForClose(stream) {
  return new Promise((resolve) => {
    const sock = stream.sock;
    if (!sock || sock.destroyed) return resolve();
    sock.once("close", resolve);
  });
}

/**
 * superviseConnection keeps the worker connected to the Control until it is told to stop.
 *
 * It resolves ONLY when the worker is shutting down (stream.stopped, or `signal` aborted). It never
 * rejects and never resolves for any other reason: there is no give-up path.
 *
 * @param {object} o
 * @param {object} o.stream     a WorkerStream (start() -> Promise resolved once registered; sock; stopped)
 * @param {function} o.getDials () => {baseMs, capMs, jitterPct}; read before EVERY wait, so a dial the
 *                              Control delivered on the last register_ack takes effect on the next wait
 * @param {function} [o.sleep]  (ms, signal) => Promise; default: a real timer that a shutdown aborts
 * @param {function} [o.rand]   () => u in [0, 1); default Math.random
 * @param {function} [o.log]    (line) => void; default console.log. Never receives a secret: the only
 *                              dynamic text is an error message from the dial/handshake (no credential)
 * @param {AbortSignal} [o.signal] aborts on shutdown
 */
export async function superviseConnection(o) {
  const { stream, getDials } = o;
  const sleep = o.sleep || defaultSleep;
  const rand = o.rand || Math.random;
  const log = o.log || ((line) => console.log(line));

  // ONE internal signal that fires on either shutdown route — the caller's `signal` aborting, or
  // stream.stop() being called — so a backoff wait ends at once whichever route was taken.
  const inner = new AbortController();
  const abortInner = () => inner.abort();
  if (o.signal) {
    if (o.signal.aborted) abortInner();
    else o.signal.addEventListener("abort", abortInner, { once: true });
  }
  if (stream.whenStopped) stream.whenStopped.then(abortInner);
  const signal = inner.signal;
  const stopping = () => stream.stopped || signal.aborted;

  let failures = 0; // consecutive failed attempts since the last success
  while (!stopping()) {
    try {
      await stream.start();
    } catch (e) {
      if (stopping()) break;
      failures++;
      const delay = backoffDelayMs(failures, getDials(), rand);
      if (shouldLogFailure(failures)) {
        log(`control unreachable or register refused (${e && e.message ? e.message : "error"}); ${failures} consecutive failed attempt(s); next attempt in ${delay} ms; the worker keeps retrying until it is shut down`);
      }
      await sleep(delay, signal);
      continue;
    }
    // Registered: the outage, if there was one, is over.
    if (failures > 0) log(`reconnected after ${failures} failed attempt(s); unacked outcomes are replayed`);
    failures = 0;
    const d = getDials();
    log(`registered over mTLS stream; awaiting pushed jobs (buffered outcomes replayed on connect); reconnect backoff in effect: base ${d.baseMs} ms, cap ${d.capMs} ms, jitter ${d.jitterPct} %`);
    await waitForClose(stream);
    if (stopping()) break;
    log("control stream closed; reconnecting (outcome buffer retained)");
    await sleep(backoffDelayMs(1, getDials(), rand), signal);
  }
}
