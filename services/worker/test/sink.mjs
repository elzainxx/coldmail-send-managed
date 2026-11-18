// A REAL, controllable throwaway SMTP server for worker unit tests (no mocks — a genuine socket
// server the real src/smtp.js talks to). Configurable to exercise the worker's security paths:
//   - advertiseStartTLS:false  → the worker must ABORT BEFORE AUTH (S5).
//   - authResult:"fail"        → 535 at AUTH (S1 failed-AUTH trigger; S3 error exit).
//   - authResult:"ok"          → full accept (positive control).
// It records every line the client sent so a test can assert the credential was NEVER transmitted
// on the no-STARTTLS path (S5).

import net from "node:net";
import tls from "node:tls";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
// Reuse the dev sink leaf cert for TLS (loopback target; the worker skips cert validation for
// private hosts as the documented test affordance, §4.2).
const certDir = path.resolve(here, "../../../secrets/dev/certs");

export function startSink(opts = {}) {
  const advertiseStartTLS = opts.advertiseStartTLS !== false; // default true
  const authResult = opts.authResult || "ok"; // "ok" | "fail"
  // echoAuthInError: a hostile/verbose server that quotes the received AUTH argument (the
  // base64-encoded password) back in its 535 text. Used to prove scrubSecret redacts the base64
  // form, not just the plaintext (§4.4). Default false (well-behaved server).
  const echoAuthInError = opts.echoAuthInError === true;
  const received = []; // every line the client sent (plaintext + TLS phases)
  // messages: the DATA payload of every message the sink ACCEPTED, as the wire carried it: CRLF line endings,
  // SMTP dot-stuffing undone (RFC 5321 4.5.2), without the terminating "." line. This is what a downstream
  // server (and anything that signs or parses the message) actually receives from the worker.
  const messages = [];
  let tlsActive = false;

  const tlsOpts = {
    cert: fs.readFileSync(path.join(certDir, "sink.crt")),
    key: fs.readFileSync(path.join(certDir, "sink.key")),
  };

  const server = net.createServer((sock) => handleConn(sock, false));

  function handleConn(sock, isTls) {
    sock.setEncoding("utf8");
    let buf = "";
    let inData = false;
    // A server greets ONCE, on the plaintext connection. After STARTTLS (RFC 3207 §4.2) it does NOT greet again — the
    // client simply restarts the session with EHLO. This used to greet on the secure socket too, which put an unsolicited
    // "220" on the wire ahead of the reply to the client's post-TLS EHLO. src/smtp.js USED TO drop a reply that arrived while
    // no read() was pending (#pushLine), so whether the stray line was swallowed or taken for the EHLO reply — shifting every
    // later reply by one, so AUTH LOGIN was "answered" with a 250 — depended on event-loop timing between TLS 'secureConnect'
    // and the first data event. MEASURED 2026-11-08 (module 3, same sink): on the Windows dev host the stray line arrived
    // before any read() was pending and was swallowed (every test green); on Linux — Node 20.20 AND Node 24.22, 3 of 3
    // runs each — it arrived after, was taken for the EHLO reply, and 10 tests failed with auth_failed. So it was never
    // a Node-version difference; the dev host's timing was hiding it. Pinned by sink_fidelity.test.mjs. (Since the 2026-11-17
    // reply-queue fix, src/smtp.js keeps a reply that arrives before read(), so a stray greeting would now be taken for the
    // EHLO reply every time, on every host — still never send one.)
    if (!isTls) write(sock, "220 test-sink ESMTP ready");
    sock.on("data", (chunk) => {
      buf += chunk;
      let idx;
      while ((idx = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, idx).replace(/\r$/, "");
        buf = buf.slice(idx + 1);
        received.push((isTls ? "[tls] " : "[plain] ") + line);
        onLine(sock, line, isTls, () => (inData = true), () => (inData = false), () => inData);
      }
    });
    sock.on("error", () => {});
  }

  function onLine(sock, line, isTls, setData, clearData, inData) {
    if (inData()) {
      if (line === ".") {
        clearData();
        messages.push((sock._data || []).join("\r\n"));
        sock._data = [];
        write(sock, "250 2.0.0 Ok: queued as SINK12345");
      } else {
        (sock._data ||= []).push(line.startsWith("..") ? line.slice(1) : line);
      }
      return;
    }
    const up = line.toUpperCase();
    if (up.startsWith("EHLO") || up.startsWith("HELO")) {
      const caps = ["250-test-sink greets you"];
      if (advertiseStartTLS && !isTls) caps.push("250-STARTTLS");
      caps.push("250-AUTH LOGIN PLAIN");
      caps.push("250 8BITMIME");
      write(sock, caps.join("\r\n"));
    } else if (up === "STARTTLS") {
      if (!advertiseStartTLS) { write(sock, "502 not supported"); return; }
      write(sock, "220 ready to start TLS");
      const secure = new tls.TLSSocket(sock, { isServer: true, ...tlsOpts });
      tlsActive = true;
      handleConn(secure, true);
    } else if (up.startsWith("AUTH")) {
      // AUTH LOGIN: reply 334, then username(334), then password → 235/535.
      write(sock, "334 VXNlcm5hbWU6"); // "Username:"
      sock._authStep = "user";
    } else if (sock._authStep === "user") {
      write(sock, "334 UGFzc3dvcmQ6"); // "Password:"
      sock._authStep = "pass";
    } else if (sock._authStep === "pass") {
      sock._authStep = null;
      if (authResult === "fail") {
        // `line` here is the base64-encoded password the client just sent. A hostile server may
        // echo it in the error; a well-behaved one does not.
        if (echoAuthInError) write(sock, `535 5.7.8 Authentication failed for argument '${line}'`);
        else write(sock, "535 5.7.8 Authentication credentials invalid");
      } else {
        write(sock, "235 2.7.0 Authentication successful");
      }
    } else if (up.startsWith("MAIL FROM")) {
      write(sock, "250 2.1.0 Ok");
    } else if (up.startsWith("RCPT TO")) {
      write(sock, "250 2.1.5 Ok");
    } else if (up === "DATA") {
      write(sock, "354 End data with <CR><LF>.<CR><LF>");
      sock._data = [];
      setData();
    } else if (up === "QUIT") {
      write(sock, "221 2.0.0 Bye");
      sock.end();
    } else {
      write(sock, "250 2.0.0 Ok");
    }
  }

  function write(sock, line) {
    try { sock.write(line + "\r\n"); } catch { /* ignore */ }
  }

  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const port = server.address().port;
      resolve({
        port,
        received,
        messages,
        tlsWasActive: () => tlsActive,
        close: () => new Promise((r) => server.close(r)),
      });
    });
  });
}
