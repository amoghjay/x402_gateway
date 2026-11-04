// Package buyer is the paying side of x402 V2 for Radius: it signs EIP-3009
// authorizations, drips from the signed testnet faucet, and runs the 402 → pay → 200
// loop against any x402 server. The CLI in cmd/x402-buyer is a thin wrapper.
//
// The key lives in the Wallet, in the caller's process. Nothing here logs it.
package buyer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
	"github.com/amoghjay/x402_gateway/internal/x402/eip3009"
)

// Wallet is a key and the network it pays on.
type Wallet struct {
	Key     *evm.PrivateKey
	Network radius.Profile
	HTTP    *http.Client
}

func (w *Wallet) client() *http.Client {
	if w.HTTP != nil {
		return w.HTTP
	}
	return http.DefaultClient
}

// Address of the wallet.
func (w *Wallet) Address() evm.Address { return w.Key.Address() }

// Domain is SBC's EIP-712 domain on the wallet's network.
func (w *Wallet) Domain() eip3009.Domain {
	return eip3009.Domain{Name: radius.SBCName, Version: radius.SBCVersion, ChainID: w.Network.ChainID, VerifyingContract: radius.SBCAddress}
}

// Sign builds the PaymentPayload for one `exact` EIP-3009 requirement: a fresh random
// nonce, validAfter 0 (the seller may sit behind a slow clock), validBefore now +
// validFor, capped at the requirement's maxTimeoutSeconds. The payload echoes the
// requirement as `accepted`, including its transfer method, as the scheme spec asks.
func (w *Wallet) Sign(req x402.Requirements, resource *x402.Resource, now time.Time, validFor time.Duration) (x402.PaymentPayload, error) {
	if req.Scheme != x402.SchemeExact || req.TransferMethod() != x402.TransferEIP3009 {
		return x402.PaymentPayload{}, fmt.Errorf("buyer: can only pay exact/eip3009, offered %s/%s", req.Scheme, req.TransferMethod())
	}
	if req.Network != w.Network.Network() {
		return x402.PaymentPayload{}, fmt.Errorf("buyer: offer is on %s, wallet is on %s", req.Network, w.Network.Network())
	}
	if !strings.EqualFold(req.Asset, radius.SBC) {
		return x402.PaymentPayload{}, fmt.Errorf("buyer: offer asset %s is not SBC", req.Asset)
	}
	to, err := evm.ParseAddress(req.PayTo)
	if err != nil {
		return x402.PaymentPayload{}, fmt.Errorf("buyer: offer payTo: %w", err)
	}
	value, err := evm.ParseUint256(req.Amount)
	if err != nil {
		return x402.PaymentPayload{}, fmt.Errorf("buyer: offer amount: %w", err)
	}
	if req.MaxTimeoutSeconds > 0 && validFor > time.Duration(req.MaxTimeoutSeconds)*time.Second {
		validFor = time.Duration(req.MaxTimeoutSeconds) * time.Second
	}
	auth := eip3009.Authorization{
		From:        w.Address(),
		To:          to,
		Value:       value,
		ValidAfter:  big.NewInt(0),
		ValidBefore: big.NewInt(now.Add(validFor).Unix()),
	}
	if _, err := rand.Read(auth.Nonce[:]); err != nil {
		return x402.PaymentPayload{}, err
	}
	sig := w.Key.Sign(eip3009.Digest(w.Domain(), auth))
	payload, err := json.Marshal(x402.EIP3009Payload{Signature: evm.Hex(sig), Authorization: auth.Wire()})
	if err != nil {
		return x402.PaymentPayload{}, err
	}
	return x402.PaymentPayload{X402Version: x402.Version, Resource: resource, Accepted: req, Payload: payload}, nil
}

// Choose picks the offer this wallet can pay: exact, eip3009, SBC, on its network, and
// no dearer than maxAmount (atomic units; nil means any price).
func (w *Wallet) Choose(pr x402.PaymentRequired, maxAmount *big.Int) (x402.Requirements, error) {
	var seen []string
	for _, req := range pr.Accepts {
		seen = append(seen, fmt.Sprintf("%s/%s on %s for %s", req.Scheme, req.TransferMethod(), req.Network, req.Amount))
		if req.Scheme != x402.SchemeExact || req.TransferMethod() != x402.TransferEIP3009 ||
			req.Network != w.Network.Network() || !strings.EqualFold(req.Asset, radius.SBC) {
			continue
		}
		amount, err := evm.ParseUint256(req.Amount)
		if err != nil {
			continue
		}
		if maxAmount != nil && amount.Cmp(maxAmount) > 0 {
			return x402.Requirements{}, fmt.Errorf("buyer: offer asks %s SBC, above the threshold %s SBC", radius.FormatSBC(amount), radius.FormatSBC(maxAmount))
		}
		return req, nil
	}
	return x402.Requirements{}, fmt.Errorf("buyer: no payable offer among [%s]", strings.Join(seen, "; "))
}

// Outcome is what one paid request produced.
type Outcome struct {
	Required   x402.PaymentRequired // the 402 that was answered
	Paid       x402.Requirements    // the offer that was paid
	Status     int                  // status of the retried request
	Body       []byte
	Header     http.Header
	Settlement *x402.SettleResponse // decoded PAYMENT-RESPONSE, if present
}

// Pay performs the x402 loop: request, read the 402, sign the chosen offer, retry with
// PAYMENT-SIGNATURE. A first response that is not a 402 is returned as-is with no
// payment. The request body, if any, is sent on both attempts.
func (w *Wallet) Pay(ctx context.Context, method, url string, body []byte, headers http.Header, maxAmount *big.Int, validFor time.Duration) (Outcome, error) {
	do := func(extra http.Header) (*http.Response, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		for k, vs := range headers {
			req.Header[k] = vs
		}
		for k, vs := range extra {
			req.Header[k] = vs
		}
		resp, err := w.client().Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return resp, b, err
	}

	first, firstBody, err := do(nil)
	if err != nil {
		return Outcome{}, err
	}
	if first.StatusCode != http.StatusPaymentRequired {
		return Outcome{Status: first.StatusCode, Body: firstBody, Header: first.Header}, nil
	}
	hdr := first.Header.Get(x402.HeaderPaymentRequired)
	if hdr == "" {
		return Outcome{Status: first.StatusCode, Body: firstBody, Header: first.Header}, errors.New("buyer: 402 without a PAYMENT-REQUIRED header")
	}
	pr, err := x402.DecodePaymentRequired(hdr)
	if err != nil {
		return Outcome{Status: first.StatusCode, Body: firstBody, Header: first.Header}, err
	}
	offer, err := w.Choose(pr, maxAmount)
	if err != nil {
		return Outcome{Required: pr, Status: first.StatusCode, Body: firstBody, Header: first.Header}, err
	}
	pp, err := w.Sign(offer, &pr.Resource, time.Now(), validFor)
	if err != nil {
		return Outcome{Required: pr}, err
	}
	sigHeader, err := pp.Encode()
	if err != nil {
		return Outcome{Required: pr}, err
	}
	second, secondBody, err := do(http.Header{x402.HeaderPaymentSignature: {sigHeader}})
	if err != nil {
		return Outcome{Required: pr, Paid: offer}, err
	}
	out := Outcome{Required: pr, Paid: offer, Status: second.StatusCode, Body: secondBody, Header: second.Header}
	if h := second.Header.Get(x402.HeaderPaymentResponse); h != "" {
		if s, err := x402.DecodeSettleResponse(h); err == nil {
			out.Settlement = &s
		}
	}
	return out, nil
}

// Drip asks the signed testnet faucet for SBC: the first request returns the challenge
// text, which the wallet signs with personal_sign, and the second carries the
// signature. The challenge must be exactly the faucet's known sentence; anything else
// is not signed.
func (w *Wallet) Drip(ctx context.Context) (DripResult, error) {
	if w.Network.Faucet == "" {
		return DripResult{}, fmt.Errorf("buyer: no faucet on %s", w.Network.Name)
	}
	addr := w.Address().String()
	first, err := w.drip(ctx, map[string]string{"address": addr, "token": "SBC"})
	if err != nil {
		return DripResult{}, err
	}
	if first.Success {
		return first, nil
	}
	if first.Error == nil || first.Error.Code != "signature_required" {
		return first, fmt.Errorf("buyer: faucet: %s", first.errorText())
	}
	want := "Radius Faucet: drip SBC to " + addr
	if first.Error.Details.Challenge != want {
		return first, fmt.Errorf("buyer: faucet challenge %q is not the expected text; not signing", first.Error.Details.Challenge)
	}
	sig := w.Key.Sign(evm.PersonalHash([]byte(want)))
	second, err := w.drip(ctx, map[string]string{"address": addr, "token": "SBC", "signature": evm.Hex(sig)})
	if err != nil {
		return DripResult{}, err
	}
	if !second.Success {
		return second, fmt.Errorf("buyer: faucet: %s", second.errorText())
	}
	return second, nil
}

// DripResult is the faucet's answer.
type DripResult struct {
	Success      bool   `json:"success"`
	TxHash       string `json:"tx_hash"`
	RetryAfterMS int64  `json:"retry_after_ms"`
	Error        *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Challenge string `json:"challenge"`
		} `json:"details"`
	} `json:"error"`
}

func (d DripResult) errorText() string {
	if d.Error == nil {
		return "no error given"
	}
	return d.Error.Code + ": " + d.Error.Message
}

func (w *Wallet) drip(ctx context.Context, in map[string]string) (DripResult, error) {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.Network.Faucet+"/drip", bytes.NewReader(body))
	if err != nil {
		return DripResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client().Do(req)
	if err != nil {
		return DripResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out DripResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return DripResult{}, fmt.Errorf("buyer: faucet: HTTP %d: %w", resp.StatusCode, err)
	}
	return out, nil
}
