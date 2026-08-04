// mime-codec.js — minimal, dependency-free quoted-printable and base64 codecs for the worker's
// Content-Transfer-Encoding rotation (toggle 6, §5). The worker ships no SMTP/MIME library (CLAUDE.md
// Rule 6 — no npm dependencies), so these are hand-rolled rather than pulled from a package; they
// exist ONLY to round-trip a body through an alternate wire encoding, never to touch anything
// credential-related.
//
// Both encoders are pure functions of the input string — no seed needed here; WHICH encoding a
// given job uses is the seeded decision (variation-headers.js), not the encoding itself.

const QP_LINE_LIMIT = 75; // RFC 2045 §6.7: encoded lines must not exceed 76 chars; soft-break at 75.

// escapeTrailingWhitespace applies RFC 2045 §6.7 rule 3: trailing space/tab at the end of an
// encoded LINE (i.e. right before a real, hard line break — not one of our own inserted soft
// `=\r\n` breaks, which are invisible once decoded) must be represented as `=20`/`=09`, because
// "many implementations discard trailing white space" on a line. Without this, a relay/MTA that
// trims trailing line whitespace (common) silently drops it from the decoded body — a real
// content-corruption risk for the encoding-rotation toggle (R3 review finding). Escapes the WHOLE
// trailing run, not just its last character, since a typical stripping regex removes the entire
// run.
function escapeTrailingWhitespace(out) {
  return out.replace(/[ \t]+$/, (ws) => ws.replace(/ /g, "=20").replace(/\t/g, "=09"));
}

// encodeQuotedPrintable encodes UTF-8 bytes per RFC 2045: printable ASCII (0x20-0x7e, excluding
// `=`) passes through; everything else becomes `=XX` (uppercase hex); CRLF is preserved as a real
// line break (not escaped, but its trailing whitespace IS — rule 3 above); long lines get a soft
// `=\r\n` break so no line exceeds the limit.
export function encodeQuotedPrintable(str) {
  const bytes = Buffer.from(str, "utf8");
  let out = "";
  let lineLen = 0;
  const emit = (s) => {
    if (lineLen + s.length > QP_LINE_LIMIT) {
      out += "=\r\n";
      lineLen = 0;
    }
    out += s;
    lineLen += s.length;
  };
  for (let i = 0; i < bytes.length; i++) {
    const b = bytes[i];
    if (b === 0x0d && bytes[i + 1] === 0x0a) {
      out = escapeTrailingWhitespace(out);
      out += "\r\n";
      lineLen = 0;
      i++;
      continue;
    }
    if (b === 0x3d || b < 0x20 || b > 0x7e) {
      emit("=" + b.toString(16).toUpperCase().padStart(2, "0"));
    } else {
      emit(String.fromCharCode(b));
    }
  }
  // The final line (no trailing CRLF) is still subject to the same rule — its trailing whitespace
  // sits right before the SMTP DATA terminator, which is equally at risk of being trimmed.
  return escapeTrailingWhitespace(out);
}

// decodeQuotedPrintable inverts encodeQuotedPrintable: drop soft line breaks, resolve `=XX` escapes
// back to raw bytes, then decode the byte sequence as UTF-8.
export function decodeQuotedPrintable(str) {
  const resolved = str.replace(/=\r\n/g, "").replace(/=([0-9A-Fa-f]{2})/g, (_, hex) => String.fromCharCode(parseInt(hex, 16)));
  return Buffer.from(resolved, "binary").toString("utf8");
}

const B64_LINE_LIMIT = 76;

// encodeBase64 base64-encodes the UTF-8 bytes of str and wraps at the standard 76-char line width.
export function encodeBase64(str) {
  const b64 = Buffer.from(str, "utf8").toString("base64");
  const lines = [];
  for (let i = 0; i < b64.length; i += B64_LINE_LIMIT) lines.push(b64.slice(i, i + B64_LINE_LIMIT));
  return lines.join("\r\n");
}

// decodeBase64 inverts encodeBase64.
export function decodeBase64(str) {
  return Buffer.from(str.replace(/\r\n/g, ""), "base64").toString("utf8");
}

// encodeForTransfer/decodeForTransfer dispatch on the Content-Transfer-Encoding value. "8bit" (the
// Phase 3 default) and any unrecognized value pass through UNCHANGED — this is what keeps the §4
// identity property intact: encoding_rotation's neutral default never reaches these two branches.
export function encodeForTransfer(body, transferEncoding) {
  switch (transferEncoding) {
    case "quoted-printable":
      return encodeQuotedPrintable(body);
    case "base64":
      return encodeBase64(body);
    default:
      return body;
  }
}

export function decodeForTransfer(encoded, transferEncoding) {
  switch (transferEncoding) {
    case "quoted-printable":
      return decodeQuotedPrintable(encoded);
    case "base64":
      return decodeBase64(encoded);
    default:
      return encoded;
  }
}
