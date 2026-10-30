package verify

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
	"github.com/amoghjay/x402_gateway/internal/x402/eip3009"
)

const (
	vectorKey = "0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	payTo     = "0x209693Bc6afc0C5328bA36FaF03C514EF312287C"
)

var (
	now    = time.Unix(1_800_000_000, 0)
	domain = func() eip3009.Domain {
		c, _ := evm.ParseAddress(radius.SBC)
		return eip3009.Domain{Name: radius.SBCName, Version: radius.SBCVersion, ChainID: radius.Testnet.ChainID, VerifyingContract: c}
	}()
	route = radius.Testnet.ExactRequirements("1000", payTo, 300)
)

// sign builds a PaymentPayload for route, signed by key, with the given authorization
// fields (the buyer side of the protocol, in miniature).
func sign(t *testing.T, key *evm.PrivateKey, req x402.Requirements, value, validAfter, validBefore int64) x402.PaymentPayload {
	t.Helper()
	to, _ := evm.ParseAddress(req.PayTo)
	auth := eip3009.Authorization{
		From: key.Address(), To: to, Value: big.NewInt(value),
		ValidAfter: big.NewInt(validAfter), ValidBefore: big.NewInt(validBefore),
	}
	auth.Nonce = evm.Keccak256([]byte(time.Now().String()))
	sig := key.Sign(eip3009.Digest(domain, auth))
	payload, _ := json.Marshal(x402.EIP3009Payload{Signature: evm.Hex(sig), Authorization: auth.Wire()})
	return x402.PaymentPayload{X402Version: x402.Version, Accepted: req, Payload: payload}
}

func key(t *testing.T) *evm.PrivateKey {
	t.Helper()
	k, err := evm.ParsePrivateKey(vectorKey)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestGoldenVectorVerifies(t *testing.T) {
	b, err := os.ReadFile("../testdata/radius_eip3009.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Header string `json:"header"`
		Signer string `json:"signer"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	pp, err := x402.DecodePaymentPayload(v.Header)
	if err != nil {
		t.Fatal(err)
	}
	// The vector's validBefore is 1.9e9; verify it a minute before, inside the route's
	// 300 s accept window. At any earlier clock the cap refuses it, correctly.
	if _, err := Verify(now, route, domain, pp); Reason(err) != x402.ReasonValidBefore {
		t.Errorf("accept-window cap did not refuse a 3-year window: %v", err)
	}
	res, err := Verify(time.Unix(1_900_000_000-60, 0), route, domain, pp)
	if err != nil {
		t.Fatalf("%v (reason %q)", err, Reason(err))
	}
	if res.Payer.String() != v.Signer || res.Expires.Unix() != 1_900_000_000 || res.Authorization.Value.Int64() != 1000 {
		t.Errorf("%+v", res)
	}
}

func TestOurOwnSignatureVerifies(t *testing.T) {
	k := key(t)
	pp := sign(t, k, route, 1000, 0, now.Unix()+60)
	res, err := Verify(now, route, domain, pp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Payer != k.Address() {
		t.Errorf("payer %s", res.Payer)
	}
	// Overpaying is fine: the price is a floor.
	if _, err := Verify(now, route, domain, sign(t, k, route, 5000, 0, now.Unix()+60)); err != nil {
		t.Errorf("overpayment refused: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	k := key(t)
	other, _ := evm.GeneratePrivateKey()
	good := func() x402.PaymentPayload { return sign(t, k, route, 1000, 0, now.Unix()+60) }
	withAccepted := func(f func(r *x402.Requirements)) x402.PaymentPayload {
		pp := good()
		r := pp.Accepted
		r.Extra = &x402.ExactExtra{AssetTransferMethod: route.Extra.AssetTransferMethod, Name: route.Extra.Name, Version: route.Extra.Version}
		f(&r)
		pp.Accepted = r
		return pp
	}
	pricier := route
	pricier.Amount = "2000"
	elsewhere := radius.Testnet.ExactRequirements("1000", "0x857b06519E91e3A54538791bDbb0E22373e36b66", 300)

	cases := map[string]struct {
		route x402.Requirements
		pp    x402.PaymentPayload
		want  string
	}{
		"version 1":    {route, func() x402.PaymentPayload { p := good(); p.X402Version = 1; return p }(), x402.ReasonInvalidVersion},
		"scheme echo":  {route, withAccepted(func(r *x402.Requirements) { r.Scheme = "upto" }), x402.ReasonInvalidScheme},
		"network echo": {route, withAccepted(func(r *x402.Requirements) { r.Network = "eip155:723487" }), x402.ReasonInvalidNetwork},
		"asset echo":   {route, withAccepted(func(r *x402.Requirements) { r.Asset = "0x036CbD53842c5426634e7929541eC2318f3dCF7e" }), x402.ReasonInvalidPayload},
		"payTo echo":   {route, withAccepted(func(r *x402.Requirements) { r.PayTo = elsewhere.PayTo }), x402.ReasonRecipientMismatch},
		"amount echo":  {route, withAccepted(func(r *x402.Requirements) { r.Amount = "1" }), x402.ReasonValueMismatch},
		"permit2 echo": {route, withAccepted(func(r *x402.Requirements) { r.Extra.AssetTransferMethod = "permit2" }), x402.ReasonInvalidPayload},
		"underpaid":    {pricier, func() x402.PaymentPayload { p := sign(t, k, pricier, 1000, 0, now.Unix()+60); return p }(), x402.ReasonValueMismatch},
		"wrong recipient": {route, func() x402.PaymentPayload {
			p := sign(t, k, elsewhere, 1000, 0, now.Unix()+60)
			p.Accepted = route
			return p
		}(), x402.ReasonRecipientMismatch},
		"not yet valid":   {route, sign(t, k, route, 1000, now.Unix()+10, now.Unix()+60), x402.ReasonValidAfter},
		"expired":         {route, sign(t, k, route, 1000, 0, now.Unix()), x402.ReasonValidBefore},
		"window too long": {route, sign(t, k, route, 1000, 0, now.Unix()+301), x402.ReasonValidBefore},
		"other signer": {route, func() x402.PaymentPayload {
			// Signed by k, but claiming to be from `other`: the recovered address must
			// equal authorization.from, so this is a signature failure.
			p := good()
			var e x402.EIP3009Payload
			_ = json.Unmarshal(p.Payload, &e)
			e.Authorization.From = other.Address().String()
			p.Payload, _ = json.Marshal(e)
			return p
		}(), x402.ReasonSignature},
		"someone else pays": {route, sign(t, other, route, 1000, 0, now.Unix()+60), ""}, // a valid payment from another wallet
		"corrupt sig": {route, func() x402.PaymentPayload {
			p := good()
			var e x402.EIP3009Payload
			_ = json.Unmarshal(p.Payload, &e)
			// Flip a nibble inside r (v=0 would just be 27 spelled differently).
			b := []byte(e.Signature)
			if b[20] == '0' {
				b[20] = '1'
			} else {
				b[20] = '0'
			}
			e.Signature = string(b)
			p.Payload, _ = json.Marshal(e)
			return p
		}(), x402.ReasonSignature},
		"malformed payload": {route, func() x402.PaymentPayload { p := good(); p.Payload = json.RawMessage(`{"signature":"0x"}`); return p }(), x402.ReasonInvalidPayload},
		"bad nonce": {route, func() x402.PaymentPayload {
			p := good()
			var e x402.EIP3009Payload
			_ = json.Unmarshal(p.Payload, &e)
			e.Authorization.Nonce = "1"
			p.Payload, _ = json.Marshal(e)
			return p
		}(), x402.ReasonInvalidPayload},
	}
	for name, c := range cases {
		_, err := Verify(now, c.route, domain, c.pp)
		if got := Reason(err); got != c.want {
			t.Errorf("%s: reason %q, want %q (%v)", name, got, c.want, err)
		}
	}
}

func TestRouteRequirementsAreTheTerms(t *testing.T) {
	// A payment signed for a different (cheaper) route is refused against this route
	// even though it echoes its own terms consistently: the route decides.
	k := key(t)
	cheap := radius.Testnet.ExactRequirements("1", payTo, 300)
	pp := sign(t, k, cheap, 1, 0, now.Unix()+60)
	if _, err := Verify(now, route, domain, pp); Reason(err) != x402.ReasonValueMismatch {
		t.Errorf("cheap echo accepted: %v", err)
	}
	// Unsupported route configurations are refused as requirements errors.
	p2 := route
	p2.Extra = &x402.ExactExtra{AssetTransferMethod: "permit2"}
	if _, err := Verify(now, p2, domain, sign(t, k, route, 1000, 0, now.Unix()+60)); Reason(err) != x402.ReasonInvalidRequirements {
		t.Errorf("permit2 route: %v", err)
	}
}

func BenchmarkVerify(b *testing.B) {
	k, _ := evm.ParsePrivateKey(vectorKey)
	t := &testing.T{}
	pp := sign(t, k, route, 1000, 0, now.Unix()+60)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Verify(now, route, domain, pp); err != nil {
			b.Fatal(err)
		}
	}
}
