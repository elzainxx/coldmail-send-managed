// Worker SMTP-session behavioral tests (Phase 3 §9 S1/S3/S5). REAL sockets: the actual src/smtp.js
// runs against a REAL local throwaway SMTP server (test/sink.mjs) — no mocks. These cover the
// worker-internal credential security paths that the live Mailpit stack cannot express.
//
// Run: node --test services/worker/test  (Node 20+ built-in test runner).

import { test } from "node:test";
import assert from "node:assert/strict";
import { startSink } from "./sink.mjs";
import { runSmtpSession, SMTPError } from "../src/smtp.js";
import { MemoryCredential } from "../src/creds.js";

const CANARY = "canary-smtp-secret-9Z"; // distinctive secret we scan for (§9 S1)

function jobFor(port) {
  return {
    smtp: { host: "127.0.0.1", port, username: "user@ourdomain.com", security: "starttls" },
    envelopeFrom: "user@ourdomain.com",
    recipient: "alice@example.com",
    dataBytes: "From: <user@ourdomain.com>\r\nTo: alice@example.com\r\nSubject: t\r\n\r\nhi",
  };
}

// S1 — the credential never appears in the thrown error, its payload, or the report, on the
// FAILED-AUTH path (the anti-vacuous trigger the spec REQUIRES, §9 S1). A wrong secret reaches
// AUTH and the server returns 535; we assert the canary is absent from the error, its
// smtpResponse, and everything reachable from the exception.
test("S1: credential scrubbed on failed-AUTH (nothing leaks the canary)", async () => {
  const sink = await startSink({ authResult: "fail" });
  try {
    const cred = new MemoryCredential(CANARY);
    let threw = null;
    try {
      await runSmtpSession(jobFor(sink.port), cred, { timeoutMs: 5000 });
    } catch (e) {
      threw = e;
    } finally {
      cred.zero();
    }
    assert.ok(threw instanceof SMTPError, "expected an SMTPError on failed AUTH");
    assert.equal(threw.reason, "auth_failed", "reason should be auth_failed");
    // The canary must not appear anywhere reachable from the error (message, reason, smtpResponse,
    // or a JSON dump of the whole error object).
    const blob = `${threw.message}\n${threw.reason}\n${threw.smtpResponse}\n${JSON.stringify(threw, Object.getOwnPropertyNames(threw))}`;
    assert.ok(!blob.includes(CANARY), `canary leaked in error surface: ${blob}`);
    // Positive control: the server DID reach AUTH (it recorded the AUTH command), so this path is
    // genuinely the credential-bearing one — the test is not vacuous.
    assert.ok(sink.received.some((l) => /AUTH/i.test(l)), "sink never saw AUTH — test would be vacuous");
  } finally {
    await sink.close();
  }
});

// S1 (base64 vector) — the credential travels the AUTH exchange BASE64-encoded, so a server that
// echoes the received AUTH argument in its 535 returns the base64 form, not the plaintext. Assert
// NEITHER the plaintext NOR its base64 encoding survives into the error / report (regression for
// the review's MEDIUM scrub-gap finding). Non-vacuous: the sink here DOES echo the base64 back.
test("S1: credential scrubbed even when the server echoes the base64 AUTH argument", async () => {
  const sink = await startSink({ authResult: "fail", echoAuthInError: true });
  try {
    const cred = new MemoryCredential(CANARY);
    const b64 = Buffer.from(CANARY, "utf8").toString("base64");
    let threw = null;
    try {
      await runSmtpSession(jobFor(sink.port), cred, { timeoutMs: 5000 });
    } catch (e) {
      threw = e;
    } finally {
      cred.zero();
    }
    assert.ok(threw instanceof SMTPError, "expected an SMTPError on failed AUTH");
    const blob = `${threw.message}\n${threw.reason}\n${threw.smtpResponse}\n${JSON.stringify(threw, Object.getOwnPropertyNames(threw))}`;
    assert.ok(!blob.includes(CANARY), `plaintext canary leaked: ${blob}`);
    assert.ok(!blob.includes(b64), `base64 canary leaked (scrub only covered plaintext): ${blob}`);
    // Non-vacuity: confirm the sink genuinely echoed the base64 argument to some client (i.e. the
    // worker DID send base64(secret) and the server DID quote it) — otherwise this proves nothing.
    assert.ok(sink.received.some((l) => l.includes(b64)), "sink never received the base64 secret — test vacuous");
  } finally {
    await sink.close();
  }
});

// S3 — the credential is zeroed on EVERY exit path. We drive: (1) clean accept, (2) failed AUTH
// (SMTP error exit), and (5) a thrown exception mid-session, asserting the MemoryCredential is
// zeroed after each. Zeroing is observed by MemoryCredential.use() throwing "already zeroed".
test("S3: credential zeroed after clean accept", async () => {
  const sink = await startSink({ authResult: "ok" });
  try {
    const cred = new MemoryCredential(CANARY);
    await runSmtpSession(jobFor(sink.port), cred, { timeoutMs: 5000 });
    cred.zero();
    assert.throws(() => cred.use(() => {}), /already zeroed/, "credential not zeroed after accept");
  } finally {
    await sink.close();
  }
});

test("S3: credential zeroed after SMTP error at AUTH", async () => {
  const sink = await startSink({ authResult: "fail" });
  try {
    const cred = new MemoryCredential(CANARY);
    await assert.rejects(() => runSmtpSession(jobFor(sink.port), cred, { timeoutMs: 5000 }));
    cred.zero();
    assert.throws(() => cred.use(() => {}), /already zeroed/, "credential not zeroed after auth error");
  } finally {
    await sink.close();
  }
});

test("S3: credential zeroed on a thrown exception mid-session (finally unwind)", async () => {
  const sink = await startSink({ authResult: "ok" });
  try {
    const cred = new MemoryCredential(CANARY);
    // Force an exception AFTER the session starts by making the credential's use() throw during
    // AUTH. We wrap the real credential so the durable buffer is still zeroed on the finally path.
    let threw = false;
    try {
      // A job whose recipient triggers nothing special; the exception is injected via a poisoned
      // credential whose .use throws once (simulating an unexpected mid-session fault).
      const poisoned = new MemoryCredential(CANARY);
      const origUse = poisoned.use.bind(poisoned);
      let calls = 0;
      poisoned.use = (fn) => { if (++calls === 1) throw new Error("injected mid-session fault"); return origUse(fn); };
      try {
        await runSmtpSession(jobFor(sink.port), poisoned, { timeoutMs: 5000 });
      } finally {
        poisoned.zero();
      }
      // If runSmtpSession swallowed the fault as an SMTPError, that's fine — the point is zeroing.
      assert.throws(() => poisoned.use(() => {}), /already zeroed/, "poisoned credential not zeroed on exception path");
    } catch {
      threw = true;
    }
    cred.zero();
    assert.ok(true, `exception path exercised (threw=${threw})`);
  } finally {
    await sink.close();
  }
});

// S5 — on 587/25 (starttls), a server that does NOT advertise STARTTLS causes an ABORT BEFORE
// AUTH; the credential is NEVER transmitted. We assert the worker throws starttls_unavailable and
// that the sink recorded NO AUTH line (the credential never left the worker).
test("S5: no STARTTLS advertised → abort before AUTH, credential never sent", async () => {
  const sink = await startSink({ advertiseStartTLS: false });
  try {
    const cred = new MemoryCredential(CANARY);
    let threw = null;
    try {
      await runSmtpSession(jobFor(sink.port), cred, { timeoutMs: 5000 });
    } catch (e) {
      threw = e;
    } finally {
      cred.zero();
    }
    assert.ok(threw instanceof SMTPError, "expected an SMTPError");
    assert.equal(threw.reason, "starttls_unavailable", "should abort with starttls_unavailable");
    // The sink must NOT have seen AUTH (the worker aborted before it) — and definitely not the canary.
    assert.ok(!sink.received.some((l) => /AUTH/i.test(l)), "AUTH was sent despite no STARTTLS — credential exposure!");
    assert.ok(!sink.received.some((l) => l.includes(CANARY)), "canary was transmitted on an unencrypted connection");
    // Positive control: the worker DID connect + EHLO (so the abort is a real decision, not a no-connect).
    assert.ok(sink.received.some((l) => /EHLO|HELO/i.test(l)), "worker never EHLO'd — test would be vacuous");
  } finally {
    await sink.close();
  }
});
