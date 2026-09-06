package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cryptoscruffy/inference-exchange/registry/internal/hcs"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/policy"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/store"
)

// handleDispatch accepts a job, starts it, and answers immediately with its id.
//
// Dispatch accepts a quote; it does not describe a job. The prompt, the ceiling and the price all
// come from the quote the provider itself gave, so there is nothing left here for a buyer to
// declare and nothing to disagree about later.
//
// Dispatch is free and asynchronous. The buyer polls /p/job/{id}/status and pays at collect, for
// actual usage, bounded by the quoted estimate. Two things fall out of not blocking:
//
//   - The caller learns the job id before the work finishes, so it can report progress and, if its
//     process dies, come back to a job already in flight.
//   - No Hedera transaction is in flight while the work runs, so job duration is not bounded by the
//     180-second transaction validity window.
//
// A job that fails is never billable, which is what keeps payment delivery-conditional now that the
// work no longer sits between verify and settle.
func (s *server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	buyer := r.Header.Get(buyerHeader)
	if buyer == "" {
		writeErr(w, http.StatusBadRequest, buyerHeader+" is required")
		return
	}

	var in struct {
		QuoteID string `json:"quote_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed job: "+err.Error())
		return
	}
	if in.QuoteID == "" {
		writeErr(w, http.StatusBadRequest, "quote_id is required; get one from POST /p/{id}/quote")
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

	// The prompt is not accepted here — it comes from the quote. Letting a buyer resend it would
	// allow a swap: quote a cheap prompt, dispatch an expensive one against the cheap price.
	quote, err := s.store.Quote(in.QuoteID, buyer)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": policy.Encode(policy.PrefixQuoteInvalid, map[string]any{
				"quote_id": in.QuoteID, "detail": err.Error()}),
		})
		return
	}
	if quote.ProviderID != pid {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": policy.Encode(policy.PrefixQuoteInvalid, map[string]any{
				"quote_id": in.QuoteID, "detail": "quote is for a different provider"}),
		})
		return
	}

	// Judged again against the same price, because a budget can move between quoting and accepting.
	if d := s.evaluate(hcs.PhaseDispatch, "", buyer, p, quote.PriceTinybar); d.Deny {
		s.log.Info("dispatch denied", "buyer", buyer, "provider", pid, "rule", d.Rule)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": d.Reason})
		return
	}

	// Expiry and reuse are checked inside the same lock that creates the job, so two concurrent
	// dispatches cannot both spend one quote.
	job, spent, err := s.store.SpendQuote(in.QuoteID, buyer)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": policy.Encode(policy.PrefixQuoteInvalid, map[string]any{
				"quote_id": in.QuoteID, "detail": err.Error()}),
		})
		return
	}

	s.store.MarkRunning(job.ID)
	go s.runJob(job.ID, pid, spent.Prompt, spent.EstimateUnits, p.RatePerUnit)

	// 202: accepted and started, not finished. No price yet — it depends on units the provider has
	// not reported, so it appears on the status endpoint once the work completes.
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID, "state": store.JobRunning, "quote_id": spent.ID,
		"agreed_units": spent.EstimateUnits, "agreed_price_tinybar": spent.PriceTinybar,
		"status":  fmt.Sprintf("/p/job/%s/status", job.ID),
		"collect": fmt.Sprintf("/p/job/%s", job.ID),
	})
}

// runJob drives one job to a terminal state. It owns the job from here: whatever happens, the job
// ends up completed or failed, never left running for a buyer that is polling.
//
// The context is deliberately independent of the request that started it — the buyer hanging up
// must not cancel work the provider is already doing and expects to be paid for.
func (s *server) runJob(jobID, providerID, prompt string, maxUnits, rate int64) {
	ctx, cancel := context.WithTimeout(context.Background(), s.jobTimeout)
	defer cancel()

	out, err := s.hub.Dispatch(ctx, providerID, jobID, prompt, maxUnits)
	if err != nil {
		s.store.Fail(jobID, err.Error())
		s.audit.Write(hcs.Record{
			Decision: hcs.DecisionFailed, Phase: hcs.PhaseDispatch,
			JobID: jobID, Reason: err.Error(),
			Declared: hcs.Declared{ProviderID: providerID},
		})
		s.log.Info("job failed", "job", jobID, "provider", providerID, "err", err)
		return
	}

	done, err := s.store.Complete(jobID, out.Units, out.Result, rate)
	if err != nil {
		// Completing an already-terminal job: it was swept or failed while the provider worked.
		s.log.Warn("could not record a result", "job", jobID, "err", err)
		return
	}
	if done.Reported > done.Priced {
		// Retained rather than rejected: a single over-report means little, but the aggregate is
		// the only evidence of systematic inflation available to anyone.
		s.log.Warn("provider used more units than it quoted; billing the quote",
			"job", done.ID, "provider", providerID, "reported", done.Reported, "quoted", done.MaxUnits)
	}
	s.log.Info("job completed", "job", done.ID, "reported", done.Reported,
		"priced", done.Priced, "price_tinybar", done.Price)
}
