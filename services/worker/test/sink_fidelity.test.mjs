// Fidelity of the throwaway SMTP sink (test/sink.mjs) — the one place where the sink must behave exactly like a real server,
// because src/smtp.js's reply reader is sensitive to it.
//
// THE DEFECT THIS PINS (found 2026-11-08 on the first real-Linux run of the worker unit tests): the sink greeted a SECOND time
// on the TLS socket right after STARTTLS. A real server never does (RFC 3207 §4.2: the client restarts with EHLO). The reader
// in src/smtp.js dropped a reply that arrived while no read() was pending (it now queues it — the 2026-11-17 reply-queue fix —
// which makes a stray "220" deterministically fatal rather than timing-dependent), so the stray "220" was either swallowed (the tests
// passed) or taken for the reply to the client's post-TLS EHLO, shifting every later reply by one — AUTH LOGIN "answered" with
// a 250 and 10 tests failing with auth_failed, S1/S3 (credential scrub/zeroing) among them. Which one happened depended on
// event-loop timing between 'secureConnect' and the first data event, i.e. on the PLATFORM: measured 2026-11-08, 10 red on
// Linux (Node 20.20 and Node 24.22 alike, 3 of 3 runs each), all green on the Windows dev host. It is not a Node-version
// difference. A green that depends on a race is not evidence.
//
// WHY A TEST AND NOT JUST THE FIX: the fix is one removed line and looks like harmless tidying to reverse. This fails the
// moment the sink speaks first after STARTTLS, on any Node, deterministically (it looks at the wire, not at a downstream
// symptom), and it has its own control proving the detector can see stray bytes at all.

import { test } from "node:test";
import assert from "node:assert/strict";
import net from "node:net";
import tls from "node:tls";
import { startSink } from "./sink.mjs";

// collect(sock, ms): every byte that arrives on `sock` within `ms`, as a string. The point is the EMPTY case, so this must be
// able to return non-empty — see the control below.
function collect(sock, ms) {
  return new Promise((resolve) => {
    let got = "";
    const onData = (d) => { got += d.toString("utf8"); };
    sock.on("data", onData);
    setTimeout(() => { sock.off("data", onData); resolve(got); }, ms);
  });
}

// readUntil(sock, re): resolve with everything read once `re` matches the accumulated text (or reject on timeout).
function readUntil(sock, re, ms = 3000) {
  return new Promise((resolve, reject) => {
    let got = "";
    const to = setTimeout(() => { sock.off("data", onData); reject(new Error(`timed out waiting for ${re}; got ${JSON.stringify(got)}`)); }, ms);
    const onData = (d) => {
      got += d.toString("utf8");
      if (re.test(got)) { clearTimeout(to); sock.off("data", onData); resolve(got); }
    };
    sock.on("data", onData);
  });
}

// CONTROL: the detector is only evidence if it can fail. A server that talks unprompted must make collect() non-empty.
test("sink fidelity CONTROL: collect() sees bytes a chatty server sends unprompted (the detector is not vacuous)", async () => {
  const chatty = net.createServer((s) => { setTimeout(() => s.write("220 stray greeting\r\n"), 20); });
  await new Promise((r) => chatty.listen(0, "127.0.0.1", r));
  try {
    const c = net.connect({ host: "127.0.0.1", port: chatty.address().port });
    await new Promise((r) => c.once("connect", r));
    const got = await collect(c, 300);
    assert.match(got, /^220 stray greeting/, "collect() did not observe an unprompted line — the silence assertion below could not fail");
    c.destroy();
  } finally { await new Promise((r) => chatty.close(r)); }
});

test("sink fidelity: after STARTTLS the sink stays silent until the client speaks (RFC 3207 — no second greeting)", async () => {
  const sink = await startSink({ authResult: "ok" });
  const plain = net.connect({ host: "127.0.0.1", port: sink.port });
  try {
    await new Promise((r) => plain.once("connect", r));
    const greeting = await readUntil(plain, /^220 .*\r?\n/);
    assert.match(greeting, /^220 test-sink/, "the plaintext connection must be greeted exactly once");
    plain.write("EHLO fidelity.test\r\n");
    await readUntil(plain, /250 8BITMIME/);
    plain.write("STARTTLS\r\n");
    const ready = await readUntil(plain, /220 ready to start TLS\r?\n/);
    assert.equal(ready.split(/\r?\n/).filter((l) => /^\d{3} /.test(l)).length, 1, "exactly one reply to STARTTLS");

    // hand the raw socket to TLS (our own reader must be off it first, or it would read ciphertext)
    plain.removeAllListeners("data");
    const secure = tls.connect({ socket: plain, rejectUnauthorized: false });
    await new Promise((resolve, reject) => { secure.once("secureConnect", resolve); secure.once("error", reject); });

    const unsolicited = await collect(secure, 300);
    assert.equal(unsolicited, "", `the sink spoke first after STARTTLS (RFC 3207 forbids a second greeting): ${JSON.stringify(unsolicited)}`);

    // and the FIRST thing on the secure channel is the reply to the client's own EHLO — in step, not shifted by one
    secure.write("EHLO fidelity.test\r\n");
    const ehlo = await readUntil(secure, /250 8BITMIME/);
    assert.match(ehlo, /^250-test-sink greets you/, `the first reply after the post-TLS EHLO must be the EHLO reply, got ${JSON.stringify(ehlo)}`);
    assert.ok(!/^220/m.test(ehlo), "a 220 appeared ahead of the EHLO reply");
    secure.destroy();
  } finally {
    plain.destroy();
    await sink.close();
  }
});
