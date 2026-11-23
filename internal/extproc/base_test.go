package extproc

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/amoghjay/x402_gateway/internal/balance"
	"github.com/amoghjay/x402_gateway/internal/buyer"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// A second network is a profile: the same filter prices a route in Base Sepolia's USDC,
// tells the buyer so in the 402, and recovers the signature against USDC's EIP-712
// domain, not SBC's.

// basePrices is the example table with the demo route moved to Base Sepolia.
func basePrices(t *testing.T) *pricing.Store {
	t.Helper()
	b, err := os.ReadFile("../pricing/testdata/prices.json")
	if err != nil {
		t.Fatal(err)
	}
	var tbl map[string]any
	if err := json.Unmarshal(b, &tbl); err != nil {
		t.Fatal(err)
	}
	r := tbl["routes"].(map[string]any)["httproute/demo/httpbin"].(map[string]any)
	r["network"], r["asset"] = radius.BaseSepolia.Network(), radius.BaseSepolia.Token.Address
	b, _ = json.Marshal(tbl)
	p, err := pricing.Parse(b, radius.Testnet, radius.BaseSepolia)
	if err != nil {
		t.Fatal(err)
	}
	s := pricing.NewStore()
	s.Set(p)
	return s
}

func setupBase(t *testing.T) *paid {
	t.Helper()
	return setupWith(t, 5000, 0, basePrices(t), func(pay *Payments, node *radius.RPC) {
		testnet := pay.Balances[radius.Testnet.Network()]
		pay.Balances[radius.BaseSepolia.Network()] = &balance.Cache{
			Redis: testnet.Redis, RPC: node, Token: radius.BaseSepolia.TokenAddress(), Log: testnet.Log,
		}
	})
}

// sign pays the demo route's terms as a wallet on the given network would.
func (p *paid) sign(network radius.Profile, terms x402.Requirements) string {
	p.t.Helper()
	w := &buyer.Wallet{Key: p.w.Key, Network: network}
	pp, err := w.Sign(terms, &x402.Resource{URL: "http://gateway.example/get"}, time.Now(), 5*time.Minute)
	if err != nil {
		p.t.Fatal(err)
	}
	hdr, err := pp.Encode()
	if err != nil {
		p.t.Fatal(err)
	}
	return hdr
}

func TestBaseSepoliaRouteIsPricedInUSDC(t *testing.T) {
	p := setupBase(t)
	stream := open(t, p.conn)
	ir := roundTrip(t, stream, requestHeaders(priced)).GetImmediateResponse()
	if ir.GetStatus().GetCode() != 402 {
		t.Fatalf("status = %v, want 402", ir.GetStatus().GetCode())
	}
	pr, err := x402.DecodePaymentRequired(immediateHeaders(t, ir)["payment-required"])
	if err != nil || len(pr.Accepts) != 1 {
		t.Fatalf("PAYMENT-REQUIRED: %+v, %v", pr, err)
	}
	a := pr.Accepts[0]
	if a.Network != "eip155:84532" || !strings.EqualFold(a.Asset, radius.BaseSepolia.Token.Address) ||
		a.Extra == nil || a.Extra.Name != "USDC" || a.Extra.Version != "2" {
		t.Errorf("offer = %+v (extra %+v), want USDC on Base Sepolia with its EIP-712 domain", a, a.Extra)
	}
	// radius-cli 0.8.0 pays USDC on Base: its testnet side, Base as the network.
	if !strings.Contains(pr.Error, "0.001 USDC on Base Sepolia") || !strings.Contains(pr.Error, "radius-cli --network testnet wallet pay get") ||
		!strings.Contains(pr.Error, "--networks base --threshold 0.001") {
		t.Errorf("error line = %q", pr.Error)
	}
}

func TestBaseSepoliaPaymentIsVerifiedAgainstUSDCsDomain(t *testing.T) {
	p := setupBase(t)
	terms := radius.BaseSepolia.ExactRequirements("1000", "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4", 300)

	_, resp := p.request(p.sign(radius.BaseSepolia, terms))
	if resp.GetRequestHeaders() == nil {
		t.Fatalf("a payment signed for USDC on Base Sepolia was not accepted: %v", resp)
	}
	p.decided(PaymentAccepted)

	// The same terms signed against SBC's domain on the same chain id: the recovered
	// signer is someone else, so it is refused. A filter that kept using one hard-coded
	// domain would accept this one and refuse the real one.
	wrong := radius.BaseSepolia
	wrong.Token.Name, wrong.Token.Version = radius.SBCName, radius.SBCVersion
	_, resp = p.request(p.sign(wrong, terms))
	if ir := resp.GetImmediateResponse(); ir.GetStatus().GetCode() != 402 || ir.GetDetails() != "x402_payment_invalid" {
		t.Errorf("a payment signed against the wrong token domain was not refused: %v", resp)
	}
}
