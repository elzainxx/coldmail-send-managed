// variation.test.mjs — Phase 8 Chunk 2: the headline safety property (§4, P8-4) and the
// resolver/pipeline scaffolding it rests on. Real code paths, no mocks: this drives the ACTUAL
// assemble.js (toMessage/serializeMessage/assembleDataBytes) and variation.js
// (resolveVariation/isNeutral/applyVariation) against the ACTUAL frozen Phase 3 reference
// (stream.js's exported buildDataBytes) — the same function #runJob calls in production.
//
// Run: node --test services/worker/test/*.test.mjs  (Node 20+).

import { test } from "node:test";
import assert from "node:assert/strict";
import { buildDataBytes } from "../src/stream.js";
import { assembleDataBytes } from "../src/assemble.js";
import { resolveVariation, isNeutral, VARIATION_DEFAULTS, prngFor, pickSeeded } from "../src/variation.js";

// A representative Phase 3 send — the three legitimately-random inputs (Message-ID, Date, and any
// future MIME boundary) are PINNED to fixed literal values here, exactly as §4.2/§8 T1 requires,
// so the test isolates "does the variation layer alter anything when off" from "Phase 3 itself is
// non-deterministic". There is no boundary field yet (Phase 3 is single-part — no MIME boundary
// exists today), so pinning Message-ID + Date is the whole requirement until Chunk 4 introduces one.
function pinnedSend() {
  return {
    recipient: "alice@example.com",
    from_address: "sender@ourdomain.com",
    subject: "Pinned Subject",
    body: "<p>Hello <b>world</b></p>",
    headers: {
      From: '"P3 Suite" <sender@ourdomain.com>',
      To: "alice@example.com",
      Subject: "Pinned Subject",
      "Reply-To": "reply@ourdomain.com",
      "Message-ID": "<pinned-job-id@ourdomain.com>",
      Date: "Mon, 21 Jul 2026 14:05:00 +0000",
    },
  };
}

const JOB_ID = "pinned-job-id";

test("T1: variation ABSENT ⇒ assembleDataBytes is byte-for-byte the Phase 3 reference", () => {
  const send = pinnedSend();
  const phase3 = buildDataBytes(send);
  const phase8 = assembleDataBytes(send, undefined, JOB_ID);
  assert.equal(phase8, phase3, "an absent variation block must produce byte-identical output to Phase 3");
});

test("T1: variation ALL-DEFAULT ⇒ assembleDataBytes is byte-for-byte the Phase 3 reference", () => {
  const send = pinnedSend();
  const phase3 = buildDataBytes(send);
  const phase8 = assembleDataBytes(send, { ...VARIATION_DEFAULTS }, JOB_ID);
  assert.equal(phase8, phase3, "an all-default variation block must produce byte-identical output to Phase 3");
});

// Anti-vacuity for the two tests above: prove the reference itself is non-trivial (contains the
// pinned headers, the body, the MIME framing) — otherwise an empty-string equality would pass for
// the wrong reason.
test("T1 anti-vacuity: the Phase 3 reference is genuinely non-trivial", () => {
  const phase3 = buildDataBytes(pinnedSend());
  assert.ok(phase3.includes("Message-ID: <pinned-job-id@ourdomain.com>"), "reference missing pinned Message-ID");
  assert.ok(phase3.includes("Date: Mon, 21 Jul 2026 14:05:00 +0000"), "reference missing pinned Date");
  assert.ok(phase3.includes("Hello <b>world</b>"), "reference missing body");
  assert.ok(phase3.includes("MIME-Version: 1.0"), "reference missing MIME framing");
  assert.ok(phase3.length > 100, "reference suspiciously short — equality tests would be vacuous");
});

test("resolveVariation: absent/malformed block degrades to VARIATION_DEFAULTS, never throws", () => {
  assert.deepEqual(resolveVariation(undefined), VARIATION_DEFAULTS);
  assert.deepEqual(resolveVariation(null), VARIATION_DEFAULTS);
  assert.deepEqual(resolveVariation({}), VARIATION_DEFAULTS);
  // Unrecognized enum values fall back to the neutral default rather than being accepted verbatim.
  const resolved = resolveVariation({ header_style: "not-a-real-style", precedence: "bogus", comment_intensity: "extreme" });
  assert.equal(resolved.header_style, "none");
  assert.equal(resolved.precedence, "auto");
  assert.equal(resolved.comment_intensity, "low");
});

test("resolveVariation: a legitimate non-default value passes through unchanged", () => {
  const resolved = resolveVariation({ html_minify: true, header_style: "gmail", comment_intensity: "high" });
  assert.equal(resolved.html_minify, true);
  assert.equal(resolved.header_style, "gmail");
  assert.equal(resolved.comment_intensity, "high");
  // Untouched fields still resolve to their own defaults.
  assert.equal(resolved.css_micro, false);
  assert.equal(resolved.precedence, "auto");
});

test("resolveVariation: the three amended rate/magnitude parameters (P8-OD-1) pass through when valid and degrade when out-of-range", () => {
  const valid = resolveVariation({ css_magnitude_px: 5, zero_width_rate: 0.2, user_agent_rate: 0.9 });
  assert.equal(valid.css_magnitude_px, 5);
  assert.equal(valid.zero_width_rate, 0.2);
  assert.equal(valid.user_agent_rate, 0.9);

  // Out-of-range / wrong-type values degrade to the documented default rather than being accepted.
  const invalid = resolveVariation({ css_magnitude_px: 0, zero_width_rate: 1.5, user_agent_rate: "half" });
  assert.equal(invalid.css_magnitude_px, VARIATION_DEFAULTS.css_magnitude_px, "css_magnitude_px < 1 must degrade to the default");
  assert.equal(invalid.zero_width_rate, VARIATION_DEFAULTS.zero_width_rate, "zero_width_rate > 1 must degrade to the default");
  assert.equal(invalid.user_agent_rate, VARIATION_DEFAULTS.user_agent_rate, "a non-numeric user_agent_rate must degrade to the default");

  // The boundary values 0 and 1 ARE legitimate for the two rate keys (fully off / fully on).
  const boundary = resolveVariation({ zero_width_rate: 0, user_agent_rate: 1 });
  assert.equal(boundary.zero_width_rate, 0);
  assert.equal(boundary.user_agent_rate, 1);
});

test("resolveVariation: Phase 9.5 S11 — comment_intensity_rates (percent 0-100 wire -> fraction 0-1) and css_skip_below_px pass through when valid, degrade per-bucket/whole when malformed", () => {
  const valid = resolveVariation({ comment_intensity_rates: { low: 10, medium: 20, high: 40 }, css_skip_below_px: 9 });
  assert.deepEqual(valid.comment_intensity_rates, { low: 0.1, medium: 0.2, high: 0.4 });
  assert.equal(valid.css_skip_below_px, 9);

  // A partially-malformed rates object degrades ONLY the bad bucket, not the whole object.
  const partial = resolveVariation({ comment_intensity_rates: { low: 10, medium: "bad", high: 200 } });
  assert.equal(partial.comment_intensity_rates.low, 0.1);
  assert.equal(partial.comment_intensity_rates.medium, VARIATION_DEFAULTS.comment_intensity_rates.medium, "non-numeric medium must degrade to the default fraction");
  assert.equal(partial.comment_intensity_rates.high, VARIATION_DEFAULTS.comment_intensity_rates.high, "out-of-range (>100) high must degrade to the default fraction");

  // Absent/malformed block ⇒ the full default table, never throws.
  assert.deepEqual(resolveVariation(undefined).comment_intensity_rates, VARIATION_DEFAULTS.comment_intensity_rates);
  assert.equal(resolveVariation({ css_skip_below_px: -1 }).css_skip_below_px, VARIATION_DEFAULTS.css_skip_below_px, "a negative skip-below must degrade to the default");
});

test("isNeutral: true for defaults/absent, and toggle PARAMETERS alone never flip it", () => {
  assert.equal(isNeutral(resolveVariation(undefined)), true);
  assert.equal(isNeutral(resolveVariation({})), true);
  // Anti-vacuity for the "parameters are inert while off" claim: changing comment_intensity /
  // preheader_templates with their parent toggle OFF must not make the set non-neutral.
  assert.equal(isNeutral(resolveVariation({ comment_intensity: "high" })), true);
  assert.equal(isNeutral(resolveVariation({ preheader_templates: ["custom"] })), true);
  // Same claim for the three amended P8-OD-1 parameters: changing a rate/magnitude alone, with
  // its parent toggle still off, must not flip isNeutral.
  assert.equal(isNeutral(resolveVariation({ css_magnitude_px: 10 })), true);
  assert.equal(isNeutral(resolveVariation({ zero_width_rate: 0.9 })), true);
  assert.equal(isNeutral(resolveVariation({ user_agent_rate: 0.9 })), true);
  // Same claim for the two Phase 9.5 S11 parameters.
  assert.equal(isNeutral(resolveVariation({ comment_intensity_rates: { low: 90, medium: 90, high: 90 } })), true);
  assert.equal(isNeutral(resolveVariation({ css_skip_below_px: 0 })), true);
});

test("isNeutral: false once a real body/content toggle is switched on (Chunk 3 stages registered)", () => {
  const anyToggleOn = resolveVariation({ html_minify: true });
  assert.equal(isNeutral(anyToggleOn), false, "html_minify is a registered stage — isNeutral must report non-neutral");
});

test("determinism: prngFor/pickSeeded are stable for the same (jobId, stream) and diverge across streams/jobs", () => {
  const a1 = prngFor("job-A", "css")();
  const a2 = prngFor("job-A", "css")();
  assert.equal(a1, a2, "same (jobId, stream) must draw the same first value");
  const b = prngFor("job-B", "css")();
  assert.notEqual(a1, b, "different jobId must diverge (extremely unlikely collision otherwise)");
  const aOther = prngFor("job-A", "comments")();
  assert.notEqual(a1, aOther, "different stream namespace for the SAME job must diverge — toggles must not share a draw");

  const list = ["x", "y", "z", "w"];
  const p1 = pickSeeded(list, "job-A", "preheader");
  const p2 = pickSeeded(list, "job-A", "preheader");
  assert.equal(p1, p2, "pickSeeded must be stable for the same (jobId, stream)");
  assert.equal(pickSeeded([], "job-A", "preheader"), undefined, "empty candidate list yields undefined, not a throw");
});
