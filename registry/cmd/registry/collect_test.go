package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cryptoscruffy/inference-exchange/registry/internal/hub"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/policy"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/store"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/x402"
)

// fakeFacilitator stands in for Blocky402. verify and settle decide what it answers; a nil handler
// means "unreachable", which is how fail-closed is exercised.
type fakeFacilitator struct {
	srv     *httptest.Server
	verify  func(x402.Accept) x402.VerifyResponse
	settle  func(x402.Accept) x402.SettleResponse
	seenReq []x402.Accept
}

func newFakeFacilitator(t *testing.T) *fakeFacilitator {
	t.Helper()
	f := &fakeFacilitator{
		verify: func(x402.Accept) x402.VerifyResponse {
			return x402.VerifyResponse{IsValid: true, Payer: testBuyer}
		},
		settle: func(x402.Accept) x402.SettleResponse {
			return x402.SettleResponse{
				Success: true, Transaction: "0.0.7162784@1788548585.670939247",
				Network: network, Payer: testBuyer}
		},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PaymentRequirements x402.Accept `json:"paymentRequirements"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.seenReq = append(f.seenReq, body.PaymentRequirements)
		switch r.URL.Path {
		case "/verify":
			json.NewEncoder(w).Encode(f.verify(body.PaymentRequirements))
		case "/settle":
			json.NewEncoder(w).Encode(f.settle(body.PaymentRequirements))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// billableJob dispatches and completes a job, leaving it ready to collect.
func billableJob(t *testing.T, s *server, units int64) (jobID string, price int64) {
	t.Helper()
	pid, done := connectProvider(t, s, 3, answerWith("the paid-for result", units))
	t.Cleanup(done)
	jobID = quoteAndDispatch(t, s, pid, "x")
	// Dispatch returns before the work is done, so wait for the price to exist.
	st := awaitTerminal(t, s, jobID)
	return jobID, int64(st["price_tinybar"].(float64))
}

func collect(t *testing.T, s *server, jobID, buyer, paymentHeader string) *httptest.ResponseRecorder {
	t.Helper()
	h := map[string]string{}
	if buyer != "" {
		h[buyerHeader] = buyer
	}
	if paymentHeader != "" {
		h[x402.HeaderPaymentSignature] = paymentHeader
	}
	return do(t, s, "GET", "/p/job/"+jobID, nil, h)
}

// challengeOf decodes the PAYMENT-REQUIRED header off a 402.
func challengeOf(t *testing.T, w *httptest.ResponseRecorder) *x402.Challenge {
	t.Helper()
	h := w.Header().Get(x402.HeaderPaymentRequired)
	if h == "" {
		t.Fatalf("no %s header on a %d: %s", x402.HeaderPaymentRequired, w.Code, w.Body.String())
	}
	c, err := x402.DecodeChallenge(h)
	if err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	return c
}

// payment builds a PAYMENT-SIGNATURE header accepting the given terms. The transaction blob is
// opaque to the registry, so a placeholder is fine here.
func payment(accepted x402.Accept) string {
	b, _ := json.Marshal(x402.PaymentPayload{
		X402Version: x402.Version,
		Payload:     json.RawMessage(`{"transaction":"CsYBKsMBsignedbytes"}`),
		Accepted:    accepted,
	})
	return base64.StdEncoding.EncodeToString(b)
}

func TestUnpaidCollectIs402WithAPayableChallenge(t *testing.T) {
	s := newTestServer(t)
	jobID, price := billableJob(t, s, 10)

	w := collect(t, s, jobID, testBuyer, "")
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("code = %d, want 402", w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "{}" {
		t.Errorf("402 body = %q; everything the caller can act on rides in the header", body)
	}
	if strings.Contains(w.Body.String(), "the paid-for result") {
		t.Error("the result leaked on an unpaid collect")
	}

	c := challengeOf(t, w)
	if len(c.Accepts) != 1 {
		t.Fatalf("accepts = %d, want 1 payable option", len(c.Accepts))
	}
	a := c.Accepts[0]
	if a.Amount != "30" || price != 30 {
		t.Errorf("challenge amount = %q, job price = %d, want 30", a.Amount, price)
	}
	if a.PayTo != "0.0.5005" {
		t.Errorf("payTo = %q, want the provider's own account", a.PayTo)
	}
	// Without the facilitator's fee payer the buyer cannot build a transaction id, so the
	// challenge would be unpayable.
	if a.Extra["feePayer"] != s.feePayer {
		t.Errorf("feePayer = %q, want %q", a.Extra["feePayer"], s.feePayer)
	}
}

func TestPaidCollectSettlesAndReturnsTheResult(t *testing.T) {
	s := newTestServer(t)
	f := newFakeFacilitator(t)
	s.fac = x402.NewClient(f.srv.URL, 5*time.Second)
	jobID, _ := billableJob(t, s, 10)

	c := challengeOf(t, collect(t, s, jobID, testBuyer, ""))
	w := collect(t, s, jobID, testBuyer, payment(c.Accepts[0]))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body.String())
	}

	b := decodeBody(t, w)
	if b["result"] != "the paid-for result" {
		t.Errorf("result = %v", b["result"])
	}
	if b["tx_id"] == "" || b["state"] != string(store.JobCollected) {
		t.Errorf("body = %v", b)
	}

	// The success header is what tells the buyer the payment landed and where to look it up.
	var resp x402.PaymentResponse
	raw, _ := base64.StdEncoding.DecodeString(w.Header().Get(x402.HeaderPaymentResponse))
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("payment-response header: %v", err)
	}
	if !resp.Success || resp.Transaction == "" || resp.Network != network {
		t.Errorf("payment response = %+v", resp)
	}

	// What we asked the facilitator to enforce must be what we advertised.
	if len(f.seenReq) < 2 {
		t.Fatalf("facilitator saw %d calls, want verify and settle", len(f.seenReq))
	}
	for i, got := range f.seenReq {
		if got.Amount != "30" || got.PayTo != "0.0.5005" {
			t.Errorf("call %d sent requirements %+v", i, got)
		}
	}
}

// TestRecollectIsIdempotent covers the window where a buyer paid but lost the response. Charging
// again would take money for work already bought.
func TestRecollectIsIdempotent(t *testing.T) {
	s := newTestServer(t)
	f := newFakeFacilitator(t)
	s.fac = x402.NewClient(f.srv.URL, 5*time.Second)
	jobID, _ := billableJob(t, s, 10)

	c := challengeOf(t, collect(t, s, jobID, testBuyer, ""))
	first := collect(t, s, jobID, testBuyer, payment(c.Accepts[0]))
	if first.Code != http.StatusOK {
		t.Fatalf("first collect: %d", first.Code)
	}
	callsAfterFirst := len(f.seenReq)

	// No payment header at all the second time: the job is already paid for.
	second := collect(t, s, jobID, testBuyer, "")
	if second.Code != http.StatusOK {
		t.Fatalf("re-collect: %d %s", second.Code, second.Body.String())
	}
	if decodeBody(t, second)["tx_id"] != decodeBody(t, first)["tx_id"] {
		t.Error("re-collect returned a different transaction")
	}
	if len(f.seenReq) != callsAfterFirst {
		t.Errorf("re-collect hit the facilitator again: %d calls, want %d", len(f.seenReq), callsAfterFirst)
	}
}

// TestNonBillableStatesEachGetTheirOwnClass is the denial-legibility requirement: a machine caller
// must be able to tell wait from give-up from try-another-provider.
func TestNonBillableStatesEachGetTheirOwnClass(t *testing.T) {
	t.Run("still running", func(t *testing.T) {
		s := newTestServer(t)
		pid, done := connectProvider(t, s, 3, func(hub.Frame) *hub.Frame { return nil })
		defer done()
		j, err := s.store.CreateJob(pid, testBuyer, "x", 100)
		if err != nil {
			t.Fatal(err)
		}
		s.store.MarkRunning(j.ID)

		w := collect(t, s, j.ID, testBuyer, "")
		if w.Code != http.StatusAccepted {
			t.Errorf("code = %d, want 202", w.Code)
		}
		if reason, _ := decodeBody(t, w)["error"].(string); !strings.HasPrefix(reason, policy.PrefixJobRunning) {
			t.Errorf("error = %q, want %s", reason, policy.PrefixJobRunning)
		}
	})

	t.Run("failed", func(t *testing.T) {
		s := newTestServer(t)
		pid, done := connectProvider(t, s, 3, func(hub.Frame) *hub.Frame { return nil })
		defer done()
		j, _ := s.store.CreateJob(pid, testBuyer, "x", 100)
		s.store.Fail(j.ID, "the backend exploded")

		w := collect(t, s, j.ID, testBuyer, "")
		if w.Code != http.StatusOK {
			t.Errorf("code = %d, want 200 with billable:false", w.Code)
		}
		b := decodeBody(t, w)
		if b["billable"] != false {
			t.Errorf("billable = %v", b["billable"])
		}
		if reason, _ := b["error"].(string); !strings.HasPrefix(reason, policy.PrefixJobFailed) {
			t.Errorf("error = %q, want %s", reason, policy.PrefixJobFailed)
		}
		if w.Header().Get(x402.HeaderPaymentRequired) != "" {
			t.Error("a failed job issued a payment challenge")
		}
	})

	t.Run("expired", func(t *testing.T) {
		s := newTestServer(t)
		s.store = store.New(time.Nanosecond)
		pid, done := connectProvider(t, s, 3, answerWith("r", 5))
		defer done()
		jobID := quoteAndDispatch(t, s, pid, "x")
		awaitTerminal(t, s, jobID)
		time.Sleep(2 * time.Millisecond)
		s.store.Sweep()

		got := collect(t, s, jobID, testBuyer, "")
		if got.Code != http.StatusGone {
			t.Errorf("code = %d, want 410", got.Code)
		}
		if reason, _ := decodeBody(t, got)["error"].(string); !strings.HasPrefix(reason, policy.PrefixJobExpired) {
			t.Errorf("error = %q, want %s", reason, policy.PrefixJobExpired)
		}
	})
}

// TestReplayedChallengeIsRefused is the accepted-mismatch guard. Price is computed per request from
// the job record, so a challenge captured against a different job must not buy this one.
func TestReplayedChallengeIsRefused(t *testing.T) {
	s := newTestServer(t)
	f := newFakeFacilitator(t)
	s.fac = x402.NewClient(f.srv.URL, 5*time.Second)
	jobID, _ := billableJob(t, s, 10)

	stale := x402.Accept{
		Scheme: "exact", Network: network, Amount: "1", Asset: "0.0.0",
		PayTo: "0.0.9999", MaxTimeoutSeconds: 120,
		Extra: map[string]string{"feePayer": s.feePayer},
	}
	w := collect(t, s, jobID, testBuyer, payment(stale))
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("code = %d, want 402", w.Code)
	}
	if len(f.seenReq) != 0 {
		t.Error("a mismatched payload was forwarded to the facilitator")
	}

	c := challengeOf(t, w)
	var ev map[string]any
	if i := strings.Index(c.Error, " "); i > 0 {
		json.Unmarshal([]byte(c.Error[i+1:]), &ev)
	}
	if ev["rule"] != policy.RuleAcceptedMismatch {
		t.Errorf("rule = %v, want %s (reason: %s)", ev["rule"], policy.RuleAcceptedMismatch, c.Error)
	}
	// A denial offers nothing to pay: "you may not", not "here is the price".
	if len(c.Accepts) != 0 {
		t.Errorf("a denial still offered %d payable options", len(c.Accepts))
	}
}

// TestPayerMismatchIsRefused covers the one place buyer identity is cryptographic rather than
// asserted. Paying for someone else's job would otherwise buy a result you did not commission.
func TestPayerMismatchIsRefused(t *testing.T) {
	s := newTestServer(t)
	f := newFakeFacilitator(t)
	f.verify = func(x402.Accept) x402.VerifyResponse {
		return x402.VerifyResponse{IsValid: true, Payer: "0.0.9999"} // somebody else signed
	}
	s.fac = x402.NewClient(f.srv.URL, 5*time.Second)
	jobID, _ := billableJob(t, s, 10)

	c := challengeOf(t, collect(t, s, jobID, testBuyer, ""))
	w := collect(t, s, jobID, testBuyer, payment(c.Accepts[0]))
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("code = %d, want 402", w.Code)
	}

	var ev map[string]any
	reason := challengeOf(t, w).Error
	if i := strings.Index(reason, " "); i > 0 {
		json.Unmarshal([]byte(reason[i+1:]), &ev)
	}
	if ev["rule"] != policy.RulePayerMismatch {
		t.Errorf("rule = %v, want %s (reason: %s)", ev["rule"], policy.RulePayerMismatch, reason)
	}
	// Crucially: it must not have settled.
	if j, _ := s.store.Job(jobID, testBuyer); j.State == store.JobCollected {
		t.Error("a mismatched payer still collected the result")
	}
	if len(f.seenReq) != 1 {
		t.Errorf("facilitator calls = %d, want verify only (no settle)", len(f.seenReq))
	}
}

// TestFailClosedOnUnreachableFacilitator is the guarantee that an outage cannot leak a result. It
// also has to be distinguishable from a policy denial: retry later, do not change your budget.
func TestFailClosedOnUnreachableFacilitator(t *testing.T) {
	for _, phase := range []string{"verify", "settle"} {
		t.Run(phase, func(t *testing.T) {
			s := newTestServer(t)
			f := newFakeFacilitator(t)
			s.fac = x402.NewClient(f.srv.URL, 2*time.Second)
			jobID, _ := billableJob(t, s, 10)
			c := challengeOf(t, collect(t, s, jobID, testBuyer, ""))

			if phase == "verify" {
				f.srv.Close() // nothing listening at all
			} else {
				f.settle = func(x402.Accept) x402.SettleResponse {
					panic("unreachable") // httptest turns this into a 500
				}
			}

			w := collect(t, s, jobID, testBuyer, payment(c.Accepts[0]))
			if w.Code != http.StatusPaymentRequired {
				t.Fatalf("code = %d, want 402 — never a 200", w.Code)
			}
			if strings.Contains(w.Body.String(), "the paid-for result") {
				t.Error("the result leaked while the facilitator was unreachable")
			}
			reason := challengeOf(t, w).Error
			if !strings.HasPrefix(reason, policy.PrefixUpstream) {
				t.Errorf("reason = %q, want %s so the caller retries rather than changing budget",
					reason, policy.PrefixUpstream)
			}
			if j, _ := s.store.Job(jobID, testBuyer); j.State == store.JobCollected {
				t.Error("job marked collected without a settlement")
			}
		})
	}
}

// TestFacilitatorRejectionKeepsTheirReason matters for legibility: their strings and ours are
// namespaced apart so a caller can tell a malformed payment from a refused one.
func TestFacilitatorRejectionKeepsTheirReason(t *testing.T) {
	s := newTestServer(t)
	f := newFakeFacilitator(t)
	f.verify = func(x402.Accept) x402.VerifyResponse {
		return x402.VerifyResponse{IsValid: false, InvalidReason: "invalid_exact_hedera_payload_amount_mismatch"}
	}
	s.fac = x402.NewClient(f.srv.URL, 5*time.Second)
	jobID, _ := billableJob(t, s, 10)

	c := challengeOf(t, collect(t, s, jobID, testBuyer, ""))
	w := collect(t, s, jobID, testBuyer, payment(c.Accepts[0]))
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("code = %d", w.Code)
	}
	reason := challengeOf(t, w).Error
	if reason != "invalid_exact_hedera_payload_amount_mismatch" {
		t.Errorf("reason = %q, want the facilitator's verbatim", reason)
	}
	if strings.HasPrefix(reason, "FIREWALL_") {
		t.Error("their rejection was relabelled as ours")
	}
}

// TestPolicyDenialAtCollectOffersNothingToPay covers the second policy evaluation. The amount here
// is the real price, not the ceiling checked at dispatch.
func TestPolicyDenialAtCollectOffersNothingToPay(t *testing.T) {
	s := newTestServer(t)
	jobID, price := billableJob(t, s, 10)

	// Tighten the cap below the settled price after the work is done.
	s.limits.PerCallCapTinybar = price - 1

	w := collect(t, s, jobID, testBuyer, "")
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("code = %d, want 402", w.Code)
	}
	c := challengeOf(t, w)
	if len(c.Accepts) != 0 {
		t.Errorf("a denied collect offered %d payable options", len(c.Accepts))
	}
	if !strings.HasPrefix(c.Error, policy.PrefixDenied) {
		t.Errorf("error = %q, want %s", c.Error, policy.PrefixDenied)
	}
}

func TestCollectRefusesAnotherBuyer(t *testing.T) {
	s := newTestServer(t)
	jobID, _ := billableJob(t, s, 10)
	if w := collect(t, s, jobID, "0.0.9999", ""); w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestCollectUnknownJob(t *testing.T) {
	s := newTestServer(t)
	if w := collect(t, s, "job-nope", testBuyer, ""); w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestMalformedPaymentHeader(t *testing.T) {
	s := newTestServer(t)
	jobID, _ := billableJob(t, s, 10)

	w := collect(t, s, jobID, testBuyer, "!!!not base64!!!")
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("code = %d, want 402", w.Code)
	}
	// Still payable: the buyer can retry with a correct header.
	if c := challengeOf(t, w); len(c.Accepts) != 1 {
		t.Errorf("a malformed header should still leave the job payable, got %d options", len(c.Accepts))
	}
}
