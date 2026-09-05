package x402

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The fixtures were captured from a real settled payment against the live facilitator, with global
// fetch intercepted so the bodies are true wire bytes. They are the contract: if Go disagrees with
// them, Go is wrong. Every test here compares against captured bytes rather than expectations.

func fixture(t *testing.T, name string, out any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
}

// TestDecodeCapturedChallenge checks we read the real header, and in particular that the price
// field is "amount". Some x402 documentation calls it maxAmountRequired, which would decode here
// as an empty string and produce a challenge asking for nothing.
func TestDecodeCapturedChallenge(t *testing.T) {
	var f struct {
		Raw string `json:"raw"`
	}
	fixture(t, "payment-required-header.json", &f)

	got, err := DecodeChallenge(f.Raw)
	if err != nil {
		t.Fatalf("DecodeChallenge: %v", err)
	}
	if got.X402Version != Version {
		t.Errorf("x402Version = %d, want %d", got.X402Version, Version)
	}
	if len(got.Accepts) != 1 {
		t.Fatalf("accepts len = %d, want 1", len(got.Accepts))
	}
	a := got.Accepts[0]
	if a.Amount != "500" {
		t.Errorf("amount = %q, want \"500\" — is the JSON tag right?", a.Amount)
	}
	if a.Asset != "0.0.0" || a.Network != "hedera:testnet" || a.Scheme != "exact" {
		t.Errorf("accept mismatch: %+v", a)
	}
	if a.Extra["feePayer"] == "" {
		t.Error("extra.feePayer empty — the buyer needs it to build the transaction id")
	}
}

// TestChallengeRoundTripsToCapturedBytes is the strict one: re-encoding a decoded challenge must
// reproduce the captured header byte for byte. That proves field names, ordering and omissions all
// match what the reference implementation emits, which is what a real buyer will parse.
func TestChallengeRoundTripsToCapturedBytes(t *testing.T) {
	var f struct {
		Raw string `json:"raw"`
	}
	fixture(t, "payment-required-header.json", &f)

	decoded, err := DecodeChallenge(f.Raw)
	if err != nil {
		t.Fatalf("DecodeChallenge: %v", err)
	}
	reencoded, err := EncodeChallenge(*decoded)
	if err != nil {
		t.Fatalf("EncodeChallenge: %v", err)
	}
	if reencoded != f.Raw {
		gotJSON, _ := base64.StdEncoding.DecodeString(reencoded)
		wantJSON, _ := base64.StdEncoding.DecodeString(f.Raw)
		t.Errorf("re-encoded challenge differs from the capture\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

// TestEncodeChallengeDefaultsVersion guards the one field a caller is likely to leave unset.
func TestEncodeChallengeDefaultsVersion(t *testing.T) {
	h, err := EncodeChallenge(Challenge{Error: "Payment required"})
	if err != nil {
		t.Fatalf("EncodeChallenge: %v", err)
	}
	got, err := DecodeChallenge(h)
	if err != nil {
		t.Fatalf("DecodeChallenge: %v", err)
	}
	if got.X402Version != Version {
		t.Errorf("x402Version = %d, want it defaulted to %d", got.X402Version, Version)
	}
}

// TestDecodePaymentSignature checks the buyer's half, and that the raw bytes handed back for
// forwarding are exactly what arrived — the payload is a signed artifact, so any re-marshalling
// between the buyer and the facilitator is a bug.
func TestDecodePaymentSignature(t *testing.T) {
	var f struct {
		Raw string `json:"raw"`
	}
	fixture(t, "payment-signature-header.json", &f)

	p, raw, err := DecodePaymentSignature(f.Raw)
	if err != nil {
		t.Fatalf("DecodePaymentSignature: %v", err)
	}
	if p.Accepted.Amount != "500" || p.Accepted.PayTo == "" {
		t.Errorf("accepted block mismatch: %+v", p.Accepted)
	}

	var inner struct {
		Transaction string `json:"transaction"`
	}
	if err := json.Unmarshal(p.Payload, &inner); err != nil {
		t.Fatalf("payload is not {transaction}: %v", err)
	}
	if len(inner.Transaction) < 100 {
		t.Errorf("transaction blob suspiciously short: %d chars", len(inner.Transaction))
	}
	if _, err := base64.StdEncoding.DecodeString(inner.Transaction); err != nil {
		t.Errorf("transaction is not valid base64: %v", err)
	}

	wantRaw, _ := base64.StdEncoding.DecodeString(f.Raw)
	if !reflect.DeepEqual([]byte(raw), wantRaw) {
		t.Error("returned raw bytes are not the bytes that arrived — forwarding would corrupt a signed payload")
	}
}

// TestRejectsV1Payload guards the version trap. v1 used the X-PAYMENT header; the Hedera
// facilitator is v2-only. A v1 payload reaching it fails in a way that reads as a signing error,
// so it is refused here where the cause is obvious.
func TestRejectsV1Payload(t *testing.T) {
	v1 := base64.StdEncoding.EncodeToString([]byte(`{"x402Version":1,"payload":{"transaction":"AA=="}}`))
	if _, _, err := DecodePaymentSignature(v1); err == nil {
		t.Error("a v1 payload was accepted; it must be rejected before it reaches the facilitator")
	}
}

func TestDecodePaymentSignatureRejectsGarbage(t *testing.T) {
	for name, in := range map[string]string{
		"not base64":             "!!!!not base64!!!!",
		"not json":               base64.StdEncoding.EncodeToString([]byte("hello")),
		"empty":                  "",
		"json but not a payload": base64.StdEncoding.EncodeToString([]byte(`["nope"]`)),
	} {
		if _, _, err := DecodePaymentSignature(in); err == nil {
			t.Errorf("%s: accepted, want an error", name)
		}
	}
}

// TestPaymentResponseRoundTrip pins the success header returned alongside a 200.
func TestPaymentResponseRoundTrip(t *testing.T) {
	var f struct {
		Raw     string          `json:"raw"`
		Decoded PaymentResponse `json:"decoded"`
	}
	fixture(t, "payment-response-header.json", &f)

	got, err := EncodePaymentResponse(f.Decoded)
	if err != nil {
		t.Fatalf("EncodePaymentResponse: %v", err)
	}
	if got != f.Raw {
		gotJSON, _ := base64.StdEncoding.DecodeString(got)
		wantJSON, _ := base64.StdEncoding.DecodeString(f.Raw)
		t.Errorf("payment-response header differs\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

// TestParseFacilitatorResponseTypes reads the captured /verify and /settle replies into our types.
// The HTTP client that produces these calls arrives in the next commit; this only asserts that the
// shapes decode.
func TestParseFacilitatorResponseTypes(t *testing.T) {
	var calls []struct {
		Endpoint     string          `json:"endpoint"`
		ResponseBody json.RawMessage `json:"responseBody"`
	}
	fixture(t, "facilitator-calls.json", &calls)

	var sawVerify, sawSettle bool
	for _, c := range calls {
		switch c.Endpoint {
		case "/verify":
			var v VerifyResponse
			if err := json.Unmarshal(c.ResponseBody, &v); err != nil {
				t.Fatalf("parse verify response: %v", err)
			}
			if !v.IsValid || v.Payer == "" {
				t.Errorf("verify response not read correctly: %+v", v)
			}
			sawVerify = true
		case "/settle":
			var s SettleResponse
			if err := json.Unmarshal(c.ResponseBody, &s); err != nil {
				t.Fatalf("parse settle response: %v", err)
			}
			if !s.Success || s.Transaction == "" {
				t.Errorf("settle response not read correctly: %+v", s)
			}
			// The transaction id is prefixed with the facilitator's account because they pay the
			// network fee. That prefix is the visible proof settlement went through them.
			if s.Network != "hedera:testnet" {
				t.Errorf("network = %q", s.Network)
			}
			sawSettle = true
		}
	}
	if !sawVerify || !sawSettle {
		t.Errorf("fixture missed an endpoint (verify=%v settle=%v)", sawVerify, sawSettle)
	}
}

// TestFeePayerFor reads the real /supported response. Every challenge we issue must carry this
// value, and the absence of a mainnet kind is a fact the deployment depends on.
func TestFeePayerFor(t *testing.T) {
	var calls []struct {
		Endpoint     string          `json:"endpoint"`
		ResponseBody json.RawMessage `json:"responseBody"`
	}
	fixture(t, "facilitator-calls.json", &calls)

	var sup *Supported
	for _, c := range calls {
		if c.Endpoint == "/supported" {
			sup = &Supported{}
			if err := json.Unmarshal(c.ResponseBody, sup); err != nil {
				t.Fatalf("parse supported: %v", err)
			}
		}
	}
	if sup == nil {
		t.Fatal("fixture has no /supported call")
	}

	fp, ok := sup.FeePayerFor("hedera:testnet")
	if !ok || fp == "" {
		t.Fatal("FeePayerFor(hedera:testnet) found nothing in the real /supported response")
	}
	if _, ok := sup.FeePayerFor("hedera:mainnet"); ok {
		t.Error("found a hedera:mainnet kind — the facilitator does not offer one, and the deployment assumes that")
	}
	if _, ok := sup.FeePayerFor("eip155:80002"); ok {
		t.Error("returned a fee payer for a kind that advertises no extra")
	}
}

func TestFeePayerForIgnoresWrongVersionOrScheme(t *testing.T) {
	sup := &Supported{Kinds: []Kind{
		{X402Version: 1, Scheme: "exact", Network: "hedera:testnet", Extra: map[string]string{"feePayer": "0.0.1"}},
		{X402Version: Version, Scheme: "upto", Network: "hedera:testnet", Extra: map[string]string{"feePayer": "0.0.2"}},
	}}
	if got, ok := sup.FeePayerFor("hedera:testnet"); ok {
		t.Errorf("matched an unusable kind, returned %q", got)
	}
}
