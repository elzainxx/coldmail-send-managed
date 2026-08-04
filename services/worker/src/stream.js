// stream.js — the worker side of the Phase-3 persistent worker↔Control control channel (§2.1). The worker
// DIALS the Control's mTLS stream port and holds ONE long-lived connection: it sends `register`
// first, then `heartbeat`s on an interval, receives `push_job` frames, runs the SMTP session
// (src/smtp.js), and sends `report` back — all as newline-delimited JSON frames.
//
// mTLS both ways (§3.3, S4): the worker presents its client cert AND verifies the Control's server
// cert against the CA (rejectUnauthorized:true). The worker will NOT send `register` to a Control it
// cannot verify, so no credential-bearing push is ever accepted from a forged Control.
//
// The plaintext credential from push_job is held ONLY in a MemoryCredential for the one session
// and zeroed on EVERY exit path — accept, error, exception (§4.4, S1/S3). The worker has NO DB and
// NO KMS: it receives an already-decrypted secret and returns only an outcome (§3.4).
//
// Phase 7 §1.3 (P7-4) — OUTCOME BUFFERING + PERSIST-ACK. A report the worker cannot confirm was
// durably stored (stream down, Control restarting, DB outage — the worker can't tell which) is RETAINED
// in an in-memory buffer keyed by job_id and REPLAYED on every (re)connect until the Control returns
// a `report_ack` for that job — an ack the Control sends ONLY AFTER the outcome is committed. On ack
// the entry is dropped. The buffer is bounded by box_outcome_buffer_ttl_sec (delivered on
// register_ack): an entry older than the TTL is dropped and the job falls back to Control reclaim +
// the accepted duplicate. The CREDENTIAL is never buffered — only the outcome (§4.4 unchanged).
// Idempotency (provider_message_id UNIQUE + the Control's pre-persist check) makes a replay a clean
// no-op, never a second send. This one mechanism covers stream-drop, Control-restart, and DB-outage.

import tls from "node:tls";
import fs from "node:fs";
import { MemoryCredential } from "./creds.js";
import { runSmtpSession, SMTPError } from "./smtp.js";
import { assembleDataBytes } from "./assemble.js";

// defaultDial is the PRODUCTION transport: a mutual-TLS connection to the Control (§3.3). It is the
// only transport the worker ever uses in production. A test may inject an alternative dial (o.dial) to
// drive the REAL read loop / outcome buffer / replay over a plain OS socket — substituting ONLY the
// TLS wrap, which is proven separately by the live-stack mTLS suite + the S4 handshake test. This is
// the same "stand in only the already-proven wire" philosophy the Go dispatch harness uses for SMTP.
function defaultDial(o, tlsMaterial, onSecure) {
  const sock = tls.connect({
    host: o.host,
    port: o.port,
    cert: tlsMaterial.cert,
    key: tlsMaterial.key,
    ca: tlsMaterial.ca,
    servername: o.serverName || "control",
    rejectUnauthorized: true, // verify the Control (§3.3) — refuse a Control we can't authenticate
  });
  sock.once("secureConnect", onSecure);
  return sock;
}

export class WorkerStream {
  /**
   * @param {object} o
   * @param {string} o.host  Control host
   * @param {number} o.port  Control stream mTLS port
   * @param {string} o.workerId
   * @param {string} o.token registration token (authority)
   * @param {string} o.certPath worker client cert
   * @param {string} o.keyPath  worker client key
   * @param {string} o.caPath   CA that signed the Control server cert
   * @param {string} o.serverName SAN to verify ("control")
   * @param {number} o.heartbeatSec
   * @param {number} o.smtpTimeoutMs per-operation SMTP timeout (from push_job/config)
   * @param {object} o.metadata optional {box_version, hostname, started_at}
   * @param {function} o.onZeroed optional test hook: called (jobId) after the credential is zeroed
   * @param {number} o.outcomeBufferTtlMs bound on the outcome buffer (P7-4); overridden by the
   *        Control's register_ack (box_outcome_buffer_ttl_sec). Default 900_000 (15 min).
   * @param {function} o.onAck optional test hook: called (jobId) after a report_ack drops a buffered entry
   * @param {function} o.now optional clock seam (returns epoch ms) for TTL tests
   */
  constructor(o) {
    this.host = o.host;
    this.port = o.port;
    this.workerId = o.workerId;
    this.token = o.token;
    this.serverName = o.serverName || "control";
    this.heartbeatSec = o.heartbeatSec || 120;
    this.smtpTimeoutMs = o.smtpTimeoutMs || 30000;
    this.metadata = o.metadata || {};
    this.onZeroed = o.onZeroed || null;
    this.onAck = o.onAck || null;
    this.now = o.now || (() => Date.now());
    // dial is the transport factory: production uses defaultDial (mutual-TLS). Tests may inject one
    // to exercise the real read loop/buffer/replay over a plain socket (TLS proven elsewhere).
    this.dial = o.dial || defaultDial;
    this.tls = {
      cert: fs.readFileSync(o.certPath),
      key: fs.readFileSync(o.keyPath),
      ca: fs.readFileSync(o.caPath),
    };
    this.sock = null;
    this.buf = "";
    this.hbTimer = null;
    this.registered = false;
    this.stopped = false;

    // Phase 7 (§1.3, P7-4): the outcome buffer. Map<job_id, {report, firstSeenMs}>. An outcome lives
    // here from the moment it is produced until the Control acks it (dropped) or it ages past the TTL
    // (dropped → falls back to Control reclaim). NEVER holds a credential — only the report object.
    this.outcomeBuffer = new Map();
    this.outcomeBufferTtlMs = o.outcomeBufferTtlMs || 900_000; // overridden by register_ack
    // The other half of §1.3's bound ("bounded by box_outcome_buffer_ttl_sec … and a max entry
    // count"). A structural safety bound, not an operator dial — see #bufferAndSend.
    this.outcomeBufferMaxEntries = o.outcomeBufferMaxEntries || 1024;
    this.overflowDropped = 0; // observability/test counter: entries dropped by the cardinality bound
    this.supersededDropped = 0; // entries replaced because the job was pushed again (a new attempt)
  }

  // start dials the Control, completes the register handshake, and begins the read + heartbeat
  // loops. Resolves once registered (or rejects on a rejected register / TLS failure).
  start() {
    // Reset per-connection state so a reconnect (P7-4) starts clean. The outcome buffer is NOT reset
    // — it deliberately survives the reconnect so unacked outcomes replay on the new register_ack.
    this.registered = false;
    this.buf = "";
    return new Promise((resolve, reject) => {
      const onSecure = () => {
        // First frame MUST be register (§3.1).
        this.#send({
          type: "register",
          request_id: reqID(),
          box_id: this.workerId,
          token: this.token,
          box_version: this.metadata.box_version,
          hostname: this.metadata.hostname,
          started_at: this.metadata.started_at,
        });
      };
      const sock = this.dial(this, this.tls, onSecure);
      this.sock = sock;
      sock.setEncoding("utf8");

      sock.on("data", (d) => this.#onData(d, resolve, reject));
      sock.once("error", (e) => { if (!this.registered) reject(e); });
      sock.once("close", () => {
        this.#stopHeartbeat();
        if (!this.registered) reject(new Error("stream closed before register"));
      });
    });
  }

  stop() {
    this.stopped = true;
    this.#stopHeartbeat();
    if (this.sock) { try { this.sock.destroy(); } catch { /* ignore */ } }
  }

  #onData(chunk, resolve, reject) {
    this.buf += chunk;
    let idx;
    while ((idx = this.buf.indexOf("\n")) >= 0) {
      const line = this.buf.slice(0, idx);
      this.buf = this.buf.slice(idx + 1);
      if (!line.trim()) continue;
      let msg;
      try { msg = JSON.parse(line); } catch { continue; }
      this.#handle(msg, resolve, reject);
    }
  }

  #handle(msg, resolve, reject) {
    switch (msg.type) {
      case "register_ack":
        if (msg.accepted) {
          this.registered = true;
          // Adopt the Control-delivered outcome-buffer TTL (config-not-code; P7-4). Falls back to the
          // bootstrap default when the frame omits it (older Control / test).
          if (msg.outcome_buffer_ttl_sec > 0) {
            this.outcomeBufferTtlMs = msg.outcome_buffer_ttl_sec * 1000;
          }
          this.#startHeartbeat();
          // REPLAY any outcomes still unacked from before this (re)connect (§1.3): a stream that just
          // came back must re-deliver what the Control never acked. Prune expired entries first.
          this.#replayBufferedOutcomes();
          if (resolve) resolve(true);
        } else {
          // Clean reject code only (two-vocabulary); close and let start() reject.
          if (reject) reject(new Error(`register rejected: ${msg.code || "unauthenticated"}`));
          this.stop();
        }
        break;
      case "push_job":
        // Do not await here — process the job then report; keep the read loop responsive.
        this.#runJob(msg).catch(() => { /* #runJob never throws credential detail; already handled */ });
        break;
      case "report_ack":
        // The Control durably persisted (or cleanly no-op'd) this job's outcome — drop the buffered copy
        // so we stop replaying it (P7-4). Keyed by job_id so it matches across a reconnect (the buffer
        // survives the reconnect, and a replay re-sends the report VERBATIM, so the ack's echoed
        // request_id still identifies which ATTEMPT was acked). When both agree on the attempt we drop;
        // an ack echoing a DIFFERENT request_id than the buffered entry is an ack for a SUPERSEDED
        // attempt (review gate R2) and must NOT drop the live attempt's outcome — dropping it there
        // would stop the replay of an outcome the Control never committed, losing it silently.
        if (msg.job_id) {
          const held = this.outcomeBuffer.get(msg.job_id);
          if (held && msg.request_id && held.report.request_id !== msg.request_id) {
            console.error(`worker ${this.workerId}: ignoring report_ack for job ${msg.job_id} attempt ${msg.request_id} — the buffered outcome belongs to attempt ${held.report.request_id} and is still unacked`);
          } else if (this.outcomeBuffer.delete(msg.job_id) && this.onAck) {
            try { this.onAck(msg.job_id); } catch { /* test hook only */ }
          }
        }
        break;
      default:
        // Unknown frame: ignore.
    }
  }

  // #bufferAndSend records the outcome in the replay buffer (keyed by job_id) and then attempts to
  // send it. The buffer entry persists until a report_ack drops it or the TTL expires it — so if this
  // send is lost to a stream drop, the outcome is replayed on reconnect (P7-4). It is a no-op to
  // re-buffer a job already present (a replay), so firstSeenMs (the TTL anchor) is not reset.
  //
  // MAX ENTRY COUNT (§1.3 "bounded … and a max entry count"): the second half of the buffer's bound,
  // alongside the TTL. On overflow the OLDEST entry is dropped — the spec's named consequence is that
  // an overflowed outcome "is dropped and the job falls back to the continuous reclaim loop", and the
  // oldest entry is both the closest to its own TTL expiry and the most likely to already be owned by
  // reclaim. We never refuse to buffer the NEW outcome: the freshest outcome is the one whose job the
  // Control is least likely to have reclaimed yet, so keeping it minimizes duplicates. Not a config key:
  // the phase seeds exactly six (§5) and none is a buffer cardinality, so this is a structural safety
  // bound (like the worker's 30-attempt connect budget), not an operator dial. Sized far above any real
  // buffer — a worker holds at most one in-flight job, so >1 entry already means outcomes are stacking up
  // against an unreachable/DB-down Control, and the TTL is the bound that actually fires in practice.
  // SUPERSEDING ATTEMPT (review gate R2). The key is the job_id, so a job this worker is pushed a SECOND
  // time (Control reclaimed the first attempt after the ack was lost, then re-dispatched to this same
  // worker) would find its own stale entry already present and — under a plain has()-guard — never buffer
  // the LIVE attempt's outcome at all: a lost send would then replay attempt 1's result forever while
  // attempt 2's real outcome was gone. push_job.request_id is minted per attempt and echoed in the
  // report, so a differing request_id for the same job is a positive identification of a NEW attempt.
  // We REPLACE the entry (fresh firstSeenMs — this outcome's own TTL starts now) and DISCARD the
  // superseded one: attempt 1's outcome describes a send the Control has already reclaimed and re-sent,
  // so applying it to the live row would attribute the wrong attempt's result. Dropping it costs at
  // most scoring signal for a dead attempt (§1.3 already names "dropped ⇒ falls back to reclaim" as an
  // acceptable outcome); retaining it risks a wrong terminal state, which is not acceptable.
  #bufferAndSend(report) {
    const prior = this.outcomeBuffer.get(report.job_id);
    if (prior && prior.report.request_id !== report.request_id) {
      this.outcomeBuffer.delete(report.job_id);
      this.supersededDropped++;
      console.error(`worker ${this.workerId}: job ${report.job_id} was pushed again (new attempt ${report.request_id}); discarding the superseded unacked outcome from attempt ${prior.report.request_id} and buffering the live one instead`);
    }
    if (!this.outcomeBuffer.has(report.job_id)) {
      if (this.outcomeBuffer.size >= this.outcomeBufferMaxEntries) {
        this.#pruneExpiredOutcomes();
        while (this.outcomeBuffer.size >= this.outcomeBufferMaxEntries) {
          const oldest = this.outcomeBuffer.keys().next(); // Map preserves insertion order
          if (oldest.done) break;
          this.outcomeBuffer.delete(oldest.value);
          this.overflowDropped++;
          console.error(`worker ${this.workerId}: outcome buffer full (${this.outcomeBufferMaxEntries}); dropped the oldest unacked outcome — that job falls back to Control reclaim`);
        }
      }
      this.outcomeBuffer.set(report.job_id, { report, firstSeenMs: this.now() });
    }
    this.#send(report);
  }

  // #replayBufferedOutcomes re-sends every still-unacked buffered outcome (oldest first), dropping any
  // that have aged past the TTL (they fall back to Control reclaim). Bounded work: at most one
  // in-flight-per-worker means the buffer is small in practice, and TTL + max-entry-count bound it hard.
  //
  // Called from BOTH triggers, because the spec names two distinct failure shapes:
  //   - on register_ack — §1.3, the stream came back and must re-deliver what was never acked;
  //   - on every heartbeat tick — §2.3, where the stream stays LIVE but the Control "cannot persist it
  //     (DB down), so it does not ack … the worker retains the outcome and KEEPS REPLAYING until it
  //     receives a post-commit ack". A reconnect-only trigger cannot satisfy that case: no reconnect
  //     ever happens, so without this tick the outcome would sit unreplayed until its TTL and the
  //     Control's reclaim (lease 120s ≪ TTL 900s) would re-send first — a silent duplicate, which is
  //     precisely what §1.3's no-duplicate contract exists to prevent.
  // The cadence is the EXISTING heartbeat interval (heartbeat_interval_sec, already seeded and already
  // delivered to the worker) — deliberately NOT a new key: §5 seeds exactly six and none is a replay
  // cadence, and inventing a seventh would violate config-not-code's "new tunable ⇒ new seeded key".
  // Replay is idempotent by construction (§1.3: Control-side pre-persist idempotency + provider_message_id
  // UNIQUE), so re-sending on a tick is a no-op once persisted, and the ack drops the entry.
  #replayBufferedOutcomes() {
    this.#pruneExpiredOutcomes();
    for (const { report } of this.outcomeBuffer.values()) {
      this.#send(report);
    }
  }

  // #pruneExpiredOutcomes drops buffer entries older than the TTL (P7-4 "bounded" teeth): past the
  // TTL the Control-side reclaim loop owns the job, so retaining/replaying would only risk a late,
  // now-meaningless duplicate. Returns the number dropped (for tests/logs).
  #pruneExpiredOutcomes() {
    const cutoff = this.now() - this.outcomeBufferTtlMs;
    let dropped = 0;
    for (const [jobId, entry] of this.outcomeBuffer) {
      if (entry.firstSeenMs <= cutoff) {
        this.outcomeBuffer.delete(jobId);
        dropped++;
      }
    }
    return dropped;
  }

  // #runJob is the credential lifecycle boundary (§4.4). It wraps the plaintext in a
  // MemoryCredential and zeroes it in `finally` on EVERY path — accept, SMTP error, or thrown
  // exception. The plaintext is never logged and never placed in the report.
  async #runJob(pj) {
    const credential = new MemoryCredential(pj.smtp.secret);
    // Drop our own reference to the plaintext string ASAP; the durable copy is the buffer.
    pj.smtp.secret = null;

    const jobId = pj.job_id;
    let report;
    try {
      // Lease check (§2.3): decline without SMTP if the lease already passed on receipt.
      if (leaseExpired(pj.lease_expires_at)) {
        report = errorReport(pj.request_id, jobId, "lease_expired", pj.attempt_token);
      } else {
        // Phase 8 (§1.1): assemble via the variation pipeline. An absent/all-default pj.variation
        // resolves to the Phase 3 identity (§4) — assembleDataBytes then reproduces buildDataBytes'
        // bytes exactly (T1 asserts this equality directly against buildDataBytes, exported below).
        const dataBytes = assembleDataBytes(pj.send, pj.variation, jobId);
        // Per-operation SMTP timeout (Phase 6 §5, P6-11): prefer the seeded smtp_operation_timeout_sec
        // delivered on this push_job (the Control enforces it < the job lease at config-load), falling
        // back to the worker's bootstrap env default only when the frame omits it (older Control / test).
        const opTimeoutMs = pj.smtp.operation_timeout_sec > 0
          ? pj.smtp.operation_timeout_sec * 1000
          : this.smtpTimeoutMs;
        const result = await runSmtpSession(
          {
            smtp: { host: pj.smtp.host, port: pj.smtp.port, username: pj.smtp.username, security: pj.smtp.security },
            envelopeFrom: pj.send.from_address,
            recipient: pj.send.recipient,
            dataBytes,
          },
          credential,
          { timeoutMs: opTimeoutMs },
        );
        // Provider id: parse from the 250, else fall back to the Control-generated Message-ID (§4.3).
        const provID = parseProviderID(result.smtpResponse) || pj.send.headers["Message-ID"] || "";
        if (!provID) {
          report = errorReport(pj.request_id, jobId, "accepted_without_id", pj.attempt_token); // §4.3 defensive backstop
        } else {
          report = {
            type: "report",
            request_id: pj.request_id,
            job_id: jobId,
            attempt_token: pj.attempt_token,
            outcome: "accepted",
            provider_message_id: provID,
            smtp_response: result.smtpResponse,
            error_reason: null,
          };
        }
      }
    } catch (e) {
      if (e instanceof SMTPError && e.reason === "bounced") {
        report = {
          type: "report", request_id: pj.request_id, job_id: jobId, attempt_token: pj.attempt_token,
          outcome: "bounced", smtp_response: e.smtpResponse || "", error_reason: null,
        };
      } else {
        const reason = e instanceof SMTPError ? e.reason : "timeout";
        const resp = e instanceof SMTPError ? (e.smtpResponse || "") : "";
        report = { type: "report", request_id: pj.request_id, job_id: jobId, attempt_token: pj.attempt_token, outcome: "error", smtp_response: resp, error_reason: reason };
      }
    } finally {
      // LOCKED: zero the plaintext on EVERY exit path (§4.4, S3).
      credential.zero();
      if (this.onZeroed) { try { this.onZeroed(jobId); } catch { /* test hook only */ } }
    }
    // Buffer-then-send (P7-4): retain the outcome (never the credential — already zeroed above) so a
    // lost send is replayed on reconnect, until the Control acks it.
    this.#bufferAndSend(report);
  }

  // replayNow re-sends all still-unacked buffered outcomes (pruning expired ones first). The worker does
  // this automatically on every register_ack; this is a thin public seam so tests can trigger a
  // replay/prune deterministically without a full reconnect. Safe to call anytime.
  // pendingOutcomeCount prunes expired entries and returns how many unacked outcomes remain. The
  // supervise loop (index.js) uses it to decide whether exiting would DESTROY buffered outcomes: per
  // §1.3 an outcome is dropped on "failure to reconnect within the TTL", so while entries are still
  // inside their TTL the worker must keep trying to reconnect rather than exit and lose them.
  pendingOutcomeCount() {
    this.#pruneExpiredOutcomes();
    return this.outcomeBuffer.size;
  }

  replayNow() {
    this.#replayBufferedOutcomes();
  }

  #send(obj) {
    if (!this.sock) return;
    try { this.sock.write(JSON.stringify(obj) + "\n", "utf8"); } catch { /* stream gone */ }
  }

  #startHeartbeat() {
    this.#stopHeartbeat();
    this.hbTimer = setInterval(() => {
      this.#send({ type: "heartbeat", request_id: reqID(), box_id: this.workerId });
      // §2.3: keep replaying unacked outcomes on a LIVE stream too (the DB-down case, where no
      // reconnect ever occurs to trigger a replay), and prune expired entries on a real clock rather
      // than only lazily during a reconnect. Piggybacks the heartbeat's already-seeded cadence.
      this.#replayBufferedOutcomes();
    }, this.heartbeatSec * 1000);
    if (this.hbTimer.unref) this.hbTimer.unref();
  }

  #stopHeartbeat() {
    if (this.hbTimer) { clearInterval(this.hbTimer); this.hbTimer = null; }
  }
}

// --- helpers ---------------------------------------------------------------------------------

function reqID() {
  return "req_" + Math.floor(Date.now() / 1000).toString(16) + Math.random().toString(16).slice(2, 10);
}

function leaseExpired(iso) {
  if (!iso) return false;
  const t = Date.parse(iso);
  return Number.isFinite(t) && t <= Date.now();
}

// errorReport builds a failure report. attemptToken is ECHOED VERBATIM from the push_job (Phase 8
// Chunk 1, P7-OD-6): the worker never mints, derives, or defaults it — an absent token stays absent on
// the wire (omitted by the JSON encoder as undefined), which the Control reads as "no token" and treats
// as never-a-mismatch, so a pre-0020 Control is unaffected.
function errorReport(requestId, jobId, reason, attemptToken) {
  return { type: "report", request_id: requestId, job_id: jobId, attempt_token: attemptToken, outcome: "error", smtp_response: "", error_reason: reason };
}

// buildDataBytes assembles the RFC 5322 message from the Control-supplied headers (§8) + body. The
// worker sends EXACTLY the headers it was given (it does not invent or reorder in Phase 3, §8.1),
// plus the transport-mandatory MIME framing.
//
// Exported (Phase 8, §4/§8 T1) as the frozen PHASE 3 REFERENCE: the headline byte-identity test
// pins Message-ID/Date/boundary and asserts assembleDataBytes (assemble.js, the variation-pipeline
// path #runJob actually calls) reproduces this function's output exactly when variation is
// off/absent. This function itself is otherwise untouched by Phase 8 — it is the fixed target, not
// participant, of the identity guarantee.
export function buildDataBytes(send) {
  const h = send.headers || {};
  const order = ["From", "To", "Subject", "Reply-To", "Message-ID", "Date"];
  const lines = [];
  for (const k of order) {
    if (h[k] != null && h[k] !== "") lines.push(`${k}: ${h[k]}`);
  }
  // Any additional Control-supplied headers (none in Phase 3 baseline, but forward-compatible).
  for (const k of Object.keys(h)) {
    if (!order.includes(k) && h[k] != null && h[k] !== "") lines.push(`${k}: ${h[k]}`);
  }
  lines.push("MIME-Version: 1.0");
  lines.push('Content-Type: text/html; charset="utf-8"');
  lines.push("Content-Transfer-Encoding: 8bit");
  lines.push(""); // header/body separator
  lines.push(String(send.body || ""));
  return lines.join("\r\n");
}

// parseProviderID extracts a provider message id from a 250 response where present (§4.3). It is
// intentionally conservative: common forms are "250 OK queued as ABCD1234" and Google's
// "250 2.0.0 OK <ts> <id> - gsmtp". Returns "" if nothing parseable (caller falls back to
// Message-ID).
export function parseProviderID(resp) {
  if (!resp) return "";
  let m = resp.match(/queued as ([A-Za-z0-9._-]+)/i);
  if (m) return m[1];
  m = resp.match(/\b2\.0\.0 OK\s+\S+\s+([A-Za-z0-9._-]+)/i);
  if (m) return m[1];
  m = resp.match(/\bid=([A-Za-z0-9._@-]+)/i);
  if (m) return m[1];
  return "";
}
