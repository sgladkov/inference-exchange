package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// permissive is a Limits with every ceiling off, so a test can switch on exactly one rule and know
// that is what fired.
var permissive = Limits{}

func decode(t *testing.T, reason string) (prefix string, evidence map[string]any) {
	t.Helper()
	i := strings.Index(reason, " ")
	if i < 0 {
		return reason, nil
	}
	prefix = reason[:i]
	if err := json.Unmarshal([]byte(reason[i+1:]), &evidence); err != nil {
		t.Fatalf("evidence after %q is not JSON: %v\nreason: %s", prefix, err, reason)
	}
	return prefix, evidence
}

func TestAllowsWhenNothingIsConfigured(t *testing.T) {
	d := Evaluate(permissive, Request{Amount: 1 << 40, CallsInWindow: 10_000})
	if d.Deny {
		t.Errorf("denied with no limits set: %s", d.Reason)
	}
	if d.Rule != "" || d.Reason != "" {
		t.Errorf("an allow carried a rule or reason: %+v", d)
	}
}

func TestPerCallCap(t *testing.T) {
	l := Limits{PerCallCapTinybar: 500}
	if d := Evaluate(l, Request{Amount: 500}); d.Deny {
		t.Error("denied a payment exactly at the cap; the cap is inclusive")
	}
	d := Evaluate(l, Request{Amount: 501})
	if !d.Deny || d.Rule != RulePerCallCap {
		t.Fatalf("over cap: %+v", d)
	}
	_, ev := decode(t, d.Reason)
	if ev["amount"] != float64(501) || ev["limit"] != float64(500) {
		t.Errorf("evidence = %v, want the amount and the limit", ev)
	}
}

func TestDailyBudgetCountsSpendToDate(t *testing.T) {
	l := Limits{DailyBudgetTinybar: 1000}
	if d := Evaluate(l, Request{Amount: 400, SpendToday: 600}); d.Deny {
		t.Error("denied a payment that lands exactly on the budget")
	}
	d := Evaluate(l, Request{Amount: 401, SpendToday: 600})
	if !d.Deny || d.Rule != RuleDailyBudget {
		t.Fatalf("over budget: %+v", d)
	}
	_, ev := decode(t, d.Reason)
	if ev["spent"] != float64(600) {
		t.Errorf("evidence should show spend to date, got %v", ev)
	}
}

// TestVelocityIsAtOrOver pins the boundary: the limit is how many calls are allowed in the window,
// so the Nth call is fine and the N+1th is not. CallsInWindow already counts the calls before this
// one, which is why the comparison is >= rather than >.
func TestVelocityIsAtOrOver(t *testing.T) {
	l := Limits{VelocityCalls: 3, VelocityWindow: time.Minute}
	if d := Evaluate(l, Request{CallsInWindow: 2}); d.Deny {
		t.Error("denied the third call in a window of three")
	}
	d := Evaluate(l, Request{CallsInWindow: 3})
	if !d.Deny || d.Rule != RuleVelocity {
		t.Fatalf("fourth call: %+v", d)
	}
	_, ev := decode(t, d.Reason)
	if ev["window_seconds"] != float64(60) {
		t.Errorf("window not reported in the evidence: %v", ev)
	}
}

func TestAllowlist(t *testing.T) {
	l := Limits{Allowlist: []string{"prov-good", "0.0.999"}}

	if d := Evaluate(l, Request{ProviderID: "prov-good", PayTo: "0.0.1"}); d.Deny {
		t.Error("denied a provider listed by id")
	}
	if d := Evaluate(l, Request{ProviderID: "prov-other", PayTo: "0.0.999"}); d.Deny {
		t.Error("denied a provider listed by payee account")
	}
	d := Evaluate(l, Request{ProviderID: "prov-evil", PayTo: "0.0.666"})
	if !d.Deny || d.Rule != RuleAllowlist {
		t.Fatalf("unlisted provider: %+v", d)
	}
	// An empty allowlist means "no allowlist", not "allow nothing" — otherwise the default
	// configuration would deny every payment.
	if d := Evaluate(Limits{}, Request{ProviderID: "anyone"}); d.Deny {
		t.Error("an empty allowlist denied everything")
	}
}

// TestUnprovenProviderCap covers the bounded-detection-latency rule: inflation only shows up
// across many calls, so cumulative spend with a provider is capped until enough calls exist for
// the comparison to mean anything.
func TestUnprovenProviderCap(t *testing.T) {
	l := Limits{UnprovenSpendCap: 1000, UnprovenAfterCalls: 10}

	if d := Evaluate(l, Request{Amount: 400, SpendWithProv: 600, CallsWithProv: 2}); d.Deny {
		t.Error("denied a payment landing exactly on the unproven cap")
	}
	d := Evaluate(l, Request{Amount: 401, SpendWithProv: 600, CallsWithProv: 2})
	if !d.Deny || d.Rule != RuleUnprovenProvider {
		t.Fatalf("over the unproven cap: %+v", d)
	}
	_, ev := decode(t, d.Reason)
	if ev["proven_at"] != float64(10) || ev["calls_with"] != float64(2) {
		t.Errorf("evidence should say how far off proven the provider is: %v", ev)
	}

	// Once proven, the cap stops applying and the daily budget takes over.
	if d := Evaluate(l, Request{Amount: 100_000, SpendWithProv: 600, CallsWithProv: 10}); d.Deny {
		t.Errorf("a proven provider was still capped: %s", d.Reason)
	}
}

// TestAbandonmentRatio is what makes free dispatch safe: a caller that repeatedly commissions work
// and never pays for it burns provider compute at no cost to itself.
func TestAbandonmentRatio(t *testing.T) {
	l := Limits{MaxAbandonRatio: 0.5, MinJobsBeforeRatio: 4}

	// Below the sample floor, even total abandonment is tolerated — a first-time buyer should not
	// be judged on one data point.
	if d := Evaluate(l, Request{Abandoned: 3, CompletedTotal: 3}); d.Deny {
		t.Error("judged a buyer before the minimum sample")
	}
	if d := Evaluate(l, Request{Abandoned: 2, CompletedTotal: 4}); d.Deny {
		t.Error("denied at exactly the ratio; the limit is inclusive")
	}
	d := Evaluate(l, Request{Abandoned: 3, CompletedTotal: 4})
	if !d.Deny || d.Rule != RuleAbandonment {
		t.Fatalf("over the abandon ratio: %+v", d)
	}
	_, ev := decode(t, d.Reason)
	if ev["abandoned"] != float64(3) || ev["completed"] != float64(4) {
		t.Errorf("evidence = %v", ev)
	}
}

// TestFirstDenialWins fixes the order. It is not cosmetic: the rule name is what the calling agent
// branches on, so a request breaking several limits must always report the same one.
func TestFirstDenialWins(t *testing.T) {
	l := Limits{
		PerCallCapTinybar: 100, DailyBudgetTinybar: 100,
		VelocityCalls: 1, VelocityWindow: time.Minute,
		UnprovenSpendCap: 10, UnprovenAfterCalls: 5,
		MaxAbandonRatio: 0.1, MinJobsBeforeRatio: 1,
		Allowlist: []string{"prov-allowed"},
	}
	r := Request{
		ProviderID: "prov-blocked", Amount: 1_000_000, SpendToday: 1_000_000,
		CallsInWindow: 99, SpendWithProv: 1_000_000, CallsWithProv: 0,
		Abandoned: 10, CompletedTotal: 10,
	}
	if d := Evaluate(l, r); d.Rule != RuleAllowlist {
		t.Errorf("rule = %q, want the allowlist to win over everything", d.Rule)
	}

	r.ProviderID = "prov-allowed"
	if d := Evaluate(l, r); d.Rule != RulePerCallCap {
		t.Errorf("rule = %q, want the per-call cap next", d.Rule)
	}

	r.Amount = 1
	if d := Evaluate(l, r); d.Rule != RuleDailyBudget {
		t.Errorf("rule = %q, want the daily budget next", d.Rule)
	}

	r.SpendToday = 0
	if d := Evaluate(l, r); d.Rule != RuleVelocity {
		t.Errorf("rule = %q, want velocity next", d.Rule)
	}

	r.CallsInWindow = 0
	if d := Evaluate(l, r); d.Rule != RuleUnprovenProvider {
		t.Errorf("rule = %q, want the unproven cap next", d.Rule)
	}

	r.SpendWithProv = 0
	if d := Evaluate(l, r); d.Rule != RuleAbandonment {
		t.Errorf("rule = %q, want abandonment last", d.Rule)
	}
}

// TestEvidenceAlwaysCarriesTheRule matters because the caller reads the rule out of the JSON, not
// out of the Decision struct, which does not survive the wire.
func TestEvidenceAlwaysCarriesTheRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		l    Limits
		r    Request
	}{
		{"cap", Limits{PerCallCapTinybar: 1}, Request{Amount: 2}},
		{"budget", Limits{DailyBudgetTinybar: 1}, Request{Amount: 2}},
		{"velocity", Limits{VelocityCalls: 1, VelocityWindow: time.Second}, Request{CallsInWindow: 1}},
		{"allowlist", Limits{Allowlist: []string{"x"}}, Request{ProviderID: "y"}},
		{"unproven", Limits{UnprovenSpendCap: 1, UnprovenAfterCalls: 5}, Request{Amount: 2}},
		{"abandon", Limits{MaxAbandonRatio: 0.1, MinJobsBeforeRatio: 1}, Request{Abandoned: 1, CompletedTotal: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Evaluate(tc.l, tc.r)
			if !d.Deny {
				t.Fatal("expected a denial")
			}
			prefix, ev := decode(t, d.Reason)
			if prefix != PrefixDenied {
				t.Errorf("prefix = %q, want %q", prefix, PrefixDenied)
			}
			if ev["rule"] != d.Rule {
				t.Errorf("evidence rule = %v, struct rule = %q; the wire form is what the caller sees",
					ev["rule"], d.Rule)
			}
		})
	}
}

// TestPrefixesCannotCollideWithTheFacilitator guards the whole denial-legibility scheme. Our
// denials and the facilitator's arrive through the same field, and a caller must be able to tell
// "your policy stopped this" from "the payment itself was bad".
func TestPrefixesCannotCollideWithTheFacilitator(t *testing.T) {
	ours := []string{PrefixDenied, PrefixUpstream, PrefixJobFailed, PrefixJobRunning, PrefixJobExpired, PrefixNotBillable}

	// Captured verbatim from the live facilitator during probing.
	theirs := []string{
		"invalid_exact_hedera_payload_amount_mismatch",
		"invalid_exact_hedera_payload_extra_positive_transfers",
		"invalid_exact_hedera_payload_preflight_failed",
	}

	seen := map[string]bool{}
	for _, p := range ours {
		if seen[p] {
			t.Errorf("two of our prefixes are identical: %q", p)
		}
		seen[p] = true
		if strings.ContainsAny(p, " {}\"") {
			t.Errorf("prefix %q contains a character that breaks `PREFIX {json}` parsing", p)
		}
		for _, f := range theirs {
			if strings.HasPrefix(f, p) || strings.HasPrefix(p, f) {
				t.Errorf("our prefix %q collides with the facilitator's %q", p, f)
			}
		}
	}
}

func TestEncode(t *testing.T) {
	if got := Encode(PrefixUpstream, nil); got != PrefixUpstream {
		t.Errorf("nil evidence should give a bare prefix, got %q", got)
	}
	got := Encode(PrefixJobFailed, map[string]any{"job_id": "job-1"})
	prefix, ev := decode(t, got)
	if prefix != PrefixJobFailed || ev["job_id"] != "job-1" {
		t.Errorf("Encode = %q", got)
	}
	// Quotes must survive: the caller parses the JSON that follows the prefix.
	if !strings.Contains(got, `"job_id":"job-1"`) {
		t.Errorf("evidence was mangled: %q", got)
	}
}

func TestDefaultsAreUsable(t *testing.T) {
	l := Defaults()
	ordinary := Request{ProviderID: "prov-1", Amount: 500, CallsWithProv: 0}
	if d := Evaluate(l, ordinary); d.Deny {
		t.Errorf("the default limits deny an ordinary first payment: %s", d.Reason)
	}
	if d := Evaluate(l, Request{Amount: l.PerCallCapTinybar + 1}); !d.Deny {
		t.Error("the default per-call cap does not bite")
	}
}
