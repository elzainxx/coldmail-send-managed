// smtp.js — the full SMTP session the worker runs itself (Phase 3 §4). Dependency-free: Node's
// built-in net/tls only (the worker ships NO npm packages — structural no-DB/no-KMS boundary, and
// no SMTP library either). Sequence (§4.1): connect, EHLO, STARTTLS (587/25) or implicit TLS
// (465), AUTH, MAIL FROM, RCPT TO, DATA, QUIT.
//
// LOCKED security properties (§4.2/§4.4):
//   - AUTH is NEVER sent before TLS. On 587/25, if the server does not advertise STARTTLS the
//     session ABORTS BEFORE AUTH (error_reason=starttls_unavailable) — the credential is never
//     transmitted over an unencrypted connection (S5).
//   - The plaintext credential is memory-only (held in a MemoryCredential), never written to
//     disk, never logged, never in a metric/error/exception payload, and zeroed on EVERY exit
//     path — clean QUIT, SMTP error at AUTH, connection failure, timeout, thrown exception (S1/S3).
//   - Any SMTP response line is scrubbed of the credential before it leaves this module (§4.4).

import net from "node:net";
import tls from "node:tls";

// SMTPError carries an internal error_reason slug (§2.4) plus the last SMTP response line. It is
// deliberately built to NEVER carry the credential (only the server's own response text).
export class SMTPError extends Error {
  constructor(reason, smtpResponse) {
    super(reason);
    this.name = "SMTPError";
    this.reason = reason; // internal slug: connect_refused | auth_failed | starttls_unavailable | ...
    this.smtpResponse = smtpResponse || "";
  }
}

// A tiny line-oriented SMTP reader/writer over a socket. Reads multiline replies (RFC 5321:
// "250-" continuation lines, "250 " final line) and returns { code, text }.
//
// REPLIES ARE QUEUED, NOT ONLY DELIVERED TO A WAITER (the M2 close-out, ported from module 3's fix of the
// identical code). A complete reply used to be handed to the waiter registered by read() — and DROPPED
// when none was registered yet. Every command is `await conn.write(line)` then `conn.read()`, and a fast
// server can answer before the write's continuation has called read(), so that reply vanished and the
// session sat until the 30 s timeout: ~1 session in 10 against a fast server (0 of 240 at 8 ms reply
// latency, which is why real servers rarely hit it). A complete reply that arrives with no waiter now
// goes into `replies`, and read() takes from there first. Order is preserved (one reply per read(),
// FIFO), and the waiter path is unchanged for a reply that arrives after read().
//
// Exported only so test/smtp_reply_queue.test.mjs can drive the reader over a real socket without
// standing up a whole session; the session code in this file is the only production user.
export class SmtpConn {
  constructor(socket, timeoutMs) {
    this.sock = socket;
    this.timeoutMs = timeoutMs;
    this.buf = "";
    this.waiters = [];
    this.replies = []; // complete replies that arrived before any read() was waiting for them
    this.closed = false;
    this.sock.setEncoding("utf8");
    this.sock.on("data", (d) => this.#onData(d));
    this.sock.on("error", () => this.#fail());
    this.sock.on("close", () => this.#fail());
  }

  #fail() {
    this.closed = true;
    for (const w of this.waiters.splice(0)) w.reject(new SMTPError("connection_closed", ""));
  }

  #onData(chunk) {
    this.buf += chunk;
    let idx;
    // A complete reply ends with a line "NNN " (space after code) — collect until we see one.
    while ((idx = this.buf.indexOf("\n")) >= 0) {
      const line = this.buf.slice(0, idx).replace(/\r$/, "");
      this.buf = this.buf.slice(idx + 1);
      this.#pushLine(line);
    }
  }

  #pushLine(line) {
    if (!this._acc) this._acc = [];
    this._acc.push(line);
    // Final line of a reply: "NNN<space>..."; continuation: "NNN-...".
    if (/^\d{3} /.test(line)) {
      const code = parseInt(line.slice(0, 3), 10);
      const text = this._acc.join("\n");
      this._acc = null;
      const reply = { code, text };
      const w = this.waiters.shift();
      if (w) w.resolve(reply);
      else this.replies.push(reply); // nobody is reading yet: keep it for the next read(), never drop it
    }
  }

  // read() awaits the next complete SMTP reply. A reply that already arrived is returned at once, FIFO;
  // that holds even if the socket has since closed — a reply that was received is delivered, exactly as
  // one that arrived while a waiter was registered always was — and only when nothing is queued does a
  // closed connection reject.
  read() {
    return new Promise((resolve, reject) => {
      const queued = this.replies.shift();
      if (queued) return resolve(queued);
      if (this.closed) return reject(new SMTPError("connection_closed", ""));
      this.waiters.push({ resolve, reject });
    });
  }

  write(line) {
    return new Promise((resolve, reject) => {
      if (this.closed) return reject(new SMTPError("connection_closed", ""));
      this.sock.write(line + "\r\n", "utf8", (err) => (err ? reject(new SMTPError("write_failed", "")) : resolve()));
    });
  }

  // writeRaw sends bytes with no CRLF appended (used for the DATA payload which manages its own).
  writeRaw(data) {
    return new Promise((resolve, reject) => {
      if (this.closed) return reject(new SMTPError("connection_closed", ""));
      this.sock.write(data, "utf8", (err) => (err ? reject(new SMTPError("write_failed", "")) : resolve()));
    });
  }

  end() {
    try { this.sock.end(); } catch { /* ignore */ }
    try { this.sock.destroy(); } catch { /* ignore */ }
  }
}

// connectPlain / upgradeTLS helpers -----------------------------------------------------------

function connectPlain(host, port, timeoutMs) {
  return new Promise((resolve, reject) => {
    const sock = net.connect({ host, port });
    const to = setTimeout(() => { sock.destroy(); reject(new SMTPError("timeout", "")); }, timeoutMs);
    sock.once("connect", () => { clearTimeout(to); resolve(sock); });
    sock.once("error", () => { clearTimeout(to); reject(new SMTPError("connect_refused", "")); });
  });
}

function connectImplicitTLS(host, port, timeoutMs, allowSelfSigned) {
  return new Promise((resolve, reject) => {
    const sock = tls.connect({ host, port, servername: host, rejectUnauthorized: !allowSelfSigned });
    const to = setTimeout(() => { sock.destroy(); reject(new SMTPError("timeout", "")); }, timeoutMs);
    sock.once("secureConnect", () => { clearTimeout(to); resolve(sock); });
    sock.once("error", () => { clearTimeout(to); reject(new SMTPError("connect_refused", "")); });
  });
}

function upgradeToTLS(plainSock, host, timeoutMs, allowSelfSigned) {
  return new Promise((resolve, reject) => {
    const sock = tls.connect({ socket: plainSock, servername: host, rejectUnauthorized: !allowSelfSigned });
    const to = setTimeout(() => { sock.destroy(); reject(new SMTPError("timeout", "")); }, timeoutMs);
    sock.once("secureConnect", () => { clearTimeout(to); resolve(sock); });
    sock.once("error", () => { clearTimeout(to); reject(new SMTPError("starttls_failed", "")); });
  });
}

// isPrivateHost reports whether a host is loopback/RFC-1918/link-local, i.e. a test/sink target
// on a private network. Certificate validation MAY be skipped ONLY for such targets, and ONLY
// as a test affordance (§4.2) — never for a public host. Mirrors the inspiration worker's
// shouldSkipCertValidation. A public hostname always requires a valid cert.
export function isPrivateHost(host) {
  if (!host) return false;
  if (host === "localhost" || host === "smtp-sink") return true;
  if (/^127\./.test(host) || host === "::1") return true;
  if (/^10\./.test(host)) return true;
  if (/^192\.168\./.test(host)) return true;
  if (/^172\.(1[6-9]|2\d|3[01])\./.test(host)) return true;
  if (/^169\.254\./.test(host)) return true;
  return false;
}

// expect asserts the reply code is in the accepted set; else throws an SMTPError with the given
// reason and the (scrubbed) server response.
function expect(reply, okCodes, reason) {
  if (!okCodes.includes(reply.code)) {
    throw new SMTPError(reason, reply.text);
  }
  return reply;
}

/**
 * runSmtpSession performs one full SMTP send. The credential is provided as a MemoryCredential
 * and used ONLY inside this function; the caller zeroes it after (and this function tolerates it
 * already being zeroed on the abort-before-AUTH path). Returns { smtpResponse } on a 250 accept.
 * Throws SMTPError (reason + scrubbed smtpResponse) on any failure — the caller maps it to a
 * report. This function NEVER logs and NEVER puts the secret into an error.
 *
 * @param {object} job  { smtp:{host,port,username,security}, dataBytes:string, envelopeFrom, recipient }
 * @param {MemoryCredential} credential  memory-only plaintext secret
 * @param {object} opts { timeoutMs }
 */
export async function runSmtpSession(job, credential, opts = {}) {
  const timeoutMs = opts.timeoutMs || 30000;
  const { host, port, username, security } = job.smtp;
  const allowSelfSigned = isPrivateHost(host); // test affordance ONLY (§4.2)

  let conn = null;
  let sock = null;
  try {
    // 1) Connect (implicit TLS at connect for 465; plain otherwise).
    if (security === "implicit_tls") {
      sock = await connectImplicitTLS(host, port, timeoutMs, allowSelfSigned);
    } else {
      sock = await connectPlain(host, port, timeoutMs);
    }
    conn = new SmtpConn(sock, timeoutMs);

    // Server greeting.
    expect(await withTimeout(conn.read(), timeoutMs), [220], "connect_refused");

    // 2) EHLO with the sending domain derived from from_address (§4.1, P3-11 default).
    const heloName = "mail." + sendingDomain(job.envelopeFrom);
    let ehlo = expect(await ehloOrHelo(conn, heloName, timeoutMs), [250], "ehlo_failed");

    // 3) STARTTLS if requested. ABORT BEFORE AUTH if not advertised (§4.2, S5).
    if (security !== "implicit_tls") {
      if (!advertisesStartTLS(ehlo.text)) {
        throw new SMTPError("starttls_unavailable", ehlo.text);
      }
      expect(await withTimeout(conn.write("STARTTLS").then(() => conn.read()), timeoutMs), [220], "starttls_unavailable");
      // Upgrade the socket and re-EHLO over the now-encrypted channel.
      const tlsSock = await upgradeToTLS(sock, host, timeoutMs, allowSelfSigned);
      conn = new SmtpConn(tlsSock, timeoutMs);
      sock = tlsSock;
      ehlo = expect(await ehloOrHelo(conn, heloName, timeoutMs), [250], "ehlo_failed");
    }

    // 4) AUTH — now, and only now, is the credential put on the wire (over TLS). AUTH LOGIN.
    await doAuthLogin(conn, username, credential, timeoutMs);

    // 5) MAIL FROM.
    expect(await cmd(conn, `MAIL FROM:<${job.envelopeFrom}>`, timeoutMs), [250], "mail_from_rejected");

    // 6) RCPT TO (single recipient in Phase 3).
    const rcpt = await cmd(conn, `RCPT TO:<${job.recipient}>`, timeoutMs);
    if (rcpt.code === 550 || rcpt.code === 551 || rcpt.code === 553) {
      throw new SMTPError("bounced", rcpt.text); // hard reject → sending→bounced (§5.4)
    }
    expect(rcpt, [250, 251], "rcpt_rejected");

    // 7) DATA.
    expect(await cmd(conn, "DATA", timeoutMs), [354], "data_rejected_4xx");
    const payload = dotStuff(job.dataBytes) + "\r\n.\r\n";
    await conn.writeRaw(payload);
    const dataReply = await withTimeout(conn.read(), timeoutMs);
    if (dataReply.code >= 500) throw new SMTPError("data_rejected_5xx", dataReply.text);
    if (dataReply.code >= 400) throw new SMTPError("data_rejected_4xx", dataReply.text);
    expect(dataReply, [250], "data_rejected_4xx");

    // 8) QUIT (best-effort). SAFETY BOUND, deliberately short and NOT timeoutMs (Phase 4 closeout
    // triage, Docs/history/open-decisions.md): the send already succeeded by this point (DATA got 250), so
    // QUIT's outcome is discarded either way — waiting the full per-operation timeoutMs here would
    // only slow down freeing this connection for no benefit. Not a tunable send-behavior dial.
    try { await withTimeout(conn.write("QUIT").then(() => conn.read()), 2000); } catch { /* ignore */ }

    return { smtpResponse: scrubSecret(dataReply.text, credential) };
  } catch (e) {
    // Re-map generic errors to an SMTPError; scrub any credential from the response text.
    if (e instanceof SMTPError) {
      e.smtpResponse = scrubSecret(e.smtpResponse, credential);
      throw e;
    }
    throw new SMTPError("timeout", "");
  } finally {
    // Close the socket on every exit path. (Credential zeroing is the caller's finally, but we
    // also never retained the plaintext outside the MemoryCredential here.)
    if (conn) conn.end();
    else if (sock) { try { sock.destroy(); } catch { /* ignore */ } }
  }
}

// --- helpers ---------------------------------------------------------------------------------

function withTimeout(promise, ms) {
  return Promise.race([
    promise,
    new Promise((_, reject) => setTimeout(() => reject(new SMTPError("timeout", "")), ms)),
  ]);
}

async function cmd(conn, line, timeoutMs) {
  await conn.write(line);
  return withTimeout(conn.read(), timeoutMs);
}

async function ehloOrHelo(conn, name, timeoutMs) {
  await conn.write("EHLO " + name);
  const reply = await withTimeout(conn.read(), timeoutMs);
  if (reply.code === 250) return reply;
  // Fall back to HELO for servers that reject EHLO.
  await conn.write("HELO " + name);
  return withTimeout(conn.read(), timeoutMs);
}

function advertisesStartTLS(ehloText) {
  return /(^|\n)\d{3}[ -]STARTTLS\b/i.test(ehloText);
}

// doAuthLogin runs AUTH LOGIN (base64 username then password). The password is read from the
// MemoryCredential and base64-encoded in a transient Buffer that is zeroed immediately. On a
// non-235 reply it throws auth_failed WITHOUT including the credential.
async function doAuthLogin(conn, username, credential, timeoutMs) {
  const start = await cmd(conn, "AUTH LOGIN", timeoutMs);
  expect(start, [334], "auth_failed");
  await conn.write(Buffer.from(username, "utf8").toString("base64"));
  const userReply = await withTimeout(conn.read(), timeoutMs);
  expect(userReply, [334], "auth_failed");

  // Encode the password from the memory-only buffer; zero the transient b64 immediately.
  let b64 = null;
  await credential.use((buf) => {
    b64 = buf.toString("base64");
  });
  await conn.write(b64);
  // Best-effort scrub of the transient base64 string (JS strings are immutable, so at least drop
  // the reference; the durable secret lives only in the MemoryCredential, which the caller zeroes).
  b64 = null;

  const authReply = await withTimeout(conn.read(), timeoutMs);
  if (authReply.code !== 235) {
    // NEVER include the credential; only the server's own response text (scrubbed defensively).
    throw new SMTPError("auth_failed", scrubSecret(authReply.text, credential));
  }
}

// sendingDomain extracts the domain from an email address for the HELO name (§4.1).
function sendingDomain(addr) {
  const at = String(addr || "").lastIndexOf("@");
  return at >= 0 ? addr.slice(at + 1) : "localhost";
}

// dotStuff applies SMTP transparency (RFC 5321 §4.5.2): a line beginning with "." gets an extra ".".
function dotStuff(data) {
  return String(data).replace(/\r\n/g, "\n").replace(/\n/g, "\r\n").replace(/(^|\r\n)\./g, "$1..");
}

// scrubSecret removes the credential from a string defense-in-depth (§4.4). It reads the secret
// from the MemoryCredential ONLY to redact it; if the buffer is already zeroed it is a no-op.
// CRITICAL: the credential travels the AUTH exchange BASE64-encoded (AUTH LOGIN sends
// base64(password)), so a server that echoes the offending AUTH argument in its error text
// returns the base64 form — NOT the plaintext. We therefore redact BOTH the utf8 plaintext AND
// its base64 / base64url encodings, so a `535` that quotes the sent line cannot carry a decodable
// secret into report.smtp_response → send_logs. Never logs.
function scrubSecret(text, credential) {
  if (!text) return text || "";
  const forms = [];
  try {
    credential.use((buf) => {
      forms.push(buf.toString("utf8"));                                  // plaintext
      const b64 = buf.toString("base64");
      forms.push(b64);                                                   // AUTH LOGIN wire form
      forms.push(b64.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "")); // base64url
    });
  } catch {
    return text; // already zeroed — nothing to scrub against
  }
  let out = text;
  for (const f of forms) {
    if (f && out.includes(f)) out = out.split(f).join("[redacted]");
  }
  return out;
}
