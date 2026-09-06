// Package hcs writes the registry's spend decisions to a Hedera Consensus Service topic.
//
// What this is: a consensus-ordered, tamper-evident audit trail of what the exchange decided and
// why. What it is not: proof that a decision was correct, or a write-ahead log. Records are
// submitted asynchronously after the decision, because an HCS submit takes one to two seconds and
// blocking every request on one would make the registry's availability depend on the topic's.
//
// That ordering is stated rather than glossed. A record establishes that a decision was written at
// a consensus timestamp; it does not establish that it preceded the action it describes.
package hcs

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	hiero "github.com/hiero-ledger/hiero-sdk-go/v2/sdk"
)

// Decision kinds. A record says which of these happened, so a reader never has to infer it from
// which fields are populated.
const (
	DecisionAllow   = "ALLOW"
	DecisionDeny    = "DENY"
	DecisionSettled = "SETTLED"
	DecisionFailed  = "FAILED"
	// A provider refused work at quote time. Recorded because it is a decision someone made about a
	// buyer, but it is the provider's decision, not the exchange's policy.
	DecisionDeclined = "DECLINED"
)

// Phases a decision can be made at. The same job is judged twice — once against the buyer's
// declared ceiling, once against the price the work actually came to — and the two are separate
// decisions with separate records.
const (
	PhaseQuote    = "quote"
	PhaseDispatch = "dispatch"
	PhaseCollect  = "collect"
)

// Settled holds only what the ledger stands behind.
type Settled struct {
	Tx      string `json:"tx"`
	Payer   string `json:"payer,omitempty"`
	Network string `json:"network,omitempty"`
}

// Declared holds only what the provider asserted. It is never verified by anything.
//
// The split from Settled is the point of this structure rather than a tidiness preference: a reader
// of the log must not be able to mistake a self-report for a settled fact, and separate objects
// make that hold in every rendering, including ones nobody has written yet.
type Declared struct {
	ProviderID    string `json:"provider_id,omitempty"`
	ReportedUnits int64  `json:"reported_units,omitempty"`
	Backend       string `json:"backend,omitempty"`
	Model         string `json:"model,omitempty"`
}

// Record is one decision.
type Record struct {
	V        int       `json:"v"`
	At       time.Time `json:"at"`
	Decision string    `json:"decision"`
	Phase    string    `json:"phase,omitempty"`
	Rule     string    `json:"rule,omitempty"`
	Reason   string    `json:"reason,omitempty"`

	JobID  string `json:"job_id,omitempty"`
	Buyer  string `json:"buyer,omitempty"`
	PayTo  string `json:"pay_to,omitempty"`
	Amount int64  `json:"amount_tinybar"`
	Asset  string `json:"asset"`

	Settled  Settled  `json:"settled"`
	Declared Declared `json:"declared"`
}

// Logger records decisions. The registry holds one and never checks whether it is real, so a
// deployment without a topic behaves identically except that nothing is published.
type Logger interface {
	Write(Record)
	Close()
}

// Discard is the no-op logger used when no topic is configured. The exchange must run without one:
// an audit trail is a bonus, not a dependency.
type Discard struct{}

func (Discard) Write(Record) {}
func (Discard) Close()       {}

// submitFunc publishes one already-encoded record. Injected so the queue can be tested without a
// network or an account.
type submitFunc func(ctx context.Context, payload []byte) (string, error)

// Topic publishes records to one HCS topic through a bounded background queue.
type Topic struct {
	TopicID string

	submit submitFunc
	log    *slog.Logger
	queue  chan Record
	wg     sync.WaitGroup
	closed chan struct{}
	once   sync.Once

	mu      sync.Mutex
	dropped int
}

// Options configure a Topic.
type Options struct {
	// QueueSize bounds how many records may be waiting. Once full, new records are dropped and
	// counted rather than blocking a request: a slow topic must not become a slow exchange.
	QueueSize int
	// Timeout bounds one submit.
	Timeout time.Duration
}

// New builds a Topic from Hedera credentials and starts its worker.
//
// operatorKey signs the submissions and pays for them, so this is the one place the registry holds
// a private key — for its own audit trail, never for anyone's payments. Payments settle
// buyer → provider directly and the registry is never custodial.
func New(network, topicID, operatorID, operatorKey string, log *slog.Logger, opts Options) (*Topic, error) {
	var client *hiero.Client
	switch network {
	case "hedera:testnet", "testnet":
		client = hiero.ClientForTestnet()
	case "hedera:mainnet", "mainnet":
		client = hiero.ClientForMainnet()
	default:
		return nil, fmt.Errorf("unknown network %q", network)
	}

	id, err := hiero.AccountIDFromString(operatorID)
	if err != nil {
		return nil, fmt.Errorf("operator account id: %w", err)
	}
	key, err := parseKey(operatorKey)
	if err != nil {
		return nil, fmt.Errorf("operator key: %w", err)
	}
	client.SetOperator(id, key)

	topic, err := hiero.TopicIDFromString(topicID)
	if err != nil {
		return nil, fmt.Errorf("topic id: %w", err)
	}

	submit := func(ctx context.Context, payload []byte) (string, error) {
		resp, err := hiero.NewTopicMessageSubmitTransaction().
			SetTopicID(topic).
			SetMessage(payload).
			Execute(client)
		if err != nil {
			return "", err
		}
		receipt, err := resp.GetReceipt(client)
		if err != nil {
			return "", err
		}
		_ = receipt
		return resp.TransactionID.String(), nil
	}
	return NewWithSubmit(topicID, submit, log, opts), nil
}

// parseKey accepts the key forms an operator is likely to have on hand.
//
// The Go SDK's PrivateKeyFromString rejects a leading "0x", which the JavaScript SDK accepts and
// which is how ECDSA keys are usually written down — including in the .env files this project uses
// on both sides. Rather than make the operator remember which half of the stack wants which form,
// strip it and try raw ECDSA bytes before falling back to the SDK's own parsing (DER, Ed25519).
func parseKey(s string) (hiero.PrivateKey, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if b, err := hex.DecodeString(trimmed); err == nil && len(b) == 32 {
		if k, err := hiero.PrivateKeyFromBytesECDSA(b); err == nil {
			return k, nil
		}
	}
	return hiero.PrivateKeyFromString(trimmed)
}

// NewWithSubmit builds a Topic around an arbitrary publish function.
func NewWithSubmit(topicID string, submit submitFunc, log *slog.Logger, opts Options) *Topic {
	if opts.QueueSize <= 0 {
		opts.QueueSize = 256
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	t := &Topic{
		TopicID: topicID,
		submit:  submit,
		log:     log,
		queue:   make(chan Record, opts.QueueSize),
		closed:  make(chan struct{}),
	}
	t.wg.Add(1)
	go t.run(opts.Timeout)
	return t
}

// Write queues a record. It never blocks: a full queue drops the record and counts it, because the
// audit trail must not be able to stall the payment path it is auditing.
func (t *Topic) Write(r Record) {
	if r.V == 0 {
		r.V = 1
	}
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	if r.Asset == "" {
		r.Asset = "0.0.0" // HBAR
	}

	select {
	case <-t.closed:
		return
	default:
	}

	select {
	case t.queue <- r:
	default:
		t.mu.Lock()
		t.dropped++
		n := t.dropped
		t.mu.Unlock()
		t.log.Warn("hcs queue full, dropped a decision record", "total_dropped", n, "job", r.JobID)
	}
}

// Dropped reports how many records were discarded because the queue was full. Exposed so the gap
// is visible rather than silent: a log with holes in it should say so.
func (t *Topic) Dropped() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dropped
}

func (t *Topic) run(timeout time.Duration) {
	defer t.wg.Done()
	for r := range t.queue {
		payload, err := json.Marshal(r)
		if err != nil {
			t.log.Warn("could not encode a decision record", "err", err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		txID, err := t.submit(ctx, payload)
		cancel()
		if err != nil {
			// A failed submit loses that record. It is not retried: the payment already happened
			// either way, and a retry queue would trade a visible gap for an invisible backlog.
			t.log.Warn("could not publish a decision record", "err", err, "job", r.JobID)
			continue
		}
		t.log.Debug("decision recorded", "topic", t.TopicID, "tx", txID,
			"decision", r.Decision, "job", r.JobID)
	}
}

// Close stops accepting records and waits for the queue to drain.
func (t *Topic) Close() {
	t.once.Do(func() {
		close(t.closed)
		close(t.queue)
		t.wg.Wait()
	})
}
