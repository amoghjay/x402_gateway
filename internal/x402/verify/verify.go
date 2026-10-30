// Package verify runs the x402 `exact` EIP-3009 checks locally, without a facilitator:
// the spec's verification steps 1, 3 and 4 (signature, amount and window, token and
// network). Step 2, the balance, is the filter's cached RPC read; step 5, simulation,
// is the facilitator's at settle time.
//
// It is pure: no clock of its own, no network, no state. The caller passes `now` and
// the route's own requirements, which are the only requirements that count (legacy
// REPORT §10.1: never let the client's echo set the terms).
package verify

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/x402"
	"github.com/amoghjay/x402_gateway/internal/x402/eip3009"
)

// Error is a refusal with a spec reason code, for the 402's error field.
type Error struct {
	Reason string // one of x402.Reason*
	Err    error
}

func (e *Error) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func refuse(reason, format string, args ...any) error {
	return &Error{Reason: reason, Err: fmt.Errorf(format, args...)}
}

// Reason extracts the reason code from an error returned by Verify, or "" for nil.
func Reason(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	if err != nil {
		return x402.ReasonUnexpectedVerify
	}
	return ""
}

// Result is what a verified payment commits to.
type Result struct {
	Payer         evm.Address
	Authorization eip3009.Authorization
	Signature     []byte
	// Expires is validBefore, the moment after which the signature is dead on chain.
	// Reservations and the replay guard must outlive it.
	Expires time.Time
}

// Verify checks a PaymentPayload against the route's requirements and the token's
// domain. The requirements are the route's, from the price table; pp.Accepted is only
// checked to match them, never used as the terms.
func Verify(now time.Time, route x402.Requirements, domain eip3009.Domain, pp x402.PaymentPayload) (Result, error) {
	if pp.X402Version != x402.Version {
		return Result{}, refuse(x402.ReasonInvalidVersion, "x402Version %d", pp.X402Version)
	}
	if route.Scheme != x402.SchemeExact {
		return Result{}, refuse(x402.ReasonInvalidRequirements, "route scheme %q", route.Scheme)
	}
	if route.TransferMethod() != x402.TransferEIP3009 {
		return Result{}, refuse(x402.ReasonInvalidRequirements, "route transfer method %q is not built", route.TransferMethod())
	}
	price, err := evm.ParseUint256(route.Amount)
	if err != nil {
		return Result{}, refuse(x402.ReasonInvalidRequirements, "route amount: %v", err)
	}
	payTo, err := evm.ParseAddress(route.PayTo)
	if err != nil {
		return Result{}, refuse(x402.ReasonInvalidRequirements, "route payTo: %v", err)
	}

	// The client's echo must match the route: same scheme, network, asset, payTo and
	// amount. Anything else is a payment signed for different terms than ours.
	acc := pp.Accepted
	if acc.Scheme != route.Scheme {
		return Result{}, refuse(x402.ReasonInvalidScheme, "accepted scheme %q, route %q", acc.Scheme, route.Scheme)
	}
	if acc.Network != route.Network {
		return Result{}, refuse(x402.ReasonInvalidNetwork, "accepted network %q, route %q", acc.Network, route.Network)
	}
	if !strings.EqualFold(acc.Asset, route.Asset) {
		return Result{}, refuse(x402.ReasonInvalidPayload, "accepted asset %q, route %q", acc.Asset, route.Asset)
	}
	if !strings.EqualFold(acc.PayTo, route.PayTo) {
		return Result{}, refuse(x402.ReasonRecipientMismatch, "accepted payTo %q, route %q", acc.PayTo, route.PayTo)
	}
	if acc.Amount != route.Amount {
		return Result{}, refuse(x402.ReasonValueMismatch, "accepted amount %q, route %q", acc.Amount, route.Amount)
	}
	if acc.TransferMethod() != x402.TransferEIP3009 {
		return Result{}, refuse(x402.ReasonInvalidPayload, "accepted transfer method %q", acc.TransferMethod())
	}

	raw, err := pp.EIP3009()
	if err != nil {
		return Result{}, refuse(x402.ReasonInvalidPayload, "%v", err)
	}
	auth, err := eip3009.Parse(raw.Authorization)
	if err != nil {
		return Result{}, refuse(x402.ReasonInvalidPayload, "authorization %v", err)
	}
	sig, err := evm.ParseSignature(raw.Signature)
	if err != nil {
		return Result{}, refuse(x402.ReasonSignature, "%v", err)
	}

	// Terms inside the signed message. The price is a floor, as the legacy gateway
	// learned (§10.1): the signature proves the payer agreed to this number, not that
	// the number is enough.
	if auth.To != payTo {
		return Result{}, refuse(x402.ReasonRecipientMismatch, "authorization.to %s, route payTo %s", auth.To, payTo)
	}
	if auth.Value.Cmp(price) < 0 {
		return Result{}, refuse(x402.ReasonValueMismatch, "authorization.value %s is below the price %s", auth.Value, price)
	}

	// Validity window, with the accept-window cap: a signature that lives longer than
	// the route's maxTimeoutSeconds would stretch every TTL and the time at risk.
	unix := big.NewInt(now.Unix())
	if auth.ValidAfter.Cmp(unix) > 0 {
		return Result{}, refuse(x402.ReasonValidAfter, "validAfter %s is in the future (now %d)", auth.ValidAfter, unix)
	}
	if auth.ValidBefore.Cmp(unix) <= 0 {
		return Result{}, refuse(x402.ReasonValidBefore, "validBefore %s has passed (now %d)", auth.ValidBefore, unix)
	}
	limit := new(big.Int).Add(unix, big.NewInt(int64(route.MaxTimeoutSeconds)))
	if route.MaxTimeoutSeconds > 0 && auth.ValidBefore.Cmp(limit) > 0 {
		return Result{}, refuse(x402.ReasonValidBefore, "validBefore %s is later than now + maxTimeoutSeconds (%s)", auth.ValidBefore, limit)
	}
	if !auth.ValidBefore.IsInt64() {
		return Result{}, refuse(x402.ReasonValidBefore, "validBefore %s is not a timestamp", auth.ValidBefore)
	}

	// The signature, last: it is the only expensive check (~0.1 ms), so the cheap
	// refusals above run first.
	signer, err := evm.Recover(eip3009.Digest(domain, auth), sig)
	if err != nil {
		return Result{}, refuse(x402.ReasonSignature, "%v", err)
	}
	if signer != auth.From {
		return Result{}, refuse(x402.ReasonSignature, "signature recovers to %s, authorization.from is %s", signer, auth.From)
	}

	return Result{
		Payer:         signer,
		Authorization: auth,
		Signature:     sig,
		Expires:       time.Unix(auth.ValidBefore.Int64(), 0),
	}, nil
}
