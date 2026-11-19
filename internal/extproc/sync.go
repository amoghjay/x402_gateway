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
// So a stale cached balance, a cancelled authorization or an emptied wallet can cost the
// seller one backend call, never a response.
//
// Replay needs one more step. A facilitator may answer /settle for a payment that has
// already settled with success and the old transaction (Radius testnet does: seen on
// 2026-11-19, a payment settled outside the gate and then sent through it was released).
// So a successful /settle does not prove this request paid. The filter therefore reads
// authorizationState first: a nonce already used on chain is a replay and gets a 402
// without calling /settle. The Redis nonce claim at the door is still the first guard,
// and the only one against two copies arriving at the same moment; this read covers the
// case where Redis lost the claim (a restart, a failover, an eviction). If the read
// fails the response is withheld with a 503: without it a replay cannot be ruled out.
//
// An unclear settle (a timeout, a 5xx) is the one case where a buyer can be charged and
// get nothing: the transfer may land after the filter stopped waiting. So the settle
// budget is generous, and after an unclear answer the filter keeps asking the chain for
// a while before it gives up. A slow paid response is better than a paid 503.
//
// Budget: the chain read, the settle call (with a retry gap if the facilitator's wallet
// pool is busy), and the wait for the chain after an unclear settle must all fit under
// Envoy's per-message timeout for a Sync route (pricing.SyncMessageTimeout). Past that
// Envoy answers 504 itself and closes the stream, and the outcome is logged as unknown.
const (
	DefaultSettleTimeout = 6 * time.Second
	// DefaultConfirmWait is how long the chain is asked whether an unclear settle landed.
	DefaultConfirmWait = 2500 * time.Millisecond
	confirmEvery       = 250 * time.Millisecond
	chainCheckTimeout  = 500 * time.Millisecond
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

	// Already used on chain: this signature paid for something once, somewhere, and is
	// not paying for this response.
	switch used, cerr := s.usedOnChain(ctx, st); {
	case cerr != nil:
		s.release(st, DependencyUnavailable)
		st.log.Error("could not read authorizationState before settling: response withheld, not charged", "decision", DependencyUnavailable, "paymentId", id, "err", cerr)
		return retryLater(DependencyUnavailable)
	case used:
		return s.withhold(st, ReasonReplay, Replay, nil, time.Since(t0).Milliseconds())
	}

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

	// A definite no is believed as it is. (If the nonce is used by now, another copy of
	// this payment settled in the meantime; that copy's request is the one that paid.)
	if refused, reason := refusal(resp, err); refused {
		return s.withhold(st, reason, SettleRefused, err, ms)
	}
	// No clear answer: ask the chain, for a while. The nonce was unused a moment ago, so
	// if it is used now, this settle went through and only its answer was lost or late.
	// The reads run on their own clock: if Envoy has already given up on the stream, the
	// log should still say which way it went.
	used, cerr := s.confirmOnChain(ctx, st)
	ms = time.Since(t0).Milliseconds()
	if cerr == nil && used {
		return s.release2xx(st, code, resp.Transaction, SettledOnChain, ms)
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

// withhold answers 402 in place of the backend's response: the settlement was refused
// for good, or the payment was already spent, so this request charged nothing and is
// given nothing. The 402 carries the reason and the terms, like a payment refused at
// the door; the buyer signs again.
func (s *Server) withhold(st *request, reason string, d Decision, cause error, ms int64) *extprocv3.ProcessingResponse {
	if reason == "" {
		reason = x402.ReasonUnexpectedSettle
	}
	s.release(st, d)
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
	st.log.Warn("not paid by this request: response withheld", "decision", d, "paymentId", paymentID(st.res),
		"reason", reason, "settleErr", errText(cause), "ms", ms)
	return immediate(resp.Status, resp.Headers, resp.Body, d)
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

// confirmOnChain asks whether the nonce is used until it is, or until ConfirmWait is up.
func (s *Server) confirmOnChain(ctx context.Context, st *request) (bool, error) {
	deadline := time.Now().Add(s.pay.confirmWait())
	for {
		used, err := s.usedOnChain(ctx, st)
		if (err == nil && used) || time.Until(deadline) < confirmEvery {
			return used, err
		}
		time.Sleep(confirmEvery)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
