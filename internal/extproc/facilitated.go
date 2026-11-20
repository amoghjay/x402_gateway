package extproc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/amoghjay/x402_gateway/internal/facilitator"
	"github.com/amoghjay/x402_gateway/internal/paywall"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// The facilitator-only path: a platform payment option this filter cannot check itself
// (Permit2, or a token on a network it has no profile for). It is the gate standard's §6.2
// path, plus a replay claim:
//
//	request:  identify the payment (x402.Identify), claim it in Redis, then the option's
//	          facilitator's /verify; continue only on a yes
//	response: a chargeable response is held while the option's facilitator settles; settled →
//	          released with the receipt, refused → 402 in its place, unclear → 503
//
// What it does not have, next to the EIP-3009 path: no chain read before the settle (so a
// payment settled outside the gate after its claim expired is not caught here; the claim
// lives until the signature's own deadline, so only a facilitator that settles a repeat can
// let it through), no held-payment record (a filter killed mid-settle leaves no trace for the
// settler), and no second try on a passing refusal (safe only with a chain read). The claim is
// a stated deviation from §6.2, as on the EIP-3009 path: the Radius testnet facilitator
// answers a repeated /settle with success.

// remotePayment is a payment accepted on the facilitator-only path.
type remotePayment struct {
	id    string // x402.Identify's ID: the claim key, and the payment id in logs and the receipt
	payer string
	terms x402.Requirements // the offered option, as sent to the facilitator
}

// acceptRemote takes a payment for an option only its facilitator can check.
func (s *Server) acceptRemote(ctx context.Context, st *request, pw paywall.Request, pp x402.PaymentPayload, o offer) *extprocv3.ProcessingResponse {
	log := st.log.With("path", "facilitator")
	st.log = log
	id, err := x402.Identify(pp)
	if err != nil {
		return s.refuse(st, pw, x402.ReasonInvalidPayload, PaymentInvalid, err)
	}
	if !id.Expires.IsZero() && !id.Expires.After(s.pay.now()) {
		return s.refuse(st, pw, x402.ReasonInvalidPayload, PaymentInvalid, fmt.Errorf("the payment expired at %s", id.Expires.UTC().Format(time.RFC3339)))
	}
	log = log.With("payer", id.Payer, "paymentId", id.ID)
	st.log = log

	cctx, cancel := context.WithTimeout(ctx, s.pay.deadline())
	fresh, err := s.pay.Reserve.Claim(cctx, id.ID, id.Expires)
	cancel()
	if err != nil {
		log.Error("replay claim failed; not charged", "decision", DependencyUnavailable, "err", err)
		return retryLater(DependencyUnavailable)
	}
	if !fresh {
		return s.refuse(st, pw, ReasonReplay, Replay, errors.New("this payment was already presented"))
	}

	fac := s.pay.facilitatorAt(o.facilitator)
	vctx, vcancel := context.WithTimeout(ctx, s.pay.verifyTimeout())
	t0 := time.Now()
	vr, err := fac.Verify(vctx, pp, o.req)
	vcancel()
	ms := time.Since(t0).Milliseconds()
	var fs *facilitator.Status
	switch {
	case err == nil && vr.IsValid && (vr.Payer == "" || strings.EqualFold(vr.Payer, id.Payer)):
	case err == nil && vr.IsValid:
		log.Error("facilitator verified a different payer; not charged", "decision", DependencyUnavailable, "facilitatorPayer", vr.Payer, "ms", ms)
		return retryLater(DependencyUnavailable)
	case err == nil, errors.As(err, &fs) && fs.Definite():
		reason := vr.InvalidReason
		if reason == "" && fs != nil {
			var body x402.VerifyResponse
			if json.Unmarshal([]byte(fs.Body), &body) == nil {
				reason = body.InvalidReason
			}
		}
		if reason == "" {
			reason = x402.ReasonUnexpectedVerify
		}
		return s.refuse(st, pw, reason, PaymentInvalid, fmt.Errorf("facilitator /verify said no in %d ms: %s", ms, reason))
	default:
		log.Error("facilitator /verify gave no answer; not charged", "decision", DependencyUnavailable, "err", err, "ms", ms)
		return retryLater(DependencyUnavailable)
	}

	st.remote = &remotePayment{id: id.ID, payer: id.Payer, terms: o.req}
	st.fac, st.pp, st.pw = fac, pp, pw
	log.Info("payment accepted", "decision", PaymentAccepted, "amount", o.req.Amount, "asset", o.req.Asset, "method", o.req.TransferMethod(), "verifyMs", ms)
	return continueWith(&extprocv3.HeaderMutation{
		RemoveHeaders: []string{strings.ToLower(x402.HeaderPaymentSignature)},
		SetHeaders:    setHeaders(HeaderPayer, id.Payer, HeaderPaymentID, id.ID),
	})
}

// settleRemote settles a facilitator-only payment while Envoy holds the response.
func (s *Server) settleRemote(ctx context.Context, code int, st *request) *extprocv3.ProcessingResponse {
	rp := st.remote
	if !chargeable(code) {
		// Nothing was served: nothing is settled. The claim stays, so the buyer signs again.
		st.log.Info("not served; not settled", "decision", ReleasedNotServed, "status", code)
		return continueResponse(nil)
	}
	t0 := time.Now()
	sctx, cancel := context.WithTimeout(ctx, s.pay.settleTimeout())
	resp, err := st.fac.Settle(sctx, st.pp, rp.terms)
	for noWalletFree(err) && wait(sctx, walletRetry) == nil {
		resp, err = st.fac.Settle(sctx, st.pp, rp.terms)
	}
	cancel()
	ms := time.Since(t0).Milliseconds()

	if err == nil && resp.Success {
		st.settled = true
		st.log.Info("settled; released", "decision", Settled, "tx", resp.Transaction, "status", code, "ms", ms)
		set := []string{"cache-control", "private, no-store"}
		receipt, rerr := x402.SettleResponse{
			Success: true, Transaction: resp.Transaction, Network: rp.terms.Network, Payer: rp.payer,
			Extensions: map[string]any{x402.SettlementExtension: map[string]string{"status": "settled", "paymentId": rp.id}},
		}.Encode()
		if rerr == nil {
			set = append(set, strings.ToLower(x402.HeaderPaymentResponse), receipt, "access-control-expose-headers", x402.HeaderPaymentResponse)
		}
		return continueResponse(&extprocv3.HeaderMutation{SetHeaders: setHeaders(set...)})
	}
	if refused, reason := refusal(resp, err); refused {
		if reason == "" {
			reason = x402.ReasonUnexpectedSettle
		}
		pw := st.pw
		pw.Refused = reason
		r, perr := paywall.Render(st.entry, pw)
		if perr != nil {
			st.log.Error("could not build the 402", "decision", PaywallError, "err", perr)
			return plain(http.StatusInternalServerError, "x402 filter: could not build the payment request\n", PaywallError)
		}
		if receipt, rerr := (x402.SettleResponse{Success: false, ErrorReason: reason, Network: rp.terms.Network, Payer: rp.payer}).Encode(); rerr == nil {
			r.Headers.Set(x402.HeaderPaymentResponse, receipt)
		}
		st.log.Warn("not paid by this request: response withheld", "decision", SettleRefused, "reason", reason, "settleErr", errText(err), "ms", ms)
		return immediate(r.Status, r.Headers, r.Body, SettleRefused)
	}
	// Unclear, and this path has no chain read to settle the question: withheld. The buyer
	// may have been charged; the log line is the only record (see the comment at the top).
	st.log.Error("settlement outcome unknown: response withheld (facilitator-only path: no chain read, no record)", "decision", SettlementUnknown,
		"status", code, "settleErr", errText(err), "ms", ms)
	h := http.Header{}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	return immediate(http.StatusServiceUnavailable, h,
		[]byte(ReasonSettlementUnknown+": the payment could not be confirmed, so the response was withheld. Payment "+rp.id+"\n"), SettlementUnknown)
}
