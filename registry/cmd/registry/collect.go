package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cryptoscruffy/inference-exchange/registry/internal/hcs"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/policy"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/store"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/x402"
)

// handleCollect is the only paid endpoint in the exchange.
//
// The work is already finished and stored, so this is a payment round trip of a couple of seconds
// — far inside the transaction validity window that would otherwise cap how long a job could run.
// That is the whole reason payment is deferred to here rather than wrapped around the work.
func (s *server) handleCollect(w http.ResponseWriter, r *http.Request) {
	buyer := r.Header.Get(buyerHeader)
	j, err := s.store.Job(r.PathValue("id"), buyer)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	p, err := s.store.Provider(j.ProviderID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// States that are not a sale. Each gets its own class so a calling agent knows what to do:
	// wait, give up, try another provider, or stop asking.
	switch j.State {
	case store.JobCollected:
		// Idempotent. A buyer who paid but lost the response must be able to ask again without
		// paying twice — settlement already happened and the result is theirs.
		s.writeResult(w, j, "")
		return
	case store.JobFailed:
		writeJSON(w, http.StatusOK, map[string]any{
			"job_id": j.ID, "state": j.State, "billable": false,
			"error": policy.Encode(policy.PrefixJobFailed, map[string]any{
				"job_id": j.ID, "detail": j.Error}),
		})
		return
	case store.JobExpired:
		writeJSON(w, http.StatusGone, map[string]any{
			"error": policy.Encode(policy.PrefixJobExpired, map[string]any{"job_id": j.ID})})
		return
	case store.JobDispatched, store.JobRunning:
		writeJSON(w, http.StatusAccepted, map[string]any{
			"job_id": j.ID, "state": j.State,
			"error": policy.Encode(policy.PrefixJobRunning, map[string]any{"job_id": j.ID})})
		return
	}
	if !j.Billable() {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": policy.Encode(policy.PrefixNotBillable, map[string]any{
				"job_id": j.ID, "state": j.State})})
		return
	}

	// Requirements are authored here, from our own job record. The provider has no say in the
	// price or the payee, and the facilitator checks the signed transfer against exactly this —
	// which is why the registry never has to open the transaction itself.
	reqs := x402.Accept{
		Scheme: "exact", Network: network,
		Amount: strconv.FormatInt(j.Price, 10), Asset: "0.0.0",
		PayTo: p.AccountID, MaxTimeoutSeconds: 120,
		Extra: map[string]string{"feePayer": s.feePayer},
	}
	resource := x402.Resource{
		URL:         s.baseURL + r.URL.Path,
		Description: fmt.Sprintf("collect job %s from %s", j.ID, p.ID),
	}

	header := r.Header.Get(x402.HeaderPaymentSignature)
	if header == "" {
		// Re-evaluate against the real price, not the ceiling checked at dispatch. Two separate
		// decisions: the amount differs and the budget may have moved between them.
		if d := s.evaluate(hcs.PhaseCollect, j.ID, buyer, p, j.Price); d.Deny {
			s.challenge(w, resource, nil, d.Reason)
			return
		}
		s.challenge(w, resource, []x402.Accept{reqs}, "Payment required")
		return
	}

	payload, raw, err := x402.DecodePaymentSignature(header)
	if err != nil {
		s.challenge(w, resource, []x402.Accept{reqs}, "invalid_payment_signature_header")
		return
	}

	// The buyer echoes back the requirements it accepted, and they must be the ones just built.
	// Price is computed per request from the job record, so a challenge captured against a
	// different job would otherwise be replayable against this one.
	if payload.Accepted.Amount != reqs.Amount || payload.Accepted.PayTo != reqs.PayTo {
		s.challenge(w, resource, nil, policy.Encode(policy.PrefixDenied, map[string]any{
			"rule":            policy.RuleAcceptedMismatch,
			"expected_amount": reqs.Amount, "expected_payTo": reqs.PayTo,
			"actual_amount": payload.Accepted.Amount, "actual_payTo": payload.Accepted.PayTo,
		}))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	ver, err := s.fac.Verify(ctx, raw, reqs)
	if err != nil {
		// Fail closed. An unreachable facilitator denies; it never passes through unsettled, and
		// its class is distinct from a policy denial so the caller retries later rather than
		// changing its budget.
		s.log.Warn("facilitator unreachable at verify", "job", j.ID, "err", err)
		s.challenge(w, resource, []x402.Accept{reqs},
			policy.Encode(policy.PrefixUpstream, map[string]any{"phase": "verify"}))
		return
	}
	if !ver.IsValid {
		// Their reason, verbatim. It is namespaced differently from ours so the caller can tell a
		// bad payment from a refused one.
		s.challenge(w, resource, []x402.Accept{reqs}, ver.InvalidReason)
		return
	}

	// The facilitator reports who actually signed. This is the one point where buyer identity is
	// cryptographic rather than asserted, so it is checked here: paying for someone else's job
	// would otherwise let a caller read a result it did not commission.
	if ver.Payer != "" && j.BuyerAccount != "" && ver.Payer != j.BuyerAccount {
		s.challenge(w, resource, nil, policy.Encode(policy.PrefixDenied, map[string]any{
			"rule": policy.RulePayerMismatch, "expected": j.BuyerAccount, "actual": ver.Payer}))
		return
	}
	if d := s.evaluate(hcs.PhaseCollect, j.ID, buyer, p, j.Price); d.Deny {
		s.challenge(w, resource, nil, d.Reason)
		return
	}

	set, err := s.fac.Settle(ctx, raw, reqs)
	if err != nil {
		s.log.Warn("facilitator unreachable at settle", "job", j.ID, "err", err)
		s.challenge(w, resource, []x402.Accept{reqs},
			policy.Encode(policy.PrefixUpstream, map[string]any{"phase": "settle"}))
		return
	}
	if !set.Success {
		s.challenge(w, resource, []x402.Accept{reqs}, set.ErrorReason)
		return
	}

	s.store.MarkCollected(j.ID, set.Transaction)
	j.TxID, j.State = set.Transaction, store.JobCollected
	// The only record here the ledger stands behind. Everything the provider said about the work
	// stays under declared, where it cannot be mistaken for it.
	s.audit.Write(hcs.Record{
		Decision: hcs.DecisionSettled, Phase: hcs.PhaseCollect,
		JobID: j.ID, Buyer: j.BuyerAccount, PayTo: p.AccountID, Amount: j.Price,
		Settled:  hcs.Settled{Tx: set.Transaction, Payer: set.Payer, Network: network},
		Declared: hcs.Declared{ProviderID: p.ID, ReportedUnits: j.Reported},
	})
	s.log.Info("job collected", "job", j.ID, "tx", set.Transaction,
		"payer", set.Payer, "payTo", p.AccountID, "tinybar", j.Price)

	respHeader, _ := x402.EncodePaymentResponse(x402.PaymentResponse{
		Success: true, Payer: set.Payer, Transaction: set.Transaction, Network: network})
	s.writeResult(w, j, respHeader)
}

// challenge writes a 402 carrying the PAYMENT-REQUIRED header.
//
// reason lands in the header's error field, which is the only thing that reaches the calling
// agent: the body is empty and every other field is dropped in transit. Passing nil accepts says
// "you may not pay for this", as opposed to "here is what it costs".
func (s *server) challenge(w http.ResponseWriter, res x402.Resource, accepts []x402.Accept, reason string) {
	if accepts == nil {
		accepts = []x402.Accept{}
	}
	h, err := x402.EncodeChallenge(x402.Challenge{
		X402Version: x402.Version, Error: reason, Resource: res, Accepts: accepts})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not build challenge")
		return
	}
	w.Header().Set(x402.HeaderPaymentRequired, h)
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	w.Write([]byte("{}"))
}

func (s *server) writeResult(w http.ResponseWriter, j *store.Job, paymentResponse string) {
	if paymentResponse != "" {
		w.Header().Set(x402.HeaderPaymentResponse, paymentResponse)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job_id": j.ID, "state": store.JobCollected, "result": j.Result,
		"reported_units": j.Reported, "priced_units": j.Priced,
		"price_tinybar": j.Price, "tx_id": j.TxID,
	})
}
