package x402

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These run against httptest rather than the live facilitator: the request bodies are asserted
// against the captured fixtures, and the responses are replayed from them. No network, no HBAR.

// capturedResponse pulls one endpoint's real response body out of the fixture.
func capturedResponse(t *testing.T, endpoint string) json.RawMessage {
	t.Helper()
	var calls []struct {
		Endpoint     string          `json:"endpoint"`
		ResponseBody json.RawMessage `json:"responseBody"`
	}
	fixture(t, "facilitator-calls.json", &calls)
	for _, c := range calls {
		if c.Endpoint == endpoint {
			return c.ResponseBody
		}
	}
	t.Fatalf("fixture has no %s call", endpoint)
	return nil
}

// TestRequestShapeMatchesCapture pins the body we POST against the one the reference client sent.
// A wrong field name here is rejected upstream in a way that reads as a signing problem, so it is
// caught against the capture instead.
func TestRequestShapeMatchesCapture(t *testing.T) {
	var calls []struct {
		Endpoint    string          `json:"endpoint"`
		RequestBody json.RawMessage `json:"requestBody"`
	}
	fixture(t, "facilitator-calls.json", &calls)

	for _, c := range calls {
		if c.Endpoint != "/verify" && c.Endpoint != "/settle" {
			continue
		}
		var want, got map[string]json.RawMessage
		if err := json.Unmarshal(c.RequestBody, &want); err != nil {
			t.Fatalf("parse captured %s body: %v", c.Endpoint, err)
		}
		b, err := json.Marshal(facilitatorRequest{Version, json.RawMessage(`{}`), Accept{}})
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		for k := range want {
			if _, ok := got[k]; !ok {
				t.Errorf("%s: our request omits %q, which the real one carries", c.Endpoint, k)
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("%s: our request adds %q, which the real one does not have", c.Endpoint, k)
			}
		}
	}
}

// TestVerifyForwardsPayloadVerbatim is the important one. The payload holds a signed transaction;
// if we re-marshal it anywhere between the buyer and the facilitator the signature breaks.
func TestVerifyForwardsPayloadVerbatim(t *testing.T) {
	payload := json.RawMessage(`{"transaction":"CsYBKsMB","quirk":"field we do not model"}`)
	var seen facilitatorRequest
	var seenRaw map[string]json.RawMessage

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{"isValid": true, "payer": "0.0.1"})
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &seen)
		json.Unmarshal(raw, &seenRaw)
		w.Write(body)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second)
	if _, err := c.Verify(context.Background(), payload, Accept{Amount: "500"}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if string(seen.PaymentPayload) != string(payload) {
		t.Errorf("payload altered in transit\n got: %s\nwant: %s", seen.PaymentPayload, payload)
	}
	if !strings.Contains(string(seenRaw["paymentPayload"]), "field we do not model") {
		t.Error("a field absent from our structs was dropped; the payload must pass through untouched")
	}
}

func TestVerifyAndSettleParseCapturedResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(capturedResponse(t, r.URL.Path))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, 5*time.Second)

	v, err := c.Verify(context.Background(), json.RawMessage(`{}`), Accept{})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !v.IsValid || v.Payer == "" {
		t.Errorf("verify: %+v", v)
	}

	s, err := c.Settle(context.Background(), json.RawMessage(`{}`), Accept{})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if !s.Success || s.Transaction == "" {
		t.Errorf("settle: %+v", s)
	}
}

// TestRejectionIsNotAnError separates the facilitator saying no from us failing to ask. A rejection
// arrives as a normal response with IsValid false, and its reason is theirs to give.
func TestRejectionIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"isValid":false,"invalidReason":"invalid_exact_hedera_payload_amount_mismatch"}`))
	}))
	defer srv.Close()

	v, err := NewClient(srv.URL, 5*time.Second).Verify(context.Background(), json.RawMessage(`{}`), Accept{})
	if err != nil {
		t.Fatalf("a structured 4xx rejection must decode, not error: %v", err)
	}
	if v.IsValid {
		t.Error("rejection read as valid")
	}
	if !strings.HasPrefix(v.InvalidReason, "invalid_exact_hedera_") {
		t.Errorf("lost the facilitator's own reason: %q", v.InvalidReason)
	}
}

// TestUnreachableFacilitatorErrors is the fail-closed guarantee at this layer: an unreachable
// upstream must surface as an error so the caller denies. Silently returning a zero VerifyResponse
// would read as "invalid", which is the right outcome by accident and the wrong one to rely on.
func TestUnreachableFacilitatorErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close() // nothing is listening now

	c := NewClient(dead, time.Second)
	if _, err := c.Verify(context.Background(), json.RawMessage(`{}`), Accept{}); err == nil {
		t.Error("Verify against a dead port returned no error")
	}
	if _, err := c.Settle(context.Background(), json.RawMessage(`{}`), Accept{}); err == nil {
		t.Error("Settle against a dead port returned no error — this would drop a payment silently")
	}
}

func TestServerErrorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>gateway timeout</html>"))
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, 5*time.Second).Settle(context.Background(), json.RawMessage(`{}`), Accept{}); err == nil {
		t.Error("a 502 with an HTML body was treated as a settlement")
	}
}

func TestMalformedBodyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json at all"))
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, 5*time.Second).Verify(context.Background(), json.RawMessage(`{}`), Accept{}); err == nil {
		t.Error("unparseable body accepted")
	}
}

// TestSupportedCachesAndServesStale covers the outage behaviour the registry depends on: once it
// has discovered the fee payer, a later facilitator outage must not stop it issuing correct 402s.
// Challenges keep flowing and payments fail at verify, which is the recoverable shape.
func TestSupportedCachesAndServesStale(t *testing.T) {
	var up bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("down"))
			return
		}
		w.Write(capturedResponse(t, "/supported"))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, 5*time.Second)

	up = true
	got, cached, err := c.Supported(context.Background())
	if err != nil || cached {
		t.Fatalf("first fetch: cached=%v err=%v", cached, err)
	}
	fp, ok := got.FeePayerFor("hedera:testnet")
	if !ok {
		t.Fatal("no fee payer in the live response")
	}
	if c.CachedAt().IsZero() {
		t.Error("CachedAt not set after a successful fetch")
	}

	up = false
	stale, cached, err := c.Supported(context.Background())
	if err != nil {
		t.Fatalf("outage should serve stale, got: %v", err)
	}
	if !cached {
		t.Error("served a fresh result during an outage")
	}
	if got, _ := stale.FeePayerFor("hedera:testnet"); got != fp {
		t.Errorf("stale fee payer = %q, want %q", got, fp)
	}
}

// TestSupportedColdStartFails states the limit of that mitigation plainly: with nothing cached,
// there is nothing to fall back to, so a process starting during an outage cannot come up.
func TestSupportedColdStartFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, _, err := NewClient(srv.URL, time.Second).Supported(context.Background()); err == nil {
		t.Error("cold start during an outage must fail rather than serve an empty kind list")
	}
}

func TestContextCancellationPropagates(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // held open until the test lets go, so srv.Close() cannot deadlock on us
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := NewClient(srv.URL, 30*time.Second).Verify(ctx, json.RawMessage(`{}`), Accept{}); err == nil {
		t.Error("a cancelled context did not abort the request")
	}
}

// TestSupportedDoesNotPoisonCache covers the failure that motivated the status and emptiness
// checks: a facilitator answering 200 with an error object would decode to zero kinds, and caching
// that would replace a working fee payer with one that cannot issue a challenge.
func TestSupportedDoesNotPoisonCache(t *testing.T) {
	var healthy bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy {
			w.Write(capturedResponse(t, "/supported"))
			return
		}
		w.WriteHeader(http.StatusOK) // 200, but useless
		w.Write([]byte(`{"error":"temporarily unavailable"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, 5*time.Second)

	healthy = true
	good, _, err := c.Supported(context.Background())
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	want, _ := good.FeePayerFor("hedera:testnet")

	healthy = false
	after, cached, err := c.Supported(context.Background())
	if err != nil {
		t.Fatalf("should have fallen back to cache: %v", err)
	}
	if !cached {
		t.Error("a 200 with no kinds was accepted as a fresh answer")
	}
	if got, _ := after.FeePayerFor("hedera:testnet"); got != want {
		t.Errorf("cache poisoned: fee payer %q, want %q", got, want)
	}
}
