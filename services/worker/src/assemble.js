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

// HEADER_ORDER mirrors stream.js's buildDataBytes exactly: the §8 minimum guaranteed set in the
// Control's fixed order, then any additional Control-supplied headers in their map iteration order.
const HEADER_ORDER = ["From", "To", "Subject", "Reply-To", "Message-ID", "Date"];

// toMessage converts push_job.send into the pipeline's ordered-header intermediate
// representation. MIME-Version/Content-Type/Content-Transfer-Encoding are part of `headers` (not
// appended separately at serialization) so the header-style-reorder and encoding-rotation stages
// can act on them like any other header — exactly mirroring the reference's model, where
// Content-Type/MIME-Version/Content-Transfer-Encoding participate in HEADER_ORDERS (Chunk 4).
// boundary is null (single-part, the Phase 3 shape) until the mime_boundary_rotation stage wraps
// the body in a multipart envelope (Chunk 4, §5 toggle 7).
export function toMessage(send) {
  const h = send.headers || {};
  const headers = [];
  for (const k of HEADER_ORDER) {
    if (h[k] != null && h[k] !== "") headers.push([k, h[k]]);
  }
  for (const k of Object.keys(h)) {
    if (!HEADER_ORDER.includes(k) && h[k] != null && h[k] !== "") headers.push([k, h[k]]);
  }
  headers.push(["MIME-Version", "1.0"]);
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

// assembleDataBytes is the worker's actual send-path call (§1.1): build the Phase 3 representation,
// run it through the variation pipeline (identity when variationBlock is absent/all-default,
// §4), and serialize. jobId seeds every stage's determinism (§7) — the same job always produces
// the same variation.
export function assembleDataBytes(send, variationBlock, jobId) {
  const resolved = resolveVariation(variationBlock);
  const msg = applyVariation(toMessage(send), resolved, jobId);
  return serializeMessage(msg);
}
