package main

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cryptoscruffy/inference-exchange/registry/internal/hcs"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/hub"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/x402"
)

// spyAudit captures decision records in memory, so what the payment path chooses to record is
// testable without a topic, an operator key, or a network.
type spyAudit struct {
	mu   sync.Mutex
	seen []hcs.Record
}

func (a *spyAudit) Write(r hcs.Record) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, r)
}
func (a *spyAudit) Close() {}

func (a *spyAudit) records() []hcs.Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]hcs.Record(nil), a.seen...)
}

func (a *spyAudit) find(decision string) []hcs.Record {
	var out []hcs.Record
	for _, r := range a.records() {
		if r.Decision == decision {
			out = append(out, r)
		}
	}
	return out
}

func auditing(t *testing.T) (*server, *spyAudit) {
	t.Helper()
	s := newTestServer(t)
	a := &spyAudit{}
	s.audit = a
	return s, a
}

// TestDenialsAreRecorded covers the audit trail's reason for existing: a refusal that leaves no
// trace cannot be reviewed or disputed afterwards.
func TestDenialsAreRecorded(t *testing.T) {
	s, audit := auditing(t)
	over := s.limits.PerCallCapTinybar/3 + 100
	pid, done := connectProvider(t, s, 3, answerWith("r", over))
	defer done()

	if w := askQuote(t, s, pid, "x", testBuyer); w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403: %s", w.Code, w.Body.String())
	}

	denials := audit.find(hcs.DecisionDeny)
	if len(denials) != 1 {
		t.Fatalf("recorded %d denials, want 1: %+v", len(denials), audit.records())
	}
	d := denials[0]
	if d.Rule != "per-call-cap" {
		t.Errorf("rule = %q", d.Rule)
	}
	if d.Phase != hcs.PhaseQuote {
		t.Errorf("phase = %q, want quote — a refusal now lands before any work is asked for", d.Phase)
	}
	if d.Buyer != testBuyer || d.PayTo == "" || d.Amount == 0 {
		t.Errorf("record does not identify the refused payment: %+v", d)
	}
	// A denial settled nothing, and must not imply otherwise.
	if d.Settled.Tx != "" {
		t.Errorf("a denial carries a transaction: %+v", d.Settled)
	}
}

// The same job is judged twice — against the ceiling, then against the real price — and both are
// decisions worth a record. Auditing inside evaluate is what guarantees neither is skipped.
func TestBothEvaluationsAreRecorded(t *testing.T) {
	s, audit := auditing(t)
	jobID, _ := billableJob(t, s, 10)

	if w := collect(t, s, jobID, testBuyer, ""); w.Code != http.StatusPaymentRequired {
		t.Fatalf("code = %d", w.Code)
	}

	var phases []string
	for _, r := range audit.find(hcs.DecisionAllow) {
		phases = append(phases, r.Phase)
	}
	if len(phases) < 2 {
		t.Fatalf("phases recorded = %v, want a dispatch and a collect decision", phases)
	}
	var sawDispatch, sawCollect bool
	for _, p := range phases {
		sawDispatch = sawDispatch || p == hcs.PhaseDispatch
		sawCollect = sawCollect || p == hcs.PhaseCollect
	}
	if !sawDispatch || !sawCollect {
		t.Errorf("phases = %v, want both dispatch and collect", phases)
	}
}

// TestSettlementRecordSeparatesLedgerFromClaim is the structural point. The transaction id is what
// the ledger stands behind; the unit count is what the provider said. They must never appear as
// peers, because a reader would have no way to tell which is which.
func TestSettlementRecordSeparatesLedgerFromClaim(t *testing.T) {
	s, audit := auditing(t)
	f := newFakeFacilitator(t)
	s.fac = x402.NewClient(f.srv.URL, 5*time.Second)

	pid, done := connectProvider(t, s, 3, quotesAt(10, func(f hub.Frame) *hub.Frame {
		if f.Type != hub.FrameJob {
			return nil
		}
		return &hub.Frame{Type: hub.FrameResult, JobID: f.JobID, Result: "the paid-for result", Units: 5000}
	}))
	defer done()
	jobID := quoteAndDispatch(t, s, pid, "x")
	awaitTerminal(t, s, jobID)

	c := challengeOf(t, collect(t, s, jobID, testBuyer, ""))
	if got := collect(t, s, jobID, testBuyer, payment(c.Accepts[0])); got.Code != http.StatusOK {
		t.Fatalf("collect: %d %s", got.Code, got.Body.String())
	}

	settled := audit.find(hcs.DecisionSettled)
	if len(settled) != 1 {
		t.Fatalf("recorded %d settlements, want 1", len(settled))
	}
	r := settled[0]
	if r.Settled.Tx == "" || r.Settled.Payer == "" || r.Settled.Network != network {
		t.Errorf("settled block incomplete: %+v", r.Settled)
	}
	// The provider claimed 5000 units; the buyer paid for 10. The record keeps the claim, under
	// declared, because the gap is the only cross-call evidence of inflation.
	if r.Declared.ReportedUnits != 5000 {
		t.Errorf("declared units = %d, want the provider's raw claim", r.Declared.ReportedUnits)
	}
	if r.Amount != 30 {
		t.Errorf("amount = %d, want the clamped price of 30", r.Amount)
	}
}

func TestFailedJobIsRecorded(t *testing.T) {
	s, audit := auditing(t)
	pid, done := connectProvider(t, s, 3, quotesAt(100, func(f hub.Frame) *hub.Frame {
		if f.Type != hub.FrameJob {
			return nil
		}
		return &hub.Frame{Type: hub.FrameFailed, JobID: f.JobID, Error: "the backend exploded"}
	}))
	defer done()

	jobID := quoteAndDispatch(t, s, pid, "x")
	awaitTerminal(t, s, jobID)

	failures := audit.find(hcs.DecisionFailed)
	if len(failures) != 1 {
		t.Fatalf("recorded %d failures, want 1: %+v", len(failures), audit.records())
	}
	if failures[0].JobID != jobID {
		t.Errorf("job id = %q, want %q", failures[0].JobID, jobID)
	}
	// Nothing settled, and the record must say so rather than leave it to inference.
	if failures[0].Settled.Tx != "" {
		t.Error("a failed job carries a transaction")
	}
	// A provider that broke is not a provider that refused. Both must exist in the log, distinctly,
	// or reputation cannot tell "unreliable" from "picky".
	if len(audit.find(hcs.DecisionDeclined)) != 0 {
		t.Error("a failure was recorded as a decline")
	}
}

// The exchange must take payments with no topic configured: an audit trail is a bonus criterion,
// not a dependency.
func TestAuditingOffStillTakesPayments(t *testing.T) {
	s := newTestServer(t) // its audit is hcs.Discard{}
	f := newFakeFacilitator(t)
	s.fac = x402.NewClient(f.srv.URL, 5*time.Second)
	jobID, _ := billableJob(t, s, 10)

	c := challengeOf(t, collect(t, s, jobID, testBuyer, ""))
	if w := collect(t, s, jobID, testBuyer, payment(c.Accepts[0])); w.Code != http.StatusOK {
		t.Fatalf("payment failed with auditing off: %d %s", w.Code, w.Body.String())
	}
}
