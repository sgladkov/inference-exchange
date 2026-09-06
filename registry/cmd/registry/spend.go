package main

import (
	"net/http"
	"time"
)

// handleSpend reports what a buyer has spent and how much headroom each limit leaves.
//
// Note how this differs in trust from the decision log. `why_blocked` deliberately reads from a
// mirror node so the registry cannot misreport what it decided. Spend has no such independent
// source: budgets, velocity and abandonment are registry-side state by design — that is precisely
// what makes them enforceable against a buyer who could otherwise edit them — so this endpoint is
// the registry reporting on itself, and it can only be taken as such.
//
// What a buyer can check independently is the settlements: every collected job carries a
// transaction id that resolves on a public mirror node.
func (s *server) handleSpend(w http.ResponseWriter, r *http.Request) {
	buyer := r.Header.Get(buyerHeader)
	if buyer == "" {
		writeErr(w, http.StatusBadRequest, buyerHeader+" is required")
		return
	}

	spentToday, callsToday, abandoned, completed := s.store.BuyerStats(buyer, 24*time.Hour)
	_, callsInWindow, _, _ := s.store.BuyerStats(buyer, s.limits.VelocityWindow)
	byProvider := s.store.SpendByProvider(buyer)

	var lifetime int64
	var lifetimeCalls int
	for _, p := range byProvider {
		lifetime += p.Spend
		lifetimeCalls += p.Calls
	}

	// Headroom rather than just limits: an agent deciding whether to dispatch cares about what is
	// left, and making it subtract for itself invites the arithmetic to be got wrong.
	writeJSON(w, http.StatusOK, map[string]any{
		"buyer": buyer,
		"day": map[string]any{
			"spent_tinybar":     spentToday,
			"budget_tinybar":    s.limits.DailyBudgetTinybar,
			"remaining_tinybar": headroom(s.limits.DailyBudgetTinybar, spentToday),
			"jobs":              callsToday,
		},
		"velocity": map[string]any{
			"calls":          callsInWindow,
			"limit":          s.limits.VelocityCalls,
			"window_seconds": int(s.limits.VelocityWindow.Seconds()),
		},
		"per_call_cap_tinybar": s.limits.PerCallCapTinybar,
		"lifetime": map[string]any{
			"settled_tinybar": lifetime,
			"settled_jobs":    lifetimeCalls,
		},
		"abandonment": map[string]any{
			"abandoned":    abandoned,
			"completed":    completed,
			"ratio":        ratio(abandoned, completed),
			"limit_ratio":  s.limits.MaxAbandonRatio,
			"judged_after": s.limits.MinJobsBeforeRatio,
		},
		"by_provider": byProvider,
		"unproven_provider": map[string]any{
			"cap_tinybar": s.limits.UnprovenSpendCap,
			"proven_at":   s.limits.UnprovenAfterCalls,
		},
		// Stated rather than left to be inferred: this is the registry's own accounting. The
		// settlements behind it are independently checkable on a mirror node; these totals are not.
		"source": "registry-side policy state; settlement transaction ids are independently verifiable",
	})
}

// headroom returns what is left under a limit, or -1 when the limit is unset, so "unlimited" is
// never reported as a large but finite number.
func headroom(limit, used int64) int64 {
	if limit <= 0 {
		return -1
	}
	if used >= limit {
		return 0
	}
	return limit - used
}

func ratio(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole)
}
