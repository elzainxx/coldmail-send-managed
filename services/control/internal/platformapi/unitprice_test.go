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
}
