package platformapi

// unitprice_test.go — Phase 9.5 S10 (§2.4), O7's quote/describe half. Real Postgres, real HTTP
// against the actual production Server (httptest) — no mocks (CLAUDE.md Rule 3). Proves
// unit_price_credits is read LIVE from send_managed.config, not the old hardcoded constant: with the
// seeded default (1) quote/describe charge 1 credit/recipient; a live PUT-equivalent write to
// unit_price_credits (direct SQL, mirroring what PUT /v1/admin/config/{key} would do) is reflected
// on the VERY NEXT request with no server restart.

import (
	"testing"
)

func (e *testEnv) setUnitPriceCredits(t *testing.T, v int) {
	t.Helper()
	if _, err := e.admin.Exec(bgctx(),
		`UPDATE send_managed.config SET value = jsonb_build_object('v', $1::int) WHERE key='unit_price_credits'`, v); err != nil {
		t.Fatalf("set unit_price_credits: %v", err)
	}
}

// TestUnitPrice_QuoteAndDescribeReflectLiveConfig (O7) — defaults preserve today's behavior (1
// credit/recipient); a live config write is reflected on the next request, no restart.
func TestUnitPrice_QuoteAndDescribeReflectLiveConfig(t *testing.T) {
	e := newTestEnv(t, "unitprice")

	// Seeded default: 1 credit/recipient (behavior-preserving per spec §2.4).
	resp := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": 10}).requireStatus(t, 200)
	if cost, ok := resp.body["cost"].(float64); !ok || cost != 10 {
		t.Fatalf("quote cost=%v, want 10 (default unit_price_credits=1): %s", resp.body["cost"], resp.raw)
	}
	descResp := e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 200)
	pricing, ok := descResp.body["pricing"].(map[string]any)
	if !ok {
		t.Fatalf("describe missing pricing object: %s", descResp.raw)
	}
	if unit, ok := pricing["unit_price_credits"].(float64); !ok || unit != 1 {
		t.Fatalf("describe pricing.unit_price_credits=%v, want 1 (default): %s", pricing["unit_price_credits"], descResp.raw)
	}
	// D22 / fix-plan 5b: describe declares whether quote's cost half is inert, so a client never has
	// to infer it from a bare 0. At M2's seeded default the module really does charge, so this is
	// false — and note that `false` is ALSO what a hardcoded literal would say here, which is exactly
	// why the zero-retune block at the end of this test exists as the control.
	// FAIL-VERIFY: delete the `cost_fields_inert` line from handleDescribe and this fails.
	inert, ok := pricing["cost_fields_inert"].(bool)
	if !ok {
		t.Fatalf("describe pricing missing cost_fields_inert (D22): %s", descResp.raw)
	}
	if inert {
		t.Fatalf("describe pricing.cost_fields_inert=true at the seeded default unit_price_credits=1, "+
			"want false (this module DOES charge per recipient): %s", descResp.raw)
	}

	// Live write: PUT unit_price_credits=3 (direct SQL, same effect as the real admin endpoint) —
	// the VERY NEXT quote/describe must reflect it, no server restart.
	e.setUnitPriceCredits(t, 3)
	resp2 := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": 10}).requireStatus(t, 200)
	if cost, ok := resp2.body["cost"].(float64); !ok || cost != 30 {
		t.Fatalf("quote cost=%v after PUT unit_price_credits=3, want 30: %s", resp2.body["cost"], resp2.raw)
	}
	descResp2 := e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 200)
	pricing2, ok := descResp2.body["pricing"].(map[string]any)
	if !ok {
		t.Fatalf("describe missing pricing object (2nd): %s", descResp2.raw)
	}
	if unit, ok := pricing2["unit_price_credits"].(float64); !ok || unit != 3 {
		t.Fatalf("describe pricing.unit_price_credits=%v after PUT unit_price_credits=3, want 3: %s", pricing2["unit_price_credits"], descResp2.raw)
	}
	// Both tiers move together with the single source of truth (Phase 9 §2.2, E2).
	tiers, ok := pricing2["tiers"].([]any)
	if !ok || len(tiers) != 2 {
		t.Fatalf("describe pricing.tiers=%v, want 2 tiers: %s", pricing2["tiers"], descResp2.raw)
	}
	for _, tr := range tiers {
		tm, ok := tr.(map[string]any)
		if !ok {
			t.Fatalf("tier entry not an object: %v", tr)
		}
		if u, ok := tm["unit_price_credits"].(float64); !ok || u != 3 {
			t.Fatalf("tier %v unit_price_credits=%v after PUT=3, want 3", tm["id"], tm["unit_price_credits"])
		}
	}

	// THE ANTI-VACUITY CONTROL for cost_fields_inert (Rule 10). Every assertion above would pass
	// against a hardcoded `"cost_fields_inert": false`, because false IS the right answer at both
	// unit=1 and unit=3. So this block drives the one input that separates a derived value from a
	// literal: retune to 0. That is not a hypothetical — unit_price_credits is the same
	// live-retunable config key here as in module 3 (Rule 1, no redeploy), so an operator can put
	// this module into the silent-zero state D22 describes, where quote returns 200 with cost:0 and
	// a cost-preview UI shows "0 credits" with no error. Under a declared flat_per_recipient model,
	// 0 contradicts the model, which makes the declaration MORE load-bearing here, not less.
	// FAIL-VERIFY: change handleDescribe's `unit == 0` to a literal `false` and this fails
	// ("cost_fields_inert=false at unit_price_credits=0").
	e.setUnitPriceCredits(t, 0)
	resp3 := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": 10}).requireStatus(t, 200)
	if cost, ok := resp3.body["cost"].(float64); !ok || cost != 0 {
		t.Fatalf("quote cost=%v after PUT unit_price_credits=0, want 0: %s", resp3.body["cost"], resp3.raw)
	}
	descResp3 := e.do(t, "GET", "/v1/describe", nil).requireStatus(t, 200)
	pricing3, ok := descResp3.body["pricing"].(map[string]any)
	if !ok {
		t.Fatalf("describe missing pricing object (3rd): %s", descResp3.raw)
	}
	inert3, ok := pricing3["cost_fields_inert"].(bool)
	if !ok {
		t.Fatalf("describe pricing missing cost_fields_inert after zero-retune: %s", descResp3.raw)
	}
	if !inert3 {
		t.Fatalf("describe pricing.cost_fields_inert=false at unit_price_credits=0, want true — quote "+
			"just returned cost:0, and a client that cannot tell that from a broken price lookup is "+
			"exactly D22: %s", descResp3.raw)
	}
}
