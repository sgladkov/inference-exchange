package hcs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recorder captures what would have been published, so the queue is testable without a network,
// an account, or spending anything.
type recorder struct {
	mu      sync.Mutex
	got     [][]byte
	err     error
	release chan struct{} // when non-nil, submits block on it
}

func (r *recorder) submit(ctx context.Context, payload []byte) (string, error) {
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	r.got = append(r.got, append([]byte(nil), payload...))
	return "0.0.1@1.2", nil
}

func (r *recorder) records(t *testing.T) []Record {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Record, 0, len(r.got))
	for _, b := range r.got {
		var rec Record
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Fatalf("published payload is not a Record: %v\n%s", err, b)
		}
		out = append(out, rec)
	}
	return out
}

func TestWritesAndDrains(t *testing.T) {
	r := &recorder{}
	tp := NewWithSubmit("0.0.999", r.submit, quiet(), Options{})
	tp.Write(Record{Decision: DecisionDeny, Phase: PhaseDispatch, Rule: "per-call-cap", JobID: "job-1"})
	tp.Write(Record{Decision: DecisionSettled, Phase: PhaseCollect, JobID: "job-1",
		Settled: Settled{Tx: "0.0.7162784@1.2"}})
	tp.Close()

	got := r.records(t)
	if len(got) != 2 {
		t.Fatalf("published %d records, want 2", len(got))
	}
	if got[0].Rule != "per-call-cap" || got[1].Settled.Tx == "" {
		t.Errorf("records = %+v", got)
	}
}

// TestDefaultsAreFilled covers the fields a caller should not have to remember.
func TestDefaultsAreFilled(t *testing.T) {
	r := &recorder{}
	tp := NewWithSubmit("0.0.999", r.submit, quiet(), Options{})
	tp.Write(Record{Decision: DecisionAllow, JobID: "job-1"})
	tp.Close()

	got := r.records(t)[0]
	if got.V != 1 {
		t.Errorf("version = %d, want 1 so a reader can tell the shape", got.V)
	}
	if got.At.IsZero() {
		t.Error("no timestamp; a record with no time is not an audit trail")
	}
	if got.Asset != "0.0.0" {
		t.Errorf("asset = %q, want HBAR by default", got.Asset)
	}
}

// TestSettledAndDeclaredNeverMerge is the structural claim of this package. A reader must not be
// able to mistake what a provider asserted for what the ledger settled, and separate objects make
// that hold in every rendering rather than only in the ones written so far.
func TestSettledAndDeclaredNeverMerge(t *testing.T) {
	r := &recorder{}
	tp := NewWithSubmit("0.0.999", r.submit, quiet(), Options{})
	tp.Write(Record{
		Decision: DecisionSettled,
		JobID:    "job-1",
		Amount:   30,
		Settled:  Settled{Tx: "0.0.7162784@1.2", Payer: "0.0.1001", Network: "hedera:testnet"},
		Declared: Declared{ProviderID: "prov-1", ReportedUnits: 5000, Model: "claude-opus-5"},
	})
	tp.Close()

	var raw map[string]json.RawMessage
	r.mu.Lock()
	payload := r.got[0]
	r.mu.Unlock()
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"settled", "declared"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("record has no %q object: %s", k, payload)
		}
	}
	// The unverifiable fields must live only under declared, never alongside the settled ones.
	for _, leaked := range []string{"reported_units", "model", "provider_id"} {
		if _, ok := raw[leaked]; ok {
			t.Errorf("%q appears at the top level, next to settled facts", leaked)
		}
	}
	var settled map[string]any
	json.Unmarshal(raw["settled"], &settled)
	for _, leaked := range []string{"reported_units", "model"} {
		if _, ok := settled[leaked]; ok {
			t.Errorf("provider self-report %q leaked into the settled object", leaked)
		}
	}
	// And a denial must still carry an empty settled object rather than omitting it, so its absence
	// is never ambiguous.
	if !strings.Contains(string(payload), `"settled"`) {
		t.Error("settled omitted entirely")
	}
}

func TestDenialCarriesAnEmptySettled(t *testing.T) {
	r := &recorder{}
	tp := NewWithSubmit("0.0.999", r.submit, quiet(), Options{})
	tp.Write(Record{Decision: DecisionDeny, Rule: "daily-budget", JobID: "job-1"})
	tp.Close()

	got := r.records(t)[0]
	if got.Settled.Tx != "" {
		t.Errorf("a denial claims a settlement: %+v", got.Settled)
	}
}

// TestWriteNeverBlocks is the availability property. The audit trail must not be able to stall the
// payment path it is auditing, so a stuck topic drops records rather than queueing requests behind
// it.
func TestWriteNeverBlocks(t *testing.T) {
	r := &recorder{release: make(chan struct{})}
	tp := NewWithSubmit("0.0.999", r.submit, quiet(), Options{QueueSize: 2})

	done := make(chan struct{})
	go func() {
		for i := range 50 {
			tp.Write(Record{Decision: DecisionAllow, JobID: "job-" + string(rune('a'+i%26))})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked while the topic was stuck")
	}

	if tp.Dropped() == 0 {
		t.Error("nothing was dropped, so the queue was not actually bounded")
	}
	close(r.release)
	tp.Close()
}

// A dropped record is a hole in the audit trail, and a log with holes should say so.
func TestDroppedRecordsAreCounted(t *testing.T) {
	r := &recorder{release: make(chan struct{})}
	tp := NewWithSubmit("0.0.999", r.submit, quiet(), Options{QueueSize: 1})
	for range 20 {
		tp.Write(Record{Decision: DecisionAllow})
	}
	if n := tp.Dropped(); n == 0 {
		t.Error("Dropped() = 0 after overfilling a queue of 1")
	}
	close(r.release)
	tp.Close()
}

// A topic that refuses submissions must not take the exchange down with it.
func TestPublishFailureIsSurvivable(t *testing.T) {
	r := &recorder{err: errors.New("topic is gone")}
	tp := NewWithSubmit("0.0.999", r.submit, quiet(), Options{})
	for range 5 {
		tp.Write(Record{Decision: DecisionAllow, JobID: "job-1"})
	}
	tp.Close() // must not hang or panic
}

func TestCloseIsIdempotent(t *testing.T) {
	tp := NewWithSubmit("0.0.999", (&recorder{}).submit, quiet(), Options{})
	tp.Close()
	tp.Close() // a double close would panic on a closed channel
	tp.Write(Record{Decision: DecisionAllow})
}

// TestDiscardIsUsableAsTheDefault matters because the exchange must run with no topic configured:
// an audit trail is a bonus criterion, not a dependency of taking payments.
func TestDiscardIsUsableAsTheDefault(t *testing.T) {
	var l Logger = Discard{}
	l.Write(Record{Decision: DecisionSettled})
	l.Close()
}

func TestNewRejectsBadConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, network, topic, id, key string }{
		{"unknown network", "hedera:devnet", "0.0.1", "0.0.2", "302e0201"},
		{"bad account", "testnet", "0.0.1", "not-an-account", "302e0201"},
		{"bad topic", "testnet", "nope", "0.0.2", "302e0201"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.network, tc.topic, tc.id, tc.key, quiet(), Options{}); err == nil {
				t.Error("accepted an unusable configuration")
			}
		})
	}
}

// TestParseKeyAcceptsTheFormsAnOperatorHas covers a real interop gap: the Go SDK rejects the "0x"
// prefix that the JavaScript SDK accepts, and both halves of this project read the same .env.
func TestParseKeyAcceptsTheFormsAnOperatorHas(t *testing.T) {
	const raw = "1c8364c6c4107f42a371aff4e1b1d93aa6ef8ea6038f4c4fc25e7da918152038"
	for _, in := range []string{raw, "0x" + raw, "  0x" + raw + "  "} {
		k, err := parseKey(in)
		if err != nil {
			t.Fatalf("parseKey(%q): %v", in, err)
		}
		if k.PublicKey().String() == "" {
			t.Errorf("parseKey(%q) produced an unusable key", in)
		}
	}
	if _, err := parseKey("not a key"); err == nil {
		t.Error("accepted a string that is not a key")
	}
}
