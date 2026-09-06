// Package store holds the registry's state: registered providers, and the jobs they run.
//
// In-memory and mutex-guarded. That is a deliberate scope choice, not an oversight: nothing here
// is custodial — payments settle buyer→provider directly, so the registry never holds funds — and
// a restart costs at most the jobs currently in flight. Persistence would be the first change if
// this outlived the event.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrNoSuchProvider = errors.New("no such provider")
	ErrNoSuchJob      = errors.New("no such job")
	ErrNotYours       = errors.New("job belongs to a different buyer")
)

// Provider is a registered seller. It never listens on a port: it dials the registry and holds the
// connection open, which is what lets a machine behind NAT sell.
//
// AccountID is supplied by the provider at registration and is where its payments land. The
// registry never sees a private key and never holds funds on a provider's behalf.
type Provider struct {
	ID          string `json:"provider_id"`
	AccountID   string `json:"pay_to"`
	PublicKey   string `json:"-"`
	DisplayName string `json:"display_name"`
	Capability  string `json:"capability"`

	// RatePerUnit is tinybar per unit of work, quoted by the provider at registration.
	RatePerUnit int64 `json:"rate_per_unit"`

	// Declared is provider-asserted and never verified. The field name is the mechanism: it keeps
	// self-reported claims visibly separate from settled facts wherever a listing is rendered.
	Declared map[string]any `json:"declared"`

	Online       bool      `json:"online"`
	RegisteredAt time.Time `json:"registered_at"`
	LastSeen     time.Time `json:"last_seen"`
}

// JobState is the lifecycle of one delegated task.
//
// The payment sits at the collect step, after the work is finished, so no Hedera transaction is
// ever in flight while a job runs. That is what removes the 180-second transaction validity
// ceiling from job duration.
type JobState string

const (
	JobDispatched JobState = "dispatched" // accepted, not yet picked up
	JobRunning    JobState = "running"    // provider is working
	JobCompleted  JobState = "completed"  // result held, awaiting payment
	JobCollected  JobState = "collected"  // paid and delivered
	JobFailed     JobState = "failed"     // provider errored — never billable
	JobExpired    JobState = "expired"    // TTL passed uncollected
)

// Terminal reports whether a job can still change state.
func (s JobState) Terminal() bool {
	return s == JobCollected || s == JobFailed || s == JobExpired
}

// Job is one delegated task.
//
// MaxUnits is the buyer's ceiling, declared at dispatch and never quoted by the provider. It is
// the whole of the metering defence: the provider both performs the work and counts the units it
// bills for, so the design bounds what a dishonest count can cost rather than trying to make it
// honest.
type Job struct {
	ID           string   `json:"job_id"`
	ProviderID   string   `json:"provider_id"`
	BuyerAccount string   `json:"buyer"`
	Prompt       string   `json:"-"`
	MaxUnits     int64    `json:"max_units"`
	State        JobState `json:"state"`

	// Reported is what the provider says it used. Priced is what the buyer is charged, which is
	// Reported clamped to MaxUnits. Both are kept: the gap is the only evidence of inflation
	// available across calls, and it is worthless unless retained per job.
	Reported int64 `json:"reported_units"`
	Priced   int64 `json:"priced_units"`
	Price    int64 `json:"price_tinybar"`

	Result string `json:"-"`
	Error  string `json:"error,omitempty"`
	TxID   string `json:"tx_id,omitempty"`

	CreatedAt   time.Time `json:"created_at"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Billable reports whether this job may be charged for. A failed job never produces a 402: the
// buyer gets the error for free. This is what preserves delivery-conditional payment once the work
// no longer sits between verify and settle.
func (j *Job) Billable() bool { return j.State == JobCompleted }

// Store is the registry's whole state.
type Store struct {
	mu        sync.RWMutex
	providers map[string]*Provider
	jobs      map[string]*Job
	byBuyer   map[string][]string // buyer account → job ids, for budget and abandonment checks
	ttl       time.Duration
}

func New(resultTTL time.Duration) *Store {
	if resultTTL == 0 {
		resultTTL = 30 * time.Minute
	}
	return &Store{
		providers: map[string]*Provider{},
		jobs:      map[string]*Job{},
		byBuyer:   map[string][]string{},
		ttl:       resultTTL,
	}
}

func id(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not a condition worth degrading into a guessable id for: job ids
		// are what stop one buyer collecting another's result.
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// --- providers -------------------------------------------------------------

// Register adds a provider. The registry stores its account id and public key; it never sees,
// generates, or holds a private key.
func (s *Store) Register(p *Provider) *Provider {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" {
		p.ID = id("prov")
	}
	p.RegisteredAt = time.Now()
	p.LastSeen = p.RegisteredAt
	s.providers[p.ID] = p
	return p
}

func (s *Store) Provider(pid string) (*Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.providers[pid]
	if !ok {
		return nil, ErrNoSuchProvider
	}
	cp := *p
	return &cp, nil
}

// Providers lists registered providers, optionally filtered.
func (s *Store) Providers(capability string, maxRate int64, onlineOnly bool) []*Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Provider, 0, len(s.providers))
	for _, p := range s.providers {
		if capability != "" && p.Capability != capability {
			continue
		}
		if maxRate > 0 && p.RatePerUnit > maxRate {
			continue
		}
		if onlineOnly && !p.Online {
			continue
		}
		cp := *p
		out = append(out, &cp)
	}
	return out
}

// SetOnline marks a provider connected or disconnected as its socket opens and closes.
func (s *Store) SetOnline(pid string, online bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.providers[pid]; ok {
		p.Online = online
		p.LastSeen = time.Now()
	}
}

// --- jobs ------------------------------------------------------------------

// CreateJob records a dispatched job. Free: no payment is taken at this point, which is what makes
// the abandonment rule in the policy package necessary.
func (s *Store) CreateJob(providerID, buyer, prompt string, maxUnits int64) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.providers[providerID]; !ok {
		return nil, ErrNoSuchProvider
	}
	now := time.Now()
	j := &Job{
		ID: id("job"), ProviderID: providerID, BuyerAccount: buyer,
		Prompt: prompt, MaxUnits: maxUnits, State: JobDispatched,
		CreatedAt: now, ExpiresAt: now.Add(s.ttl),
	}
	s.jobs[j.ID] = j
	s.byBuyer[buyer] = append(s.byBuyer[buyer], j.ID)
	return j, nil
}

// Job returns a job, enforcing that the caller owns it. Job ids are unguessable, but ownership is
// checked anyway rather than relying on that.
func (s *Store) Job(jobID, buyer string) (*Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrNoSuchJob
	}
	if buyer != "" && j.BuyerAccount != buyer {
		return nil, ErrNotYours
	}
	cp := *j
	return &cp, nil
}

func (s *Store) MarkRunning(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[jobID]; ok && j.State == JobDispatched {
		j.State = JobRunning
	}
}

// Complete stores a result and prices it.
//
// The price is computed from reported units clamped to the buyer's ceiling. Clamping rather than
// rejecting is deliberate: the buyer gets the work it asked for at the price it agreed to, and the
// over-report is retained as evidence instead of becoming a failed job.
func (s *Store) Complete(jobID string, reported int64, result string, rate int64) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return nil, ErrNoSuchJob
	}
	if j.State.Terminal() {
		return nil, fmt.Errorf("job %s already %s", jobID, j.State)
	}
	priced := reported
	if priced > j.MaxUnits {
		priced = j.MaxUnits
	}
	if priced < 0 {
		priced = 0
	}
	j.Reported, j.Priced, j.Price = reported, priced, priced*rate
	j.Result, j.State, j.CompletedAt = result, JobCompleted, time.Now()
	cp := *j
	return &cp, nil
}

// Fail marks a job failed. A failed job is never billable.
func (s *Store) Fail(jobID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[jobID]; ok && !j.State.Terminal() {
		j.State, j.Error, j.CompletedAt = JobFailed, reason, time.Now()
	}
}

// MarkCollected records settlement. Called only after the facilitator confirms.
func (s *Store) MarkCollected(jobID, txID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[jobID]; ok {
		j.State, j.TxID = JobCollected, txID
	}
}

// BuyerStats reports what the policy package needs about a buyer's recent behaviour: settled spend
// in the window, calls in the window, and how many completed jobs were never collected.
// Abandonment is the cost of free dispatch and the only thing that makes it safe.
func (s *Store) BuyerStats(buyer string, window time.Duration) (spend int64, calls int, abandoned, completed int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cutoff := time.Now().Add(-window)
	for _, jid := range s.byBuyer[buyer] {
		j, ok := s.jobs[jid]
		if !ok {
			continue
		}
		if j.CreatedAt.After(cutoff) {
			calls++
			if j.State == JobCollected {
				spend += j.Price
			}
		}
		switch j.State {
		case JobCollected:
			completed++
		case JobExpired:
			completed++
			abandoned++
		}
	}
	return spend, calls, abandoned, completed
}

// SpendWith reports total settled spend with one provider, for the unproven-provider cap.
func (s *Store) SpendWith(buyer, providerID string) (spend int64, calls int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, jid := range s.byBuyer[buyer] {
		if j, ok := s.jobs[jid]; ok && j.ProviderID == providerID && j.State == JobCollected {
			spend += j.Price
			calls++
		}
	}
	return spend, calls
}

// ProviderSpend is what one buyer has settled with one provider.
type ProviderSpend struct {
	ProviderID string `json:"provider_id"`
	Spend      int64  `json:"spend_tinybar"`
	Calls      int    `json:"calls"`
}

// SpendByProvider breaks a buyer's settled spend down by counterparty.
//
// Only providers the buyer has actually paid appear: a listing they never used is not part of their
// spending history, and padding the report with zeroes would bury the rows that matter.
func (s *Store) SpendByProvider(buyer string) []ProviderSpend {
	s.mu.RLock()
	defer s.mu.RUnlock()

	byProvider := map[string]*ProviderSpend{}
	for _, jid := range s.byBuyer[buyer] {
		j, ok := s.jobs[jid]
		if !ok || j.State != JobCollected {
			continue
		}
		e, seen := byProvider[j.ProviderID]
		if !seen {
			e = &ProviderSpend{ProviderID: j.ProviderID}
			byProvider[j.ProviderID] = e
		}
		e.Spend += j.Price
		e.Calls++
	}

	out := make([]ProviderSpend, 0, len(byProvider))
	for _, e := range byProvider {
		out = append(out, *e)
	}
	// Biggest spend first: the rows a buyer is deciding about are the expensive ones.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Spend != out[j].Spend {
			return out[i].Spend > out[j].Spend
		}
		return out[i].ProviderID < out[j].ProviderID
	})
	return out
}

// Sweep expires completed-but-uncollected jobs whose TTL has passed, freeing held results.
// Returns how many it expired.
func (s *Store) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	now := time.Now()
	for _, j := range s.jobs {
		if !j.State.Terminal() && now.After(j.ExpiresAt) {
			j.State, j.Result = JobExpired, ""
			n++
		}
	}
	return n
}
