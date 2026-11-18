package dispatch

// text_part_test.go — Phase 3 close-out B3, the Control half of the optional plain-text part: what a job's
// rendered_text becomes on the worker wire.
//
//   - sendText (pure): NULL, "" and whitespace-only are ONE meaning, "no text part" (""), anything else is returned
//     VERBATIM — never trimmed, never re-wrapped, whitespace at the ends included;
//   - both claim queries (the single claim and the per-user claim) RETURN rendered_text, so it reaches the Control;
//   - the push_job frame carries `send.text` for a real text and OMITS the key for absent/empty/whitespace-only, so
//     a message without a text part is the same frame an older Control sent (the worker then renders today's bytes).
//
// The paired control of every "absent" case is the "present" case in the same test (and the reverse): a Control that
// always sent the text, or never sent it, fails one of the two.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

func strptr(s string) *string { return &s }

func TestSendText_NullEmptyAndBlankAreOneMeaning_EverythingElseIsVerbatim(t *testing.T) {
	for name, tc := range map[string]struct {
		in   *string
		want string
	}{
		"NULL":                        {nil, ""},
		"empty string":                {strptr(""), ""},
		"spaces":                      {strptr("   "), ""},
		"newlines and tabs":           {strptr("\r\n\t \n"), ""},
		"no-break and em spaces":      {strptr("\u00a0\u2003"), ""},
		"a letter":                    {strptr("x"), "x"},
		"surrounding whitespace kept": {strptr("  hello \n"), "  hello \n"},
		"multi-line text kept":        {strptr("line one\r\nline two\n\nline four"), "line one\r\nline two\n\nline four"},
		"unicode kept":                {strptr("café ☃"), "café ☃"},
	} {
		if got := sendText(tc.in); got != tc.want {
			t.Errorf("%s: sendText = %q, want %q", name, got, tc.want)
		}
	}
}

func (e *dispatchEnv) setRenderedText(t *testing.T, jobID string, text *string) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(), `UPDATE send_managed.jobs SET rendered_text=$2 WHERE id=$1`, jobID, text); err != nil {
		t.Fatalf("set rendered_text: %v", err)
	}
}

// Both claim queries must RETURN rendered_text: a claim that did not would hand the dispatcher a job without its
// text and every text part would silently vanish.
func TestTextPart_BothClaimQueriesReturnRenderedText(t *testing.T) {
	e := newDispatchEnv(t, "textclaim")
	ctx := context.Background()

	withText := e.seedQueuedJob(t, "claim-text@example.com")
	e.setRenderedText(t, withText, strptr("the plain-text alternative"))
	j, ok, err := e.d.claimOne(ctx, e.px.workerID, 120)
	if err != nil || !ok {
		t.Fatalf("claimOne: ok=%v err=%v", ok, err)
	}
	if j.id != withText || j.text == nil || *j.text != "the plain-text alternative" {
		t.Fatalf("claimOne returned job %s text=%v, want job %s with its rendered_text", j.id, j.text, withText)
	}

	user := e.seedUser(t, "claim-text-tenant@example.com")
	userJob := e.seedQueuedJobFor(t, user, "claim-text-2@example.com")
	e.setRenderedText(t, userJob, strptr("per-user claim text"))
	noText := e.seedQueuedJobFor(t, user, "claim-text-3@example.com")
	got, err := e.d.claimForUser(ctx, user, 10, 120)
	if err != nil || len(got) != 2 {
		t.Fatalf("claimForUser: %d jobs err=%v, want 2", len(got), err)
	}
	for _, g := range got {
		switch g.id {
		case userJob:
			if g.text == nil || *g.text != "per-user claim text" {
				t.Fatalf("claimForUser dropped rendered_text for %s: %v", g.id, g.text)
			}
		case noText:
			if g.text != nil {
				t.Fatalf("claimForUser invented a text for the job that has none: %q", *g.text)
			}
		default:
			t.Fatalf("claimForUser returned an unexpected job %s", g.id)
		}
	}
}

func TestTextPart_ReachesThePushJobOnlyWhenThereIsOne(t *testing.T) {
	e := newDispatchEnv(t, "textpush")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "secret-for-text-test")

	cases := []struct {
		name     string
		text     *string
		wantText string
		wantKey  bool // does the serialized send object carry a "text" key
	}{
		{"a text part", strptr("Hi there,\r\nplain text.\r\n"), "Hi there,\r\nplain text.\r\n", true},
		{"a text part with surrounding whitespace is verbatim", strptr("  padded \n"), "  padded \n", true},
		{"NULL", nil, "", false},
		{"empty string", strptr(""), "", false},
		{"whitespace only", strptr(" \r\n\t "), "", false},
	}
	for i, c := range cases {
		job := e.seedQueuedJob(t, fmt.Sprintf("textpush%d@example.com", i))
		e.setRenderedText(t, job, c.text)
		pushed, err := e.d.TryDispatch(ctx)
		if err != nil || pushed != job {
			t.Fatalf("%s: TryDispatch pushed %q err=%v, want %s", c.name, pushed, err, job)
		}
		pj, ok := e.px.lastPush()
		if !ok {
			t.Fatalf("%s: no push recorded", c.name)
		}
		if pj.Send.Text != c.wantText {
			t.Fatalf("%s: push_job.send.text = %q, want %q", c.name, pj.Send.Text, c.wantText)
		}
		raw, err := json.Marshal(pj.Send)
		if err != nil {
			t.Fatalf("%s: marshal send: %v", c.name, err)
		}
		if has := strings.Contains(string(raw), `"text":`); has != c.wantKey {
			t.Fatalf("%s: the serialized send object has a \"text\" key = %v, want %v: %s", c.name, has, c.wantKey, raw)
		}
		// The rest of the frame is untouched by the text part: body and headers are exactly as before.
		if pj.Send.Body != "<p>b</p>" || pj.Send.Headers["To"] != fmt.Sprintf("textpush%d@example.com", i) {
			t.Fatalf("%s: the HTML body or headers changed: body=%q headers=%v", c.name, pj.Send.Body, pj.Send.Headers)
		}
		// finish the job so the next case can be dispatched (one job in flight per worker)
		e.oh.Handle(e.px.workerID, workerproto.Report{Type: workerproto.TypeReport, JobID: job, Outcome: workerproto.OutcomeAccepted,
			ProviderMessageID: fmt.Sprintf("PM-TEXT-%d", i), SMTPResponse: "250 2.0.0 Ok"})
		e.px.freeAllWorkers()
	}
}
