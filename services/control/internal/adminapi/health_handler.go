package adminapi

// health_handler.go — Phase 9.5 B3 (§3): GET /v1/admin/health. Operator-key auth (Domain B, the
// same requireAuth gate every other /v1/admin/* route uses), read-only, safe to poll. The response
// shape is exactly §3.2's jsonc example; active_conditions (§4, B4) is populated by health_alerts.go
// and appended here so the two chunks compose without re-plumbing this handler.

import (
	"net/http"
)

// healthResponse is the wire shape (§3.2), field-for-field.
type healthResponse struct {
	GeneratedAt string `json:"generated_at"`
	WindowSec   int    `json:"window_sec"`

	Dispatch struct {
		// DispatchPaused stays, unchanged in meaning and never removed (Rule 5, additive-only): it is
		// "the effective rung is not serving." Since send_managed_0025 it is derived from BOTH dials
		// rather than from dispatch_enabled alone — see resolveRung. Every input that made it true
		// before still makes it true; it simply stops reading `false` while the Control is stopped.
		DispatchPaused bool `json:"dispatch_paused"`
		// DispatchRung is ADDITIVE (fix-plan item 12) and is the field an operator actually needs:
		// there are now three rungs at which dispatch_paused is true and they differ in what else is
		// running (intake, the reclaim sweep). A boolean cannot express which one is pulled, and
		// during an incident "paused" without "which pause" is the ambiguity that makes an operator
		// distrust the dashboard. A client that ignores this field behaves exactly as before.
		DispatchRung    string  `json:"dispatch_rung"`
		TicksPerMin     float64 `json:"ticks_per_min"`
		SendsLastWindow int     `json:"sends_last_window"`
		SendRatePerMin  float64 `json:"send_rate_per_min"`
	} `json:"dispatch"`

	Queue struct {
		Queued             int `json:"queued"`
		Assigned           int `json:"assigned"`
		Sending            int `json:"sending"`
		Sent               int `json:"sent"`
		Failed             int `json:"failed"`
		Bounced            int `json:"bounced"`
		WaitingForCapacity int `json:"waiting_for_capacity"`
	} `json:"queue"`

	Workers struct {
		Active                      int  `json:"active"`
		Stale                       int  `json:"stale"`
		Disabled                    int  `json:"disabled"`
		OldestActiveHeartbeatAgeSec *int `json:"oldest_active_heartbeat_age_sec,omitempty"`
	} `json:"boxes"`

	Accounts struct {
		Healthy      int `json:"healthy"`
		Paused       int `json:"paused"`
		Quarantined  int `json:"quarantined"`
		Disabled     int `json:"disabled"`
		AtDailyLimit int `json:"at_daily_limit"`
	} `json:"accounts"`

	Pools []poolHeadroomJSON `json:"pools"`

	ErrorsLastWindow map[string]int `json:"errors_last_window"`

	Shadow struct {
		AutoquarantineMode   string               `json:"autoquarantine_mode"`
		WouldQuarantineCount int                  `json:"would_quarantine_count"`
		Recent               []shadowDecisionJSON `json:"recent"`
	} `json:"shadow"`

	ActiveConditions []activeConditionJSON `json:"active_conditions"`
}

type poolHeadroomJSON struct {
	PoolID           string `json:"pool_id"`
	Kind             string `json:"kind"`
	EligibleAccounts int    `json:"eligible_accounts"`
	Headroom         string `json:"headroom"`
}

type shadowDecisionJSON struct {
	AccountID string `json:"account_id"`
	Score     int    `json:"score"`
	Threshold int    `json:"threshold"`
	At        string `json:"at"`
}

// errorsLastWindowClasses is the fixed §1.1 error_class vocabulary — the response always includes
// every class with a 0 default, never omitting a key just because it didn't fire in the window
// (stable shape for a poller that indexes the map by class name).
var errorsLastWindowClasses = []string{
	"bounce_permanent", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown",
}

// handleHealth: GET /v1/admin/health (§3.1). Operator auth only; every field derives from
// COUNT/GROUP BY over existing tables plus the live dispatch_enabled/autoquarantine_mode config —
// no schema change, no new persisted state.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := dbCtx(r.Context())
	defer cancel()

	windowSec, err := readConfigInt(ctx, s.db, "health_metrics_window_sec")
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "health_window_read_failed: "+err.Error()))
		return
	}
	dispatchEnabled, err := readConfigBool(ctx, s.db, "dispatch_enabled")
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "health_dispatch_enabled_read_failed: "+err.Error()))
		return
	}
	// dispatch_mode (send_managed_0025, fix-plan item 12) — read alongside dispatch_enabled because the
	// snapshot reports the EFFECTIVE rung, which is a function of both. A read error fails the request
	// like every other config read here: this endpoint's job is to state the system's condition, and a
	// health surface that guesses one field is worse than one that admits it could not answer.
	dispatchMode, err := readConfigString(ctx, s.db, "dispatch_mode")
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "health_dispatch_mode_read_failed: "+err.Error()))
		return
	}
	autoquarantineMode, err := readConfigString(ctx, s.db, "autoquarantine_mode")
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "health_autoquarantine_mode_read_failed: "+err.Error()))
		return
	}

	snap, err := buildHealthSnapshot(ctx, s.db, windowSec, resolveRung(dispatchMode, dispatchEnabled), autoquarantineMode)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "health_snapshot_failed: "+err.Error()))
		return
	}

	// active_conditions (§4, B4): pool_exhausted / account_quarantined (enforce) / worker_death.
	conditions, err := buildActiveConditions(ctx, s.db, snap)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "health_conditions_failed: "+err.Error()))
		return
	}

	// §4.3 the optional webhook: fire on each NEW rising edge, never on every poll (dedup). Reads
	// alert_webhook_url fresh (no redeploy needed for an operator to set/unset it) and fans out in
	// its own goroutine so a slow/unreachable operator endpoint can never delay this response —
	// this GET is otherwise entirely read-only.
	webhookURL, err := readConfigString(ctx, s.db, "alert_webhook_url")
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "health_webhook_url_read_failed: "+err.Error()))
		return
	}
	go fireWebhooks(webhookURL, conditions)

	resp := healthResponse{
		GeneratedAt: snap.GeneratedAt.Format("2006-01-02T15:04:05Z"),
		WindowSec:   snap.WindowSec,
	}
	resp.Dispatch.DispatchPaused = snap.DispatchPaused
	resp.Dispatch.DispatchRung = snap.DispatchRung
	resp.Dispatch.TicksPerMin = snap.TicksPerMin
	resp.Dispatch.SendsLastWindow = snap.SendsLastWindow
	resp.Dispatch.SendRatePerMin = snap.SendRatePerMin

	resp.Queue.Queued = snap.Queue.Queued
	resp.Queue.Assigned = snap.Queue.Assigned
	resp.Queue.Sending = snap.Queue.Sending
	resp.Queue.Sent = snap.Queue.Sent
	resp.Queue.Failed = snap.Queue.Failed
	resp.Queue.Bounced = snap.Queue.Bounced
	resp.Queue.WaitingForCapacity = snap.Queue.WaitingForCapacity

	resp.Workers.Active = snap.Workers.Active
	resp.Workers.Stale = snap.Workers.Stale
	resp.Workers.Disabled = snap.Workers.Disabled
	resp.Workers.OldestActiveHeartbeatAgeSec = snap.Workers.OldestActiveHeartbeatAgeSec

	resp.Accounts.Healthy = snap.Accounts.Healthy
	resp.Accounts.Paused = snap.Accounts.Paused
	resp.Accounts.Quarantined = snap.Accounts.Quarantined
	resp.Accounts.Disabled = snap.Accounts.Disabled
	resp.Accounts.AtDailyLimit = snap.Accounts.AtDailyLimit

	resp.Pools = make([]poolHeadroomJSON, 0, len(snap.Pools))
	for _, p := range snap.Pools {
		resp.Pools = append(resp.Pools, poolHeadroomJSON{
			PoolID: p.PoolID, Kind: p.Kind, EligibleAccounts: p.EligibleAccounts, Headroom: p.Headroom,
		})
	}

	resp.ErrorsLastWindow = make(map[string]int, len(errorsLastWindowClasses))
	for _, class := range errorsLastWindowClasses {
		resp.ErrorsLastWindow[class] = snap.ErrorsLastWindow[class] // 0 default for a class absent from the map
	}

	resp.Shadow.AutoquarantineMode = snap.Shadow.AutoquarantineMode
	resp.Shadow.WouldQuarantineCount = snap.Shadow.WouldQuarantineCount
	resp.Shadow.Recent = make([]shadowDecisionJSON, 0, len(snap.Shadow.Recent))
	for _, d := range snap.Shadow.Recent {
		resp.Shadow.Recent = append(resp.Shadow.Recent, shadowDecisionJSON{
			AccountID: d.AccountID, Score: d.Score, Threshold: d.Threshold,
			At: d.At.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	resp.ActiveConditions = conditions
	writeJSON(w, http.StatusOK, resp)
}
