package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cryptoscruffy/inference-exchange/registry/internal/hcs"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/policy"
)

// quoteTimeout bounds how long a provider may take to price a prompt.
//
// Short on purpose. Estimating is meant to be arithmetic, or at most one small model call; a
// provider that needs longer is doing the work rather than pricing it, which is exactly what
// quoting exists to prevent.
const quoteTimeout = 20 * time.Second

// handleQuote asks a provider what a specific prompt would cost, and commissions nothing.
//
// This exists because a buyer cannot know what a prompt will cost — token usage depends on the
// prompt and on the provider's own backend, and only the provider can judge the pair. Before this,
// the buyer guessed a ceiling and `Complete` clamped to it with no floor, so a buyer could set
// `max_units: 1` and receive a full job for three tinybar. The quote replaces that guess with a
// price the provider itself named.
//
// A quote is binding on the provider: whatever it estimates becomes the job's ceiling, and an
// overrun comes out of its own margin. That puts estimation risk on the party able to judge it.
func (s *server) handleQuote(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	buyer := r.Header.Get(buyerHeader)
	if buyer == "" {
		writeErr(w, http.StatusBadRequest, buyerHeader+" is required")
		return
	}

	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed quote request: "+err.Error())
		return
	}
	if in.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "prompt is required")
		return
	}

	p, err := s.store.Provider(pid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if !s.hub.Online(pid) {
		writeErr(w, http.StatusServiceUnavailable, "provider is offline")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), quoteTimeout)
	defer cancel()

	// A correlation id distinct from any job id: quotes and jobs share one socket.
	est, err := s.hub.Quote(ctx, pid, "q-"+pid+"-"+fmt.Sprint(time.Now().UnixNano()), in.Prompt)
	if err != nil {
		s.log.Info("quote failed", "provider", pid, "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": policy.Encode(policy.PrefixQuoteFailed, map[string]any{
				"provider": pid, "detail": err.Error()}),
		})
		return
	}

	if est.Declined {
		// A refusal, not a failure. The provider looked at the work and said no, which is a
		// legitimate answer and must never be scored against it.
		s.audit.Write(hcs.Record{
			Decision: hcs.DecisionDeclined, Phase: hcs.PhaseQuote,
			Buyer: buyer, PayTo: p.AccountID, Reason: est.Reason,
			Declared: hcs.Declared{ProviderID: pid},
		})
		s.log.Info("provider declined", "provider", pid, "buyer", buyer, "reason", est.Reason)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": policy.Encode(policy.PrefixDeclined, map[string]any{
				"provider": pid, "reason": est.Reason}),
		})
		return
	}
	if est.Units <= 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": policy.Encode(policy.PrefixQuoteFailed, map[string]any{
				"provider": pid, "detail": "provider quoted a non-positive estimate"}),
		})
		return
	}

	// Judge the real price now rather than a guess. The buyer learns immediately whether it could
	// afford this, instead of discovering it after a provider has already been asked to work.
	price := est.Units * p.RatePerUnit
	if d := s.evaluate(hcs.PhaseQuote, "", buyer, p, price); d.Deny {
		s.log.Info("quote denied", "buyer", buyer, "provider", pid, "rule", d.Rule)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": d.Reason})
		return
	}

	q, err := s.store.CreateQuote(pid, buyer, in.Prompt, est.Units, p.RatePerUnit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("quoted", "quote", q.ID, "provider", pid, "buyer", buyer,
		"units", q.EstimateUnits, "tinybar", q.PriceTinybar)

	writeJSON(w, http.StatusOK, map[string]any{
		"quote_id": q.ID, "provider_id": pid,
		"estimate_units": q.EstimateUnits, "price_tinybar": q.PriceTinybar,
		"rate_per_unit": q.RatePerUnit, "expires_at": q.ExpiresAt,
		"accept": fmt.Sprintf("/p/%s/job", pid),
		// Said plainly because it is the whole point of the round trip: this price is the provider's
		// own, and it is what will be charged even if the work runs over.
		"binding": "the provider absorbs any overrun beyond estimate_units",
	})
}
