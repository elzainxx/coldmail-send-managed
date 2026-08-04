// variation-security.test.mjs — Phase 8 CHUNK 6, §8 S1: re-verify the Phase 3 credential-security
// guarantee with variation GENUINELY ACTIVE through a real SMTP session, not just via variation.js's
// own unit tests (which never touch the credential) or buffer.test.mjs's tests (which all push an
// EXPIRED lease, so #runJob short-circuits before ever reaching runSmtpSession/MemoryCredential).
//
// This closes that gap: a REAL WorkerStream, driven by a REAL fake-Control socket server, pushes a
// push_job carrying BOTH a live (non-expired) lease AND a rich variation block, against a REAL
// local SMTP sink (test/sink.mjs, the same one smtp.test.mjs uses). The full #runJob path — real
// variation pipeline, real SMTP session, real credential zeroing — runs end to end. NO mocks
// (CLAUDE.md Rule 3): only the mTLS wrap is substituted (a plain socket, same rationale as
// buffer.test.mjs's Norton-interception note); the SMTP session and the variation pipeline are the
// actual production code.

import { test } from "node:test";
import assert from "node:assert/strict";
import net from "node:net";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { WorkerStream } from "../src/stream.js";
import { startSink } from "./sink.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const CERTS = path.resolve(__dirname, "../../../secrets/dev/certs");
const CANARY = "canary-smtp-secret-variation-9Z";

// startFakeControl mirrors buffer.test.mjs's helper: a plain-TCP server speaking the newline-JSON worker
// protocol. onFrame(frame, conn) handles each frame the worker sends; conn.reply(obj) pushes one back.
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
    conn.on("error", () => {});
    server._lastConn = conn;
    if (opts.onConnect) opts.onConnect(conn);
  });
  server._lastConn = null;
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  return { server, port: server.address().port, close: () => new Promise((r) => server.close(r)) };
}

function plainDial(o, _tlsMaterial, onSecure) {
  return net.connect({ host: o.host, port: o.port }, onSecure);
}

function makeWorker(port, extra = {}) {
  return new WorkerStream({
    host: "127.0.0.1",
    port,
    workerId: "worker-var-sec-test",
    token: "tok",
    certPath: path.join(CERTS, "worker.crt"),
    keyPath: path.join(CERTS, "worker.key"),
    caPath: path.join(CERTS, "ca.crt"),
    serverName: "control",
    heartbeatSec: 3600,
    dial: plainDial,
    ...extra,
  });
}

const waitFor = async (pred, ms = 5000) => {
  const start = Date.now();
  while (Date.now() - start < ms) {
    if (pred()) return true;
    await new Promise((r) => setTimeout(r, 15));
  }
  return false;
};

function richVariationPushJob(jobId, sinkPort) {
  return {
    type: "push_job",
    request_id: "req-" + jobId,
    job_id: jobId,
    send: {
      recipient: "alice@example.com",
      from_address: "sender@ourdomain.com",
      subject: "Variation security test",
      body: '<div style="padding: 20px;"><p>Hello real SMTP session under variation</p><a href="https://example.com/a">a</a></div>',
      headers: {
        From: '"P8 Suite" <sender@ourdomain.com>',
        To: "alice@example.com",
        Subject: "Variation security test",
        "Message-ID": `<${jobId}@ourdomain.com>`,
        Date: new Date().toUTCString(),
      },
    },
    smtp: { host: "127.0.0.1", port: sinkPort, username: "sender@ourdomain.com", secret: CANARY, security: "starttls" },
    // A LIVE (non-expired) lease — #runJob must reach the real SMTP session, unlike every
    // buffer.test.mjs fixture which pushes an already-expired lease and short-circuits.
    lease_expires_at: new Date(Date.now() + 60_000).toISOString(),
    variation: {
      html_minify: true,
      comment_injection: true,
      comment_intensity: "high",
      css_micro: true,
      unique_url_params: true,
      encoding_rotation: true,
      header_style: "gmail",
      user_agent_injection: true,
      user_agent_rate: 1,
      precedence: "marketing",
    },
  };
}

test("S1 (Phase 8): the credential never leaks through a REAL SMTP session with variation genuinely active", async () => {
  const sink = await startSink({ authResult: "ok" });
  let report = null;
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      if (msg.type === "register") {
        conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true });
        conn.reply(richVariationPushJob("job-var-sec-1", sink.port));
      } else if (msg.type === "report") {
        report = msg;
      }
    },
  });
  // A short smtpTimeoutMs (matching smtp.test.mjs's own 5000ms convention) — the sink here answers
  // instantly, so this bounds only how long a stray dangling timer inside smtp.js's withTimeout
  // (a pre-existing, out-of-Phase-8-scope gap: it never clears the loser of its Promise.race) can
  // hold the test process open after the assertions below have already passed.
  const worker = makeWorker(control.port, { smtpTimeoutMs: 5000 });
  try {
    await worker.start();
    const got = await waitFor(() => report !== null, 8000);
    assert.ok(got, "the worker never reported an outcome for the variation-on push_job — the real SMTP session path did not complete");

    assert.equal(report.outcome, "accepted", `expected accepted, got ${report.outcome} (${report.error_reason || ""})`);

    // THE ASSERTION: nowhere reachable from the worker's report frame does the plaintext canary appear.
    const reportBlob = JSON.stringify(report);
    assert.ok(!reportBlob.includes(CANARY), `canary leaked into the worker's report frame: ${reportBlob}`);

    // Non-vacuity: the sink genuinely saw AUTH (the credential path was exercised, not
    // short-circuited) and genuinely saw DATA (the variation-assembled message was transmitted).
    assert.ok(sink.received.some((l) => /AUTH/i.test(l)), "sink never saw AUTH — the credential path was not exercised, test is vacuous");
    assert.ok(sink.received.some((l) => /^\[.*\]\s*DATA$/i.test(l)), "sink never saw DATA — the variation-assembled message was never transmitted, test is vacuous");
    // Defense-in-depth: the canary must not appear anywhere in what the sink recorded either.
    for (const line of sink.received) {
      assert.ok(!line.includes(CANARY), `canary appeared in plaintext on the wire: ${line}`);
    }
  } finally {
    worker.stop();
    await control.close();
    await sink.close();
  }
});
