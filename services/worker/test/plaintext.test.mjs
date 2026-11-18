// Phase 3 close-out B3 — THE OPTIONAL PLAIN-TEXT PART. Behavioural tests of the worker's MIME assembly with and
// without `push_job.send.text`, from the renderer up to the bytes a real SMTP server receives.
//
// What each group pins, and the paired control that lets it fail:
//   1  GOLDEN: with `text` ABSENT (omitted, null, "" or whitespace-only) every one of the golden cases — the neutral
//      message and every variation profile (encoding / boundary rotation, content stages, header stages, the kitchen
//      sink) — renders BYTE-FOR-BYTE what the renderer rendered BEFORE the text part existed
//      (test/testdata/golden/*.eml, captured from assemble.js at send-managed bffd424).
//      <- the same cases with a text present must DIFFER from the golden (so the comparison can fail), and the
//         golden set is non-trivial (a floor of cases, each distinct)
//   2  STRUCTURE: text present -> multipart/alternative, text/plain FIRST then text/html LAST, charset and
//      Content-Transfer-Encoding stated on each part, the HTML part exactly the body that was sent, no top-level CTE
//      <- the same input without text is single-part text/html
//   3  DETERMINISM (the duplicate guarantee): a retry of the same item renders byte-identical bytes, with no random
//      and no clock read; a different item has a different boundary; a delimiter collision picks the next
//      deterministic candidate
//   4  VARIATION x TEXT: encoding rotation puts the SAME rotated encoding on both parts and never on the multipart;
//      boundary rotation keeps the type multipart/alternative and the part order; content stages leave the text alone
//   5  THE WIRE: through the REAL worker (WorkerStream -> runSmtpSession) to a REAL SMTP server, the message the server
//      accepts is exactly the assembled bytes (dot-stuffing undone), for text absent and for text present
//
// There is no DKIM / signing code in this module: the account's SMTP provider signs (if it signs at all) the bytes the
// worker hands to DATA, and group 5 proves those bytes are the assembled message, final, with nothing appended after.
//
// Run: node --test services/worker/test/plaintext.test.mjs   (Node 24)

import { test } from "node:test";
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { alternativeBoundary, assembleDataBytes, textPartOf } from "../src/assemble.js";
import { decodeForTransfer } from "../src/mime-codec.js";
import { FIXED_JOB_ID, GOLDEN_CASES, GOLDEN_HTML, goldenSend } from "./golden-cases.mjs";
import { makeStream, startFakeControl, waitFor } from "./control-helpers.mjs";
import { startSink } from "./sink.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const GOLDEN_DIR = path.join(here, "testdata", "golden");
const TEXT = "Hello,\nthis is the plain-text alternative.\n\n.a line that starts with a dot\nCafé ☃";
const TEXT_CRLF = TEXT.replace(/\n/g, "\r\n");

const utf8 = (s) => Buffer.from(s, "utf8");
const golden = (name) => fs.readFileSync(path.join(GOLDEN_DIR, name + ".eml"));
const withText = (send, text) => ({ ...send, text });

// parseEntity splits one MIME entity at its first blank line into ordered headers and the raw body.
function parseEntity(raw) {
  const i = raw.indexOf("\r\n\r\n");
  assert.ok(i > 0, "no header/body separator in the entity");
  const headers = raw.slice(0, i).split("\r\n").map((l) => {
    const k = l.indexOf(": ");
    assert.ok(k > 0, `malformed header line ${JSON.stringify(l)}`);
    return [l.slice(0, k), l.slice(k + 2)];
  });
  const get = (name) => headers.filter(([k]) => k.toLowerCase() === name.toLowerCase()).map(([, v]) => v);
  return { headers, body: raw.slice(i + 4), get };
}

// parseAlternative parses a multipart message into its top-level entity and its parts, asserting the delimiter grammar.
function parseMultipart(raw) {
  const top = parseEntity(raw);
  const ct = top.get("Content-Type");
  assert.equal(ct.length, 1, "exactly one Content-Type at the top level");
  const m = /^multipart\/(\w+); boundary="([^"]+)"$/.exec(ct[0]);
  assert.ok(m, `the top-level Content-Type is not a quoted-boundary multipart: ${ct[0]}`);
  const [, subtype, boundary] = m;
  const open = `--${boundary}\r\n`;
  const close = `\r\n--${boundary}--`;
  assert.ok(top.body.startsWith(open), "the body does not open with the first delimiter");
  assert.ok(top.body.endsWith(close), "the body does not end with the closing delimiter");
  const inner = top.body.slice(open.length, top.body.length - close.length);
  const parts = inner.split(`\r\n--${boundary}\r\n`).map(parseEntity);
  return { top, subtype, boundary, parts };
}

// ===== 1. GOLDEN: text absent is byte-identical to the pre-text renderer ========================================

test("B3 golden: with text ABSENT every golden case renders byte-for-byte what the renderer rendered before the text part existed", () => {
  const absentVariants = {
    "text omitted": (s) => s,
    "text null": (s) => withText(s, null),
    "text empty string": (s) => withText(s, ""),
    "text whitespace only": (s) => withText(s, " \r\n\t  "),
  };
  assert.ok(GOLDEN_CASES.length >= 19, `the golden set shrank to ${GOLDEN_CASES.length} cases (floor 19)`);
  let compared = 0;
  for (const c of GOLDEN_CASES) {
    const want = golden(c.name);
    assert.ok(want.length > 400, `golden ${c.name} is suspiciously small (${want.length} bytes)`);
    for (const [label, mk] of Object.entries(absentVariants)) {
      const got = utf8(assembleDataBytes(mk(goldenSend(c.jobId)), c.variation, c.jobId));
      if (!got.equals(want)) {
        let i = 0;
        while (i < got.length && i < want.length && got[i] === want[i]) i++;
        assert.fail(`${c.name} (${label}): rendered ${got.length} bytes, golden ${want.length}; first difference at byte ${i}: got ${JSON.stringify(got.subarray(i, i + 60).toString())} want ${JSON.stringify(want.subarray(i, i + 60).toString())}`);
      }
      compared++;
    }
  }
  assert.equal(compared, GOLDEN_CASES.length * Object.keys(absentVariants).length);
});

test("B3 golden — paired control: with a text present EVERY golden case renders differently (so the comparison above can fail), and the goldens are distinct and non-trivial", () => {
  const seen = new Set();
  for (const c of GOLDEN_CASES) {
    const want = golden(c.name);
    const got = utf8(assembleDataBytes(withText(goldenSend(c.jobId), TEXT), c.variation, c.jobId));
    assert.ok(!got.equals(want), `${c.name}: a message WITH a text part is byte-identical to the text-absent golden — the golden cannot tell them apart`);
    assert.ok(got.toString().includes("multipart/alternative"), `${c.name}: a text part did not make the message multipart/alternative`);
    seen.add(crypto.createHash("sha256").update(want).digest("hex"));
  }
  // identical goldens are allowed only where the variation block is neutral (three cases) — everything else must differ
  assert.ok(seen.size >= GOLDEN_CASES.length - 2, `only ${seen.size} distinct goldens for ${GOLDEN_CASES.length} cases`);
  // the three neutral cases ARE the Phase 3 single-part message
  for (const name of ["neutral-block-absent", "neutral-block-empty", "neutral-block-explicit-defaults"]) {
    const text = golden(name).toString();
    assert.match(text, /\r\nContent-Type: text\/html; charset="utf-8"\r\nContent-Transfer-Encoding: 8bit\r\n\r\n/);
    assert.ok(!text.includes("multipart"), `${name} is multipart`);
  }
});

test("B3 textPartOf: only a string with a non-whitespace character is a text part", () => {
  for (const absent of [undefined, null, "", " ", "\r\n\t", "  ", 0, 5, {}, [], true]) {
    assert.equal(textPartOf({ text: absent }), null, `${JSON.stringify(absent)} must mean no text part`);
  }
  assert.equal(textPartOf({}), null);
  assert.equal(textPartOf(undefined), null);
  for (const present of ["x", "  x  ", "\nhello\n", "café"]) assert.equal(textPartOf({ text: present }), present, "a text part is returned verbatim");
});

// ===== 2. STRUCTURE =================================================================================================

test("B3 structure: text present -> multipart/alternative, text/plain FIRST, text/html LAST, charset and CTE on each part, the HTML exactly as sent", () => {
  const send = withText(goldenSend(), TEXT);
  const raw = assembleDataBytes(send, undefined, FIXED_JOB_ID);
  const { top, subtype, boundary, parts } = parseMultipart(raw);

  assert.equal(subtype, "alternative");
  assert.equal(parts.length, 2, "exactly two parts");
  assert.deepEqual(parts.map((p) => p.get("Content-Type")[0]), ['text/plain; charset="utf-8"', 'text/html; charset="utf-8"'], "text/plain must come FIRST and text/html LAST (RFC 2046 5.1.4), each with a stated charset");
  for (const p of parts) assert.deepEqual(p.get("Content-Transfer-Encoding"), ["8bit"], "each part states its Content-Transfer-Encoding (8bit, as the single HTML part always did)");
  assert.equal(parts[0].body, TEXT_CRLF, "the text part is the text, with CRLF line endings");
  assert.equal(parts[1].body, GOLDEN_HTML, "the HTML part is EXACTLY the body that was sent");

  // the outer headers: the same set, in the same order, as the single-part message, minus the single-part CTE
  const single = parseEntity(assembleDataBytes(goldenSend(), undefined, FIXED_JOB_ID));
  const names = (e) => e.headers.map(([k]) => k);
  assert.deepEqual(names(top), names(single).filter((k) => k !== "Content-Transfer-Encoding"), "the outer headers are the single-part ones without a top-level Content-Transfer-Encoding (a multipart cannot carry a quoted-printable/base64 label)");
  for (const k of ["From", "To", "Subject", "Reply-To", "Message-ID", "Date", "MIME-Version"]) assert.deepEqual(top.get(k), single.get(k), `${k} changed`);
  assert.deepEqual(top.get("MIME-Version"), ["1.0"]);
  assert.equal(top.get("Content-Transfer-Encoding").length, 0, "a multipart entity must not carry a Content-Transfer-Encoding");
  assert.match(boundary, /^----=_Alt_[0-9a-f]{24}$/);
  assert.ok(!GOLDEN_HTML.includes(`--${boundary}`) && !TEXT.includes(`--${boundary}`), "the delimiter occurs inside a part");
});

test("B3 structure — paired control: the SAME input without a text part is the single-part text/html message (no multipart anywhere)", () => {
  const raw = assembleDataBytes(goldenSend(), undefined, FIXED_JOB_ID);
  const e = parseEntity(raw);
  assert.deepEqual(e.get("Content-Type"), ['text/html; charset="utf-8"']);
  assert.deepEqual(e.get("Content-Transfer-Encoding"), ["8bit"]);
  assert.ok(!raw.includes("multipart") && !raw.includes("text/plain") && !raw.includes("----=_Alt_"));
  assert.equal(e.body, GOLDEN_HTML);
});

// ===== 3. DETERMINISM — the duplicate guarantee ======================================================================

test("B3 determinism: a retry of the same item renders byte-identical bytes, and nothing random or clock-based is read", () => {
  const send = withText(goldenSend(), TEXT);
  const profiles = [undefined, { encoding_rotation: true }, { mime_boundary_rotation: true }, GOLDEN_CASES.at(-1).variation];
  for (const v of profiles) {
    const first = assembleDataBytes(send, v, FIXED_JOB_ID);
    const retry = assembleDataBytes(structuredClone(send), v, FIXED_JOB_ID);
    assert.equal(retry, first, `a retry rendered different bytes (variation ${JSON.stringify(v)})`);
  }
  // No Math.random, no Date, no crypto.randomBytes during rendering: make each one throw and render again.
  const realRandom = Math.random;
  const realNow = Date.now;
  const RealDate = globalThis.Date;
  const realRandomBytes = crypto.randomBytes;
  try {
    Math.random = () => { throw new Error("rendering read Math.random"); };
    RealDate.now = () => { throw new Error("rendering read Date.now"); };
    globalThis.Date = class extends RealDate { constructor(...a) { if (a.length === 0) throw new Error("rendering read the clock (new Date())"); super(...a); } };
    crypto.randomBytes = () => { throw new Error("rendering read crypto.randomBytes"); };
    const a = assembleDataBytes(send, GOLDEN_CASES.at(-1).variation, FIXED_JOB_ID);
    const b = assembleDataBytes(send, GOLDEN_CASES.at(-1).variation, FIXED_JOB_ID);
    assert.equal(a, b);
  } finally {
    Math.random = realRandom; RealDate.now = realNow; globalThis.Date = RealDate; crypto.randomBytes = realRandomBytes;
  }
});

test("B3 determinism — paired control: a DIFFERENT item gets a different boundary (and different bytes); the same item keeps its boundary whatever the content", () => {
  const boundaries = new Set();
  for (let i = 0; i < 50; i++) {
    const jobId = `6f0c1e52-3b7a-4d19-9a4e-${String(i).padStart(12, "0")}`;
    const raw = assembleDataBytes(withText(goldenSend(jobId), TEXT), undefined, jobId);
    boundaries.add(parseMultipart(raw).boundary);
  }
  assert.equal(boundaries.size, 50, "50 different items must have 50 different boundaries");
  // the boundary is a function of the item alone: another body or another text does not move it
  const b1 = parseMultipart(assembleDataBytes(withText(goldenSend(), TEXT), undefined, FIXED_JOB_ID)).boundary;
  const b2 = parseMultipart(assembleDataBytes(withText({ ...goldenSend(), body: "<p>another body</p>" }, "another text"), undefined, FIXED_JOB_ID)).boundary;
  assert.equal(b1, b2, "the boundary changed with the content");
  assert.equal(b1, alternativeBoundary(FIXED_JOB_ID, GOLDEN_HTML, TEXT_CRLF));
});

test("B3 determinism: if the first delimiter occurs inside a part the next deterministic candidate is used — still stable, still collision-free", () => {
  const first = alternativeBoundary(FIXED_JOB_ID);
  const nasty = `intro\r\n--${first}\r\nmore`; // a text that contains the first candidate's delimiter line
  const send = withText(goldenSend(), nasty);
  const raw = assembleDataBytes(send, undefined, FIXED_JOB_ID);
  const { boundary, parts } = parseMultipart(raw);
  assert.notEqual(boundary, first, "the colliding first candidate was used");
  assert.equal(parts.length, 2, "the collision split the message");
  assert.equal(parts[0].body, nasty);
  assert.equal(assembleDataBytes(send, undefined, FIXED_JOB_ID), raw, "the fallback boundary is not stable");
  // control: without the collision the first candidate is the boundary
  assert.equal(parseMultipart(assembleDataBytes(withText(goldenSend(), TEXT), undefined, FIXED_JOB_ID)).boundary, first);
});

// ===== 4. VARIATION x TEXT ==========================================================================================

test("B3 variation: encoding rotation puts the SAME encoding on both parts, never on the multipart, and both decode back", () => {
  const seen = new Set();
  for (let i = 1; i <= 12; i++) {
    const jobId = `golden-job-enc-${i}`;
    const send = withText(goldenSend(jobId), TEXT);
    const { top, parts } = parseMultipart(assembleDataBytes(send, { encoding_rotation: true }, jobId));
    assert.equal(top.get("Content-Transfer-Encoding").length, 0, "the multipart carries a Content-Transfer-Encoding");
    const [textEnc] = parts[0].get("Content-Transfer-Encoding");
    const [htmlEnc] = parts[1].get("Content-Transfer-Encoding");
    assert.equal(textEnc, htmlEnc, "the two parts use different encodings");
    assert.ok(["quoted-printable", "base64"].includes(textEnc), `unexpected rotated encoding ${textEnc}`);
    seen.add(textEnc);
    assert.equal(decodeForTransfer(parts[0].body, textEnc), TEXT_CRLF, "the text part does not decode back to the text");
    assert.equal(decodeForTransfer(parts[1].body, htmlEnc), GOLDEN_HTML, "the HTML part does not decode back to the HTML");
  }
  assert.equal(seen.size, 2, "both rotated encodings must be exercised");
});

test("B3 variation: boundary rotation keeps multipart/alternative, the part order and the content, in every boundary style", () => {
  const styles = new Set();
  for (let i = 1; i <= 12; i++) {
    const jobId = `golden-job-bnd-${i}`;
    const send = withText(goldenSend(jobId), TEXT);
    const { subtype, boundary, parts } = parseMultipart(assembleDataBytes(send, { mime_boundary_rotation: true }, jobId));
    assert.equal(subtype, "alternative", "boundary rotation must not turn the alternative into a mixed wrapper");
    assert.deepEqual(parts.map((p) => p.get("Content-Type")[0].split(";")[0]), ["text/plain", "text/html"]);
    assert.equal(parts[0].body, TEXT_CRLF);
    assert.equal(parts[1].body, GOLDEN_HTML);
    styles.add(boundary.startsWith("----=_Part_") ? "dashes" : boundary.startsWith("_NextPart_") ? "outlook" : boundary.startsWith("boundary_") ? "plain" : "other:" + boundary);
  }
  assert.deepEqual([...styles].sort(), ["dashes", "outlook", "plain"], "all three rotation styles must be exercised, and none may be the unrotated one");
});

test("B3 variation: the content stages change the HTML part and leave the text part verbatim; header stages keep the structure", () => {
  const jobId = "golden-job-content";
  const content = GOLDEN_CASES.find((c) => c.name === "content-stages").variation;
  const { parts } = parseMultipart(assembleDataBytes(withText(goldenSend(jobId), TEXT), content, jobId));
  assert.equal(parts[0].body, TEXT_CRLF, "a content stage altered the plain-text part");
  assert.notEqual(parts[1].body, GOLDEN_HTML, "the content stages did not run on the HTML part (the control that the text part is spared on purpose)");
  const headers = GOLDEN_CASES.find((c) => c.name === "header-stages").variation;
  const out = parseMultipart(assembleDataBytes(withText(goldenSend("golden-job-headers"), TEXT), headers, "golden-job-headers"));
  assert.equal(out.parts.length, 2);
  assert.equal(out.parts[0].body, TEXT_CRLF);
});

// ===== 5. THE WIRE =================================================================================================

// sendThroughWorker runs ONE job through the real worker (push_job -> assemble -> runSmtpSession) against a real SMTP
// server and returns what that server accepted, plus the worker's report.
async function sendThroughWorker({ text, variation, jobId = FIXED_JOB_ID }) {
  const sink = await startSink();
  const reports = [];
  const send = { ...goldenSend(jobId) };
  const frame = {
    type: "push_job", request_id: "req-wire", job_id: jobId, attempt_token: 1,
    send: text === undefined ? { recipient: send.recipient, from_address: send.from_address, subject: send.subject, body: send.body, headers: send.headers }
      : { recipient: send.recipient, from_address: send.from_address, subject: send.subject, body: send.body, headers: send.headers, text },
    smtp: { host: "127.0.0.1", port: sink.port, username: "u", secret: "S-" + crypto.randomBytes(8).toString("hex"), security: "starttls", operation_timeout_sec: 3 },
    lease_expires_at: new Date(Date.now() + 60_000).toISOString(),
    ...(variation ? { variation } : {}),
  };
  const control = await startFakeControl({
    onFrame: (msg, conn) => {
      if (msg.type === "register") { conn.reply({ type: "register_ack", request_id: msg.request_id, accepted: true }); setImmediate(() => conn.reply(frame)); }
      if (msg.type === "report") reports.push(msg);
    },
  });
  const stream = makeStream(control.port);
  try {
    await stream.start();
    assert.ok(await waitFor(() => reports.length === 1, 15000), "the worker never reported on the job");
    return { report: reports[0], accepted: sink.messages.slice(), send: frame.send };
  } finally {
    stream.stop();
    await control.close();
    await sink.close();
  }
}

const crlf = (s) => s.replace(/\r\n/g, "\n").replace(/\n/g, "\r\n"); // what smtp.js's DATA normalisation does to bare LFs

test("B3 wire: with NO text the SMTP server receives exactly the golden single-part message", async () => {
  const { report, accepted } = await sendThroughWorker({ text: undefined });
  assert.equal(report.outcome, "accepted", `the worker's report was ${JSON.stringify(report)}`);
  assert.equal(accepted.length, 1, "the server must accept exactly one message");
  assert.equal(accepted[0], crlf(golden("neutral-block-absent").toString("utf8")), "the bytes the server received differ from the text-absent golden");
  assert.ok(!accepted[0].includes("multipart"), "a message without text was sent as multipart");
});

test("B3 wire — paired control: WITH a text the SMTP server receives multipart/alternative, text/plain first, and exactly the assembled bytes (dot-stuffing undone)", async () => {
  const { report, accepted, send } = await sendThroughWorker({ text: TEXT });
  assert.equal(report.outcome, "accepted", `the worker's report was ${JSON.stringify(report)}`);
  assert.equal(accepted.length, 1);
  const assembled = crlf(assembleDataBytes(send, undefined, FIXED_JOB_ID));
  assert.equal(accepted[0], assembled, "the bytes on the wire are not the assembled message — something rewrote, appended to or truncated them after assembly (a signer would sign the wrong bytes)");
  const { subtype, parts } = parseMultipart(accepted[0]);
  assert.equal(subtype, "alternative");
  assert.deepEqual(parts.map((p) => p.get("Content-Type")[0].split(";")[0]), ["text/plain", "text/html"]);
  assert.equal(parts[0].body, TEXT_CRLF, "the text part lost its leading-dot line to dot-stuffing");
  assert.ok(accepted[0].endsWith(`--${parseMultipart(accepted[0]).boundary}--`), "something follows the closing delimiter");
});

test("B3 wire: a retry (the same job pushed again) puts byte-identical bytes on the wire, text absent and present", async () => {
  for (const text of [undefined, TEXT]) {
    const one = await sendThroughWorker({ text });
    const two = await sendThroughWorker({ text });
    assert.equal(one.accepted[0], two.accepted[0], `a retry sent different bytes (text ${text === undefined ? "absent" : "present"})`);
  }
});

test("B3 wire: with encoding rotation the text and HTML parts arrive under the SAME rotated encoding and decode to the originals", async () => {
  const { accepted } = await sendThroughWorker({ text: TEXT, variation: { encoding_rotation: true }, jobId: "golden-job-enc-1" });
  const { parts } = parseMultipart(accepted[0]);
  const enc = parts[0].get("Content-Transfer-Encoding")[0];
  assert.equal(enc, parts[1].get("Content-Transfer-Encoding")[0]);
  assert.equal(decodeForTransfer(parts[0].body, enc), TEXT_CRLF);
});
