package x402

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// facilitatorRequest is the shared body of /verify and /settle. Both take the payment payload
// verbatim alongside the requirements we are asserting it should satisfy.
type facilitatorRequest struct {
	X402Version         int             `json:"x402Version"`
	PaymentPayload      json.RawMessage `json:"paymentPayload"`
	PaymentRequirements Accept          `json:"paymentRequirements"`
}

// Client talks to a hosted x402 facilitator.
//
// Supported results are cached after the first success and served stale on later failures. A
// facilitator outage must not stop a running registry from issuing correct 402s — it should keep
// serving challenges and fail at verify time, which is the recoverable shape. This does not help a
// cold start: the first call has nothing to fall back to.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	mu       sync.RWMutex
	cached   *Supported
	cachedAt time.Time
}

// NewClient builds a facilitator client. baseURL takes no trailing slash and no /v1 prefix —
// the marketing documentation shows one, and every /v1 path 404s.
func NewClient(baseURL string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: timeout}}
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("content-type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s unreachable: %w", path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read %s response: %w", path, err)
	}
	if res.StatusCode >= 500 {
		return fmt.Errorf("%s returned %d: %s", path, res.StatusCode, truncate(raw, 200))
	}
	// A 4xx still carries a structured rejection, so it is decoded rather than treated as a
	// transport failure. Only a malformed body is an error here.
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("parse %s response (%d): %s", path, res.StatusCode, truncate(raw, 200))
	}
	return nil
}

// Verify asks the facilitator whether a payment payload satisfies the requirements.
//
// A returned error means we could not get an answer, and the caller must deny: never treat an
// unreachable facilitator as a pass. A non-error response with IsValid false is the facilitator's
// own rejection, and its reason belongs to it rather than to us.
func (c *Client) Verify(ctx context.Context, payload json.RawMessage, reqs Accept) (*VerifyResponse, error) {
	var out VerifyResponse
	err := c.post(ctx, "/verify", facilitatorRequest{Version, payload, reqs}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Settle submits the payment. The facilitator co-signs as fee payer and pushes it to the network,
// which is why the returned transaction id is prefixed with their account and not ours.
func (c *Client) Settle(ctx context.Context, payload json.RawMessage, reqs Accept) (*SettleResponse, error) {
	var out SettleResponse
	err := c.post(ctx, "/settle", facilitatorRequest{Version, payload, reqs}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Supported fetches supported kinds, falling back to the last good response on failure.
// The bool reports whether the result came from cache.
func (c *Client) Supported(ctx context.Context) (*Supported, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/supported", nil)
	if err == nil {
		var res *http.Response
		if res, err = c.HTTP.Do(req); err == nil {
			defer res.Body.Close()
			var s Supported
			switch {
			case res.StatusCode != http.StatusOK:
				err = fmt.Errorf("/supported returned %d", res.StatusCode)
			default:
				err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&s)
			}
			// An empty kind list is not a usable answer, and caching one would overwrite a good
			// cache with something that cannot issue a challenge — the opposite of the point.
			if err == nil && len(s.Kinds) == 0 {
				err = fmt.Errorf("/supported returned no kinds")
			}
			if err == nil {
				c.mu.Lock()
				c.cached, c.cachedAt = &s, time.Now()
				c.mu.Unlock()
				return &s, false, nil
			}
		}
	}
	c.mu.RLock()
	cached := c.cached
	c.mu.RUnlock()
	if cached != nil {
		return cached, true, nil
	}
	return nil, false, fmt.Errorf("facilitator discovery failed with no cached value: %w", err)
}

// CachedAt reports when the supported-kinds cache was last refreshed, zero if never.
func (c *Client) CachedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cachedAt
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
