package platformapi

// rendered_text_run_test.go — Phase 3 close-out B3, the Domain-A half: POST /v1/run copies the platform's optional
// public.job_items.rendered_text (the plain-text alternative of the message) into send_managed.jobs.rendered_text
// in the SAME statement that copies rendered_body, bounded at 1 MiB; and describe advertises the capability.
//
// Per behaviour, with its paired control:
//   - text present  -> the job row holds it VERBATIM (no trim, no re-wrap); CONTROL: text absent (NULL) -> NULL;
//   - '' and whitespace-only are copied verbatim too (the module decides what they MEAN at claim time, where
//     dispatch.sendText turns them into "no text part": see dispatch/text_part_test.go);
//   - exactly 1 MiB is accepted (202), 1 MiB + 1 byte is refused 400 validation_failed and NO job row is written
//     (the paired control is the at-limit run, so the refusal is the limit and nothing else);
//   - describe.capabilities.plain_text_alternative is true, and the capabilities that were there still are.

import (
	"strings"
	"testing"
)

// seedJobItemText inserts a public.job_items row carrying rendered_text (nil = NULL) and returns its id.
func (e *testEnv) seedJobItemText(t *testing.T, userID, recipient string, text *string) string {
	t.Helper()
	var id string
	if err := e.admin.QueryRow(bgctx(),
		`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, rendered_text, reply_to, status)
		 VALUES ($1, $1, $2, 'Subj', '<p>b</p>', $3, 'reply@ourdomain.com', 'pending') RETURNING id`,
		userID, recipient, text).Scan(&id); err != nil {
		t.Fatalf("seed job_item with rendered_text: %v", err)
	}
	return id
}

func (e *testEnv) jobRenderedText(t *testing.T, jobID string) *string {
	t.Helper()
	var txt *string
	if err := e.admin.QueryRow(bgctx(), `SELECT rendered_text FROM send_managed.jobs WHERE id=$1`, jobID).Scan(&txt); err != nil {
		t.Fatalf("read jobs.rendered_text: %v", err)
	}
	return txt
}

func (e *testEnv) runItem(t *testing.T, itemID, userID string) resp {
	t.Helper()
	return e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": itemID, "user_id": userID})
}

func ptr(s string) *string { return &s }

func TestRun_CopiesRenderedTextVerbatim_AndNullStaysNull(t *testing.T) {
	e := newTestEnv(t, "rtcopy")
	user := e.seedUser(t, "rtcopy@example.com")

	// Each case is a DIFFERENT job_item of the SAME campaign (campaign_id = user id by seedJobItem's default),
	// so the one-active-campaign rule never gets in the way.
	cases := []struct {
		name string
		text *string
	}{
		{"plain text", ptr("Hello,\r\nthis is the plain-text alternative.\r\n")},
		{"leading/trailing whitespace and unicode are kept verbatim", ptr("  café ☃ \n  ")},
		{"empty string", ptr("")},
		{"whitespace only", ptr("   \n\t  ")},
		{"absent (NULL)", nil},
	}
	for i, c := range cases {
		item := e.seedJobItemText(t, user, "rtcopy"+strings.Repeat("x", i)+"@example.com", c.text)
		r := e.runItem(t, item, user).requireStatus(t, 202)
		jobID := r.str("job_id")
		got := e.jobRenderedText(t, jobID)
		switch {
		case c.text == nil && got != nil:
			t.Fatalf("%s: jobs.rendered_text = %q, want NULL (an absent text must stay absent)", c.name, *got)
		case c.text != nil && (got == nil || *got != *c.text):
			t.Fatalf("%s: jobs.rendered_text = %v, want %q verbatim", c.name, got, *c.text)
		}
	}
}

func TestRun_RenderedTextIsBoundedAt1MiB(t *testing.T) {
	e := newTestEnv(t, "rtlimit")
	user := e.seedUser(t, "rtlimit@example.com")

	const limit = 1 << 20
	// CONTROL first: exactly at the limit is accepted and stored whole.
	atLimit := strings.Repeat("x", limit)
	okItem := e.seedJobItemText(t, user, "atlimit@example.com", &atLimit)
	ok := e.runItem(t, okItem, user).requireStatus(t, 202)
	if got := e.jobRenderedText(t, ok.str("job_id")); got == nil || len(*got) != limit {
		t.Fatalf("control: the at-limit text was not stored whole")
	}

	// One byte more is refused as a clean 400 and writes NOTHING (the reason is the limit, not the data).
	over := strings.Repeat("x", limit+1)
	badItem := e.seedJobItemText(t, user, "overlimit@example.com", &over)
	r := e.runItem(t, badItem, user)
	if r.status != 400 || r.errorCode() != "validation_failed" {
		t.Fatalf("over-limit text: status=%d code=%q, want 400 validation_failed; body=%s", r.status, r.errorCode(), r.raw)
	}
	var n int
	if err := e.admin.QueryRow(bgctx(), `SELECT count(*) FROM send_managed.jobs WHERE job_item_id=$1`, badItem).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if n != 0 {
		t.Fatalf("the refused run left %d job row(s) for the item, want 0", n)
	}
	// ...and the same item is runnable again once its text is fixed (the refusal was not sticky).
	if _, err := e.admin.Exec(bgctx(), `UPDATE public.job_items SET rendered_text='short' WHERE id=$1`, badItem); err != nil {
		t.Fatalf("fix text: %v", err)
	}
	e.runItem(t, badItem, user).requireStatus(t, 202)
}

func TestDescribe_AdvertisesThePlainTextAlternative(t *testing.T) {
	e := newTestEnv(t, "rtdescribe")
	r := e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 200)
	caps, _ := r.body["capabilities"].(map[string]any)
	if caps["plain_text_alternative"] != true {
		t.Fatalf("capabilities.plain_text_alternative = %v, want true; capabilities=%v", caps["plain_text_alternative"], caps)
	}
	// the capabilities that were there are unchanged (additive-only)
	for key, want := range map[string]any{
		"single_send": true, "sequences": false, "reply_to_per_campaign": true,
		"open_tracking": false, "click_tracking": false, "content_variation": true,
	} {
		if caps[key] != want {
			t.Fatalf("capabilities.%s = %v, want %v (an existing capability changed)", key, caps[key], want)
		}
	}
}
