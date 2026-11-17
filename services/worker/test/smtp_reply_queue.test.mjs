// SmtpConn reply-queue tests (the M2 close-out: module 3 found this defect in the identical src/smtp.js and
// fixed it; ported here with these tests). REAL sockets: the actual SmtpConn reads from a real TCP
// connection to a real local peer — no mocks.
//
// THE DEFECT. SmtpConn used to hand a completed reply to the waiter registered by read() and DROP it
// when no waiter was registered yet. Every SMTP command is `await conn.write(line)` followed by
// `conn.read()`, and a fast server can answer before the write's continuation has called read() — then
// the reply vanished and the session sat until its 30 s timeout (~1 session in 10 against a fast
// server). A complete reply that arrives with no waiter is now kept and the next read() returns it.
//
// EACH TEST IS ITS OWN PAIRED CONTROL ON THE OLD CODE. The three the fix is about (a reply written
// BEFORE read(); two back-to-back replies; a reply received just before the close) are RED on the old
// code — they time out waiting for a reply the old reader had thrown away — and the two that must not
// change (a reply written AFTER read(); read() with nothing to return) are GREEN on both. The race is
// FORCED, not hoped for: the peer's reply is confirmed parsed by SmtpConn (a second 'data' listener,
// registered after SmtpConn's own, fires only once SmtpConn has consumed the chunk) BEFORE the test
// calls read(), so "the reply arrived first" is a fact of the test, not a matter of timing.

import { test } from "node:test";
import assert from "node:assert/strict";
import net from "node:net";
import { SmtpConn, SMTPError } from "../src/smtp.js";

// within rejects if p has not settled in ms — the old reader never settled, and a 30 s hang is not a
// useful failure.
function within(p, ms, what) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what}: still pending after ${ms} ms — the reply was dropped`)), ms);
  });
  return Promise.race([p, timeout]).finally(() => clearTimeout(timer));
}

const delay = (ms) => new Promise((r) => setTimeout(r, ms));

// pair opens a real loopback TCP connection and wraps the client end in an SmtpConn.
async function pair() {
  const server = net.createServer();
  const accepted = new Promise((resolve) => server.once("connection", resolve));
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const client = net.connect({ host: "127.0.0.1", port: server.address().port });
  await new Promise((resolve, reject) => {
    client.once("connect", resolve);
    client.once("error", reject);
  });
  const peer = await accepted;
  const conn = new SmtpConn(client, 5000); // registers SmtpConn's own 'data' listener FIRST

  // Registered AFTER SmtpConn's listener, so when it fires SmtpConn has already parsed the chunk.
  let seen = "";
  const watchers = [];
  client.on("data", (d) => {
    seen += d;
    for (const w of watchers.slice()) {
      if (seen.includes(w.needle)) {
        watchers.splice(watchers.indexOf(w), 1);
        w.resolve();
      }
    }
  });
  const arrived = (needle) =>
    seen.includes(needle)
      ? Promise.resolve()
      : within(new Promise((resolve) => watchers.push({ needle, resolve })), 3000, `waiting for ${JSON.stringify(needle)} to reach the client`);

  const close = async () => {
    client.destroy();
    peer.destroy();
    await new Promise((resolve) => server.close(resolve));
  };
  return { conn, client, peer, arrived, close };
}

test("a reply that arrives BEFORE read() is registered is returned (the old reader dropped it)", async () => {
  const p = await pair();
  try {
    const parsed = p.arrived("250 ok");
    p.peer.write("250 ok\r\n");
    await parsed; // SmtpConn has consumed the reply; no read() has been called yet
    const reply = await within(p.conn.read(), 1000, "read() after the reply had already arrived");
    assert.deepEqual(reply, { code: 250, text: "250 ok" });
  } finally {
    await p.close();
  }
});

test("a reply that arrives AFTER read() is registered still works (the waiter path is unchanged)", async () => {
  const p = await pair();
  try {
    const pending = p.conn.read(); // registered first
    p.peer.write("220 hello\r\n");
    const reply = await within(pending, 1000, "read() registered before the reply");
    assert.deepEqual(reply, { code: 220, text: "220 hello" });
  } finally {
    await p.close();
  }
});

test("two replies back to back are returned in order, one per read()", async () => {
  const p = await pair();
  try {
    const parsed = p.arrived("250 second");
    p.peer.write("220 first\r\n250 second\r\n");
    await parsed; // both are parsed and queued before the first read()
    const first = await within(p.conn.read(), 1000, "first read()");
    const second = await within(p.conn.read(), 1000, "second read()");
    assert.deepEqual(first, { code: 220, text: "220 first" }, "the FIRST read() must return the FIRST reply");
    assert.deepEqual(second, { code: 250, text: "250 second" }, "the SECOND read() must return the SECOND reply");

    // Mixed: the queue is empty now, so a third read() waits and a LATER reply reaches it.
    const third = p.conn.read();
    p.peer.write("354 third\r\n");
    assert.deepEqual(await within(third, 1000, "third read() (waiter path after the queue drained)"), { code: 354, text: "354 third" });
  } finally {
    await p.close();
  }
});

test("a multi-line reply that arrived early is ONE reply, and nothing else is left behind", async () => {
  const p = await pair();
  try {
    const parsed = p.arrived("250 AUTH LOGIN");
    p.peer.write("250-mail.example\r\n250-STARTTLS\r\n250 AUTH LOGIN\r\n");
    await parsed;
    const reply = await within(p.conn.read(), 1000, "read() of an early multi-line reply");
    assert.equal(reply.code, 250);
    assert.equal(reply.text, "250-mail.example\n250-STARTTLS\n250 AUTH LOGIN");
    // Exactly one reply was queued: the next read() must WAIT, not return a phantom.
    const next = await Promise.race([p.conn.read().then(() => "returned"), delay(200).then(() => "waiting")]);
    assert.equal(next, "waiting", "a second read() returned something after one multi-line reply");
  } finally {
    await p.close();
  }
});

test("read() with nothing queued WAITS (the queue never invents a reply)", async () => {
  const p = await pair();
  try {
    const idle = p.conn.read();
    const state = await Promise.race([idle.then(() => "returned"), delay(200).then(() => "waiting")]);
    assert.equal(state, "waiting", "read() resolved with no reply having been sent");
    p.peer.write("250 late\r\n"); // and the waiter is still live
    assert.deepEqual(await within(idle, 1000, "the waiting read()"), { code: 250, text: "250 late" });
  } finally {
    await p.close();
  }
});

test("a reply received just before the connection closed is delivered; only then does read() reject connection_closed", async () => {
  const p = await pair();
  try {
    const parsed = p.arrived("221 bye");
    const closed = new Promise((resolve) => p.client.once("close", resolve)); // after SmtpConn's own 'close' listener
    p.peer.write("221 bye\r\n");
    await parsed;
    p.peer.end();
    await within(closed, 3000, "the client socket closing");

    const reply = await within(p.conn.read(), 1000, "read() after the reply arrived and the socket then closed");
    assert.deepEqual(reply, { code: 221, text: "221 bye" });
    // Nothing is queued now, so a closed connection rejects exactly as it always did.
    await assert.rejects(within(p.conn.read(), 1000, "read() on a closed connection with nothing queued"),
      (e) => e instanceof SMTPError && e.reason === "connection_closed");
  } finally {
    await p.close();
  }
});
