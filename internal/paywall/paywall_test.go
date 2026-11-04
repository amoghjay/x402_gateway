package paywall

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/amoghjay/x402_gateway/internal/buyer"
	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
	"github.com/amoghjay/x402_gateway/internal/x402/verify"
)

const url = "http://34.173.224.114/get?q=1&r=2"

func entry(t *testing.T, mutate func(*pricing.Route)) *pricing.Entry {
	t.Helper()
	b, err := os.ReadFile("../pricing/testdata/prices.json")
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		var tbl pricing.Table
		_ = json.Unmarshal(b, &tbl)
		r := tbl.Routes["httproute/demo/httpbin"]
		mutate(&r)
		tbl.Routes["httproute/demo/httpbin"] = r
		b, _ = json.Marshal(tbl)
	}
	p, err := pricing.Parse(b, radius.Testnet, radius.Mainnet)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := p.Lookup("httproute/demo/httpbin")
	return e
}

// cli is what radius-cli sends: no Accept or User-Agent of its own (Node's defaults).
var cli = Request{Method: http.MethodGet, URL: url, Accept: "*/*", UserAgent: "node"}

var browser = Request{Method: http.MethodGet, URL: url,
	Accept:    "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15"}

func decodeHeader(t *testing.T, r Response) x402.PaymentRequired {
	t.Helper()
	pr, err := x402.DecodePaymentRequired(r.Headers.Get(x402.HeaderPaymentRequired))
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

func TestAPIClientGetsHeaderAndJSONBody(t *testing.T) {
	e := entry(t, nil)
	r, err := Render(e, cli)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != http.StatusPaymentRequired {
		t.Fatalf("status %d", r.Status)
	}
	for k, want := range map[string]string{
		"Content-Type":                  "application/json",
		"Cache-Control":                 "no-store",
		"Link":                          `<https://docs.radiustech.xyz/build/x402/>; rel="help"`,
		"Access-Control-Expose-Headers": "PAYMENT-REQUIRED",
	} {
		if got := r.Headers.Get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}

	pr := decodeHeader(t, r)
	if pr.X402Version != 2 || len(pr.Accepts) != 1 || !reflect.DeepEqual(pr.Accepts[0], e.Requirements) {
		t.Errorf("header %+v", pr)
	}
	if pr.Resource.URL != url || pr.Resource.Description != "httpbin, one call" || pr.Resource.MimeType != "application/json" {
		t.Errorf("resource %+v", pr.Resource)
	}
	// Clients echo header extensions into PAYMENT-SIGNATURE; how-to-pay stays in the body.
	if len(pr.Extensions) != 0 {
		t.Errorf("header carries extensions %v", pr.Extensions)
	}
	for _, want := range []string{"0.001 SBC on Radius testnet", "RADIUS_NETWORK=testnet radius-cli wallet pay get '" + url + "' --threshold 0.001", "npm i -g radius-cli", "createRadiusFetch", "get test SBC"} {
		if !strings.Contains(pr.Error, want) {
			t.Errorf("error line lacks %q:\n%s", want, pr.Error)
		}
	}
	if strings.IndexFunc(pr.Error, func(c rune) bool { return c < 0x20 || c > 0x7e }) >= 0 {
		t.Errorf("error line is not one line of printable ASCII: %q", pr.Error)
	}

	var body struct {
		x402.PaymentRequired
		Extensions map[string]struct {
			Info   HowToPay        `json:"info"`
			Schema json.RawMessage `json:"schema"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body.Accepts, pr.Accepts) || body.Error != pr.Error || !reflect.DeepEqual(body.Resource, pr.Resource) {
		t.Error("body and header disagree")
	}
	ext, ok := body.Extensions[HowToPayExtension]
	if !ok || !json.Valid(ext.Schema) || len(ext.Schema) < 10 {
		t.Fatalf("how-to-pay extension %+v", body.Extensions)
	}
	if ext.Info.Price != "0.001" || ext.Info.Faucet != radius.Testnet.Faucet || ext.Info.SDK.Import != "radius-sdk/client" {
		t.Errorf("info %+v", ext.Info)
	}
}

func TestBrowserGetsHTMLAndStillTheHeader(t *testing.T) {
	r, err := Render(entry(t, nil), browser)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != http.StatusPaymentRequired || r.Headers.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("status %d, content-type %q", r.Status, r.Headers.Get("Content-Type"))
	}
	decodeHeader(t, r) // an x402 client that sends a browser's headers still gets the protocol
	if r.Headers.Get("Cache-Control") != "no-store" {
		t.Error("no no-store on the HTML 402")
	}
	html := string(r.Body)
	for _, want := range []string{"0.001 SBC", "Radius testnet", "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4", "npm i -g radius-cli", "faucet"} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// Coinbase's rule: both conditions, never one alone.
func TestBrowserNeedsBothAcceptAndUserAgent(t *testing.T) {
	for name, req := range map[string]Request{
		"text/html, curl UA":  {Method: "GET", URL: url, Accept: "text/html", UserAgent: "curl/8.7.1"},
		"Mozilla, JSON first": {Method: "GET", URL: url, Accept: "application/json", UserAgent: "Mozilla/5.0"},
		"nothing":             {Method: "GET", URL: url},
	} {
		r, err := Render(entry(t, nil), req)
		if err != nil || r.Headers.Get("Content-Type") != "application/json" {
			t.Errorf("%s: got %q %v", name, r.Headers.Get("Content-Type"), err)
		}
	}
}

func TestPageEscapesTheURL(t *testing.T) {
	req := browser
	req.URL = `http://x/"><script>alert(1)</script>`
	r, err := Render(entry(t, nil), req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(r.Body), "<script>") {
		t.Fatal("request URL reached the page unescaped")
	}
}

func TestHelpOverrides(t *testing.T) {
	e := entry(t, func(r *pricing.Route) {
		r.Help = &pricing.Help{DocsURL: "https://api.example/pay", Message: "Ask ops for credit."}
	})
	r, err := Render(e, cli)
	if err != nil {
		t.Fatal(err)
	}
	pr := decodeHeader(t, r)
	if r.Headers.Get("Link") != `<https://api.example/pay>; rel="help"` || !strings.HasSuffix(pr.Error, "Docs: https://api.example/pay Ask ops for credit.") {
		t.Errorf("link %q, error %q", r.Headers.Get("Link"), pr.Error)
	}
}

func TestMainnetHasNoFaucetOrNetworkPrefix(t *testing.T) {
	e := entry(t, func(r *pricing.Route) { r.Network = radius.Mainnet.Network(); r.Help = nil })
	r, err := Render(e, Request{Method: http.MethodPost, URL: url})
	if err != nil {
		t.Fatal(err)
	}
	pr := decodeHeader(t, r)
	if strings.Contains(pr.Error, "RADIUS_NETWORK") || !strings.Contains(pr.Error, "radius-cli wallet pay post ") || !strings.Contains(pr.Error, "Radius mainnet") {
		t.Errorf("error %q", pr.Error)
	}
	if strings.Contains(string(r.Body), `"faucet":"`) { // the schema names the property; no value
		t.Error("mainnet 402 offers a faucet")
	}
}

func TestLongURLIsElidedFromTheHint(t *testing.T) {
	req := cli
	req.URL = "http://x/" + strings.Repeat("a", 2000)
	r, err := Render(entry(t, nil), req)
	if err != nil {
		t.Fatal(err)
	}
	pr := decodeHeader(t, r)
	if strings.Contains(pr.Error, "aaaa") || !strings.Contains(pr.Error, "<this URL>") || pr.Resource.URL != req.URL {
		t.Errorf("error %q, resource %q", pr.Error[:120], pr.Resource.URL[:20])
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`http://x/it's?a=1&b=2`); got != `'http://x/it'\''s?a=1&b=2'` {
		t.Errorf("%s", got)
	}
}

// TestOurBuyerPaysOurPaywall closes the loop: the M2 buyer reads the header this package
// renders, chooses the offer under the CLI's threshold, and the filter's verifier accepts
// what it signed against the route's own requirements.
func TestOurBuyerPaysOurPaywall(t *testing.T) {
	e := entry(t, nil)
	r, err := Render(e, cli)
	if err != nil {
		t.Fatal(err)
	}
	pr := decodeHeader(t, r)
	key, _ := evm.ParsePrivateKey("0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d")
	w := &buyer.Wallet{Key: key, Network: radius.Testnet}
	offer, err := w.Choose(pr, e.Price)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pp, err := w.Sign(offer, &pr.Resource, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify.Verify(now, e.Requirements, w.Domain(), pp); err != nil {
		t.Fatal(err)
	}
}
