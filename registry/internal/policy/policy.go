// Package policy is the spend control, evaluated inside the registry.
//
// It lives here rather than in the buyer's client because the client is installed inside a host
// agent application that must be assumed compromised: its task comes from its inputs and its
// inputs may be attacker-controlled. A control the governed party can edit is advisory. The
// registry is the only x402 resource server in the system, so every payment passes this by
// construction rather than by anyone's cooperation.
//
// Denials are packed into a reason string because that is the only field that survives to the
// calling agent: the 402 body is empty and every structured field is dropped in transit.
package policy

import (
	"encoding/json"
	"fmt"
	"time"
)

// Rule names, carried inside the denial so a machine caller can branch on them.
const (
	RulePerCallCap       = "per-call-cap"
	RuleAllowlist        = "provider-allowlist"
	RuleDailyBudget      = "daily-budget"
	RuleVelocity         = "velocity"
	RuleUnprovenProvider = "unproven-provider-cap"
	RuleAbandonment      = "abandoned-job-ratio"
	RulePayerMismatch    = "payer-mismatch"
	RuleAcceptedMismatch = "accepted-mismatch"
)

// Reason prefixes. Namespaced so they cannot collide with the facilitator's own strings, which are
// all of the form invalid_exact_hedera_*. A caller matches on the prefix and reads the JSON that
// follows it.
const (
	PrefixDenied      = "FIREWALL_DENIED"
	PrefixUpstream    = "FIREWALL_UPSTREAM_UNAVAILABLE"
	PrefixJobFailed   = "JOB_FAILED"
	PrefixJobRunning  = "JOB_RUNNING"
	PrefixJobExpired  = "JOB_EXPIRED"
	PrefixNotBillable = "JOB_NOT_BILLABLE"
)

// Limits is the configured policy. Zero means unlimited for every ceiling.
type Limits struct {
	PerCallCapTinybar  int64         `json:"per_call_cap_tinybar"`
	DailyBudgetTinybar int64         `json:"daily_budget_tinybar"`
	VelocityCalls      int           `json:"velocity_calls"`
	VelocityWindow     time.Duration `json:"velocity_window"`

	// UnprovenSpendCap bounds cumulative spend with a provider until it has accumulated
	// UnprovenAfterCalls settled calls. Inflation is only detectable across many calls, so this
	// converts an unbounded detection latency into a bounded loss.
	UnprovenSpendCap   int64 `json:"unproven_spend_cap_tinybar"`
	UnprovenAfterCalls int   `json:"unproven_after_calls"`

	// MaxAbandonRatio bounds the share of a buyer's completed jobs that were never collected.
	// Dispatch is free, so without this a caller can burn provider compute at no cost. It applies
	// only after MinJobsBeforeRatio jobs, so a first-time buyer is not judged on one sample.
	MaxAbandonRatio    float64 `json:"max_abandon_ratio"`
	MinJobsBeforeRatio int     `json:"min_jobs_before_ratio"`

	Allowlist []string `json:"allowlist"`
}

// Defaults are deliberately loose enough to demo and tight enough to deny visibly.
func Defaults() Limits {
	return Limits{
		PerCallCapTinybar:  10_000,
		DailyBudgetTinybar: 100_000,
		VelocityCalls:      30,
		VelocityWindow:     time.Minute,
		UnprovenSpendCap:   5_000,
		UnprovenAfterCalls: 10,
		MaxAbandonRatio:    0.5,
		MinJobsBeforeRatio: 4,
	}
}

// Request is what a decision is made about.
type Request struct {
	Buyer      string
	ProviderID string
	PayTo      string
	Amount     int64 // tinybar

	SpendToday     int64
	CallsInWindow  int
	SpendWithProv  int64
	CallsWithProv  int
	Abandoned      int
	CompletedTotal int
}

// Decision is the outcome. Deny carries a reason the caller can act on.
type Decision struct {
	Deny   bool
	Rule   string
	Reason string
}

// Evaluate runs the rules in order; the first denial wins.
//
// It is called at dispatch (against the buyer's declared ceiling) and again at collect (against
// the actual price). Two separate calls, two separate decisions: nothing is carried forward,
// because the amount can differ between them and the budget can have moved.
func Evaluate(l Limits, r Request) Decision {
	if len(l.Allowlist) > 0 {
		ok := false
		for _, a := range l.Allowlist {
			if a == r.ProviderID || a == r.PayTo {
				ok = true
				break
			}
		}
		if !ok {
			return deny(RuleAllowlist, map[string]any{
				"provider": r.ProviderID, "payTo": r.PayTo})
		}
	}
	if l.PerCallCapTinybar > 0 && r.Amount > l.PerCallCapTinybar {
		return deny(RulePerCallCap, map[string]any{
			"amount": r.Amount, "limit": l.PerCallCapTinybar})
	}
	if l.DailyBudgetTinybar > 0 && r.SpendToday+r.Amount > l.DailyBudgetTinybar {
		return deny(RuleDailyBudget, map[string]any{
			"spent": r.SpendToday, "amount": r.Amount, "limit": l.DailyBudgetTinybar})
	}
	if l.VelocityCalls > 0 && r.CallsInWindow >= l.VelocityCalls {
		return deny(RuleVelocity, map[string]any{
			"calls": r.CallsInWindow, "limit": l.VelocityCalls,
			"window_seconds": int(l.VelocityWindow.Seconds())})
	}
	if l.UnprovenSpendCap > 0 && r.CallsWithProv < l.UnprovenAfterCalls &&
		r.SpendWithProv+r.Amount > l.UnprovenSpendCap {
		return deny(RuleUnprovenProvider, map[string]any{
			"provider": r.ProviderID, "spent_with": r.SpendWithProv,
			"amount": r.Amount, "limit": l.UnprovenSpendCap,
			"calls_with": r.CallsWithProv, "proven_at": l.UnprovenAfterCalls})
	}
	if l.MaxAbandonRatio > 0 && r.CompletedTotal >= l.MinJobsBeforeRatio {
		ratio := float64(r.Abandoned) / float64(r.CompletedTotal)
		if ratio > l.MaxAbandonRatio {
			return deny(RuleAbandonment, map[string]any{
				"abandoned": r.Abandoned, "completed": r.CompletedTotal,
				"ratio": ratio, "limit": l.MaxAbandonRatio})
		}
	}
	return Decision{}
}

func deny(rule string, evidence map[string]any) Decision {
	evidence["rule"] = rule
	return Decision{Deny: true, Rule: rule, Reason: Encode(PrefixDenied, evidence)}
}

// Encode packs a prefix and evidence into the single string that survives to the caller.
//
// The shape is `PREFIX {json}` — a stable token a machine matches on, followed by structured
// detail. Verified to survive the transport verbatim, quotes intact.
func Encode(prefix string, evidence map[string]any) string {
	if evidence == nil {
		return prefix
	}
	b, err := json.Marshal(evidence)
	if err != nil {
		return prefix
	}
	return fmt.Sprintf("%s %s", prefix, b)
}
