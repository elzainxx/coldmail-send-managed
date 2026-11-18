// assemble.js — the Phase 8 worker-side MIME assembly entry point (§1.1). Bridges the Phase 3
// header/body shape (push_job.send) to the variation pipeline's intermediate representation
// (variation.js's STAGES operate on `{ headers, body, transferEncoding, boundary }`, never on raw
// bytes) and serializes the result back to RFC 5322 DATA bytes.
//
// This module is what makes the §4 identity property checkable on both sides of the same seam:
// `toMessage` + `serializeMessage` alone (no variation applied) MUST reproduce stream.js's
// `buildDataBytes` byte-for-byte, and `assembleDataBytes` with an absent/all-default variation
// block MUST reproduce it too — that equality (asserted by the T1 test) is what proves the
// pipeline is genuinely the identity function when off, not merely "produces similar output."

import { resolveVariation, applyVariation } from "./variation.js";
import { encodeForTransfer } from "./mime-codec.js";
import { hashFor } from "./variation-seed.js";

// HEADER_ORDER mirrors stream.js's buildDataBytes exactly: the §8 minimum guaranteed set in the
// Control's fixed order, then any additional Control-supplied headers in their map iteration order.
const HEADER_ORDER = ["From", "To", "Subject", "Reply-To", "Message-ID", "Date"];

// textPartOf returns the optional plain-text alternative carried by push_job.send.text, or null when
// the message has none. ONLY a string with at least one non-whitespace character counts: absent, null, a
// non-string, "" and a whitespace-only string all mean "no text part", and the message is then the
// single-part text/html message it has always been, byte for byte. (A blank text/plain alternative is
// not an alternative, and "a new field cannot be ambiguous": NULL, '' and '   ' are one meaning. The
// Control applies the same rule before it ever sends the field, dispatch.sendText; the worker is total.)
export function textPartOf(send) {
  const t = send ? send.text : undefined;
  return typeof t === "string" && /\S/.test(t) ? t : null;
}

// alternativeBoundary is the multipart/alternative boundary of one message, DETERMINISTIC in the job id
// (the same inputs a retry of the same item has) and never random or clock-based: the SAME job renders
// the SAME boundary on every attempt, so a retry/duplicate is byte-identical to the first send, and a
// different job gets a different boundary. It is a hash of the job id (md5 is fine here: the value
// selects a delimiter, it protects nothing). If the (unlikely) delimiter line appears inside either
// part the next deterministic candidate is taken, so the result is still a pure function of its inputs.
export function alternativeBoundary(seed, ...contents) {
  for (let n = 0; n < 64; n++) {
    const boundary = `----=_Alt_${hashFor(seed, n === 0 ? "alt_boundary" : `alt_boundary:${n}`, 24)}`;
    if (contents.every((c) => !String(c).includes(`--${boundary}`))) return boundary;
  }
  throw new Error("no collision-free multipart boundary found");
}

// normalizeCRLF gives the plain-text part RFC 5322 line endings (CRLF) however the caller wrote it, so
// the part encodes identically under every Content-Transfer-Encoding. (The HTML body is deliberately NOT
// normalized: its bytes are today's bytes.)
function normalizeCRLF(s) {
  return String(s).replace(/\r\n/g, "\n").replace(/\n/g, "\r\n");
}

// toMessage converts push_job.send into the pipeline's ordered-header intermediate
// representation. MIME-Version/Content-Type/Content-Transfer-Encoding are part of `headers` (not
// appended separately at serialization) so the header-style-reorder and encoding-rotation stages
// can act on them like any other header — exactly mirroring the reference's model, where
// Content-Type/MIME-Version/Content-Transfer-Encoding participate in HEADER_ORDERS (Chunk 4).
// boundary is null (single-part, the Phase 3 shape) until the mime_boundary_rotation stage wraps
// the body in a multipart envelope (Chunk 4, §5 toggle 7).
//
// PLAIN-TEXT PART (Phase 3 close-out B3). When push_job.send carries a non-empty `text`, the message is
// instead a multipart/alternative whose FIRST part is the text/plain alternative and whose LAST part is
// the text/html body (RFC 2046 §5.1.4: parts are ordered from least to most faithful, the last part being
// the one the sender prefers). The outer message carries no Content-Transfer-Encoding (a multipart entity
// must be 7bit/8bit/binary, RFC 2045 §6.4); each part states its own charset and encoding. With `text`
// absent this function returns EXACTLY what it returned before the text part existed, field for field.
// jobId seeds the boundary; when it is not supplied the Control's Message-ID (itself derived from the job
// id) is the seed, so the same item still renders the same boundary.
export function toMessage(send, jobId) {
  const h = send.headers || {};
  const headers = [];
  for (const k of HEADER_ORDER) {
    if (h[k] != null && h[k] !== "") headers.push([k, h[k]]);
  }
  for (const k of Object.keys(h)) {
    if (!HEADER_ORDER.includes(k) && h[k] != null && h[k] !== "") headers.push([k, h[k]]);
  }
  headers.push(["MIME-Version", "1.0"]);
  const text = textPartOf(send);
  if (text !== null) {
    const body = String(send.body || "");
    const textCRLF = normalizeCRLF(text);
    const boundary = alternativeBoundary(jobId || h["Message-ID"] || "", body, textCRLF);
    headers.push(["Content-Type", `multipart/alternative; boundary="${boundary}"`]);
    return { headers, body, text: textCRLF, transferEncoding: "8bit", boundary, alternative: true };
  }
  headers.push(["Content-Type", 'text/html; charset="utf-8"']);
  headers.push(["Content-Transfer-Encoding", "8bit"]);
  return { headers, body: String(send.body || ""), transferEncoding: "8bit", boundary: null };
}

// serializeMessage renders the intermediate representation to RFC 5322 DATA bytes.
//
// Single-part (boundary === null): headers, blank line, the body encoded per msg.transferEncoding
// (encodeForTransfer's "8bit"/default branch is a pure pass-through — this is what makes the
// all-off/all-default case reproduce stream.js's buildDataBytes byte-for-byte; T1 pins this down).
//
// Multipart (boundary set, toggle 7 ON — a flagged deviation documented in variation-headers.js:
// Phase 3 has no multipart structure to "rotate the boundary of", so this toggle wraps the single
// body in a genuine multipart/mixed envelope containing EXACTLY ONE part): the outer headers (which
// mime_boundary_rotation already rewrote to a multipart Content-Type and stripped of a top-level
// Content-Transfer-Encoding) are followed by one MIME part carrying its own Content-Type/
// Content-Transfer-Encoding and the (possibly re-encoded) body.
export function serializeMessage(msg) {
  const headerLines = msg.headers.map(([k, v]) => `${k}: ${v}`);
  if (msg.alternative) return serializeAlternative(msg, headerLines);
  if (msg.boundary == null) {
    return [...headerLines, "", encodeForTransfer(msg.body, msg.transferEncoding)].join("\r\n");
  }
  return [
    ...headerLines,
    "",
    `--${msg.boundary}`,
    'Content-Type: text/html; charset="utf-8"',
    `Content-Transfer-Encoding: ${msg.transferEncoding}`,
    "",
    encodeForTransfer(msg.body, msg.transferEncoding),
    `--${msg.boundary}--`,
  ].join("\r\n");
}

// serializeAlternative renders the multipart/alternative message (Phase 3 close-out B3): the outer
// headers, then the text/plain part FIRST and the text/html part LAST (RFC 2046 §5.1.4, least to most
// faithful), then the closing delimiter. Each part states its charset and its Content-Transfer-Encoding
// (8bit unless the encoding-rotation toggle chose quoted-printable/base64, in which case both parts use
// the same encoding, exactly as the single HTML part does today). No random value and no clock is read:
// the bytes are a pure function of the message, so a retry of the same item renders the same bytes.
function serializeAlternative(msg, headerLines) {
  const te = msg.transferEncoding;
  const part = (type, content) => [
    `--${msg.boundary}`,
    `Content-Type: ${type}; charset="utf-8"`,
    `Content-Transfer-Encoding: ${te}`,
    "",
    encodeForTransfer(content, te),
  ];
  return [
    ...headerLines,
    "",
    ...part("text/plain", msg.text),
    ...part("text/html", msg.body),
    `--${msg.boundary}--`,
  ].join("\r\n");
}

// assembleDataBytes is the worker's actual send-path call (§1.1): build the Phase 3 representation,
// run it through the variation pipeline (identity when variationBlock is absent/all-default,
// §4), and serialize. jobId seeds every stage's determinism (§7) — the same job always produces
// the same variation (and, with a plain-text part, the same multipart boundary).
export function assembleDataBytes(send, variationBlock, jobId) {
  const resolved = resolveVariation(variationBlock);
  const msg = applyVariation(toMessage(send, jobId), resolved, jobId);
  return serializeMessage(msg);
}
