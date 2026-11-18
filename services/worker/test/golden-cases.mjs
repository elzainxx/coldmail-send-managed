// golden-cases.mjs — the fixed inputs of the "text absent is byte-identical to today" golden test.
//
// WHAT THIS PINS. Before the optional plain-text part (`send.text`) existed, the worker rendered every
// message from `push_job.send` (headers + one HTML body) plus the variation block. The bytes below were
// captured by running THAT renderer (assemble.js as it was at send-managed bffd424, before the text
// part was written) over these inputs and storing the result under testdata/golden/*.eml. The golden
// test (plaintext.test.mjs) re-renders each case with the CURRENT renderer, for `text` absent, `null`
// and omitted, and requires the bytes to be equal to the stored file. A change to the renderer that
// alters the text-absent output of ANY case (including every variation profile) fails that test.
//
// Every value is fixed: the job id seeds every variation stage, the Date and Message-ID are inputs
// (they come from the Control), nothing is random and nothing reads a clock. Addresses are
// documentation addresses (example.com / example.org / example.net).

export const FIXED_JOB_ID = "6f0c1e52-3b7a-4d19-9a4e-2c8f5b1d7e30";

// A body with non-ASCII text (8bit pass-through), many closing tags (the comment-injection draw is
// per tag, so a body with few tags would make that stage a coin flip, see phase8_variation.sh), a link
// (unique URL params), a <style>-free inline px (css micro) and a <body> tag (dynamic preheader).
export const GOLDEN_HTML =
  "<html><head><title>Golden</title></head><body>" +
  '<p style="margin:10px 4px;padding:8px">Hello, café ☃ — this is the golden message.</p>' +
  '<a href="https://example.com/landing?x=1">landing page</a>' +
  "<div><span>a</span><span>b</span><span>c</span><span>d</span></div>" +
  "<div><span>e</span><span>f</span><span>g</span><span>h</span></div>" +
  "<ul><li>one</li><li>two</li><li>three</li><li>four</li></ul>\n" +
  "<p>The next line starts with a dot (SMTP dot-stuffing).</p>\n" +
  ".this line starts with a dot\n" +
  "</body></html>";

export function goldenSend(jobId = FIXED_JOB_ID) {
  return {
    recipient: "rcpt@example.com",
    from_address: "sender@example.org",
    from_name: "Golden Sender",
    reply_to: "reply@example.net",
    subject: "Golden message",
    body: GOLDEN_HTML,
    headers: {
      From: '"Golden Sender" <sender@example.org>',
      To: "rcpt@example.com",
      Subject: "Golden message",
      "Reply-To": "reply@example.net",
      "Message-ID": `<${jobId}@example.org>`,
      Date: "Wed, 18 Nov 2026 10:00:00 +0000",
    },
  };
}

// Each case: name (the file stem under testdata/golden), jobId, variation (the push_job.variation
// block, or undefined when the block is absent).
export const GOLDEN_CASES = [
  { name: "neutral-block-absent", jobId: FIXED_JOB_ID, variation: undefined },
  { name: "neutral-block-empty", jobId: FIXED_JOB_ID, variation: {} },
  {
    name: "neutral-block-explicit-defaults",
    jobId: FIXED_JOB_ID,
    variation: {
      html_minify: false, comment_injection: false, comment_intensity: "low", css_micro: false,
      unique_url_params: false, zero_width: false, encoding_rotation: false, mime_boundary_rotation: false,
      dynamic_preheader: false, header_style: "none", user_agent_injection: false, precedence: "auto", thread_index: false,
    },
  },
  { name: "encoding-rotation-1", jobId: "golden-job-enc-1", variation: { encoding_rotation: true } },
  { name: "encoding-rotation-2", jobId: "golden-job-enc-2", variation: { encoding_rotation: true } },
  { name: "encoding-rotation-3", jobId: "golden-job-enc-3", variation: { encoding_rotation: true } },
  { name: "boundary-rotation-1", jobId: "golden-job-bnd-1", variation: { mime_boundary_rotation: true } },
  { name: "boundary-rotation-2", jobId: "golden-job-bnd-2", variation: { mime_boundary_rotation: true } },
  { name: "boundary-rotation-3", jobId: "golden-job-bnd-3", variation: { mime_boundary_rotation: true } },
  { name: "boundary-rotation-4", jobId: "golden-job-bnd-4", variation: { mime_boundary_rotation: true } },
  { name: "encoding-and-boundary-1", jobId: "golden-job-both-1", variation: { encoding_rotation: true, mime_boundary_rotation: true } },
  { name: "encoding-and-boundary-2", jobId: "golden-job-both-2", variation: { encoding_rotation: true, mime_boundary_rotation: true } },
  {
    name: "content-stages",
    jobId: "golden-job-content",
    variation: {
      html_minify: true, comment_injection: true, comment_intensity: "high", css_micro: true,
      unique_url_params: true, zero_width: true, dynamic_preheader: true,
    },
  },
  {
    name: "header-stages",
    jobId: "golden-job-headers",
    variation: { header_style: "random", user_agent_injection: true, user_agent_rate: 1, precedence: "marketing" },
  },
  { name: "header-style-gmail", jobId: "golden-job-gmail", variation: { header_style: "gmail" } },
  { name: "header-style-outlook", jobId: "golden-job-outlook", variation: { header_style: "outlook" } },
  { name: "header-style-apple", jobId: "golden-job-apple", variation: { header_style: "apple", encoding_rotation: true } },
  { name: "header-style-thunderbird", jobId: "golden-job-tbird", variation: { header_style: "thunderbird", mime_boundary_rotation: true } },
  {
    name: "kitchen-sink",
    jobId: "golden-job-sink",
    variation: {
      html_minify: true, comment_injection: true, comment_intensity: "medium", css_micro: true,
      unique_url_params: true, zero_width: true, encoding_rotation: true, mime_boundary_rotation: true,
      dynamic_preheader: true, header_style: "random", user_agent_injection: true, user_agent_rate: 1,
      precedence: "marketing", thread_index: true,
    },
  },
];
