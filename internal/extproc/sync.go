package extproc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/amoghjay/x402_gateway/internal/facilitator"
	"github.com/amoghjay/x402_gateway/internal/paywall"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// Sync capture: settle, then release.
//
// The backend has answered 2xx and Envoy is holding that response: it sends nothing to
// the client until this phase is answered. So the filter settles here, and what the
// client gets depends on the outcome:
//
//	settled                      → the backend's response, with the receipt (a real transaction)
//	refused (the chain or the    → 402 in place of the response: nothing was charged,
//	  facilitator said no)         and nothing is given away
//	unknown (timeout, 5xx,       → 503 in place of the response: the payment may or may
//	  no answer)                   not have gone through, so the response is withheld
//
// This is what makes the chain the replay guard. An EIP-3009 nonce transfers once, and a
// response is released only against a transfer, so a lost Redis write, a stale cached
// balance, a cancelled authorization or an emptied wallet can cost the seller one
// backend call, never a response. Redis only saves those wasted calls.
//
// Budget: the settle call, one retry gap if the facilitator's wallet pool is busy, and
// one chain read must all fit under Envoy's per-message timeout for a Sync route
// (pricing.SyncMessageTimeout). Past that Envoy answers 504 itself and closes the
// stream, and the outcome is logged as unknown.
const (
	DefaultSettleTimeout = 3 * time.Second
	chainCheckTimeout    = time.Second
	// walletRetry is the gap before asking again when the facilitator has no wallet free
	// (its documented answer is "retry in 1-2 s"; the settle budget bounds the retries).
	walletRetry = 500 * time.Millisecond
)

// ReasonSettlementUnknown is the 503's error code when a settle gave no clear answer.
const ReasonSettlementUnknown = "settlement_unknown"

// settleHeld settles a Sync route's payment while Envoy holds the backend's 2xx.
func (s *Server) settleHeld(ctx context.Context, code int, st *request) *extprocv3.ProcessingResponse {
	entry, id := st.entry, paymentID(st.res)
	fac := s.pay.Facilitators[entry.Network.Network()] // not nil: accept refused the request otherwise
	t0 := time.Now()

	sctx, cancel := context.WithTimeout(ctx, s.pay.settleTimeout())
	resp, err := fac.Settle(sctx, st.pp, entry.Requirements)
	for noWalletFree(err) && wait(sctx, walletRetry) == nil {
		resp, err = fac.Settle(sctx, st.pp, entry.Requirements)
	}
	cancel()
	ms := time.Since(t0).Milliseconds()
	if err == nil && resp.Success {
		return s.release2xx(st, code, resp.Transaction, Settled, ms)
	}

	// A failure of any kind: ask the chain before believing it. A used nonce means the
	// transfer happened (this attempt's answer was lost, or an earlier attempt's was).
	// The read runs on its own clock: if Envoy has already given up on the stream, the
	// log should still say which way it went.
	refused, reason := refusal(resp, err)
	used, cerr := s.usedOnChain(ctx, st)
	switch {
	case cerr == nil && used:
		return s.release2xx(st, code, resp.Transaction, SettledOnChain, ms)
	case refused:
		return s.withholdRefused(st, reason, err, ms)
	}
	// Unknown. The reservation is not released (the money may have moved; the in-flight
	// TTL clears it) and nothing retries the settle: a later success would charge for a
	// response that was never delivered.
	st.unknown = true
	st.log.Error("settlement outcome unknown: response withheld", "decision", SettlementUnknown, "paymentId", id,
		"status", code, "settleErr", errText(err), "chainErr", errText(cerr), "ms", ms)
	h := http.Header{}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	return immediate(http.StatusServiceUnavailable, h,
		[]byte(ReasonSettlementUnknown+": the payment could not be confirmed, so the response was withheld. Payment "+id+"\n"), SettlementUnknown)
}

// release2xx lets the held response go, with the receipt.
func (s *Server) release2xx(st *request, code int, tx string, d Decision, ms int64) *extprocv3.ProcessingResponse {
	entry, id := st.entry, paymentID(st.res)
	st.settled = true
	s.release(st, d) // the transfer is on chain: the in-flight amount has done its job
	st.log.Info("settled; released", "decision", d, "paymentId", id, "tx", tx, "status", code, "ms", ms)
	set := []string{"cache-control", "private, no-store"} // a shared cache must never resell a paid response
	receipt, err := x402.SettleResponse{
		Success:     true,
		Transaction: tx, // "" only when the chain, not the facilitator, confirmed it
		Network:     entry.Network.Network(),
		Payer:       st.res.Payer.String(),
		Extensions:  map[string]any{x402.SettlementExtension: map[string]string{"status": "settled", "paymentId": id}},
	}.Encode()
	if err != nil {
		st.log.Error("receipt header could not be built", "err", err) // paid and served; only the header is missing
	} else {
		set = append(set, strings.ToLower(x402.HeaderPaymentResponse), receipt, "access-control-expose-headers", x402.HeaderPaymentResponse)
	}
	return continueResponse(&extprocv3.HeaderMutation{SetHeaders: setHeaders(set...)})
}

// withholdRefused answers 402 in place of the backend's response: the settlement was
// refused for good, so nothing was charged and nothing is given away. The 402 carries
// the reason and the terms, like a payment refused at the door; the buyer signs again.
func (s *Server) withholdRefused(st *request, reason string, cause error, ms int64) *extprocv3.ProcessingResponse {
	if reason == "" {
		reason = x402.ReasonUnexpectedSettle
	}
	s.release(st, SettleRefused)
	pw := st.pw
	pw.Refused = reason
	resp, err := paywall.Render(st.entry, pw)
	if err != nil {
		st.log.Error("could not build the 402", "decision", PaywallError, "err", err)
		return plain(http.StatusInternalServerError, "x402 filter: could not build the payment request\n", PaywallError)
	}
	receipt, err := x402.SettleResponse{
		Success: false, ErrorReason: reason, Network: st.entry.Network.Network(), Payer: st.res.Payer.String(),
	}.Encode()
	if err == nil {
		resp.Headers.Set(x402.HeaderPaymentResponse, receipt)
	}
	st.log.Warn("settlement refused: response withheld", "decision", SettleRefused, "paymentId", paymentID(st.res),
		"reason", reason, "settleErr", errText(cause), "ms", ms)
	return immediate(resp.Status, resp.Headers, resp.Body, SettleRefused)
}

// refusal reports whether a failed settle is a definite no, and the reason given. A 4xx
// is the facilitator (or the chain, through it) turning the payment down. A 200 that
// says success:false is the same, unless it names a transaction or says the settlement
// is pending: then something was broadcast, and the outcome is not known yet.
func refusal(resp x402.SettleResponse, err error) (bool, string) {
	var fs *facilitator.Status
	switch {
	case err == nil:
		return resp.Transaction == "" && resp.ErrorReason != x402.ReasonSettlementPending, resp.ErrorReason
	case errors.As(err, &fs) && fs.Definite() && !noWalletFree(err):
		return true, resp.ErrorReason
	}
	return false, ""
}

// noWalletFree is the facilitator saying its own signer pool is busy: nothing was
// submitted, and its documented answer is to ask again shortly.
func noWalletFree(err error) bool {
	var fs *facilitator.Status
	return errors.As(err, &fs) && strings.Contains(fs.Body, "No available wallets")
}

var errNoRPC = errors.New("no RPC configured for the route's network")

// usedOnChain reads authorizationState for the payment's payer and nonce.
func (s *Server) usedOnChain(ctx context.Context, st *request) (bool, error) {
	rpc := s.pay.RPCs[st.entry.Network.Network()]
	if rpc == nil {
		return false, errNoRPC
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chainCheckTimeout)
	defer cancel()
	return rpc.AuthorizationState(ctx, st.entry.Network.TokenAddress(), st.res.Payer, st.res.Authorization.Nonce)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
