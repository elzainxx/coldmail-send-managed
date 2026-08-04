package dispatch

import "testing"

// headers_test.go — Phase 8 CHUNK 4 (P8-5): the deterministic Message-ID claim.
//
// §7.3 adopts a job_id-derived Message-ID (a compatible refinement of Phase 3 §8.1's
// "<uuid@sending-domain>" — only the uuid SOURCE changes, from random to job_id-derived) so a
// duplicate from the accepted crash-after-250 window shares one Message-ID across both copies,
// and the provider_message_id UNIQUE backstop becomes effective on the no-provider-id fallback
// path. This was already implemented (messageIDFor, headers.go) ahead of this chunk landing; this
// test is Chunk 4's obligation to actually PROVE the claim rather than let it stand asserted only
// in a doc comment.
func TestMessageIDFor_DeterministicFromJobID(t *testing.T) {
	a := messageIDFor("job-abc-123", "sender@ourdomain.com")
	b := messageIDFor("job-abc-123", "sender@ourdomain.com")
	if a != b {
		t.Fatalf("messageIDFor is not deterministic: %q != %q for the same (jobID, fromAddress)", a, b)
	}
	if a == "" || a[0] != '<' || a[len(a)-1] != '>' {
		t.Fatalf("messageIDFor produced a non-angle-bracketed id: %q", a)
	}
}

// TestMessageIDFor_DistinctJobsGetDistinctIDs — the other half of the claim (§7.3: "distinct
// job_ids still yield a distinct, unique Message-ID"). Without this, a determinism fix that
// collapsed every job to the SAME id would trivially "pass" the test above.
func TestMessageIDFor_DistinctJobsGetDistinctIDs(t *testing.T) {
	a := messageIDFor("job-one", "sender@ourdomain.com")
	b := messageIDFor("job-two", "sender@ourdomain.com")
	if a == b {
		t.Fatalf("two distinct job ids produced the SAME Message-ID: %q", a)
	}
}

// TestMessageIDFor_SendingDomainFromFromAddress — the domain half of the format
// ("<job_id@sending-domain>"), asserted directly rather than only implied by other tests.
func TestMessageIDFor_SendingDomainFromFromAddress(t *testing.T) {
	got := messageIDFor("job-xyz", "alice@example.org")
	want := "<job-xyz@example.org>"
	if got != want {
		t.Fatalf("messageIDFor = %q, want %q", got, want)
	}
}
