package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cryptoscruffy/inference-exchange/registry/internal/policy"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/store"
)

// handleDispatch accepts a job and runs it to completion before responding.
//
// Dispatch is free. The buyer pays at collect, for actual usage, bounded by the ceiling declared
// here. That ordering is the whole point of the design: no Hedera transaction is in flight while
// the work runs, so a job is not bounded by the 180-second transaction validity window.
//
// A job that fails is never billable, which is what keeps payment delivery-conditional now that
// the work no longer sits between verify and settle.
func (s *server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	buyer := r.Header.Get(buyerHeader)
	if buyer == "" {
		writeErr(w, http.StatusBadRequest, buyerHeader+" is required")
		return
	}

	var in struct {
		Prompt   string `json:"prompt"`
		MaxUnits int64  `json:"max_units"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed job: "+err.Error())
		return
	}
	if in.Prompt == "" || in.MaxUnits <= 0 {
		writeErr(w, http.StatusBadRequest, "prompt and a positive max_units are required")
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

	// Check the buyer's ceiling against policy before any work is done, so a job that could never
	// be paid for is refused rather than run. The provider's compute is the thing being protected
	// here — the buyer risks nothing by dispatching.
	if d := s.evaluate(buyer, p, in.MaxUnits*p.RatePerUnit); d.Deny {
		s.log.Info("dispatch denied", "buyer", buyer, "provider", pid, "rule", d.Rule)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": d.Reason})
		return
	}

	job, err := s.store.CreateJob(pid, buyer, in.Prompt, in.MaxUnits)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Detach from the request context. A buyer that hangs up mid-job should not cancel work the
	// provider is already doing and will expect to be paid for; the result is held for collection
	// either way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.jobTimeout)
	defer cancel()

	s.store.MarkRunning(job.ID)
	out, err := s.hub.Dispatch(ctx, pid, job.ID, in.Prompt, in.MaxUnits)
	if err != nil {
		s.store.Fail(job.ID, err.Error())
		s.log.Info("job failed", "job", job.ID, "provider", pid, "err", err)
		// 200 with billable:false, not an HTTP error. The exchange worked correctly; the provider
		// did not. The caller's correct move is to try another provider, and it needs the job id
		// to say what happened.
		writeJSON(w, http.StatusOK, map[string]any{
			"job_id": job.ID, "state": store.JobFailed, "billable": false,
			"error": policy.Encode(policy.PrefixJobFailed, map[string]any{
				"job_id": job.ID, "provider": pid, "detail": err.Error()}),
		})
		return
	}

	done, err := s.store.Complete(job.ID, out.Units, out.Result, p.RatePerUnit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if done.Reported > done.Priced {
		// Retained rather than rejected: a single over-report means little, but the aggregate is
		// the only evidence of systematic inflation available to anyone.
		s.log.Warn("provider reported more units than the buyer's ceiling",
			"job", done.ID, "provider", pid, "reported", done.Reported, "ceiling", done.MaxUnits)
	}
	s.log.Info("job completed", "job", done.ID, "reported", done.Reported,
		"priced", done.Priced, "price_tinybar", done.Price)

	writeJSON(w, http.StatusOK, map[string]any{
		"job_id": done.ID, "state": done.State, "billable": true,
		"reported_units": done.Reported, "priced_units": done.Priced,
		"price_tinybar": done.Price,
		"collect":       fmt.Sprintf("/p/job/%s", done.ID),
	})
}
