// Package facilitator is the HTTP client for an x402 facilitator (/verify, /settle,
// /supported). The settler is its only production caller; the buyer CLI uses it for
// the M2 done-check, and the request path never does.
package facilitator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/amoghjay/x402_gateway/internal/x402"
)

// Client talks to one facilitator.
type Client struct {
	URL  string
	HTTP *http.Client
}

// Status is returned for every non-2xx answer, so a caller can tell a definite refusal
// (4xx: the chain or the facilitator rejected the payment) from an uncertain one (5xx,
// or no answer at all: the transaction may or may not have gone out). When the body was
// the expected JSON it is also decoded into the caller's value, for the reason.
type Status struct {
	Code int
	Body string
}

func (s *Status) Error() string { return fmt.Sprintf("facilitator: HTTP %d: %s", s.Code, s.Body) }

// Definite reports whether the facilitator refused the payment for good (4xx), as
// opposed to failing to process it (5xx).
func (s *Status) Definite() bool { return s.Code >= 400 && s.Code < 500 }

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	decodeErr := json.Unmarshal(raw, out)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || decodeErr != nil {
		return &Status{Code: resp.StatusCode, Body: string(bytes.TrimSpace(raw))}
	}
	return nil
}

func request(pp x402.PaymentPayload, req x402.Requirements) x402.FacilitatorRequest {
	return x402.FacilitatorRequest{X402Version: x402.Version, PaymentPayload: pp, PaymentRequirements: req}
}

// Verify asks the facilitator to check a payment against the server's requirements.
func (c *Client) Verify(ctx context.Context, pp x402.PaymentPayload, req x402.Requirements) (x402.VerifyResponse, error) {
	var out x402.VerifyResponse
	err := c.post(ctx, "/verify", request(pp, req), &out)
	return out, err
}

// Settle asks the facilitator to submit the payment on chain.
func (c *Client) Settle(ctx context.Context, pp x402.PaymentPayload, req x402.Requirements) (x402.SettleResponse, error) {
	var out x402.SettleResponse
	err := c.post(ctx, "/settle", request(pp, req), &out)
	return out, err
}

// Supported reads GET /supported.
func (c *Client) Supported(ctx context.Context) (x402.Supported, error) {
	var out x402.Supported
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/supported", nil)
	if err != nil {
		return out, err
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("/supported: HTTP %d: %w", resp.StatusCode, err)
	}
	return out, nil
}
