// control-helpers.mjs — shared test scaffolding for the worker tests that need a REAL local "fake Control" (a
// net.Server speaking the newline-JSON worker protocol) and a REAL WorkerStream wired to it over a plain socket.
// Only the TLS wrap is substituted (the worker's injectable `dial` seam; buffer.test.mjs explains why): mutual TLS
// is proven by the live-stack suites. Not a test file (no .test. in the name), so node --test does not run it.

import net from "node:net";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { WorkerStream } from "../src/stream.js";

const here = path.dirname(fileURLToPath(import.meta.url));
// Read at construction (WorkerStream reads its identity files even with a custom dial); the plain dial ignores them.
export const CERTS = path.resolve(here, "../../../secrets/dev/certs");

export const waitFor = async (pred, ms = 5000) => {
  const start = Date.now();
  while (Date.now() - start < ms) {
    if (pred()) return true;
    await new Promise((r) => setTimeout(r, 10));
  }
  return false;
};

// startFakeControl: onFrame(frame, conn) handles each frame the worker sends; conn.reply(obj) answers. frames and
// conns record everything; close() drops every connection and the listener.
export async function startFakeControl(opts = {}) {
  const frames = [];
  const conns = [];
  const server = net.createServer((conn) => {
    conns.push(conn);
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
        frames.push(msg);
        if (opts.onFrame) opts.onFrame(msg, conn);
      }
    });
    conn.on("error", () => { /* peer resets */ });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  return {
    port: server.address().port, frames, conns,
    close: () => { for (const c of conns) c.destroy(); return new Promise((r) => server.close(r)); },
  };
}

export const plainDial = (o, _tls, onSecure) => net.connect({ host: o.host, port: o.port }, onSecure);

// failingSocket is a socket that fails the way a refused/unreachable dial fails: an 'error' and then 'close'.
export function failingSocket(message = "connect ECONNREFUSED (test)") {
  const s = new net.Socket();
  setImmediate(() => s.destroy(new Error(message)));
  return s;
}

export function makeStream(port, extra = {}) {
  return new WorkerStream({
    host: "127.0.0.1",
    port,
    workerId: "worker-test",
    token: extra.token || "tok",
    certPath: path.join(CERTS, "harness-client.crt"),
    keyPath: path.join(CERTS, "harness-client.key"),
    caPath: path.join(CERTS, "ca.crt"),
    serverName: "control",
    heartbeatSec: 3600,
    dial: plainDial,
    ...extra,
  });
}

// instantSleep: no wait — for tests about a sequence, not the clock.
export const instantSleep = async () => { /* no wait */ };
