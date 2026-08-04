// variation-content.js — Phase 8 CHUNKS 3+4: the body/content STAGES (toggles 1-5 shipped in
// Chunk 3; toggle 8 dynamic preheader added in Chunk 4). Ported from the reference
// (`Docs/history/mailflow-worker (Sender Inspiration)/src/features/content-variation.ts` B1-B6, B8), with
// ONE deliberate adaptation throughout: every random draw is replaced by the job_id+stream-seeded
// PRNG from variation-seed.js (P8-5) so the same job always produces the same variation — the
// reference used bare Math.random(), which this module never calls.
//
// Each stage operates on msg.body (an HTML string) and returns a new message with only `body`
// changed — headers/transferEncoding/boundary pass through untouched. Registered into STAGES by
// variation.js in the locked §1.1 pipeline order: preheader → minify → comments → css → url params
// → zero-width, then (variation-headers.js) the header/MIME stages.

import { prngFor, hashFor, pickSeeded } from "./variation-seed.js";

function withBody(msg, body) {
  return { ...msg, body };
}

// --- Toggle 8: dynamic preheader (variation_dynamic_preheader + _preheader_templates, ref B8) ---
// Ordered FIRST in the body/content pipeline (§1.1): "dynamic preheader → HTML optimization →
// comments → CSS variation → link params → image params → character variation."

// GMAIL_PREHEADER_PADDING pushes Gmail's auto-generated preview text away from the injected
// preheader (reference: U+200C ZWNJ + NBSP, x50) — ported verbatim, not a seeded/random value.
const GMAIL_PREHEADER_PADDING = "‌ ".repeat(50);

// buildPreheaderSpan wraps the chosen template in the reference's hidden-span CSS
// (display:none + 1px + overflow:hidden keeps it out of the rendered body — §5 toggle 8: "Hidden
// span -> the visible body renders identically") plus the Gmail padding.
function buildPreheaderSpan(text) {
  return `<span style="display:none;font-size:1px;color:#ffffff;max-height:0;max-width:0;opacity:0;overflow:hidden;">${text}${GMAIL_PREHEADER_PADDING}</span>`;
}

// injectDynamicPreheader picks one template SEEDED from job_id (P8-7: V1 ships no per-contact merge
// data worker-side, so any `{token}` is cleaned to empty rather than substituted — matching the
// reference's "clean up empty tokens" step) and prepends the hidden span right after `<body>`
// (case-insensitive), or at the very start of the document if there is no `<body>` tag at all.
export function injectDynamicPreheader(body, templates, jobId) {
  const template = pickSeeded(templates, jobId, "preheader");
  if (template === undefined) return body; // no candidates — nothing to inject
  const cleaned = template.replace(/\{[^}]+\}/g, "").replace(/\s{2,}/g, " ").trim();
  const span = buildPreheaderSpan(cleaned);
  const bodyTagMatch = body.match(/<body[^>]*>/i);
  if (!bodyTagMatch) return span + body;
  return body.slice(0, bodyTagMatch.index + bodyTagMatch[0].length) + span + body.slice(bodyTagMatch.index + bodyTagMatch[0].length);
}

// --- Toggle 1: HTML minification (variation_html_minify, ref B1) --------------------------------

// htmlMinify: strip HTML comments (PRESERVE Outlook conditional comments `<!--[if ...` and the
// matching `<!--<![endif]-->` close form), collapse inter-tag whitespace, collapse runs of spaces,
// trim line-trailing whitespace, collapse empty lines. Deterministic — no randomness involved, so
// no seed is needed; ported verbatim from the reference's minifyEmailHtml.
export function htmlMinify(body) {
  let result = body;
  result = result.replace(/<!--(?!\[if)(?!<!\[endif\])[\s\S]*?-->/g, "");
  result = result.replace(/>\s+</g, "><");
  result = result.replace(/\s{2,}/g, " ");
  result = result.replace(/\s+\n/g, "\n");
  result = result.replace(/\n{2,}/g, "\n");
  return result.trim();
}

// --- Toggle 2: invisible comment injection (variation_comment_injection + _intensity, ref B2) ----

// COMMENT_RATE is the fallback used when the caller passes no `rates` (or an incomplete one) —
// the Phase 8 original hardcoded table. Phase 9.5 S11 (RULED, P9.5-OD-3) promotes these three
// numbers to the `variation_comment_intensity_rates` config dial, delivered per-call via
// injectComments' 4th argument rather than as a second hardcoded constant, so there is exactly
// one source of truth once the caller supplies rates (variation.js's resolved `rates` object).
const COMMENT_RATE = { low: 0.05, medium: 0.15, high: 0.30 };

// injectComments: after each closing tag, seeded-decide (per occurrence, in document order) whether
// to append an invisible `<!--{8 seeded hex chars}-->`. Two draws per closing tag from the SAME
// job+"comments"-seeded stream — one for the rate decision, four (one per byte) for the hex payload
// when it fires — so the whole injection is reproducible for a given job_id and unaffected by any
// OTHER toggle's own stream (§7/P8-6 no hidden coupling). `rates` (optional, {low,medium,high} as
// fractions 0-1) overrides COMMENT_RATE per bucket when provided — Phase 9.5 S11.
export function injectComments(body, intensity, jobId, rates) {
  const rate = (rates && rates[intensity]) ?? COMMENT_RATE[intensity] ?? COMMENT_RATE.low;
  const rng = prngFor(jobId, "comments");
  return body.replace(/(<\/\w+>)/g, (match) => {
    if (rng() < rate) {
      let hex = "";
      for (let i = 0; i < 4; i++) hex += Math.floor(rng() * 256).toString(16).padStart(2, "0");
      return `${match}<!--${hex}-->`;
    }
    return match;
  });
}

// --- Toggle 3: CSS micro-variation (variation_css_micro, ref B3) --------------------------------

// applyCssVariation: vary integer `padding`/`margin` px values by a seeded
// {-magnitudePx, ..., +magnitudePx} (magnitudePx amended post-Chunk-5 from a fixed ±1 to the
// variation_css_magnitude_px config parameter, P8-OD-1 RULED-WIDEN). Skips values below
// skipBelowPx (amended Phase 9.5 S11, RULED P9.5-OD-3: was a fixed <3px in code; defaults to 3
// when omitted, preserving the original threshold) and decimal values (preserve precision,
// matching the reference); never lets the result go below 0. One draw per matched declaration
// from the job+"css" stream.
export function applyCssVariation(body, jobId, magnitudePx, skipBelowPx) {
  const magnitude = magnitudePx ?? 1;
  const skipBelow = skipBelowPx ?? 3;
  const span = magnitude * 2 + 1; // number of integers in [-magnitude, +magnitude]
  const rng = prngFor(jobId, "css");
  return body.replace(/(padding|margin):\s*(\d+(?:\.\d+)?)px/g, (match, prop, px) => {
    const original = parseFloat(px);
    if (original < skipBelow) return match;
    if (px.includes(".")) return match;
    const delta = Math.floor(rng() * span) - magnitude;
    const varied = Math.round(original) + delta;
    return `${prop}: ${Math.max(0, varied)}px`;
  });
}

// --- Toggle 4: unique link/image URL parameters (variation_unique_url_params, ref B4+B5) --------
// NOT tracking (§9 P8-10, permanent forward boundary): purely an inert cache-buster appended to the
// AUTHOR'S OWN destination URL. No new endpoint, no redirect, no tracking domain is ever introduced
// here — see variation.js's module header and the spec §5 toggle 4 for the forward-boundary text
// this stage must never be repurposed against.

const IMG_EXT = /\.(png|jpg|jpeg|gif|webp|svg)$/i;

// addUniqueURLParams: appends `?_ref=`/`&_ref=` to every `href="http(s)://..."` and `?_v=`/`&_v=`
// to every image `src="http(s)://....(png|jpg|jpeg|gif|webp|svg)..."`, where the hash is derived
// from job_id (§5 toggle 4: "per-recipient, since one job = one recipient") — NOT a per-URL random
// draw, so every link/image in the SAME message shares one stable per-recipient value, matching the
// reference's per-contact (not per-link) hash. Skips a URL that already carries the param.
export function addUniqueURLParams(body, jobId) {
  const refHash = hashFor(jobId, "url_ref", 8);
  const vHash = hashFor(jobId, "url_img", 6);

  let out = body.replace(/href="(https?:\/\/[^"]+)"/g, (match, url) => {
    if (url.includes("_ref=")) return match;
    return `href="${appendParam(url, "_ref", refHash)}"`;
  });
  out = out.replace(/src="(https?:\/\/[^"]+)"/g, (match, url) => {
    const withoutFragment = url.split("#")[0];
    if (!IMG_EXT.test(withoutFragment.split("?")[0])) return match;
    if (url.includes("_v=")) return match;
    return `src="${appendParam(url, "_v", vHash)}"`;
  });
  return out;
}

// appendParam inserts `name=value` before any URL fragment, using `&` if the URL already has a
// query string, `?` otherwise.
function appendParam(url, name, value) {
  const fragmentIndex = url.indexOf("#");
  const urlPart = fragmentIndex === -1 ? url : url.slice(0, fragmentIndex);
  const fragment = fragmentIndex === -1 ? "" : url.slice(fragmentIndex);
  const separator = urlPart.includes("?") ? "&" : "?";
  return `${urlPart}${separator}${name}=${value}${fragment}`;
}

// --- Toggle 5: zero-width insertion (variation_zero_width, ref B6) — DEFAULT OFF -----------------

const ZWJ = "‍";

// insertZeroWidth: within visible text nodes only (`>text<`, never inside a tag/attribute/URL),
// insert a ZWJ before a seeded `rate` fraction of spaces (rate amended post-Chunk-5 from a fixed
// 0.05 to the variation_zero_width_rate config parameter, P8-OD-1 RULED-WIDEN). Skips text nodes
// under 5 chars and skips a space immediately after an HTML entity (`&...`) so an entity is never
// split. One draw per space from the job+"zwj" stream, in document order — reproducible per job_id.
export function insertZeroWidth(body, jobId, rate) {
  const zwjRate = rate ?? 0.05;
  const rng = prngFor(jobId, "zwj");
  return body.replace(/>([^<]+)</g, (match, textContent) => {
    if (textContent.trim().length < 5) return match;
    const varied = textContent.replace(/ /g, (space, offset) => {
      const before = textContent.substring(Math.max(0, offset - 6), offset);
      if (/&\w*$/.test(before)) return space;
      return rng() < zwjRate ? `${ZWJ}${space}` : space;
    });
    return `>${varied}<`;
  });
}

// --- stage registrations, in the §1.1 body/content pipeline order -------------------------------

export const contentStages = [
  {
    name: "dynamic_preheader",
    active: (v) => v.dynamic_preheader,
    apply: (msg, v, jobId) => withBody(msg, injectDynamicPreheader(msg.body, v.preheader_templates, jobId)),
  },
  {
    name: "html_minify",
    active: (v) => v.html_minify,
    apply: (msg) => withBody(msg, htmlMinify(msg.body)),
  },
  {
    name: "comment_injection",
    active: (v) => v.comment_injection,
    apply: (msg, v, jobId) => withBody(msg, injectComments(msg.body, v.comment_intensity, jobId, v.comment_intensity_rates)),
  },
  {
    name: "css_micro",
    active: (v) => v.css_micro,
    apply: (msg, v, jobId) => withBody(msg, applyCssVariation(msg.body, jobId, v.css_magnitude_px, v.css_skip_below_px)),
  },
  {
    name: "unique_url_params",
    active: (v) => v.unique_url_params,
    apply: (msg, v, jobId) => withBody(msg, addUniqueURLParams(msg.body, jobId)),
  },
  {
    name: "zero_width",
    active: (v) => v.zero_width,
    apply: (msg, v, jobId) => withBody(msg, insertZeroWidth(msg.body, jobId, v.zero_width_rate)),
  },
];
