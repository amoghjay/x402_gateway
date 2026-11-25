package extproc

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
)

// A standalone filter: the table is keyed by envoy.yaml's route names, and a route the
// table does not name is refused, never served free.

func namedTable(t *testing.T) *pricing.Store {
	t.Helper()
	b := strings.Replace(exampleTable(t), "httproute/demo/httpbin", "paid", 1)
	p, err := pricing.ParseKeys([]byte(b), pricing.KeysName, radius.Testnet)
	if err != nil {
		t.Fatal(err)
	}
	s := pricing.NewStore()
	s.Set(p)
	return s
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestStandaloneRouteNames(t *testing.T) {
	s := NewServer(discard(), 0, namedTable(t)).WithRouteKeys(pricing.KeysName).RefuseUnpriced(true)
	conn := serve(t, s)

	ir := roundTrip(t, open(t, conn), requestHeaders("paid")).GetImmediateResponse()
	if ir.GetStatus().GetCode() != 402 {
		t.Fatalf("route %q: status %v, want the 402", "paid", ir.GetStatus().GetCode())
	}

	// A name the table does not have (a typo in envoy.yaml): 503, not free.
	ir = roundTrip(t, open(t, conn), requestHeaders("piad")).GetImmediateResponse()
	if ir.GetStatus().GetCode() != 503 || ir.GetDetails() != "x402_"+string(UnpricedRefused) {
		t.Errorf("unknown route: %v %q, want 503 %s", ir.GetStatus().GetCode(), ir.GetDetails(), UnpricedRefused)
	}

	// An Envoy Gateway name means the wrong style: not identified, so refused too.
	ir = roundTrip(t, open(t, conn), requestHeaders("has space")).GetImmediateResponse()
	if ir.GetStatus().GetCode() != 500 || ir.GetDetails() != "x402_"+string(NoRouteAttribute) {
		t.Errorf("unusable name: %v %q, want 500 %s", ir.GetStatus().GetCode(), ir.GetDetails(), NoRouteAttribute)
	}
}

// Serving unpriced routes stays possible in standalone mode, as an explicit choice.
func TestStandaloneServeUnpriced(t *testing.T) {
	s := NewServer(discard(), 0, namedTable(t)).WithRouteKeys(pricing.KeysName)
	resp := roundTrip(t, open(t, serve(t, s)), requestHeaders("free"))
	if resp.GetRequestHeaders() == nil {
		t.Errorf("unpriced route with serve: %v, want CONTINUE", resp)
	}
}
