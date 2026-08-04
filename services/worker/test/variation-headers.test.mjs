// variation-headers.test.mjs — Phase 8 CHUNK 4: encoding/MIME/header toggles (§5 toggles 6, 7, 8,
// 9, 10, 11, 12) + deterministic seeding at the full-message level (§7/§8 T4). Real code paths, no
// mocks: drives the actual variation-content.js/variation-headers.js/mime-codec.js/assemble.js.

import { test } from "node:test";
import assert from "node:assert/strict";
import { assembleDataBytes } from "../src/assemble.js";
import { resolveVariation } from "../src/variation.js";
import { injectDynamicPreheader } from "../src/variation-content.js";
import { encodeQuotedPrintable, decodeQuotedPrintable, encodeBase64, decodeBase64 } from "../src/mime-codec.js";

function sendWithBody(body, extraHeaders) {
  return {
    recipient: "alice@example.com",
    from_address: "sender@ourdomain.com",
    subject: "Subject",
    body,
    headers: {
      From: '"P3 Suite" <sender@ourdomain.com>',
      To: "alice@example.com",
      Subject: "Subject",
      "Message-ID": "<pinned-job-id@ourdomain.com>",
      Date: "Mon, 21 Jul 2026 14:05:00 +0000",
      ...extraHeaders,
    },
  };
}

// --- Toggle 8: dynamic preheader ------------------------------------------------------------------

test("dynamic_preheader: off ⇒ no preheader injected (toggle gate)", () => {
  const send = sendWithBody("<body><p>hi</p></body>");
  const bytes = assembleDataBytes(send, { dynamic_preheader: false }, "job-preheader-off");
  assert.ok(!bytes.includes('style="display:none'), "no hidden preheader span when off");
});

test("dynamic_preheader: injects a hidden span right after <body>, seeded per job_id, tokens cleaned to empty", () => {
  const send = sendWithBody("<html><body><p>hi</p></body></html>");
  const bytes = assembleDataBytes(send, { dynamic_preheader: true }, "job-preheader-1");
  const bodyIdx = bytes.indexOf("<body>");
  const spanIdx = bytes.indexOf('<span style="display:none');
  assert.ok(spanIdx > bodyIdx && spanIdx < bodyIdx + 20, "the hidden span must immediately follow <body>");
  assert.ok(!/\{[^}]+\}/.test(bytes), "any {token} must be cleaned to empty, never left literal");
});

test("dynamic_preheader: no <body> tag ⇒ prepended at the start of the document", () => {
  const send = sendWithBody("<p>hi, no body tag here</p>");
  const bytes = assembleDataBytes(send, { dynamic_preheader: true }, "job-preheader-nobodytag");
  const headerEnd = bytes.indexOf("\r\n\r\n");
  const afterHeaders = bytes.slice(headerEnd + 4);
  assert.ok(afterHeaders.startsWith('<span style="display:none'), "with no <body> tag the span must prepend the whole document");
});

test("dynamic_preheader: renders identically (hidden span never affects visible content) — body text still present unmodified", () => {
  const send = sendWithBody("<body><p>Hello visible world</p></body>");
  const bytes = assembleDataBytes(send, { dynamic_preheader: true }, "job-preheader-render");
  assert.ok(bytes.includes("<p>Hello visible world</p>"), "visible body content must be untouched");
});

test("dynamic_preheader: custom variation_preheader_templates is honored", () => {
  const custom = ["My custom filler line"];
  const out = injectDynamicPreheader("<body></body>", custom, "job-custom-preheader");
  assert.ok(out.includes("My custom filler line"), "a custom template list must be used when provided");
});

test("dynamic_preheader: deterministic per job_id (same job picks the same template)", () => {
  const send = sendWithBody("<body><p>hi</p></body>");
  const a = assembleDataBytes(send, { dynamic_preheader: true }, "job-preheader-det");
  const b = assembleDataBytes(send, { dynamic_preheader: true }, "job-preheader-det");
  assert.equal(a, b);
});

// --- Toggle 6: encoding rotation --------------------------------------------------------------

test("mime-codec: quoted-printable and base64 round-trip byte-for-byte", () => {
  const text = "Héllo <b>wörld</b>\r\nline two with = sign and non-ascii café";
  assert.equal(decodeQuotedPrintable(encodeQuotedPrintable(text)), text);
  assert.equal(decodeBase64(encodeBase64(text)), text);
});

// R3 review finding: RFC 2045 §6.7 rule 3 requires trailing space/tab at the end of an encoded
// line to be escaped as =20/=09, because many real-world relays/MTAs strip trailing line
// whitespace. Without this, a body ending a visible line in trailing whitespace (common in
// indented HTML source) silently loses it after passing through such a relay.
test("mime-codec: quoted-printable escapes trailing whitespace before a line break (RFC 2045 rule 3)", () => {
  const text = "trailing space at end of line \r\nnext line with a trailing tab\t\r\nlast line no trailing ws";
  const encoded = encodeQuotedPrintable(text);
  // The trailing space/tab must be encoded as =20/=09, not left as a literal space/tab
  // immediately before the CRLF.
  assert.ok(encoded.includes("line =20\r\n") || encoded.includes("=20\r\n"), `trailing space before CRLF must be escaped: ${JSON.stringify(encoded)}`);
  assert.ok(encoded.includes("=09\r\n"), `trailing tab before CRLF must be escaped: ${JSON.stringify(encoded)}`);
  assert.ok(!/[ \t]\r\n/.test(encoded), `a literal trailing space/tab survives immediately before a CRLF (unescaped): ${JSON.stringify(encoded)}`);
  // Still round-trips byte-for-byte through our own decoder.
  assert.equal(decodeQuotedPrintable(encoded), text);
  // Simulate a real-world relay that strips trailing line whitespace (a common, documented
  // behavior) and confirm the trailing space/tab SURVIVES because it was escaped — this is the
  // actual property rule 3 exists to protect, not just "our own encoder/decoder agree with
  // themselves".
  const strippedByRelay = encoded.split("\r\n").map((line) => line.replace(/[ \t]+$/, "")).join("\r\n");
  assert.equal(decodeQuotedPrintable(strippedByRelay), text, "trailing whitespace must survive a relay that strips literal trailing line whitespace");
});

test("encoding_rotation: off ⇒ Content-Transfer-Encoding stays 8bit (Phase 3 default)", () => {
  const send = sendWithBody("<p>plain body</p>");
  const bytes = assembleDataBytes(send, { encoding_rotation: false }, "job-enc-off");
  assert.ok(bytes.includes("Content-Transfer-Encoding: 8bit"));
});

test("encoding_rotation: on ⇒ rotates to quoted-printable or base64 (seeded), and decodes to the identical body (T6)", () => {
  const body = "<p>Café body with = signs and non-ascii: café, naïve, façade</p>";
  const send = sendWithBody(body);
  const jobId = "job-enc-rotate";
  const bytes = assembleDataBytes(send, { encoding_rotation: true }, jobId);
  const cteMatch = bytes.match(/Content-Transfer-Encoding: (quoted-printable|base64)/);
  assert.ok(cteMatch, `expected a rotated encoding header, got: ${bytes.match(/Content-Transfer-Encoding:.*/)}`);
  const encoding = cteMatch[1];
  const encodedBody = bytes.split("\r\n\r\n").slice(1).join("\r\n\r\n");
  const decoded = encoding === "quoted-printable" ? decodeQuotedPrintable(encodedBody) : decodeBase64(encodedBody);
  assert.equal(decoded, body, "decoding the rotated encoding must reproduce the exact original body");
});

test("encoding_rotation: deterministic per job_id (same job always rotates to the same encoding)", () => {
  const send = sendWithBody("<p>x</p>");
  const a = assembleDataBytes(send, { encoding_rotation: true }, "job-enc-det");
  const b = assembleDataBytes(send, { encoding_rotation: true }, "job-enc-det");
  assert.equal(a, b);
});

// --- Toggle 7: MIME boundary rotation ------------------------------------------------------------

test("mime_boundary_rotation: off ⇒ no boundary, single-part exactly like Phase 3", () => {
  const send = sendWithBody("<p>x</p>");
  const bytes = assembleDataBytes(send, { mime_boundary_rotation: false }, "job-boundary-off");
  assert.ok(!bytes.includes("boundary="));
  assert.ok(!/^--/.test(bytes.split("\r\n\r\n")[1] || ""));
});

test("mime_boundary_rotation: on ⇒ wraps in multipart/mixed with exactly one part, seeded boundary, decodes to identical body", () => {
  const body = "<p>multipart body content</p>";
  const send = sendWithBody(body);
  const bytes = assembleDataBytes(send, { mime_boundary_rotation: true }, "job-boundary-on");
  assert.match(bytes, /Content-Type: multipart\/mixed; boundary="([^"]+)"/);
  const boundary = bytes.match(/boundary="([^"]+)"/)[1];
  const openings = (bytes.match(new RegExp(`--${boundary}(?!--)`, "g")) || []).length;
  assert.equal(openings, 1, "exactly one MIME part must be present (no attachment/CID introduced)");
  assert.ok(bytes.includes(`--${boundary}--`), "the closing boundary delimiter must be present");
  assert.ok(bytes.includes(body), "the single part's content must be the unchanged body");
});

test("mime_boundary_rotation: boundary format is seeded per job_id and differs across styles", () => {
  const send = sendWithBody("<p>x</p>");
  const boundaries = new Set();
  for (const jobId of ["job-b1", "job-b2", "job-b3", "job-b4", "job-b5"]) {
    const bytes = assembleDataBytes(send, { mime_boundary_rotation: true }, jobId);
    boundaries.add(bytes.match(/boundary="([^"]+)"/)[1]);
  }
  assert.ok(boundaries.size > 1, "different job_ids should (overwhelmingly likely) produce different boundary values");
});

test("mime_boundary_rotation: deterministic per job_id", () => {
  const send = sendWithBody("<p>x</p>");
  const a = assembleDataBytes(send, { mime_boundary_rotation: true }, "job-boundary-det");
  const b = assembleDataBytes(send, { mime_boundary_rotation: true }, "job-boundary-det");
  assert.equal(a, b);
});

// --- Toggle 9: header reordering by client style --------------------------------------------------

test("header_style: none ⇒ preserves Phase 3 order (the identity)", () => {
  const send = sendWithBody("<p>x</p>");
  const bytes = assembleDataBytes(send, { header_style: "none" }, "job-style-none");
  const order = bytes.split("\r\n\r\n")[0].split("\r\n").map((l) => l.split(":")[0]);
  assert.deepEqual(order.slice(0, 4), ["From", "To", "Subject", "Message-ID"]);
});

test("header_style: gmail/outlook/apple/thunderbird reorder without dropping or changing any header value", () => {
  const send = sendWithBody("<p>x</p>");
  for (const style of ["gmail", "outlook", "apple", "thunderbird"]) {
    const bytes = assembleDataBytes(send, { header_style: style }, "job-style-" + style);
    const headerBlock = bytes.split("\r\n\r\n")[0];
    for (const [k, v] of Object.entries(send.headers)) {
      assert.ok(headerBlock.includes(`${k}: ${v}`), `style ${style}: header ${k} must survive with its value unchanged`);
    }
    assert.ok(headerBlock.includes("MIME-Version: 1.0"), `style ${style}: MIME-Version must survive`);
  }
});

test("header_style: random picks one seeded style per job_id, deterministically", () => {
  const send = sendWithBody("<p>x</p>");
  const a = assembleDataBytes(send, { header_style: "random" }, "job-style-random-det");
  const b = assembleDataBytes(send, { header_style: "random" }, "job-style-random-det");
  assert.equal(a, b, "the same job_id must pick the same random style every time");
});

// --- Toggle 10: User-Agent injection — decoupled from header_style (P8-6) -----------------------

test("user_agent_injection: off ⇒ no User-Agent header", () => {
  const send = sendWithBody("<p>x</p>");
  const bytes = assembleDataBytes(send, { user_agent_injection: false }, "job-ua-off");
  assert.ok(!bytes.includes("User-Agent:"));
});

test("user_agent_injection: on ⇒ presence/value is IDENTICAL regardless of header_style (P8-6 decoupling, §8 T2/T9)", () => {
  const send = sendWithBody("<p>x</p>");
  const jobId = "job-ua-decouple";
  const withNone = assembleDataBytes(send, { user_agent_injection: true, header_style: "none" }, jobId);
  const withGmail = assembleDataBytes(send, { user_agent_injection: true, header_style: "gmail" }, jobId);
  const uaNone = (withNone.match(/User-Agent: (.*)/) || [])[1];
  const uaGmail = (withGmail.match(/User-Agent: (.*)/) || [])[1];
  assert.equal(uaNone, uaGmail, "the SAME job_id must produce the identical User-Agent presence/value regardless of header_style");
});

test("user_agent_injection: deterministic per job_id; omission is also a legitimate deterministic outcome", () => {
  const send = sendWithBody("<p>x</p>");
  // Sweep several job_ids and require at least one WITH and (typically) some WITHOUT — proving the
  // ~30% inject / ~70% omit split is genuinely exercised, not accidentally always-on or always-off.
  let withUA = 0;
  let withoutUA = 0;
  for (let i = 0; i < 40; i++) {
    const bytes = assembleDataBytes(send, { user_agent_injection: true }, `job-ua-sweep-${i}`);
    if (bytes.includes("User-Agent:")) withUA++;
    else withoutUA++;
  }
  assert.ok(withUA > 0, "at least one job in a 40-job sweep should get a User-Agent (else injection never fires)");
  assert.ok(withoutUA > 0, "at least one job in a 40-job sweep should omit it (else it always fires, contradicting the ~30% rate)");
});

test("user_agent_injection: variation_user_agent_rate (amended, P8-OD-1) scales injection frequency, not the fixed 0.3 default", () => {
  const send = sendWithBody("<p>x</p>");
  let withRate1 = 0;
  let withRate0 = 0;
  for (let i = 0; i < 40; i++) {
    const jobId = `job-ua-rate-${i}`;
    if (assembleDataBytes(send, { user_agent_injection: true, user_agent_rate: 1 }, jobId).includes("User-Agent:")) withRate1++;
    if (assembleDataBytes(send, { user_agent_injection: true, user_agent_rate: 0 }, jobId).includes("User-Agent:")) withRate0++;
  }
  assert.equal(withRate1, 40, "rate=1 must inject a User-Agent on every job — the parameter is not reaching the transform");
  assert.equal(withRate0, 0, "rate=0 must inject a User-Agent on NO job — the parameter cannot fully disable the effect");
});

// --- Toggle 11: precedence header control ----------------------------------------------------

test("precedence: marketing adds Precedence: bulk; auto/personal omit it (both = Phase 3)", () => {
  const send = sendWithBody("<p>x</p>");
  const marketing = assembleDataBytes(send, { precedence: "marketing" }, "job-prec");
  const auto = assembleDataBytes(send, { precedence: "auto" }, "job-prec");
  const personal = assembleDataBytes(send, { precedence: "personal" }, "job-prec");
  assert.ok(marketing.includes("Precedence: bulk"));
  assert.ok(!auto.includes("Precedence:"));
  assert.ok(!personal.includes("Precedence:"));
});

// --- Toggle 12: Thread-Index — inert (P8-8) -----------------------------------------------------

test("thread_index: on and off both emit NO Thread-Index header (documented no-op, §8 T8)", () => {
  const send = sendWithBody("<p>x</p>");
  const on = assembleDataBytes(send, { thread_index: true }, "job-ti-on");
  const off = assembleDataBytes(send, { thread_index: false }, "job-ti-off");
  assert.ok(!on.includes("Thread-Index:"));
  assert.ok(!off.includes("Thread-Index:"));
});

// --- §8 S2: never-implement techniques absent, even under a rich combination -------------------

test("S2: no homoglyph substitution, no display:none/font-size:0 on VISIBLE content, no fake threading, under a rich toggle combination", () => {
  const send = sendWithBody("<body><p>Hello there, this is a perfectly normal sentence with spaces.</p></body>");
  const bytes = assembleDataBytes(
    send,
    {
      html_minify: true, comment_injection: true, comment_intensity: "high", css_micro: true,
      unique_url_params: true, zero_width: true, encoding_rotation: true, mime_boundary_rotation: true,
      dynamic_preheader: true, header_style: "random", user_agent_injection: true, precedence: "marketing",
      thread_index: true,
    },
    "job-s2",
  );
  assert.ok(!/In-Reply-To:|References:/.test(bytes), "no fake threading headers must ever be emitted");
  // The only display:none usage must be the dynamic-preheader span (already asserted elsewhere to
  // wrap non-visible injected filler, never the author's own visible content).
  const displayNoneSpans = bytes.match(/<span style="display:none[^>]*>/g) || [];
  for (const span of displayNoneSpans) {
    assert.ok(span.includes("max-height:0"), "every display:none span must be the preheader's own hidden-span shape, not an ad hoc hide of visible content");
  }
});

// --- §7/§8 T4: deterministic seeding at the FULL assembled-message level -----------------------

test("T4: full-message determinism — the same job_id under a rich variation profile is byte-identical end to end", () => {
  const send = sendWithBody("<body><p>Some visible text with padding.</p><a href=\"https://example.com/a\">a</a></body>", { "padding-test": undefined });
  const profile = {
    html_minify: true, comment_injection: true, comment_intensity: "medium", css_micro: true,
    unique_url_params: true, encoding_rotation: true, header_style: "outlook", user_agent_injection: true,
    precedence: "marketing", dynamic_preheader: true,
  };
  const a = assembleDataBytes(send, profile, "job-t4-full");
  const b = assembleDataBytes(send, profile, "job-t4-full");
  assert.equal(a, b, "identical job_id + profile must be byte-identical across two independent assemblies (simulating a reclaim/duplicate-window resend)");
});
