package platformapi

// describe_vocabularies_test.go — fix-plan items 6 and 8. Real Postgres, real HTTP against the
// actual production Server (CLAUDE.md §3 — real behavioral tests only).
//
// WHAT MAKES THESE TESTS RATHER THAN SHAPE CHECKS. Asserting `vocabularies` is non-empty, or that it
// contains the four keys this file happens to know about, would be exactly the vacuity §10 forbids: it
// would pass just as happily against a hardcoded list, which is the implementation item 8 exists to
// REMOVE. So every test here compares the PUBLISHED declaration against an INDEPENDENT source of truth
// — the live catalog, the codeStatus map, the production emission sites, the JSON tags in the
// production files — and each has a paired control that names the divergence it catches.
//
// The strongest of them is TestDescribeVocabularies_LiveEnumValueIsPublishedWithoutACodeChange: it
// runs a real ALTER TYPE ... ADD VALUE against the clone and asserts the new label appears in the very
// next describe response. A hardcoded list cannot pass that test, and no amount of list-maintenance
// discipline can make it pass. That is the property item 8 is actually about.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// --- item 8: vocabularies{} ---

// TestDescribeVocabularies_LiveEnumValueIsPublishedWithoutACodeChange is THE test for item 8's central
// claim. It adds a real value to a real enum type in the clone and asserts describe publishes it
// immediately, with no rebuild and no code edit.
//
// This is the failure mode the audit named: a module adds an enum value, every client's hardcoded list
// is now silently incomplete, and additive-only versioning did not help because the addition was never
// published. A declaration that is read from the schema per request cannot have that bug. A hardcoded
// one cannot pass this test — which is precisely why the test is written this way rather than as an
// assertion about the six job_state values that exist today.
//
// job_state is the subject because it is the vocabulary a client is MOST likely to have hardcoded (it
// is the only enum both report modes put in a response body), and the added label is inert: nothing
// reads or writes 'zzz_test_probe', no row can hold it, and the clone is dropped at cleanup, so the
// mutation cannot leak into another test or the template.
func TestDescribeVocabularies_LiveEnumValueIsPublishedWithoutACodeChange(t *testing.T) {
	e := newTestEnv(t, "descvocablive")

	before := e.describeVocabulary(t, "job_state")
	for _, v := range before {
		if v == "zzz_test_probe" {
			t.Fatalf("probe label already present before the ALTER — this test would be vacuous: %v", before)
		}
	}

	// ALTER TYPE ... ADD VALUE, committed. Postgres 16 permits it inside a transaction (verified), but
	// it must be COMMITTED for the server's own pooled connections to observe it.
	if _, err := e.admin.Exec(context.Background(),
		`ALTER TYPE send_managed.job_state ADD VALUE 'zzz_test_probe'`); err != nil {
		t.Fatalf("ALTER TYPE ADD VALUE: %v", err)
	}

	after := e.describeVocabulary(t, "job_state")
	if len(after) != len(before)+1 {
		t.Fatalf("describe published %d job_state values after the ALTER, want %d (before=%v after=%v) — "+
			"the vocabulary is not being read from the schema",
			len(after), len(before)+1, before, after)
	}
	found := false
	for _, v := range after {
		if v == "zzz_test_probe" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a value added to send_managed.job_state is MISSING from describe.vocabularies "+
			"(got %v) — the declaration is not derived from the live schema, so every client's hardcoded "+
			"list is silently incomplete the moment an enum grows", after)
	}
}

// TestDescribeVocabularies_EveryPublishedValueSetMatchesTheLiveType — the direct control on the
// db_enum half: for every declared type, the published values must equal the live pg_enum labels
// exactly, in order. Catches a stale, reordered, or partially-typed list.
func TestDescribeVocabularies_EveryPublishedValueSetMatchesTheLiveType(t *testing.T) {
	e := newTestEnv(t, "descvocabmatch")
	vocabs := e.describeVocabularies(t)

	live := e.liveEnumLabels(t)
	checked := 0
	for _, de := range exposedDBEnums {
		entry, ok := vocabs[de.Type].(map[string]any)
		if !ok {
			t.Fatalf("describe.vocabularies is missing declared enum %q", de.Type)
		}
		if src, _ := entry["source"].(string); src != "db_enum" {
			t.Fatalf("%s: source=%q, want db_enum", de.Type, src)
		}
		want := live[de.Type]
		if len(want) == 0 {
			t.Fatalf("%s is declared in exposedDBEnums but does not exist in the send_managed schema", de.Type)
		}
		got := stringSliceOf(t, entry["values"])
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s published %v, live type has %v (order is enumsortorder, and it is part of the "+
				"contract — a client rendering a legend sees it)", de.Type, got, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no db_enum vocabularies checked — this control would be vacuous")
	}
}

// TestDescribeVocabularies_EverySchemaEnumIsAccountedFor — the coverage control, and the one that keeps
// the hand-maintained half honest. Every enum type in the send_managed schema must appear EITHER in
// exposedDBEnums (published) OR in notExposedDBEnums (with a written reason). A new enum type that is
// user-facing and simply forgotten fails here rather than shipping undeclared — otherwise "absent from
// exposedDBEnums" would be indistinguishable from "nobody looked", and item 8's whole point would be
// defeated one level up in the mechanism meant to enforce it.
//
// This control carries more weight in THIS module than in module 3: five of seven enums are excluded
// here, so the exclusion list is the bulk of the accounting rather than a footnote on it.
func TestDescribeVocabularies_EverySchemaEnumIsAccountedFor(t *testing.T) {
	e := newTestEnv(t, "descvocabcover")
	live := e.liveEnumLabels(t)
	if len(live) == 0 {
		t.Fatal("no send_managed enum types found — this control would be vacuous")
	}

	exposed := map[string]bool{}
	for _, de := range exposedDBEnums {
		exposed[de.Type] = true
	}
	for typname := range live {
		_, isExcluded := notExposedDBEnums[typname]
		if !exposed[typname] && !isExcluded {
			t.Fatalf("send_managed.%s exists in the schema but is in NEITHER exposedDBEnums nor "+
				"notExposedDBEnums. If a Domain-A response can carry it, declare it; if not, add it to "+
				"notExposedDBEnums with the reason. Silence is the one option that is not allowed — it is "+
				"how an undeclared user-facing vocabulary ships.", typname)
		}
		if exposed[typname] && isExcluded {
			t.Fatalf("send_managed.%s is in BOTH exposedDBEnums and notExposedDBEnums — the accounting "+
				"must be unambiguous", typname)
		}
	}
	// The exclusion list must not accumulate names that no longer exist: a stale entry would silently
	// re-open the coverage hole for a type later re-added under the same name.
	for typname := range notExposedDBEnums {
		if len(live[typname]) == 0 {
			t.Fatalf("notExposedDBEnums names send_managed.%s, which does not exist in the schema — a stale "+
				"exclusion silently exempts a future type of the same name", typname)
		}
	}
	// Every exclusion must carry a real REASON, not an empty string: the reason is the whole reason this
	// list is allowed to exist instead of the type being published.
	for typname, reason := range notExposedDBEnums {
		if len(strings.TrimSpace(reason)) < 20 {
			t.Fatalf("notExposedDBEnums[%q] has no substantive reason (%q) — an unexplained exclusion is "+
				"indistinguishable from an oversight, which is what this list exists to rule out", typname, reason)
		}
	}
	// And an excluded type must genuinely be absent from the published set.
	vocabs := e.describeVocabularies(t)
	for typname := range notExposedDBEnums {
		if _, present := vocabs[typname]; present {
			t.Fatalf("send_managed.%s is declared NOT exposed but appears in describe.vocabularies", typname)
		}
	}
}

// TestDescribeVocabularies_TierIdsAreExactlyTheLivePoolKinds — the control that earns pool_kind its
// `source: "db_enum"` label. The tier ids reach the wire as Go literals in describeTiers, and
// defaultQuoteTier is a bare string, so nothing in the production code TIED either to the enum before
// this test existed: a third pool kind could be added to the schema and quote/describe would keep
// reporting two tiers while publishing three values.
//
// Both directions are pinned. Every tier id (and defaultQuoteTier) must be a live pool_kind label, so
// describe cannot advertise a tier the schema has no kind for; and every live label must have a tier,
// so a new kind cannot be added to the type without the tier list following. The second direction is
// the one that would have caught the drift.
func TestDescribeVocabularies_TierIdsAreExactlyTheLivePoolKinds(t *testing.T) {
	e := newTestEnv(t, "descvocabtiers")
	desc := e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 200)
	pricing, ok := desc.body["pricing"].(map[string]any)
	if !ok {
		t.Fatalf("describe missing pricing: %s", desc.raw)
	}
	tiers, ok := pricing["tiers"].([]any)
	if !ok || len(tiers) == 0 {
		t.Fatalf("describe pricing.tiers absent or empty: %s", desc.raw)
	}
	published := e.describeVocabulary(t, "pool_kind")
	live := e.liveEnumLabels(t)["pool_kind"]
	if len(live) == 0 {
		t.Fatal("send_managed.pool_kind has no labels — vacuous")
	}
	if strings.Join(published, ",") != strings.Join(live, ",") {
		t.Fatalf("published pool_kind %v != live %v", published, live)
	}

	liveSet := map[string]bool{}
	for _, v := range live {
		liveSet[v] = true
	}
	tierIDs := map[string]bool{}
	for _, raw := range tiers {
		tm, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tier entry is not an object: %#v", raw)
		}
		id, _ := tm["id"].(string)
		if id == "" {
			t.Fatalf("tier entry has no id: %#v", tm)
		}
		if !liveSet[id] {
			t.Fatalf("describe advertises tier id %q, which is NOT a live send_managed.pool_kind label "+
				"(%v) — the tier axis IS the pool-kind axis, so a tier the schema cannot express routes "+
				"nowhere", id, live)
		}
		tierIDs[id] = true
	}
	// defaultQuoteTier is what quote REPORTS, and it is a bare Go string with no compile-time tie to
	// the enum — so it gets the same check as the describe tiers.
	if !liveSet[defaultQuoteTier] {
		t.Fatalf("defaultQuoteTier=%q is not a live pool_kind label (%v) — quote would report a tier "+
			"the schema has no kind for", defaultQuoteTier, live)
	}
	// The direction that catches drift: a pool kind with no tier.
	for _, v := range live {
		if !tierIDs[v] {
			t.Fatalf("send_managed.pool_kind has label %q but describe publishes no tier for it — the "+
				"published vocabulary and the published tier list disagree about what this module offers, "+
				"and a client reading tiers[] would never learn %q exists", v, v)
		}
	}
}

// TestDescribeVocabularies_StaticSetsMatchTheirSourceOfTruth — the control on the module_constant half.
// Each static vocabulary is checked against something OTHER than a copy of itself:
//
//	detail_issue   -> every issue slug the production emission sites on THIS surface can attach,
//	                  grepped by CONSTANT NAME out of internal/platformapi's non-test source, both
//	                  directions. A fifth slug used by a new validation branch fails this test, and a
//	                  declared slug no branch emits fails it too.
//	pricing_model  -> the pricingModels slice, whose membership relationship with the reported model is
//	                  pinned by TestDescribePricing_ModelIsAMemberOfTheDeclaredSet.
//
// The grep is scoped to internal/platformapi DELIBERATELY, not to all of services/control: adminapi has
// its own six-slug vocabulary (it adds conflict and forbidden_value for config PUTs), and a
// services/control-wide walk would demand this surface declare slugs it cannot emit. What describe
// publishes is what THIS surface can produce.
func TestDescribeVocabularies_StaticSetsMatchTheirSourceOfTruth(t *testing.T) {
	e := newTestEnv(t, "descvocabstatic")
	vocabs := e.describeVocabularies(t)

	for _, sv := range staticVocabularies {
		entry, ok := vocabs[sv.Name].(map[string]any)
		if !ok {
			t.Fatalf("describe.vocabularies is missing static vocabulary %q", sv.Name)
		}
		if src, _ := entry["source"].(string); src != "module_constant" {
			t.Fatalf("%s: source=%q, want module_constant", sv.Name, src)
		}
		got := stringSliceOf(t, entry["values"])
		if strings.Join(got, ",") != strings.Join(sv.Values, ",") {
			t.Fatalf("%s published %v, constants say %v", sv.Name, got, sv.Values)
		}
		if len(got) == 0 {
			t.Fatalf("%s published an EMPTY value set — a published empty vocabulary is worse than an "+
				"absent one: a client reads it as 'this field has no values'", sv.Name)
		}
	}

	// detail_issue vs the production emission sites, by constant NAME. Comparing VALUES would pass even
	// if the declaration referenced a different constant that happened to hold the same string.
	emitted := map[string]bool{}
	pat := regexp.MustCompile(`Issue:\s+(issue[A-Za-z]+)\b`)
	for _, m := range grepPlatformAPISubmatches(t, pat) {
		emitted[m] = true
	}
	if len(emitted) == 0 {
		t.Fatal("found no `Issue: issueX` emission sites in internal/platformapi's production source — " +
			"the grep is broken and this control is vacuous")
	}
	declared := map[string]bool{}
	for _, c := range constNamesFor(t, "detail_issue", "issue") {
		declared[c] = true
	}
	if len(declared) == 0 {
		t.Fatal("read no issue* constants out of detail_issue's declaration — vacuous")
	}
	for name := range emitted {
		if !declared[name] {
			t.Fatalf("this surface emits %s but staticVocabularies' detail_issue does not declare it — a "+
				"client switching on details[].issue would meet an undeclared value", name)
		}
	}
	// Reverse direction: a declared slug no emission site can produce is dead handling a client writes
	// and never exercises. (Unlike pricing_model's deliberately forward-declared `free`, these slugs
	// describe THIS surface's own behaviour, so an unreachable one is a stale leftover.)
	for name := range declared {
		if !emitted[name] {
			t.Fatalf("detail_issue declares %s but no production emission site on this surface uses it — "+
				"describe advertises an issue slug this module cannot produce", name)
		}
	}
}

// TestDescribeVocabularies_EveryDeclaredFieldPathExists — the control on the `fields` documentation. A
// path like "report.items.results[].state" is prose, but its LEAF must be a real JSON field name in
// this package's production files, so a renamed or deleted response field cannot leave a dangling path
// pointing a client at something that no longer exists.
func TestDescribeVocabularies_EveryDeclaredFieldPathExists(t *testing.T) {
	e := newTestEnv(t, "descvocabfields")
	vocabs := e.describeVocabularies(t)

	tags := productionJSONTags(t)
	if len(tags) < 20 {
		t.Fatalf("collected only %d JSON tags from production source — the scan is broken and this "+
			"control would be vacuous", len(tags))
	}
	// Keys that appear only as map literal keys (not struct tags) in production code. All three are
	// describe/quote output built with map[string]any rather than a response struct.
	alsoValid := map[string]bool{
		"model": true, // handleDescribe's pricing map key
		"tier":  true, // handleQuote's map key
		"id":    true, // describeTiers' per-tier map key
	}
	checked := 0
	for name, raw := range vocabs {
		entry := raw.(map[string]any)
		fields := stringSliceOf(t, entry["fields"])
		if len(fields) == 0 {
			t.Fatalf("vocabulary %q declares no fields — a value set nobody can locate in a response is "+
				"not discovery", name)
		}
		for _, path := range fields {
			parts := strings.Split(path, ".")
			leaf := parts[len(parts)-1]
			if !tags[leaf] && !alsoValid[leaf] {
				t.Fatalf("vocabulary %q declares field path %q, but %q is not a JSON field name anywhere "+
					"in this package's production source — the path is dangling", name, path, leaf)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no field paths checked — vacuous")
	}
}

// --- item 8: error_codes[] ---

// TestDescribeErrorCodes_MatchesCodeStatusExactly — error_codes[] must be the codeStatus map, whole.
// Both directions: a code in the map must be published (else a client's exhaustive switch is missing a
// code the module really returns), and a published code must be in the map (else describe advertises a
// code the module cannot emit, and a client writes dead handling for it).
func TestDescribeErrorCodes_MatchesCodeStatusExactly(t *testing.T) {
	e := newTestEnv(t, "descerrcodes")
	desc := e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 200)
	raw, ok := desc.body["error_codes"].([]any)
	if !ok {
		t.Fatalf("describe missing error_codes[]: %s", desc.raw)
	}
	if len(raw) == 0 {
		t.Fatal("error_codes[] is empty — vacuous, and a client would read it as 'this module cannot fail'")
	}
	published := map[string]int{}
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("error_codes entry is not an object: %v", item)
		}
		code, _ := m["code"].(string)
		status, sok := m["http_status"].(float64)
		if code == "" || !sok {
			t.Fatalf("error_codes entry missing code/http_status: %v", m)
		}
		if _, dup := published[code]; dup {
			t.Fatalf("error_codes lists %q twice", code)
		}
		published[code] = int(status)
		// The fixed MESSAGE must never be published: it is operator-facing English, not a branchable
		// value, and publishing it invites clients to match on message text — the coupling the coded
		// envelope exists to remove.
		if _, leaked := m["message"]; leaked {
			t.Fatalf("error_codes entry for %q publishes a message: %v", code, m)
		}
	}
	if len(published) != len(codeStatus) {
		t.Fatalf("describe publishes %d error codes, codeStatus has %d: published=%v",
			len(published), len(codeStatus), published)
	}
	for code, wantStatus := range codeStatus {
		got, ok := published[code]
		if !ok {
			t.Fatalf("code %q is emittable (it is in codeStatus, which writeError resolves a status from) "+
				"but is NOT published — a client building an exhaustive switch off describe would meet it "+
				"unhandled at runtime", code)
		}
		if got != wantStatus {
			t.Fatalf("code %q published http_status=%d, writeError sends %d", code, got, wantStatus)
		}
	}
	// fixedMessages and codeStatus must agree on the code set, since writeError reads one for the status
	// and the other for the default message: a code in only one produces an envelope with an empty
	// message or a 500-by-accident status.
	if len(fixedMessages) != len(codeStatus) {
		t.Fatalf("fixedMessages has %d entries, codeStatus %d — writeError reads both",
			len(fixedMessages), len(codeStatus))
	}
	for code := range codeStatus {
		if fixedMessages[code] == "" {
			t.Fatalf("code %q has a status but no fixed message — its envelope would carry an empty message", code)
		}
	}
}

// TestDescribeErrorCodes_APublishedCodeIsReallyEmitted — the behavioral half. A published list is only
// worth anything if the codes really appear on the wire with the published status, so this provokes
// real errors over real HTTP and checks each against the declaration.
//
// Three of the five are provoked here (validation_failed, unauthenticated, not_found). The other two
// are provoked by their own dedicated tests elsewhere in this package (conflict by the
// one-campaign-per-user gate in campaign_test.go, internal_error by
// TestDescribeVocabularies_FailsClosedRatherThanPublishingAPartialSet below); duplicating those setups
// here would add fixtures without adding coverage.
func TestDescribeErrorCodes_APublishedCodeIsReallyEmitted(t *testing.T) {
	e := newTestEnv(t, "descerrlive")
	desc := e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 200)
	published := map[string]int{}
	for _, item := range desc.body["error_codes"].([]any) {
		m := item.(map[string]any)
		published[m["code"].(string)] = int(m["http_status"].(float64))
	}
	if len(published) == 0 {
		t.Fatal("no published codes read — vacuous")
	}

	// validation_failed
	r := e.do(t, "GET", "/v1/report?user_id=not-a-uuid&job_id=00000000-0000-0000-0000-000000000000", nil)
	assertPublishedCode(t, published, r, codeValidationFailed)

	// not_found
	r = e.do(t, "GET", "/v1/report?user_id="+e.seedUser(t, "descerrlive@example.com")+
		"&job_id=00000000-0000-0000-0000-000000000000", nil)
	assertPublishedCode(t, published, r, codeNotFound)

	// unauthenticated — no Authorization header at all (e.do always sets one).
	r = e.doUnauthenticated(t, "GET", "/v1/report?user_id=00000000-0000-0000-0000-000000000000&job_id=00000000-0000-0000-0000-000000000000")
	assertPublishedCode(t, published, r, codeUnauthenticated)
}

// --- item 6: pricing.model's declared set ---

// TestDescribePricing_ModelIsAMemberOfTheDeclaredSet — item 6. The reported model must appear in the
// published set; a module cannot report a value it did not declare. Also asserts the set contains the
// forward-compatible third value, since declaring only what exists today is the defect item 6 names (a
// third module's launch would otherwise be a client-side code change).
func TestDescribePricing_ModelIsAMemberOfTheDeclaredSet(t *testing.T) {
	e := newTestEnv(t, "descpricing")
	desc := e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 200)
	pricing, ok := desc.body["pricing"].(map[string]any)
	if !ok {
		t.Fatalf("describe missing pricing: %s", desc.raw)
	}
	model, _ := pricing["model"].(string)
	if model == "" {
		t.Fatalf("pricing.model is empty: %s", desc.raw)
	}
	models := stringSliceOf(t, pricing["models"])
	if len(models) == 0 {
		t.Fatalf("pricing.models is absent or empty — a client must switch on model without knowing "+
			"every module's answer in advance: %s", desc.raw)
	}
	found := false
	for _, m := range models {
		if m == model {
			found = true
		}
	}
	if !found {
		t.Fatalf("pricing.model=%q is NOT a member of the declared set %v — the module reports a value it "+
			"did not declare, which is worse than not declaring the set at all", model, models)
	}
	// This module's own answer, pinned: a silent flip to flat_subscription would otherwise still pass
	// the membership check above while changing what the platform bills.
	if model != pricingModelFlatPerRecipient {
		t.Fatalf("pricing.model=%q, want %q (this module charges per recipient — Phase 9 §2.2)",
			model, pricingModelFlatPerRecipient)
	}
	// The wire value is unchanged by the constant promotion: a typo in pricingModelFlatPerRecipient
	// would break every existing client, so the literal is pinned here too.
	if model != "flat_per_recipient" {
		t.Fatalf("pricing.model=%q, want the unchanged wire literal \"flat_per_recipient\"", model)
	}
	for _, want := range []string{pricingModelFlatPerRecipient, pricingModelFlatSubscription, pricingModelFree} {
		present := false
		for _, m := range models {
			if m == want {
				present = true
			}
		}
		if !present {
			t.Fatalf("declared set %v omits %q — the set is the platform's, not this module's, and a value "+
				"only this module uses is not the point", models, want)
		}
	}
}

// TestDescribeVocabularies_FailsClosedRatherThanPublishingAPartialSet — the deliberate contrast with
// readUnitPriceCredits, which falls back to a seeded default. A vocabulary has no honest default, so a
// declared-but-missing enum type must produce an error rather than a set the client believes is
// complete. Renaming the type out from under the declaration is the realistic form of this (a migration
// renames an enum, the Go list does not follow), and it must not silently publish one of two
// vocabularies as though that were the whole set.
func TestDescribeVocabularies_FailsClosedRatherThanPublishingAPartialSet(t *testing.T) {
	e := newTestEnv(t, "descvocabclosed")
	// Sanity: describe works before the mutation, so a failure below is attributable to it.
	e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 200)

	// pool_kind is declared in exposedDBEnums. Renaming it makes the declared name absent. Nothing else
	// in this test's lifetime reads the type, and the clone is dropped at cleanup.
	if _, err := e.admin.Exec(context.Background(),
		`ALTER TYPE send_managed.pool_kind RENAME TO pool_kind_renamed`); err != nil {
		t.Fatalf("rename enum type: %v", err)
	}

	r := e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 500)
	if r.errorCode() != codeInternalError {
		t.Fatalf("code=%q, want %q: %s", r.errorCode(), codeInternalError, r.raw)
	}
	// The two-vocabulary rule: the internal reason names the type, the WIRE must not.
	if strings.Contains(r.raw, "pool_kind") {
		t.Fatalf("the 500 envelope leaks the internal type name: %s", r.raw)
	}
	if strings.Contains(r.raw, "vocabularies") {
		t.Fatalf("the error envelope must be the module's fixed shape, not a partial describe body: %s", r.raw)
	}
}

// --- helpers ---

// describeVocabularies fetches describe and returns its vocabularies object.
func (e *testEnv) describeVocabularies(t *testing.T) map[string]any {
	t.Helper()
	desc := e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 200)
	v, ok := desc.body["vocabularies"].(map[string]any)
	if !ok {
		t.Fatalf("describe missing vocabularies{}: %s", desc.raw)
	}
	return v
}

// describeVocabulary returns one vocabulary's published values.
func (e *testEnv) describeVocabulary(t *testing.T, name string) []string {
	t.Helper()
	entry, ok := e.describeVocabularies(t)[name].(map[string]any)
	if !ok {
		t.Fatalf("describe.vocabularies has no %q", name)
	}
	return stringSliceOf(t, entry["values"])
}

// doUnauthenticated issues a request with NO Authorization header at all (e.do always sets one), for
// the genuine missing-token case rather than a wrong token.
func (e *testEnv) doUnauthenticated(t *testing.T, method, path string) resp {
	t.Helper()
	req, err := http.NewRequest(method, e.baseURL+path, bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return resp{status: res.StatusCode, body: m, raw: string(raw)}
}

// liveEnumLabels reads every send_managed enum type's labels straight from the catalog, in
// enumsortorder — the same query the production code runs, issued independently through the ADMIN
// connection so the assertion does not depend on the code under test.
func (e *testEnv) liveEnumLabels(t *testing.T) map[string][]string {
	t.Helper()
	rows, err := e.admin.Query(context.Background(), enumValuesQuery)
	if err != nil {
		t.Fatalf("query pg_enum: %v", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var typname, label string
		if err := rows.Scan(&typname, &label); err != nil {
			t.Fatalf("scan pg_enum: %v", err)
		}
		out[typname] = append(out[typname], label)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_enum rows: %v", err)
	}
	return out
}

func stringSliceOf(t *testing.T, v any) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		// Also accept a []string, for the in-process constant slices.
		if ss, ok2 := v.([]string); ok2 {
			return ss
		}
		t.Fatalf("value is not an array: %#v", v)
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("array element is not a string: %#v", item)
		}
		out = append(out, s)
	}
	return out
}

// assertPublishedCode checks a real response's code+status against the published declaration.
func assertPublishedCode(t *testing.T, published map[string]int, r resp, wantCode string) {
	t.Helper()
	if r.errorCode() != wantCode {
		t.Fatalf("got code=%q, want %q: %s", r.errorCode(), wantCode, r.raw)
	}
	wantStatus, ok := published[wantCode]
	if !ok {
		t.Fatalf("the module emitted %q but describe does not publish it", wantCode)
	}
	if r.status != wantStatus {
		t.Fatalf("%q was emitted with HTTP %d but describe publishes %d — a client degrading by status "+
			"would mis-handle it", wantCode, r.status, wantStatus)
	}
}

// constNamesFor returns the Go constant NAMES (not values) that staticVocabularies uses for a given
// vocabulary, read out of describe_vocabularies.go's own source. Needed because the test must compare
// the DECLARATION against the emission sites by identifier — comparing values would pass even if the
// declaration referenced a different constant that happened to hold the same string.
func constNamesFor(t *testing.T, vocabName, prefix string) []string {
	t.Helper()
	src := readProductionFile(t, "internal/platformapi/describe_vocabularies.go")
	// `Name:` alignment inside staticVocabularies varies (gofmt aligns per-literal), so match the
	// quoted vocabulary name rather than a fixed run of spaces.
	i := strings.Index(src, `"`+vocabName+`"`)
	if i < 0 {
		t.Fatalf("could not locate the %q entry in describe_vocabularies.go", vocabName)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n\t},"); j > 0 {
		rest = rest[:j]
	}
	return regexp.MustCompile(prefix+`[A-Za-z]+`).FindAllString(rest, -1)
}

// grepPlatformAPISubmatches returns the first submatch of every match of pat across the non-test .go
// files of THIS package. Scoped to internal/platformapi on purpose — see
// TestDescribeVocabularies_StaticSetsMatchTheirSourceOfTruth on why a services/control-wide walk would
// be wrong for a per-surface vocabulary.
func grepPlatformAPISubmatches(t *testing.T, pat *regexp.Regexp) []string {
	t.Helper()
	var out []string
	forEachPlatformAPIProductionFile(t, func(_ string, data []byte) {
		for _, m := range pat.FindAllStringSubmatch(string(data), -1) {
			out = append(out, m[1])
		}
	})
	return out
}

// productionJSONTags collects every `json:"name"` tag name in THIS package's production files.
func productionJSONTags(t *testing.T) map[string]bool {
	t.Helper()
	tags := map[string]bool{}
	pat := regexp.MustCompile(`json:"([a-z0-9_]+)`)
	forEachPlatformAPIProductionFile(t, func(_ string, data []byte) {
		for _, m := range pat.FindAllStringSubmatch(string(data), -1) {
			tags[m[1]] = true
		}
	})
	return tags
}

// forEachPlatformAPIProductionFile calls fn for every non-test .go file in internal/platformapi.
func forEachPlatformAPIProductionFile(t *testing.T, fn func(name string, data []byte)) {
	t.Helper()
	dir := filepath.Join(servicesControlRoot(t), "internal", "platformapi")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read platformapi dir: %v", err)
	}
	seen := 0
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".go") || strings.HasSuffix(ent.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", ent.Name(), err)
		}
		fn(ent.Name(), data)
		seen++
	}
	if seen == 0 {
		t.Fatalf("found no production .go files in %s — every grep-based control here would be vacuous", dir)
	}
}

func readProductionFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(servicesControlRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// servicesControlRoot locates services/control from this test file's own path, asserting go.mod is
// there so a moved package fails loudly instead of silently scanning nothing.
func servicesControlRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate services/control root")
	}
	dir := filepath.Dir(thisFile) // .../services/control/internal/platformapi
	root := filepath.Join(dir, "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected services/control/go.mod at %s: %v", root, err)
	}
	return root
}
