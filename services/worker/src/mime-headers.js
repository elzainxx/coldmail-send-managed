// mime-headers.js — small utilities over the pipeline's ordered-header representation
// (`[[name, value], ...]`, case-insensitive lookup by name). Shared by assemble.js (which owns the
// representation and needs to peel Content-Type/Content-Transfer-Encoding out for the multipart
// wrapper) and variation-headers.js (whose encoding/precedence/UA stages need to read or replace a
// single header without disturbing the rest). Kept in its own module — no import cycle between
// assemble.js and variation-headers.js either direction.

export function getHeader(headers, name) {
  const lower = name.toLowerCase();
  const found = headers.find(([k]) => k.toLowerCase() === lower);
  return found ? found[1] : undefined;
}

// setHeader replaces the value of an existing header (preserving its position) or appends a new
// one at the end. Returns a NEW array; never mutates the input.
export function setHeader(headers, name, value) {
  const lower = name.toLowerCase();
  const idx = headers.findIndex(([k]) => k.toLowerCase() === lower);
  if (idx === -1) return [...headers, [name, value]];
  const out = headers.slice();
  out[idx] = [name, value];
  return out;
}

// removeHeader drops every header matching `name` (case-insensitive). Returns a NEW array.
export function removeHeader(headers, name) {
  const lower = name.toLowerCase();
  return headers.filter(([k]) => k.toLowerCase() !== lower);
}
