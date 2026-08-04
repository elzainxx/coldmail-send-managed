// Worker outcome-buffering + persist-ack behavioral tests (Phase 7 §1.3, P7-4). These exercise the
// ACTUAL src/stream.js WorkerStream — its real read loop, real outcome buffer, real replay-on-connect,
// real report_ack drop, and real TTL prune — over a REAL OS socket to a REAL local "fake Control"
// (a net.Server). Only the TLS WRAP is substituted (via the worker's injectable `dial` seam): mutual
// TLS is proven separately by the live-stack mTLS suite + the S4 handshake test, and — critically —
// the local host runs Norton "Web/Mail Shield" which MITM-intercepts localhost TLS, so a real-TLS
// loopback test here would assert Norton's behavior, not ours. The end-to-end no-duplicate proof over
// genuine mTLS is the live-stack harness (T1, Docker, Norton-immune). This is the same "stand in only
// the already-proven wire" philosophy the Go dispatch harness uses for the SMTP wire.
//
// Run: node --test services/worker/test/buffer.test.mjs  (Node 20+).

import { test } from "node:test";
import assert from "node:assert/strict";
import net from "node:net";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { WorkerStream } from "../src/stream.js";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const CERTS = path.resolve(__dirname, "../../../secrets/dev/certs"); // read for realism; not used by the plain-socket dial

// startFakeControl: a plain-TCP server speaking the newline-JSON worker protocol. onFrame(frame, conn)
// handles each frame; conn.reply(obj) sends one back. Records connections so a test can drop one.
async function startFakeControl(opts = {}) {
  const server = net.createServer((conn) => {
    conn.setEncoding("utf8");
    conn.reply = (obj) => { try { conn.write(JSON.stringify(obj) + "\n"); } catch { /* gone */ } };
    let buf = "";
    conn.on("data", (d) => {
      buf += d;
      let i;
      while ((i = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, i);
        buf = buf.slice(i + 1);
        if (!line.trim()) continue;
        let msg;
        try { msg = JSON.parse(line); } catch { continue; }
        if (opts.onFrame) opts.onFrame(msg, conn);
      }
    });
    conn.on("error", () => { /* ignore peer resets */ });
    server._lastConn = conn;
    if (opts.onConnect) opts.onConnect(conn);
  });
  server._lastConn = null;
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  return { server, port: server.address().port, close: () => new Promise((r) => server.close(r)) };
}

// plainDial substitutes the TLS wrap with a plain TCP socket (see the file header for why). It
// preserves the exact WorkerStream contract: return a socket, fire onSecure once connected.
function plainDial(o, _tlsMaterial, onSecure) {
  const sock = net.connect({ host: o.host, port: o.port }, onSecure);
  return sock;
}

function makeWorker(port, extra = {}) {
  return new WorkerStream({
    host: "127.0.0.1",
    port,
    workerId: "worker-test",
    token: "tok",
    // certPath/keyPath/caPath are read at construction (fs.readFileSync) even with a custom dial, so
    // point them at the real dev certs; the plain dial ignores the material.
    certPath: path.join(CERTS, "worker.crt"),
    keyPath: path.join(CERTS, "worker.key"),
    caPath: path.join(CERTS, "ca.crt"),
    serverName: "control",
    heartbeatSec: 3600, // no heartbeat noise during the short test
    dial: plainDial,
    ...extra,
  });
}

const waitFor = async (pred, ms = 3000) => {
  const start = Date.now();
  while (Date.now() - start < ms) {
    if (pred()) return true;
    await new Promise((r) => setTimeout(r, 15));
  }
  return false;
};

// A push_job whose lease is already expired → the worker reports 'error' IMMEDIATELY without any SMTP
// session, exercising the true #bufferAndSend path end to end (the SMTP wire is covered elsewhere).
function expiredPushJob(jobId) {
  return {
    type: "push_job",
    request_id: "req-" + jobId,
    job_id: jobId,
    send: { recipient: "x@example.com", from_address: "u@ourdomain.com", subject: "s", body: "b", headers: {} },
    smtp: { host: "127.0.0.1", port: 1, username: "u", secret: "SECRET", security: "starttls" },
    lease_expires_at: new Date(Date.now() - 60_000).toISOString(),
  };
}

function controlPush(control, jobId) {
  const conn = control.server._lastConn;
  if (conn) conn.reply(expiredPushJob(jobId));
}

// controlPushAttempt pushes the SAME job with a DIFFERENT push_job.request_id — i.e. the Control
// reclaimed a first attempt and re-dispatched the job to this same worker. request_id is minted per
// attempt by the Control and echoed in the worker's report, so it is the worker's only attempt identity.
function controlPushAttempt(control, jobId, requestID) {
  const conn = control.server._lastConn;
  if (conn) conn.reply({ ...expiredPushJob(jobId), request_id: requestID });
}

test("P7-4: worker buffers an outcome and DROPS it only on report_ack", async () => {
  const reports = [];
  const acked = [];
  let ackNext = false; // the Control withholds the ack until we flip this
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      if (msg.type === "register") conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true, outcome_buffer_ttl_sec: 900 });
      if (msg.type === "report") {
        reports.push(msg.job_id);
        if (ackNext) conn.reply({ type: "report_ack", request_id: msg.request_id, job_id: msg.job_id, accepted: true });
      }
    },
  });
  const worker = makeWorker(control.port, { onAck: (j) => acked.push(j) });
  try {
    await worker.start();
    controlPush(control, "job-A");
    assert.ok(await waitFor(() => reports.includes("job-A")), "Control never received the outcome report");
    // No ack yet → the outcome MUST still be buffered (unacked outcomes are retained).
    assert.equal(worker.outcomeBuffer.has("job-A"), true, "outcome dropped before any ack — must be retained until acked");
    assert.equal(acked.length, 0, "onAck fired with no report_ack sent");

    // Now the Control acks → the worker drops the buffered copy.
    ackNext = true;
    worker.replayNow();
    assert.ok(await waitFor(() => acked.includes("job-A")), "worker never processed the report_ack");
    assert.equal(worker.outcomeBuffer.has("job-A"), false, "outcome NOT dropped after report_ack (would replay forever)");
  } finally {
    worker.stop();
    await control.close();
  }
});

test("P7-4: buffered outcome REPLAYS on reconnect (stream drop → the no-duplicate mechanism)", async () => {
  const reportsPerConn = [];
  let ackAll = false;
  const acked = [];
  const control = await startFakeControl({
    onConnect: () => { reportsPerConn.push([]); },
    onFrame: (msg, conn) => {
      if (msg.type === "register") conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true, outcome_buffer_ttl_sec: 900 });
      if (msg.type === "report") {
        reportsPerConn[reportsPerConn.length - 1].push(msg.job_id);
        if (ackAll) conn.reply({ type: "report_ack", request_id: msg.request_id, job_id: msg.job_id, accepted: true });
      }
    },
  });
  const worker = makeWorker(control.port, { onAck: (j) => acked.push(j) });
  try {
    await worker.start();
    controlPush(control, "job-R");
    assert.ok(await waitFor(() => reportsPerConn[0]?.includes("job-R")), "first connection never got the report");
    // The Control did NOT ack → the outcome is still buffered. Drop the connection.
    assert.equal(worker.outcomeBuffer.has("job-R"), true);
    ackAll = true; // the reconnect WILL ack
    worker.sock.destroy(); // simulate a stream drop

    // Model the in-process reconnect (index.js supervise loop): re-run start() on the SAME worker
    // instance — its buffer is intact — and assert the buffered outcome is REPLAYED on register_ack.
    await worker.start();
    assert.ok(await waitFor(() => reportsPerConn.length >= 2), "Control never saw the reconnect");
    assert.ok(await waitFor(() => reportsPerConn[1]?.includes("job-R")), "buffered outcome was NOT replayed on reconnect (duplicate/loss risk)");
    assert.ok(await waitFor(() => acked.includes("job-R")), "reconnect replay was never acked");
    assert.equal(worker.outcomeBuffer.has("job-R"), false, "outcome still buffered after the reconnect ack");
  } finally {
    worker.stop();
    await control.close();
  }
});

// §2.3 — the DB-down case: the stream stays LIVE, the Control receives the report but CANNOT persist it
// so it sends NO ack, and the spec requires the worker to "keep replaying until it receives a post-commit
// ack". This drives the PRODUCTION trigger only — the real heartbeat interval timer inside the real
// WorkerStream. It never calls replayNow() and never reconnects; if replay were reconnect-only (as it was
// before this fix) the report count would stay at 1 forever and Control-side reclaim (lease 120s ≪ TTL
// 900s) would re-send the mail: a silent duplicate.
test("P7-4/§2.3: worker KEEPS REPLAYING an unacked outcome on a LIVE stream (no reconnect, no test seam)", async () => {
  const reports = [];
  const acked = [];
  let ackNext = false; // Control is "DB down" until we flip this
  const conns = [];
  const control = await startFakeControl({
    onConnect: (c) => conns.push(c),
    onFrame: (msg, conn) => {
      if (msg.type === "register") conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true, outcome_buffer_ttl_sec: 900 });
      if (msg.type === "report") {
        reports.push(msg.job_id);
        // Persist FAILED (DB down) → deliberately send NO report_ack. This is the §2.3 condition.
        if (ackNext) conn.reply({ type: "report_ack", request_id: msg.request_id, job_id: msg.job_id, accepted: true });
      }
    },
  });
  // heartbeatSec 0.15 → the real setInterval fires the production replay pass several times/second.
  const worker = makeWorker(control.port, { heartbeatSec: 0.15, onAck: (j) => acked.push(j) });
  try {
    await worker.start();
    controlPush(control, "job-L");
    assert.ok(await waitFor(() => reports.length >= 1), "Control never received the first report");
    // THE ASSERTION: with no ack, the SAME outcome arrives again — driven only by the worker's own timer.
    assert.ok(await waitFor(() => reports.filter((j) => j === "job-L").length >= 3, 4000),
      `unacked outcome was NOT re-delivered on a live stream (saw ${reports.filter((j) => j === "job-L").length} deliveries) — §2.3 "keeps replaying until it receives a post-commit ack" unsatisfied`);
    assert.equal(conns.length, 1, "the replay must come from the LIVE stream, not a reconnect");
    assert.equal(worker.outcomeBuffer.has("job-L"), true, "outcome must stay buffered while unacked");
    assert.equal(acked.length, 0, "no ack was sent, yet the worker reported one");

    // NEGATIVE CONTROL (anti-vacuity): once the DB is back and the Control acks post-commit, replaying
    // STOPS. Without this, a test asserting "reports keep growing" would also pass a worker that replays
    // forever and ignores acks — which would hammer the Control and never converge.
    ackNext = true;
    assert.ok(await waitFor(() => acked.includes("job-L"), 4000), "worker never processed the post-commit ack");
    assert.equal(worker.outcomeBuffer.has("job-L"), false, "acked outcome still buffered");
    const settled = reports.length;
    await new Promise((r) => setTimeout(r, 700)); // several more heartbeat ticks
    assert.equal(reports.length, settled, "worker kept replaying AFTER a post-commit ack (never converges)");
  } finally {
    worker.stop();
    await control.close();
  }
});

// §1.3 requires the buffer be bounded by the TTL *and a max entry count*. Proves the cardinality bound
// exists, evicts the OLDEST, and always retains the NEWEST outcome (the one least likely to have been
// reclaimed already, so keeping it is what minimizes duplicates).
test("P7-4/§1.3: buffer is bounded by a MAX ENTRY COUNT — overflow evicts the oldest, keeps the newest", async () => {
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      // Never ack: retention is governed purely by the bounds under test.
      if (msg.type === "register") conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true, outcome_buffer_ttl_sec: 900 });
    },
  });
  const worker = makeWorker(control.port, { outcomeBufferMaxEntries: 2 });
  try {
    await worker.start();
    for (const id of ["job-1", "job-2"]) {
      controlPush(control, id);
      assert.ok(await waitFor(() => worker.outcomeBuffer.has(id)), `${id} never buffered`);
    }
    assert.equal(worker.outcomeBuffer.size, 2, "buffer should be exactly at capacity");
    // Positive control: at capacity, nothing has been dropped yet.
    assert.equal(worker.overflowDropped, 0, "dropped an entry before the buffer was over capacity");

    controlPush(control, "job-3");
    assert.ok(await waitFor(() => worker.outcomeBuffer.has("job-3")), "the NEWEST outcome was not buffered on overflow");
    assert.equal(worker.outcomeBuffer.size, 2, `buffer grew past its max entry count (size=${worker.outcomeBuffer.size}) — unbounded, §1.3 violated`);
    assert.equal(worker.outcomeBuffer.has("job-1"), false, "overflow evicted something other than the OLDEST entry");
    assert.equal(worker.outcomeBuffer.has("job-2"), true, "overflow evicted more than it needed to");
    assert.equal(worker.overflowDropped, 1, "overflow drop was not counted");
  } finally {
    worker.stop();
    await control.close();
  }
});

// Review gate R2 findings 1–2. The buffer is keyed by job_id, but a job can be pushed to this SAME worker
// TWICE: attempt 1's ack is lost, the Control reclaims the lease and re-dispatches, and the worker gets the
// same job_id again with a NEW push_job.request_id. Two distinct hazards follow, both proved here.
test("P7-4/R2: a job pushed AGAIN buffers the LIVE attempt's outcome (the superseded one is discarded)", async () => {
  const reports = [];
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      // Never ack: the buffer's contents are the whole subject of this test.
      if (msg.type === "register") conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true, outcome_buffer_ttl_sec: 900 });
      if (msg.type === "report") reports.push(msg);
    },
  });
  const worker = makeWorker(control.port);
  try {
    await worker.start();

    // Attempt 1: pushed, reported, never acked → buffered.
    controlPushAttempt(control, "job-R", "req-attempt-1");
    assert.ok(await waitFor(() => worker.outcomeBuffer.has("job-R")), "attempt 1's outcome never buffered");
    assert.equal(worker.outcomeBuffer.get("job-R").report.request_id, "req-attempt-1");
    // Positive control: with only one attempt seen, nothing was superseded.
    assert.equal(worker.supersededDropped, 0, "counted a superseded entry before any second push");

    // Attempt 2: the Control reclaimed and re-dispatched the SAME job to this worker.
    controlPushAttempt(control, "job-R", "req-attempt-2");
    assert.ok(
      await waitFor(() => worker.outcomeBuffer.get("job-R")?.report.request_id === "req-attempt-2"),
      "the LIVE attempt's outcome was NOT buffered — a stale entry for the same job_id blocked it, so a lost send would replay the dead attempt's result forever and lose the real one",
    );
    assert.equal(worker.outcomeBuffer.size, 1, "both attempts are buffered under one job_id — only the live attempt may be retained");
    assert.equal(worker.supersededDropped, 1, "the superseded entry was not counted as dropped");

    // And a replay now carries the LIVE attempt only.
    reports.length = 0;
    worker.replayNow();
    assert.ok(await waitFor(() => reports.length > 0), "replay sent nothing");
    assert.equal(reports[0].request_id, "req-attempt-2", "replay re-sent the SUPERSEDED attempt's outcome");
  } finally {
    worker.stop();
    await control.close();
  }
});

test("P7-4/R2: a report_ack for a SUPERSEDED attempt does not drop the live attempt's outcome", async () => {
  const acked = [];
  let lastConn = null;
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      lastConn = conn;
      if (msg.type === "register") conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true, outcome_buffer_ttl_sec: 900 });
    },
  });
  const worker = makeWorker(control.port, { onAck: (j) => acked.push(j) });
  try {
    await worker.start();
    controlPushAttempt(control, "job-S", "req-attempt-1");
    assert.ok(await waitFor(() => worker.outcomeBuffer.has("job-S")), "attempt 1 never buffered");
    controlPushAttempt(control, "job-S", "req-attempt-2");
    assert.ok(await waitFor(() => worker.outcomeBuffer.get("job-S")?.report.request_id === "req-attempt-2"), "attempt 2 never buffered");

    // A LATE ack for attempt 1 arrives (the Control committed the first attempt's outcome, or an ack was
    // merely delayed in flight). It must NOT drop attempt 2's still-unacked outcome.
    lastConn.reply({ type: "report_ack", request_id: "req-attempt-1", job_id: "job-S", accepted: true });
    await new Promise((r) => setTimeout(r, 200));
    assert.equal(
      worker.outcomeBuffer.has("job-S"), true,
      "an ack for the SUPERSEDED attempt dropped the live attempt's outcome — the worker would stop replaying an outcome the Control never committed, losing it silently",
    );
    assert.equal(acked.length, 0, "onAck fired for an attempt whose outcome is not the buffered one");

    // NEGATIVE CONTROL: the matching ack DOES drop it (so the assertion above is about the mismatch,
    // not about an ack path that never works).
    lastConn.reply({ type: "report_ack", request_id: "req-attempt-2", job_id: "job-S", accepted: true });
    assert.ok(await waitFor(() => acked.includes("job-S")), "the MATCHING report_ack did not drop the buffered outcome");
    assert.equal(worker.outcomeBuffer.has("job-S"), false, "buffered outcome survived its own ack (would replay forever)");
  } finally {
    worker.stop();
    await control.close();
  }
});

test("P7-4: an outcome older than the TTL is DROPPED (bounded buffer → falls back to reclaim)", async () => {
  let clock = 1_000_000;
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      // TTL = 10s; never ack, so retention is governed purely by the TTL.
      if (msg.type === "register") conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true, outcome_buffer_ttl_sec: 10 });
    },
  });
  const worker = makeWorker(control.port, { now: () => clock });
  try {
    await worker.start();
    controlPush(control, "job-T");
    assert.ok(await waitFor(() => worker.outcomeBuffer.has("job-T")), "outcome never buffered");
    // Advance the clock past the TTL and trigger a prune (a replay pass prunes first).
    clock += 11_000;
    worker.replayNow();
    assert.equal(worker.outcomeBuffer.has("job-T"), false, "outcome past TTL was NOT dropped (buffer unbounded — P7-4 violated)");
  } finally {
    worker.stop();
    await control.close();
  }
});

// --- Phase 8 CHUNK 1 (P7-OD-6): the worker's half of the per-attempt identity contract ---
//
// The Control mints send_managed.jobs.attempt_token inside the guarded assigned→sending UPDATE and puts it on
// push_job. The worker's ONLY obligation is to echo it back VERBATIM in the outcome report — it must never
// mint, increment, default, or otherwise author a token, because the token is the Control's proof of WHICH
// attempt produced an outcome. A worker that invented one could resurrect a stale buffered outcome onto a
// live attempt (the exact duplicate P7-OD-6 filed). These tests assert both directions on the real
// WorkerStream over a real socket: the token is echoed unchanged, and its ABSENCE stays absent (never
// becomes a fabricated value), so an older Control that does not send the field is not handed a token the
// worker made up.

function pushJobWithToken(jobId, attemptToken) {
  const pj = expiredPushJob(jobId);
  if (attemptToken !== undefined) pj.attempt_token = attemptToken;
  return pj;
}

test("P7-OD-6: the worker echoes push_job.attempt_token VERBATIM in its report and never mints one", async () => {
  const reports = [];
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      if (msg.type === "register") conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true, outcome_buffer_ttl_sec: 900 });
      if (msg.type === "report") {
        reports.push(msg);
        conn.reply({ type: "report_ack", request_id: msg.request_id, job_id: msg.job_id, accepted: true });
      }
    },
  });
  const worker = makeWorker(control.port);
  try {
    await worker.start();

    // A large token: proves the value is carried through as-is (not truncated, not re-derived from
    // attempt_count, not replaced by a counter the worker keeps).
    const TOKEN = 4242;
    control.server._lastConn.reply(pushJobWithToken("job-tok", TOKEN));
    assert.ok(await waitFor(() => reports.some((r) => r.job_id === "job-tok")), "Control never received the outcome report");
    const rep = reports.find((r) => r.job_id === "job-tok");
    assert.equal(rep.attempt_token, TOKEN,
      `report.attempt_token=${rep.attempt_token}, want the pushed ${TOKEN} — the Control cannot tell which attempt this outcome belongs to`);

    // A SECOND attempt of the SAME job carrying a DIFFERENT token: the worker must report the token of the
    // attempt it actually ran, not the first one it saw. (Anti-vacuity for "echoed verbatim": a worker that
    // cached the first token would pass the assertion above and fail here.)
    control.server._lastConn.reply(pushJobWithToken("job-tok2", TOKEN + 1));
    assert.ok(await waitFor(() => reports.some((r) => r.job_id === "job-tok2")), "second attempt never reported");
    assert.equal(reports.find((r) => r.job_id === "job-tok2").attempt_token, TOKEN + 1,
      "the worker reported a stale/cached attempt_token for a later attempt");
  } finally {
    worker.stop();
    await control.close();
  }
});

test("P7-OD-6: a push_job with NO attempt_token yields a report with no fabricated token", async () => {
  const reports = [];
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      if (msg.type === "register") conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true, outcome_buffer_ttl_sec: 900 });
      if (msg.type === "report") {
        reports.push(msg);
        conn.reply({ type: "report_ack", request_id: msg.request_id, job_id: msg.job_id, accepted: true });
      }
    },
  });
  const worker = makeWorker(control.port);
  try {
    await worker.start();
    control.server._lastConn.reply(pushJobWithToken("job-notok", undefined)); // field absent, as a pre-0020 Control sends
    assert.ok(await waitFor(() => reports.some((r) => r.job_id === "job-notok")), "Control never received the outcome report");
    const rep = reports.find((r) => r.job_id === "job-notok");
    // Absent must stay absent/undefined. Anything else (0, 1, a counter) is the worker AUTHORING identity,
    // and a non-zero fabricated value would make the Control's guard reject a legitimate outcome.
    assert.equal(rep.attempt_token, undefined,
      `report.attempt_token=${JSON.stringify(rep.attempt_token)} for a push with no token — the worker fabricated an attempt identity`);
  } finally {
    worker.stop();
    await control.close();
  }
});
