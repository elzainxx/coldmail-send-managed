// variation-seed.js — the job_id+stream-namespaced determinism primitives (§7, P8-5), split out
// of variation.js so the per-toggle stage modules (variation-content.js, and Chunk 4's header/MIME
// module) can depend on them WITHOUT creating an import cycle back through variation.js (which in
// turn imports the stage modules to assemble STAGES). variation.js re-exports these for existing
// callers/tests that import them from there.

import crypto from "node:crypto";

// prngFor returns a deterministic PRNG for (jobId, stream). `stream` NAMESPACES the sequence so each
// toggle draws independently: enabling one toggle cannot shift another's draws, which is exactly what
// makes the toggles independent (§8 T2, and P8-6's no-hidden-coupling requirement). The algorithm is
// a 32-bit xorshift seeded from a hash of `${jobId}:${stream}` — deterministic, dependency-free, and
// NOT security-relevant (it selects whitespace and px values; it never touches a credential, a key,
// or anything that must be unpredictable).
export function prngFor(jobId, stream) {
  const h = crypto.createHash("md5").update(`${jobId}:${stream}`).digest();
  let s = h.readUInt32BE(0) || 0x9e3779b9; // a zero seed would make xorshift degenerate to zero
  return () => {
    s ^= s << 13;
    s >>>= 0;
    s ^= s >>> 17;
    s ^= s << 5;
    s >>>= 0;
    return s / 0x100000000;
  };
}

// hashFor returns a stable lowercase hex digest of (jobId, stream), truncated to `len` chars — the
// primitive behind the inert URL cache-buster and the seeded comment payloads. Same job ⇒ same value,
// different jobs ⇒ different values.
export function hashFor(jobId, stream, len) {
  return crypto.createHash("md5").update(`${jobId}:${stream}`).digest("hex").slice(0, len);
}

// pickSeeded chooses one element of `list` deterministically for (jobId, stream). Returns undefined
// for an empty list so a caller can treat "no candidates" as "do nothing".
export function pickSeeded(list, jobId, stream) {
  if (!Array.isArray(list) || list.length === 0) return undefined;
  return list[Math.floor(prngFor(jobId, stream)() * list.length) % list.length];
}

// DEFAULT_PREHEADER_TEMPLATES is the locked spec's own literal default pool (§6.1) — token-free,
// body-agnostic filler (P8-7 Closeout Ruling 2: no template may presume a prior message/body it
// can't guarantee). Lives here (a leaf module) rather than in variation.js so both variation.js
// (VARIATION_DEFAULTS) and variation-content.js (the dynamic_preheader stage's fallback pool) can
// import the SAME list without creating an import cycle between them. Chunk 5 moves the SOURCE of
// this list into a seeded send_managed.config row (config-not-code); the value itself does not change —
// it is copied verbatim from the spec, not invented here.
export const DEFAULT_PREHEADER_TEMPLATES = [
  "A brief note for you",
  "A quick read for you",
  "Quick update for you",
  "Worth a look",
  "A quick note for you",
  "Thought you might find this useful",
  "Something relevant for you",
];
