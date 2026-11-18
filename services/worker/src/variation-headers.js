// variation-headers.js — Phase 8 CHUNK 4: the header/MIME STAGES (toggles 6, 7, 9, 10, 11 of §5;
// toggle 12 Thread-Index is INERT and deliberately registers NO stage at all — see below). Ported
// from the reference (`Docs/history/mailflow-worker (Sender Inspiration)/src/features/header-engineering.ts`
// C1/C2/C6) plus toggle 6/7 from content-variation.ts's encoding note, with TWO deliberate
// deviations from the reference, both already decided in the locked spec:
//
//   - P8-6: User-Agent injection (toggle 10) is FULLY INDEPENDENT of header_style (toggle 9) — the
//     reference links C2 to C1's selected style; here neither toggle reads the other's resolved
//     value, only its own + the job_id seed (§8 T2/T9 no-hidden-coupling).
//   - Every random draw goes through the job_id+stream-seeded PRNG (variation-seed.js), not the
//     reference's bare Math.random() (§7, P8-5 determinism).
//
// Applied AFTER every body/content stage (§1.1's two-layer ordering: body/content, then header/MIME
// engineering). Order here: encoding rotation → MIME boundary rotation → User-Agent → Precedence →
// header-style reorder LAST (it must see whatever User-Agent/Precedence added, exactly like the
// reference reorders "last" within its style block — generalized here since UA/Precedence are no
// longer nested inside that block, per the P8-6 decoupling).

import { prngFor, pickSeeded, hashFor } from "./variation-seed.js";
import { setHeader, removeHeader } from "./mime-headers.js";

// --- Toggle 6: encoding rotation (variation_encoding_rotation, ref B7) --------------------------

const ENCODINGS = ["quoted-printable", "base64"];

// The stage only decides WHICH encoding a job uses (seeded) and records it as
// msg.transferEncoding + the Content-Transfer-Encoding header value; the actual byte transform
// (mime-codec.js's encodeForTransfer) runs at serialization (assemble.js), so this stage stays a
// pure transform of the intermediate representation, like every other stage.
function rotateEncoding(msg, jobId) {
  const encoding = pickSeeded(ENCODINGS, jobId, "encoding");
  // A multipart/alternative message (the optional plain-text part, Phase 3 close-out B3) has NO top-level
  // Content-Transfer-Encoding — a multipart entity must be 7bit/8bit/binary (RFC 2045 §6.4) — so the
  // chosen encoding applies to its two PARTS only (serializeAlternative writes it on each). Adding the
  // header at the top level would label the whole multipart quoted-printable/base64, which is invalid.
  if (msg.alternative) return { ...msg, transferEncoding: encoding };
  return { ...msg, transferEncoding: encoding, headers: setHeader(msg.headers, "Content-Transfer-Encoding", encoding) };
}

// --- Toggle 7: MIME boundary rotation (variation_mime_boundary_rotation, ref C4) -----------------
//
// Phase 3 is single-part (no MIME boundary exists — buildDataBytes emits one text/html body with
// no multipart wrapper). The locked spec's toggle 7 ("varies the boundary delimiter style/format")
// presupposes a multipart structure to vary the delimiter OF. Flagged deviation, resolved
// conservatively rather than silently: when this toggle is ON, the single html body is wrapped in
// a genuine `multipart/mixed` envelope containing EXACTLY ONE part (the same text/html body,
// unchanged) — no attachment, no CID, no second part is introduced (P8-11 stays excluded). This
// keeps §8 T6's requirement checkable ("the boundary delimiter differs but the parsed MIME tree —
// parts, types, order — is identical") without expanding scope: the tree is trivially one part,
// type text/html, for every seeded boundary format. Off ⇒ no wrapper — the Phase 3 identity.

const BOUNDARY_STYLES = ["dashes", "outlook", "plain"];

function boundaryFor(style, jobId) {
  switch (style) {
    case "outlook":
      return `_NextPart_${hashFor(jobId, "boundary", 20)}_`;
    case "plain":
      return `boundary_${hashFor(jobId, "boundary", 24)}`;
    default:
      return `----=_Part_${hashFor(jobId, "boundary", 16)}`;
  }
}

function rotateBoundary(msg, jobId) {
  const style = pickSeeded(BOUNDARY_STYLES, jobId, "boundary_style");
  if (msg.alternative) {
    // The message is ALREADY a multipart/alternative (the optional plain-text part): there is nothing to
    // wrap in a multipart/mixed envelope. The toggle varies the boundary's delimiter style and nothing
    // else, so the parts, their types and their order are untouched. The candidate is a pure function of
    // (style, jobId, n); the first one that does not occur inside either part is used (n = 0 in practice).
    let boundary = boundaryFor(style, jobId);
    for (let n = 1; n < 64 && [msg.body, msg.text].some((c) => String(c).includes(`--${boundary}`)); n++) {
      boundary = boundaryFor(style, `${jobId}:${n}`);
    }
    return { ...msg, headers: setHeader(msg.headers, "Content-Type", `multipart/alternative; boundary="${boundary}"`), boundary };
  }
  const boundary = boundaryFor(style, jobId);
  const headers = removeHeader(setHeader(msg.headers, "Content-Type", `multipart/mixed; boundary="${boundary}"`), "Content-Transfer-Encoding");
  return { ...msg, headers, boundary };
}

// --- Toggle 10: User-Agent injection (variation_user_agent_injection, ref C2) — decoupled (P8-6) --

const USER_AGENTS = ["Microsoft-MacOutlook/16.80", "Mozilla Thunderbird 115.0"];

// injectRate amended post-Chunk-5 from a fixed 0.3 to the variation_user_agent_rate config
// parameter (P8-OD-1 RULED-WIDEN) — the reference's own 70%-omission/30%-inject split is now the
// dial's DEFAULT, not a hardcoded constant.
function maybeInjectUserAgent(msg, jobId, injectRate) {
  const rate = injectRate ?? 0.3;
  const rng = prngFor(jobId, "user_agent");
  if (rng() >= rate) return msg; // the omission case — no header added, still deterministic
  const ua = pickSeeded(USER_AGENTS, jobId, "user_agent_value");
  return { ...msg, headers: setHeader(msg.headers, "User-Agent", ua) };
}

// --- Toggle 11: precedence header control (variation_precedence, ref C6) ------------------------
//
// Only `marketing` differs from Phase 3 (which emits no Precedence header): `auto` and `personal`
// both OMIT it, matching Phase 3 exactly, so only `marketing` is registered as non-neutral (§4 — a
// toggle whose current value produces Phase 3's own output must never report itself active).

function applyPrecedence(msg) {
  return { ...msg, headers: setHeader(msg.headers, "Precedence", "bulk") };
}

// --- Toggle 9: header reordering by client style (variation_header_style, ref C1) ---------------

const HEADER_ORDERS = {
  gmail: ["MIME-Version", "Date", "Message-ID", "Subject", "From", "To", "Content-Type"],
  outlook: ["From", "To", "Subject", "Date", "Message-ID", "Content-Type", "MIME-Version"],
  apple: ["Content-Type", "Content-Transfer-Encoding", "MIME-Version", "Subject", "From", "Date", "Message-ID", "To"],
  thunderbird: ["Date", "From", "Subject", "To", "Message-ID", "MIME-Version", "Content-Type"],
};
const RANDOM_STYLES = ["gmail", "outlook", "apple", "thunderbird"];

// reorderHeaders places known headers (case-insensitive match) in the style's documented order
// first, then appends any remaining headers in their existing relative order — adds/removes/changes
// no header VALUE, only position (§5 toggle 9: "Reorders only").
function reorderHeaders(headers, style) {
  const order = HEADER_ORDERS[style];
  if (!order) return headers;
  const placed = new Set();
  const ordered = [];
  for (const name of order) {
    const lower = name.toLowerCase();
    const entry = headers.find(([k]) => k.toLowerCase() === lower);
    if (entry) {
      ordered.push(entry);
      placed.add(entry);
    }
  }
  for (const entry of headers) {
    if (!placed.has(entry)) ordered.push(entry);
  }
  return ordered;
}

function applyHeaderStyle(msg, v, jobId) {
  const style = v.header_style === "random" ? pickSeeded(RANDOM_STYLES, jobId, "header_style_random") : v.header_style;
  return { ...msg, headers: reorderHeaders(msg.headers, style) };
}

// --- stage registrations, applied AFTER every body/content stage --------------------------------
// Toggle 12 (Thread-Index, ref C3) is deliberately UNREGISTERED — P8-8: "the toggle exists and is
// wired [via resolveVariation's normalization], but in V1 it emits no header ... Enabling it is a
// documented no-op." Registering no stage means it is trivially inert in BOTH directions and can
// never affect isNeutral, which is the literal reading of "off and on both emit nothing in V1".

export const headerMimeStages = [
  {
    name: "encoding_rotation",
    active: (v) => v.encoding_rotation,
    apply: (msg, v, jobId) => rotateEncoding(msg, jobId),
  },
  {
    name: "mime_boundary_rotation",
    active: (v) => v.mime_boundary_rotation,
    apply: (msg, v, jobId) => rotateBoundary(msg, jobId),
  },
  {
    name: "user_agent_injection",
    active: (v) => v.user_agent_injection,
    apply: (msg, v, jobId) => maybeInjectUserAgent(msg, jobId, v.user_agent_rate),
  },
  {
    name: "precedence",
    active: (v) => v.precedence === "marketing",
    apply: (msg) => applyPrecedence(msg),
  },
  {
    name: "header_style",
    active: (v) => v.header_style !== "none",
    apply: (msg, v, jobId) => applyHeaderStyle(msg, v, jobId),
  },
];
