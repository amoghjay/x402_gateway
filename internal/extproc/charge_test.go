package extproc

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/amoghjay/x402_gateway/internal/policy"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// A platform charge rule against a TLS fake facilitator that the compiled options name: the
// 402 offers every option, and a payment is taken through the option's own facilitator.

const sellerAddr = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed" // three_options.json's pay_to

// chargeDoc is the test document with every option's facilitator pointed at url, and a field
// this filter does not know added to the first option's extra.
func chargeDoc(t *testing.T, url string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(compiledDoc(t)), &doc); err != nil {
		t.Fatal(err)
	}
	for _, r := range doc["rules"].([]any) {
		act := r.(map[string]any)["action"].(map[string]any)
		accepts, _ := act["accepts"].([]any)
		for i, o := range accepts {
			o.(map[string]any)["facilitator"] = url
			if i == 0 {
				o.(map[string]any)["extra"].(map[string]any)["futureExtra"] = map[string]any{"kept": true}
			}
		}
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

func setupCharge(t *testing.T, opts ...func(*Payments)) (*paid, *fakeFacilitator) {
	t.Helper()
	f := newFakeFacilitator()
	srv := httptest.NewTLSServer(f.handler())
	t.Cleanup(srv.Close)
	p, err := pricing.Parse([]byte(`{"schemaVersion":1,"routes":{},"platform":{"httproute/shop/metered":{"project":"`+platformProject+`","noPolicy":"deny"}}}`), radius.Testnet)
	if err != nil {
		t.Fatal(err)
	}
	p.LoadPolicies(map[string]string{pricing.PolicyKey(platformProject): chargeDoc(t, srv.URL)}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	store := pricing.NewStore()
	store.Set(p)
	pd := setupWith(t, 5000, 0, store, func(pay *Payments, node *radius.RPC) {
		pay.RPCs = map[string]*radius.RPC{radius.Testnet.Network(): node}
		pay.FacilitatorHTTP = srv.Client() // trusts the test server's certificate
		pay.ConfirmWait = 300 * time.Millisecond
		for _, o := range opts {
			o(pay)
		}
	})
	return pd, f
}

// chargeRequest sends a request for the paid rule, with a payment when hdr is set.
func (p *paid) chargeRequest(hdr string) (extprocv3.ExternalProcessor_ProcessClient, *extprocv3.ProcessingResponse) {
	p.t.Helper()
	kv := []string{":path", "/anything/platform/paid/42"}
	if hdr != "" {
		kv = append(kv, "payment-signature", hdr)
	}
	req := requestHeaders(platformRoute, kv...)
	req.Attributes[attributesKey].Fields[sourceAddressAttr] = structpb.NewStringValue("203.0.113.5:4711")
	stream := open(p.t, p.conn)
	return stream, roundTrip(p.t, stream, req)
}

// offered decodes a 402's PAYMENT-REQUIRED header, and its accepts as raw JSON.
func offered(t *testing.T, resp *extprocv3.ProcessingResponse) (x402.PaymentRequired, []map[string]any) {
	t.Helper()
	ir := resp.GetImmediateResponse()
	if ir == nil || ir.GetStatus().GetCode() != 402 {
		t.Fatalf("want a 402, got %v", resp)
	}
	h := immediateHeaders(t, ir)
	pr, err := x402.DecodePaymentRequired(h["payment-required"])
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Accepts []map[string]any `json:"accepts"`
	}
	if err := x402.DecodeHeader(h["payment-required"], &raw); err != nil {
		t.Fatal(err)
	}
	return pr, raw.Accepts
}

// signFor signs, as a buyer does, for the 402's i-th option.
func (p *paid) signFor(terms x402.Requirements) string {
	p.t.Helper()
	pp, err := p.w.Sign(terms, &x402.Resource{URL: "http://34.173.224.114/anything/platform/paid/42"}, time.Now(), time.Minute)
	if err != nil {
		p.t.Fatal(err)
	}
	hdr, err := pp.Encode()
	if err != nil {
		p.t.Fatal(err)
	}
	return hdr
}

func TestChargeOffersEveryOption(t *testing.T) {
	p, _ := setupCharge(t)
	_, resp := p.chargeRequest("")
	pr, raw := offered(t, resp)
	if len(pr.Accepts) != 3 {
		t.Fatalf("offered %d options, want all 3", len(pr.Accepts))
	}
	want := []struct{ network, asset, method string }{
		{"eip155:72344", "0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb", "eip3009"},
		{"eip155:84532", "0x036CbD53842c5426634e7929541eC2318f3dCF7e", "eip3009"},
		{"eip155:84532", "0xf9FB20B8E097904f0aB7d12e9DbeE88f2dcd0F16", "permit2"},
	}
	for i, w := range want {
		a := pr.Accepts[i]
		if a.Network != w.network || a.Asset != w.asset || a.TransferMethod() != w.method || a.Amount != "1000" ||
			a.PayTo != sellerAddr || a.MaxTimeoutSeconds != 60 || a.Scheme != "exact" {
			t.Errorf("option %d = %+v", i, a)
		}
	}
	// extra exactly as the document wrote it, including a field this filter does not model.
	if ex, _ := raw[0]["extra"].(map[string]any); ex["futureExtra"] == nil || ex["name"] != "Stable Coin" {
		t.Errorf("first option's extra = %v", raw[0]["extra"])
	}
	if pr.Resource.Description != "One paid item through the Envoy gate" {
		t.Errorf("description %q", pr.Resource.Description)
	}
	p.decided(PaymentRequired)
}

// A buyer reads the 402, signs the first option, and is served once the option's
// facilitator settles. That facilitator gets the offered terms, extra passed through.
func TestChargePaidThroughTheOptionsFacilitator(t *testing.T) {
	p, f := setupCharge(t, func(pay *Payments) { pay.FacilitatorVerify = true })
	_, resp := p.chargeRequest("")
	pr, _ := offered(t, resp)

	hdr := p.signFor(pr.Accepts[0])
	stream, resp := p.chargeRequest(hdr)
	if resp.GetRequestHeaders() == nil {
		t.Fatalf("request phase: %v (logs %s)", resp, p.logs)
	}
	if f.verifyCalls.Load() != 1 {
		t.Errorf("/verify called %d times on the option's facilitator, want 1", f.verifyCalls.Load())
	}
	resp = p.respond(stream, "200")
	set, _ := mutations(t, resp.GetResponseHeaders().GetResponse())
	receipt, err := x402.DecodeSettleResponse(set["payment-response"])
	if err != nil || !receipt.Success || receipt.Transaction != "0xfeed" {
		t.Fatalf("receipt %+v, %v (response %v)", receipt, err, resp)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("/settle called %d times on the option's facilitator", f.calls.Load())
	}
	body := f.last.Load().(string)
	for _, want := range []string{`"payTo":"` + sellerAddr + `"`, `"maxTimeoutSeconds":60`, `"futureExtra":{"kept":true}`} {
		if !strings.Contains(body, want) {
			t.Errorf("settle request lacks %s: %s", want, body)
		}
	}
	if p.inflight() != "" {
		t.Error("in-flight amount not released after settling")
	}
	p.decided(Settled)

	// The same payment again: its nonce is claimed, so a 402, and nothing is settled.
	_, resp = p.chargeRequest(hdr)
	if got, _ := offered(t, resp); !strings.HasPrefix(got.Error, ReasonReplay) {
		t.Errorf("replay: error %q", got.Error)
	}
	if f.calls.Load() != 1 {
		t.Errorf("/settle called %d times after a replay", f.calls.Load())
	}
}

// A Permit2 payment, as a client builds it for the third option. The signature is not
// checked here: on this path only the facilitator checks it.
func permit2Payment(t *testing.T, terms x402.Requirements, from, nonce, deadline string) string {
	t.Helper()
	body := `{"signature":"0x` + strings.Repeat("ab", 65) + `","permit2Authorization":{"permitted":{"token":"` + terms.Asset +
		`","amount":"1000"},"from":"` + from + `","spender":"0x4020615294c913F045dc10f0a5cdEbd86c280001","nonce":"` + nonce +
		`","deadline":"` + deadline + `","witness":{"to":"` + terms.PayTo + `","validAfter":"0","extra":"0x"}}}`
	hdr, err := x402.PaymentPayload{X402Version: x402.Version, Accepted: terms, Payload: json.RawMessage(body)}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return hdr
}

const permit2Payer = "0x857b06519E91e3A54538791bDbb0E22373e36b66"

func future() string { return strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10) }

// The Permit2 option goes through its facilitator: /verify on the request, the response held
// while it settles, the receipt on release. A re-encoded copy of the same payment is a replay.
func TestChargeFacilitatorOnlyPath(t *testing.T) {
	p, f := setupCharge(t)
	_, resp := p.chargeRequest("")
	pr, _ := offered(t, resp)
	terms := pr.Accepts[2]

	stream, resp := p.chargeRequest(permit2Payment(t, terms, permit2Payer, "255", future()))
	if resp.GetRequestHeaders() == nil {
		t.Fatalf("request phase: %v (logs %s)", resp, p.logs)
	}
	if f.verifyCalls.Load() != 1 || f.calls.Load() != 0 {
		t.Fatalf("verify %d, settle %d before the backend answered", f.verifyCalls.Load(), f.calls.Load())
	}
	set, removed := mutations(t, resp.GetRequestHeaders().GetResponse())
	if !strings.EqualFold(set[HeaderPayer], permit2Payer) || len(set[HeaderPaymentID]) != 64 || !slices.Contains(removed, "payment-signature") {
		t.Errorf("request mutations: set %v, removed %v", set, removed)
	}

	resp = p.respond(stream, "200")
	set, _ = mutations(t, resp.GetResponseHeaders().GetResponse())
	receipt, err := x402.DecodeSettleResponse(set["payment-response"])
	if err != nil || !receipt.Success || receipt.Transaction != "0xfeed" || receipt.Network != "eip155:84532" {
		t.Fatalf("receipt %+v, %v (response %v)", receipt, err, resp)
	}
	if body := f.last.Load().(string); !strings.Contains(body, `"assetTransferMethod":"permit2"`) || !strings.Contains(body, `"permit2Authorization"`) {
		t.Errorf("settle request: %s", body)
	}
	p.decided(Settled)

	// The same payment, re-encoded (hex nonce, payer in upper case): refused before the
	// facilitator is asked.
	_, resp = p.chargeRequest(permit2Payment(t, terms, strings.ToUpper(permit2Payer[2:]), "0xff", future()))
	if got, _ := offered(t, resp); !strings.HasPrefix(got.Error, x402.ReasonInvalidPayload) && !strings.HasPrefix(got.Error, ReasonReplay) {
		t.Errorf("re-encoded copy: error %q", got.Error)
	}
	_, resp = p.chargeRequest(permit2Payment(t, terms, "0x"+strings.ToUpper(permit2Payer[2:]), "0xff", future()))
	if got, _ := offered(t, resp); !strings.HasPrefix(got.Error, ReasonReplay) {
		t.Errorf("re-encoded copy: error %q", got.Error)
	}
	if f.verifyCalls.Load() != 1 || f.calls.Load() != 1 {
		t.Errorf("a replay reached the facilitator: verify %d, settle %d", f.verifyCalls.Load(), f.calls.Load())
	}
}

func TestChargeFacilitatorOnlyRefusals(t *testing.T) {
	p, f := setupCharge(t)
	_, resp := p.chargeRequest("")
	pr, _ := offered(t, resp)
	terms := pr.Accepts[2]

	// /verify says no: a 402 with its reason, nothing settled.
	f.verifyStatus.Store(400)
	f.verifyBody.Store(`{"isValid":false,"invalidReason":"permit2_allowance_required"}`)
	_, resp = p.chargeRequest(permit2Payment(t, terms, permit2Payer, "1", future()))
	if got, _ := offered(t, resp); !strings.HasPrefix(got.Error, "permit2_allowance_required") || len(got.Accepts) != 3 {
		t.Errorf("verify refused: error %q", got.Error)
	}
	f.verifyStatus.Store(0)
	f.verifyBody.Store(`{"isValid":true}`)

	// No payer or nonce to claim on: refused without asking anyone.
	n := f.verifyCalls.Load()
	hdr, _ := x402.PaymentPayload{X402Version: x402.Version, Accepted: terms, Payload: json.RawMessage(`{"signature":"0x00"}`)}.Encode()
	_, resp = p.chargeRequest(hdr)
	if got, _ := offered(t, resp); !strings.HasPrefix(got.Error, x402.ReasonInvalidPayload) {
		t.Errorf("unidentifiable: error %q", got.Error)
	}
	// Expired.
	_, resp = p.chargeRequest(permit2Payment(t, terms, permit2Payer, "2", "1700000000"))
	if got, _ := offered(t, resp); !strings.HasPrefix(got.Error, x402.ReasonInvalidPayload) {
		t.Errorf("expired: error %q", got.Error)
	}
	if f.verifyCalls.Load() != n {
		t.Error("the facilitator was asked about a payment refused at the door")
	}

	// The backend fails: nothing settled.
	stream, _ := p.chargeRequest(permit2Payment(t, terms, permit2Payer, "3", future()))
	if resp := p.respond(stream, "502"); resp.GetResponseHeaders() == nil || f.calls.Load() != 0 {
		t.Errorf("unserved: %v, settle calls %d", resp, f.calls.Load())
	}

	// Settle refused: a 402 in place of the response, every option offered.
	f.answer(400, `{"success":false,"errorReason":"insufficient_funds","transaction":"","network":"eip155:84532"}`)
	stream, _ = p.chargeRequest(permit2Payment(t, terms, permit2Payer, "4", future()))
	if got, _ := offered(t, p.respond(stream, "200")); !strings.HasPrefix(got.Error, "insufficient_funds") || len(got.Accepts) != 3 {
		t.Errorf("settle refused: error %q", got.Error)
	}

	// Settle unclear: withheld with a 503.
	f.answer(500, `oops`)
	stream, _ = p.chargeRequest(permit2Payment(t, terms, permit2Payer, "5", future()))
	resp = p.respond(stream, "200")
	if ir := resp.GetImmediateResponse(); ir == nil || ir.GetStatus().GetCode() != 503 || !strings.Contains(string(ir.GetBody()), ReasonSettlementUnknown) {
		t.Errorf("settle unclear: %v", resp)
	}
	p.decided(SettlementUnknown)
}

// A payment for terms that were never offered.
func TestChargeAcceptedMatchesNoOption(t *testing.T) {
	p, _ := setupCharge(t)
	_, resp := p.chargeRequest("")
	pr, _ := offered(t, resp)
	terms := pr.Accepts[0]
	terms.Amount = "999"
	_, resp = p.chargeRequest(p.signFor(terms))
	got, _ := offered(t, resp)
	if !strings.HasPrefix(got.Error, x402.ReasonInvalidRequirements) {
		t.Errorf("error %q", got.Error)
	}
	if p.inflight() != "" {
		t.Error("reserved for an unmatched payment")
	}
}

// A refused settle withholds the response with a 402 that still offers every option.
func TestChargeRefusedSettleOffersEveryOptionAgain(t *testing.T) {
	p, f := setupCharge(t)
	f.answer(400, `{"success":false,"errorReason":"insufficient_funds","transaction":"","network":"eip155:72344"}`)
	_, resp := p.chargeRequest("")
	pr, _ := offered(t, resp)
	stream, _ := p.chargeRequest(p.signFor(pr.Accepts[0]))
	got, _ := offered(t, p.respond(stream, "200"))
	if len(got.Accepts) != 3 || !strings.HasPrefix(got.Error, "insufficient_funds") {
		t.Errorf("withheld 402: error %q, %d options", got.Error, len(got.Accepts))
	}
	p.decided(SettleRefused)
}

// One token offered by two methods: a payment is matched by its method too, so it never runs
// the other method's checks.
func TestMatchOfferComparesTheTransferMethod(t *testing.T) {
	terms := func(method string) x402.Requirements {
		return x402.Requirements{Scheme: "exact", Network: "eip155:84532", Amount: "1000", Asset: sellerAddr, PayTo: sellerAddr,
			Extra: &x402.ExactExtra{AssetTransferMethod: method}}
	}
	offers := []offer{{req: terms(x402.TransferEIP3009), why: "first"}, {req: terms(x402.TransferPermit2), why: "second"}}
	if o, ok := matchOffer(offers, terms(x402.TransferPermit2)); !ok || o.why != "second" {
		t.Errorf("a Permit2 payment matched %q, %v", o.why, ok)
	}
	if o, ok := matchOffer(offers, terms(x402.TransferEIP3009)); !ok || o.why != "first" {
		t.Errorf("an EIP-3009 payment matched %q, %v", o.why, ok)
	}
	if _, ok := matchOffer(offers[:1], terms(x402.TransferPermit2)); ok {
		t.Error("a Permit2 payment matched an EIP-3009-only offer")
	}
}

// A compiled option's decimals are the document's word: nonsense must not crash the 402.
func TestHelpEntrySurvivesBadDecimals(t *testing.T) {
	req := x402.Requirements{Scheme: "exact", Network: "eip155:1", Amount: "1000", Asset: sellerAddr, PayTo: sellerAddr}
	for _, decimals := range []int{-1, 2_000_000_000} {
		d := policy.Decision{Accepts: []policy.Option{{Decimals: decimals}}}
		e := helpEntry("httproute/demo/x", d, []offer{{req: req}})
		if got := e.Network.Token.Decimals; got < 0 || got > maxTokenDecimals {
			t.Errorf("decimals %d kept as %d", decimals, got)
		}
		_ = e.Network.Token.Format(e.Price) // must not panic
	}
}

// extra reaches the buyer byte for byte, so an integer past 2^53 keeps its digits.
func TestExtraAsWrittenKeepsLargeIntegers(t *testing.T) {
	var o policy.Option
	if err := json.Unmarshal([]byte(`{"scheme":"exact","extra":{"name":"T","maxAmount":12345678901234567890}}`), &o); err != nil {
		t.Fatal(err)
	}
	if got := extraAsWritten(o); !strings.Contains(got, "12345678901234567890") {
		t.Errorf("extra = %s", got)
	}
}
