package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cryptoscruffy/inference-exchange/registry/internal/x402"
)

func spendOf(t *testing.T, s *server, buyer string) map[string]any {
	t.Helper()
	h := map[string]string{}
	if buyer != "" {
		h[buyerHeader] = buyer
	}
	w := do(t, s, "GET", "/me/spend", nil, h)
	if w.Code != http.StatusOK {
		t.Fatalf("spend: %d %s", w.Code, w.Body.String())
	}
	return decodeBody(t, w)
}

// buyAndSettle runs one job all the way through payment, so the spend report has something real
// behind it rather than hand-written store state.
func buyAndSettle(t *testing.T, s *server, units int64) int64 {
	t.Helper()
	pid, done := connectProvider(t, s, 3, answerWith("result", units))
	t.Cleanup(done)

	jobID := quoteAndDispatch(t, s, pid, "x")
	st := awaitTerminal(t, s, jobID)

	c := challengeOf(t, collect(t, s, jobID, testBuyer, ""))
	if got := collect(t, s, jobID, testBuyer, payment(c.Accepts[0])); got.Code != http.StatusOK {
		t.Fatalf("collect: %d %s", got.Code, got.Body.String())
	}
	return int64(st["price_tinybar"].(float64))
}

func TestSpendRequiresABuyer(t *testing.T) {
	s := newTestServer(t)
	if w := do(t, s, "GET", "/me/spend", nil, nil); w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestSpendStartsEmpty(t *testing.T) {
	s := newTestServer(t)
	b := spendOf(t, s, testBuyer)

	day := b["day"].(map[string]any)
	if day["spent_tinybar"] != float64(0) {
		t.Errorf("spent = %v, want 0", day["spent_tinybar"])
	}
	// A new buyer has the whole budget, and the report should say so rather than make the caller
	// subtract.
	if day["remaining_tinybar"] != day["budget_tinybar"] {
		t.Errorf("remaining %v != budget %v for a buyer who has spent nothing",
			day["remaining_tinybar"], day["budget_tinybar"])
	}
	if got := b["by_provider"]; got != nil {
		if list, ok := got.([]any); ok && len(list) != 0 {
			t.Errorf("by_provider = %v, want empty", got)
		}
	}
}

// TestSpendCountsOnlySettledJobs is the substance of the report: dispatch is free, so a job that
// was never collected cost the buyer nothing and must not appear as spend.
func TestSpendCountsOnlySettledJobs(t *testing.T) {
	s := newTestServer(t)
	f := newFakeFacilitator(t)
	s.fac = x402.NewClient(f.srv.URL, 5*time.Second)

	price := buyAndSettle(t, s, 10)

	// A second job that completes but is never paid for.
	pid, done := connectProvider(t, s, 3, answerWith("result", 10))
	defer done()
	awaitTerminal(t, s, quoteAndDispatch(t, s, pid, "x"))

	b := spendOf(t, s, testBuyer)
	day := b["day"].(map[string]any)
	if day["spent_tinybar"] != float64(price) {
		t.Errorf("spent = %v, want only the settled job's %d", day["spent_tinybar"], price)
	}
	// Both jobs are still calls against the velocity limit, because dispatching is the thing being
	// rate limited.
	if day["jobs"] != float64(2) {
		t.Errorf("jobs = %v, want 2 dispatches counted", day["jobs"])
	}
}

func TestSpendReportsHeadroom(t *testing.T) {
	s := newTestServer(t)
	f := newFakeFacilitator(t)
	s.fac = x402.NewClient(f.srv.URL, 5*time.Second)
	price := buyAndSettle(t, s, 10)

	day := spendOf(t, s, testBuyer)["day"].(map[string]any)
	budget := int64(day["budget_tinybar"].(float64))
	if got := int64(day["remaining_tinybar"].(float64)); got != budget-price {
		t.Errorf("remaining = %d, want %d", got, budget-price)
	}
}

// An unset limit is unlimited, and must not be reported as a large but finite headroom — an agent
// reading a number would treat it as a real ceiling.
func TestUnlimitedBudgetIsNotANumber(t *testing.T) {
	s := newTestServer(t)
	s.limits.DailyBudgetTinybar = 0

	day := spendOf(t, s, testBuyer)["day"].(map[string]any)
	if day["remaining_tinybar"] != float64(-1) {
		t.Errorf("remaining = %v, want -1 to mean unlimited", day["remaining_tinybar"])
	}
}

func TestSpendBreaksDownByProvider(t *testing.T) {
	s := newTestServer(t)
	f := newFakeFacilitator(t)
	s.fac = x402.NewClient(f.srv.URL, 5*time.Second)

	price := buyAndSettle(t, s, 10)

	list := spendOf(t, s, testBuyer)["by_provider"].([]any)
	if len(list) != 1 {
		t.Fatalf("by_provider has %d entries, want 1", len(list))
	}
	row := list[0].(map[string]any)
	if row["spend_tinybar"] != float64(price) || row["calls"] != float64(1) {
		t.Errorf("row = %v", row)
	}
	if row["provider_id"] == "" {
		t.Error("row does not name the provider")
	}
}

// One buyer must never see another's spending, and the endpoint takes the buyer from the header
// rather than a query parameter for exactly that reason.
func TestSpendIsScopedToTheBuyer(t *testing.T) {
	s := newTestServer(t)
	f := newFakeFacilitator(t)
	s.fac = x402.NewClient(f.srv.URL, 5*time.Second)
	buyAndSettle(t, s, 10)

	other := spendOf(t, s, "0.0.9999")
	day := other["day"].(map[string]any)
	if day["spent_tinybar"] != float64(0) {
		t.Errorf("another buyer sees %v of spending", day["spent_tinybar"])
	}
	if list, ok := other["by_provider"].([]any); ok && len(list) != 0 {
		t.Errorf("another buyer sees %d provider rows", len(list))
	}
}

// The report is the registry's own accounting, and says so. Presenting it as neutral fact would
// overclaim: unlike the decision log, there is no independent source to check it against.
func TestSpendDeclaresItsOwnProvenance(t *testing.T) {
	s := newTestServer(t)
	src, _ := spendOf(t, s, testBuyer)["source"].(string)
	if src == "" {
		t.Fatal("the report does not say where its numbers come from")
	}
	for _, want := range []string{"registry-side", "verifiable"} {
		if !strings.Contains(src, want) {
			t.Errorf("source = %q, want it to mention %q", src, want)
		}
	}
}

func TestSpendReportsEveryLimitThePolicyEnforces(t *testing.T) {
	s := newTestServer(t)
	b := spendOf(t, s, testBuyer)

	// Every ceiling that can refuse a dispatch should be visible here, or an agent cannot tell why
	// it is about to be refused.
	for _, path := range [][]string{
		{"day", "budget_tinybar"},
		{"velocity", "limit"},
		{"velocity", "window_seconds"},
		{"abandonment", "limit_ratio"},
		{"unproven_provider", "cap_tinybar"},
	} {
		node := b
		for i, key := range path {
			v, ok := node[key]
			if !ok {
				t.Fatalf("missing %v", path)
			}
			if i < len(path)-1 {
				node = v.(map[string]any)
			}
		}
	}
	if _, ok := b["per_call_cap_tinybar"]; !ok {
		t.Error("per-call cap missing")
	}
}
