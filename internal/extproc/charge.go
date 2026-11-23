package extproc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/paywall"
	"github.com/amoghjay/x402_gateway/internal/policy"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
	"github.com/amoghjay/x402_gateway/internal/x402/eip3009"
)

// A platform rule that charges (gate standard §6). The 402 offers every compiled payment
// option, in order (§6.1). A payment is matched to the option it says it accepted, and
// that option decides how it is taken:
//
//   - EIP-3009 in a token on a network this filter knows (its profile, RPC and balance
//     cache): the M6a Sync path, unchanged: local signature check, balance, Redis nonce
//     claim and reservation, the facilitator's /verify, response held, authorizationState
//     read, held-payment record, /settle, release. Its facilitator is the one the option
//     names, never replaced by ours.
//   - Anything else (Permit2, another token): the facilitator-only path (facilitated.go):
//     replay claim, the option's facilitator's /verify, response held, /settle, release.
//
// Keeping a replay claim and a held-payment record goes beyond §6.2, which says a gate keeps
// no record of payments. It is a stated deviation: the Radius testnet facilitator answered a
// repeated /settle with success (re-tested 2026-11-20), so the facilitator alone does not
// stop a replay.

// offerTimeoutSeconds is maxTimeoutSeconds on every platform offer; the standard says 60 is
// usual.
const offerTimeoutSeconds = 60

// offer is one compiled payment option as this filter offers it.
type offer struct {
	req         x402.Requirements
	facilitator string
	// entry is set when this filter takes the option itself (EIP-3009, a known network and
	// token): the route entry the paid path runs on. nil otherwise.
	entry *pricing.Entry
	why   string // for an option without an entry: why this filter does not take it
}

// offersFor builds the offers of a charge decision. key is the route's table key.
func offersFor(key string, d policy.Decision, table *pricing.Prices) []offer {
	out := make([]offer, 0, len(d.Accepts))
	for _, o := range d.Accepts {
		req := x402.Requirements{
			Scheme: o.Scheme, Network: o.Network, Amount: o.Amount, Asset: o.Asset, PayTo: o.PayTo,
			MaxTimeoutSeconds: offerTimeoutSeconds,
		}
		profile, known := table.Network(o.Network)
		ourToken := known && strings.EqualFold(o.Asset, profile.Token.Address)
		switch {
		case o.Extra != nil:
			// As the document wrote it (§6.1), and the fields this filter reads. The bytes
			// come from the option's own text: re-encoding the decoded map would round a
			// large integer through float64.
			req.ExtraRaw = extraAsWritten(o)
			str := func(k string) string { v, _ := o.Extra[k].(string); return v }
			req.Extra = &x402.ExactExtra{AssetTransferMethod: str("assetTransferMethod"), Name: str("name"), Version: str("version")}
		case ourToken:
			// Compiled before options carried `extra`: offered with this filter's own domain
			// for the token.
			req.Extra = &x402.ExactExtra{AssetTransferMethod: x402.TransferEIP3009, Name: profile.Token.Name, Version: profile.Token.Version}
		default:
			req.ExtraRaw = `{}`
		}
		of := offer{req: req}
		if o.Facilitator != nil {
			of.facilitator = *o.Facilitator // never null: policy.Parse refuses a null one
		}
		of.entry, of.why = localEntry(key, d, req, profile, known && ourToken)
		out = append(out, of)
	}
	return out
}

// localEntry is the route entry for an option this filter takes itself, or why it does not.
func localEntry(key string, d policy.Decision, req x402.Requirements, profile radius.Profile, ourToken bool) (*pricing.Entry, string) {
	switch {
	case !ourToken:
		return nil, fmt.Sprintf("this gate does not take %s on %s", req.Asset, req.Network)
	case req.TransferMethod() != x402.TransferEIP3009:
		return nil, fmt.Sprintf("this gate does not take %s payments yet", req.TransferMethod())
	}
	price, err := evm.ParseUint256(req.Amount)
	if err != nil || price.Sign() <= 0 || price.Cmp(pricing.MaxPrice) > 0 {
		return nil, fmt.Sprintf("amount %s is outside what this gate's ledger can hold", req.Amount)
	}
	if _, err := evm.ParseAddress(req.PayTo); err != nil {
		return nil, "payTo is not an address"
	}
	// The domain the buyer signs against is the one offered.
	name, version := profile.Token.Name, profile.Token.Version
	if req.Extra != nil && req.Extra.Name != "" {
		name, version = req.Extra.Name, req.Extra.Version
	}
	return &pricing.Entry{
		Key: key,
		Route: pricing.Route{
			Network: req.Network, Price: req.Amount, Asset: req.Asset, PayTo: req.PayTo,
			AssetTransferMethod: x402.TransferEIP3009, MaxTimeoutSeconds: offerTimeoutSeconds,
			FailureMode: pricing.FailureClosed, CaptureMode: pricing.CaptureSync, // platform routes always settle before release
			Description: d.Description,
		},
		Network:      profile,
		Price:        price,
		Requirements: req,
		Domain:       eip3009.Domain{Name: name, Version: version, ChainID: profile.ChainID, VerifyingContract: profile.TokenAddress()},
	}, ""
}

// maxTokenDecimals bounds the decimals a compiled option may claim for a token this filter
// has no profile for. No ERC-20 in use comes near it.
const maxTokenDecimals = 36

// helpEntry is the entry whose how-to-pay text the 402 carries: the first option this filter
// takes, else the first option, described generically.
func helpEntry(key string, d policy.Decision, offers []offer) *pricing.Entry {
	for _, o := range offers {
		if o.entry != nil {
			return o.entry
		}
	}
	first := offers[0].req
	price, _ := evm.ParseUint256(first.Amount)
	// The document's decimals are not validated by policy.Parse, and the 402's text formats
	// the price with them: a value outside a token's possible range must not reach Format.
	decimals := 0
	if len(d.Accepts) > 0 {
		decimals = min(max(d.Accepts[0].Decimals, 0), maxTokenDecimals)
	}
	return &pricing.Entry{
		Key:   key,
		Route: pricing.Route{Network: first.Network, Price: first.Amount, Asset: first.Asset, PayTo: first.PayTo, Description: d.Description},
		Network: radius.Profile{
			Name: first.Network, Display: first.Network, Docs: "https://x402.org",
			Token: radius.Token{Symbol: "tokens", Address: first.Asset, Decimals: decimals},
		},
		Price:        price,
		Requirements: first,
	}
}

// matchOffer finds the offer a payment says it accepted: same scheme, network, amount and
// transfer method, and the same asset and payTo (addresses compare without case). The
// method is part of the match because one token can be offered by two methods, and each
// method has its own checks: a payment must never run the other one's path.
func matchOffer(offers []offer, acc x402.Requirements) (offer, bool) {
	for _, o := range offers {
		r := o.req
		if acc.Scheme == r.Scheme && acc.Network == r.Network && acc.Amount == r.Amount &&
			acc.TransferMethod() == r.TransferMethod() &&
			strings.EqualFold(acc.Asset, r.Asset) && strings.EqualFold(acc.PayTo, r.PayTo) {
			return o, true
		}
	}
	return offer{}, false
}

// platformCharge answers a request a charge rule decided: the 402 with every option, or the
// paid path for the option the payment picked.
func (s *Server) platformCharge(ctx context.Context, h map[string]string, st *request, key string, d policy.Decision, table *pricing.Prices) *extprocv3.ProcessingResponse {
	if len(d.Accepts) == 0 { // policy.Parse refuses this; never serve a charge free
		return plain(503, "x402 filter: this route's charge has no payment options\n", PolicyInvalid)
	}
	offers := offersFor(key, d, table)
	accepts := make([]x402.Requirements, len(offers))
	for i, o := range offers {
		accepts[i] = o.req
	}
	st.entry = helpEntry(key, d, offers)
	st.ruleHeaders = d.Headers
	pw := paywall.Request{Method: h[":method"], URL: publicURL(h), Accept: h["accept"], UserAgent: h["user-agent"], Accepts: accepts}

	hdr := h[strings.ToLower(x402.HeaderPaymentSignature)]
	switch {
	case hdr == "":
		return s.paywall(st, pw, PaymentRequired)
	case s.pay == nil:
		return s.paywall(st, pw, PaymentPresentUnverified)
	}
	pp, err := x402.DecodePaymentPayload(hdr)
	if err != nil {
		return s.refuse(st, pw, x402.ReasonInvalidPayload, PaymentInvalid, err)
	}
	o, ok := matchOffer(offers, pp.Accepted)
	if !ok {
		return s.refuse(st, pw, x402.ReasonInvalidRequirements, PaymentInvalid,
			fmt.Errorf("accepted %s %s %s to %s matches none of the %d offered options", pp.Accepted.Amount, pp.Accepted.Asset, pp.Accepted.Network, pp.Accepted.PayTo, len(offers)))
	}
	if o.entry == nil {
		// Not checkable here (o.why): its facilitator decides.
		return s.acceptRemote(ctx, st, pw, pp, o)
	}
	st.entry, st.fac = o.entry, s.pay.facilitatorAt(o.facilitator)
	st.log = st.log.With("network", o.req.Network, "facilitator", o.facilitator)
	return s.accept(ctx, st, pw, hdr)
}

// withRuleHeaders adds a decision's limit headers (§4.5) to an answer: an immediate response
// (402, 503) or the released backend response. Ours overwrite any of the same names.
func withRuleHeaders(resp *extprocv3.ProcessingResponse, h map[string]string) *extprocv3.ProcessingResponse {
	extra := ruleResponseHeaders(h)
	if extra == nil || resp == nil {
		return resp
	}
	var m **extprocv3.HeaderMutation
	switch r := resp.Response.(type) {
	case *extprocv3.ProcessingResponse_ImmediateResponse:
		m = &r.ImmediateResponse.Headers
	case *extprocv3.ProcessingResponse_ResponseHeaders:
		if r.ResponseHeaders.Response == nil {
			r.ResponseHeaders.Response = &extprocv3.CommonResponse{}
		}
		m = &r.ResponseHeaders.Response.HeaderMutation
	default:
		return resp
	}
	if *m == nil {
		*m = &extprocv3.HeaderMutation{}
	}
	(*m).SetHeaders = append((*m).SetHeaders, extra.SetHeaders...)
	return resp
}

// extraAsWritten is an option's `extra` object, byte for byte as the compiled document has
// it. An option built without its text (a test) falls back to encoding the decoded map.
func extraAsWritten(o policy.Option) string {
	var doc struct {
		Extra json.RawMessage `json:"extra"`
	}
	if len(o.Raw) > 0 && json.Unmarshal(o.Raw, &doc) == nil && len(doc.Extra) > 0 {
		return string(doc.Extra)
	}
	raw, err := json.Marshal(o.Extra)
	if err != nil {
		return `{}`
	}
	return string(raw)
}
