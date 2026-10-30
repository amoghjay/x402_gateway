package x402

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MaxHeaderBytes bounds a header value before it is decoded. A PaymentPayload for
// `exact` is under 1 KiB; the cap leaves room for extensions and stops a client from
// making the filter decode megabytes.
const MaxHeaderBytes = 16 << 10

// Sentinel errors. Callers map them to responses: ErrMalformed and ErrPayload are the
// client's fault (400 or 402 with ReasonInvalidPayload), ErrVersion is
// ReasonInvalidVersion.
var (
	ErrMalformed = errors.New("x402: malformed header")
	ErrVersion   = errors.New("x402: unsupported x402Version")
)

// EncodeHeader renders v as a header value: compact JSON, base64 with the standard
// alphabet and padding, as the spec's examples are encoded.
func EncodeHeader(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// DecodeHeader parses a header value into v. Clients differ on padding and alphabet, so
// both the standard and URL alphabets are accepted, padded or not. Trailing data after
// the JSON object is rejected.
func DecodeHeader(s string, v any) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("%w: empty", ErrMalformed)
	}
	if len(s) > MaxHeaderBytes {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrMalformed, len(s), MaxHeaderBytes)
	}
	enc := base64.RawStdEncoding
	if strings.ContainsAny(s, "-_") {
		enc = base64.RawURLEncoding
	}
	raw, err := enc.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return fmt.Errorf("%w: base64: %w", ErrMalformed, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: json: %w", ErrMalformed, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after the JSON object", ErrMalformed)
	}
	return nil
}

// Encode renders the PAYMENT-REQUIRED header value.
func (r PaymentRequired) Encode() (string, error) { return EncodeHeader(r) }

// Encode renders the PAYMENT-RESPONSE header value.
func (s SettleResponse) Encode() (string, error) { return EncodeHeader(s) }

// Encode renders the PAYMENT-SIGNATURE header value (the buyer's side).
func (p PaymentPayload) Encode() (string, error) { return EncodeHeader(p) }

// DecodePaymentPayload parses a PAYMENT-SIGNATURE header value and checks the protocol
// version and that the parts every scheme needs are present. It does not check the
// terms against the route's requirements or the scheme payload; that is the verifier's
// job, which also owns the reason codes.
func DecodePaymentPayload(header string) (PaymentPayload, error) {
	var p PaymentPayload
	if err := DecodeHeader(header, &p); err != nil {
		return PaymentPayload{}, err
	}
	if p.X402Version != Version {
		return PaymentPayload{}, fmt.Errorf("%w: %d", ErrVersion, p.X402Version)
	}
	if p.Accepted.Scheme == "" || p.Accepted.Network == "" {
		return PaymentPayload{}, fmt.Errorf("%w: accepted.scheme and accepted.network are required", ErrMalformed)
	}
	if !isJSONObject(p.Payload) {
		return PaymentPayload{}, fmt.Errorf("%w: payload must be a JSON object", ErrMalformed)
	}
	return p, nil
}

// DecodePaymentRequired parses a PAYMENT-REQUIRED header value (the buyer's side).
func DecodePaymentRequired(header string) (PaymentRequired, error) {
	var r PaymentRequired
	if err := DecodeHeader(header, &r); err != nil {
		return PaymentRequired{}, err
	}
	if r.X402Version != Version {
		return PaymentRequired{}, fmt.Errorf("%w: %d", ErrVersion, r.X402Version)
	}
	if len(r.Accepts) == 0 {
		return PaymentRequired{}, fmt.Errorf("%w: accepts is empty", ErrMalformed)
	}
	return r, nil
}

// DecodeSettleResponse parses a PAYMENT-RESPONSE header value (the buyer's side).
func DecodeSettleResponse(header string) (SettleResponse, error) {
	var s SettleResponse
	if err := DecodeHeader(header, &s); err != nil {
		return SettleResponse{}, err
	}
	return s, nil
}

func isJSONObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 1 && t[0] == '{'
}
