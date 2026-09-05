// Package x402 implements the resource-server half of the x402 v2 payment protocol.
//
// The signed Hedera transaction is never opened here. It arrives base64-encoded inside the
// payment payload, is passed through verbatim, and the facilitator is what checks that it agrees
// with the requirements we stated. Probe P3 established that: a validly signed transfer whose
// amount or payee disagrees with paymentRequirements is rejected upstream with
// invalid_exact_hedera_payload_amount_mismatch. That is why this package needs no Hedera SDK.
//
// Wire shapes are pinned by fixtures/ — captured from a real settlement, not from documentation.
// This file holds the wire types and their encodings; client.go speaks to the facilitator.
package x402

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

const Version = 2

// Header names, in the canonical casing the JS implementation emits.
const (
	HeaderPaymentRequired  = "PAYMENT-REQUIRED"
	HeaderPaymentSignature = "PAYMENT-SIGNATURE"
	HeaderPaymentResponse  = "PAYMENT-RESPONSE"
)

// Accept is one payment option in a challenge, and the same shape travels back as
// paymentRequirements. The amount field is a decimal string in the asset's smallest unit
// (tinybars for HBAR, asset "0.0.0").
//
// Note the JSON name is "amount", not "maxAmountRequired" — the latter appears in some x402
// documentation but is not what this facilitator emits or accepts.
type Accept struct {
	Scheme            string            `json:"scheme"`
	Network           string            `json:"network"`
	Amount            string            `json:"amount"`
	Asset             string            `json:"asset"`
	PayTo             string            `json:"payTo"`
	MaxTimeoutSeconds int               `json:"maxTimeoutSeconds"`
	Extra             map[string]string `json:"extra,omitempty"`
}

// Resource describes what is being bought. It is echoed by the client into its payload.
type Resource struct {
	URL         string `json:"url"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

// Challenge is the body of the PAYMENT-REQUIRED header on a 402.
//
// Error carries the denial reason on a rejection. Only this field survives to the calling agent —
// the response body is {} and every structured field is dropped — so policy denials pack their
// evidence into it as `REASON_CODE {json}`.
type Challenge struct {
	X402Version int      `json:"x402Version"`
	Error       string   `json:"error"`
	Resource    Resource `json:"resource"`
	Accepts     []Accept `json:"accepts"`
}

// PaymentPayload is what the buyer sends back in PAYMENT-SIGNATURE.
//
// Payload stays raw. It holds {"transaction": "<base64 signed transfer>"} and this package has no
// business decoding it: the facilitator verifies it against Accepted and PaymentRequirements.
type PaymentPayload struct {
	X402Version int             `json:"x402Version"`
	Payload     json.RawMessage `json:"payload"`
	Resource    Resource        `json:"resource"`
	Accepted    Accept          `json:"accepted"`
}

// PaymentResponse is the body of the PAYMENT-RESPONSE header returned alongside a 200.
type PaymentResponse struct {
	Success     bool   `json:"success"`
	Payer       string `json:"payer"`
	Transaction string `json:"transaction"`
	Network     string `json:"network"`
}

// Kind is one supported (version, scheme, network) triple from the facilitator.
// Extra carries the facilitator's own feePayer, which must be copied into every Accept we issue.
type Kind struct {
	X402Version int               `json:"x402Version"`
	Scheme      string            `json:"scheme"`
	Network     string            `json:"network"`
	Extra       map[string]string `json:"extra,omitempty"`
}

// Supported is the /supported response.
type Supported struct {
	Kinds []Kind `json:"kinds"`
}

// VerifyResponse is the /verify response. IsValid true is a payload check only.
//
// Probe P5: a transaction with an out-of-range validity duration verified successfully and would
// still be refused by the network at submit. Never report a successful verify as a payment.
type VerifyResponse struct {
	IsValid       bool   `json:"isValid"`
	Payer         string `json:"payer,omitempty"`
	InvalidReason string `json:"invalidReason,omitempty"`
}

// SettleResponse is the /settle response. Transaction is the Hedera transaction id, prefixed with
// the facilitator's own account because they pay the network fee.
type SettleResponse struct {
	Success     bool   `json:"success"`
	Transaction string `json:"transaction,omitempty"`
	Network     string `json:"network,omitempty"`
	Payer       string `json:"payer,omitempty"`
	ErrorReason string `json:"errorReason,omitempty"`
}

// EncodeChallenge renders a challenge for the PAYMENT-REQUIRED header.
func EncodeChallenge(c Challenge) (string, error) {
	if c.X402Version == 0 {
		c.X402Version = Version
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode challenge: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// DecodeChallenge parses a PAYMENT-REQUIRED header. Used by tests and by the buyer-side tooling;
// the registry itself only ever writes challenges.
func DecodeChallenge(header string) (*Challenge, error) {
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return nil, fmt.Errorf("decode challenge header: %w", err)
	}
	var c Challenge
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse challenge: %w", err)
	}
	return &c, nil
}

// DecodePaymentSignature parses a PAYMENT-SIGNATURE header into both its parsed form and the raw
// JSON bytes.
//
// The raw form is what gets forwarded to the facilitator. Re-marshalling the parsed struct would
// risk dropping fields we do not model, and the payload is a signed artifact: it must reach the
// facilitator byte-identical to how the buyer sent it.
func DecodePaymentSignature(header string) (*PaymentPayload, json.RawMessage, error) {
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return nil, nil, fmt.Errorf("decode payment signature header: %w", err)
	}
	var p PaymentPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, fmt.Errorf("parse payment payload: %w", err)
	}
	if p.X402Version != Version {
		return nil, nil, fmt.Errorf("unsupported x402 version %d (want %d)", p.X402Version, Version)
	}
	return &p, json.RawMessage(raw), nil
}

// EncodePaymentResponse renders the PAYMENT-RESPONSE header for a settled payment.
func EncodePaymentResponse(pr PaymentResponse) (string, error) {
	b, err := json.Marshal(pr)
	if err != nil {
		return "", fmt.Errorf("encode payment response: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// FeePayerFor returns the facilitator's fee-payer account for a network.
//
// This value must be copied into every Accept we issue: the buyer sets it as the transaction id's
// payer account, which is why settlements appear on the mirror node under the facilitator's
// account rather than ours.
func (s *Supported) FeePayerFor(network string) (string, bool) {
	for _, k := range s.Kinds {
		if k.Network == network && k.Scheme == "exact" && k.X402Version == Version {
			if fp, ok := k.Extra["feePayer"]; ok && fp != "" {
				return fp, true
			}
		}
	}
	return "", false
}
