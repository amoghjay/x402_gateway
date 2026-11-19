package pricing

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/radius"
)

func example(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/prices.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseExample(t *testing.T) {
	p, err := Parse(example(t), radius.Testnet)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := p.Lookup("httproute/demo/httpbin")
	if !ok || p.Len() != 1 {
		t.Fatalf("lookup: %v, %d routes", ok, p.Len())
	}
	if e.Price.Int64() != 1000 || e.Network != radius.Testnet {
		t.Errorf("entry %+v", e)
	}
	// The offer is exactly what the buyers and the verifier expect for SBC on testnet.
	want := radius.Testnet.ExactRequirements("1000", "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4", 300)
	if !reflect.DeepEqual(e.Requirements, want) {
		t.Errorf("requirements %+v, want %+v", e.Requirements, want)
	}
	if _, ok := p.Lookup("httproute/demo/other"); ok {
		t.Error("unknown route found")
	}
}

// mutate applies f to the example's one route and returns the re-encoded table.
func mutate(t *testing.T, f func(r map[string]any)) []byte {
	t.Helper()
	var tbl map[string]any
	if err := json.Unmarshal(example(t), &tbl); err != nil {
		t.Fatal(err)
	}
	f(tbl["routes"].(map[string]any)["httproute/demo/httpbin"].(map[string]any))
	b, _ := json.Marshal(tbl)
	return b
}

func TestParseRejects(t *testing.T) {
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"price in display units": {mutate(t, func(r map[string]any) { r["price"] = "0.001" }), "atomic units"},
		"zero price":             {mutate(t, func(r map[string]any) { r["price"] = "0" }), "positive integer"},
		"price above 2^53":       {mutate(t, func(r map[string]any) { r["price"] = "9007199254740992" }), "2^53"},
		"price as a number":      {mutate(t, func(r map[string]any) { r["price"] = 1000 }), "decode"},
		"other asset":            {mutate(t, func(r map[string]any) { r["asset"] = radius.Permit2 }), "only SBC"},
		"permit2":                {mutate(t, func(r map[string]any) { r["assetTransferMethod"] = "permit2" }), "only \"eip3009\""},
		"missing transfer":       {mutate(t, func(r map[string]any) { delete(r, "assetTransferMethod") }), "assetTransferMethod"},
		"bad payTo":              {mutate(t, func(r map[string]any) { r["payTo"] = "0x1234" }), "payTo"},
		"zero payTo":             {mutate(t, func(r map[string]any) { r["payTo"] = "0x0000000000000000000000000000000000000000" }), "zero address"},
		"mainnet not allowed":    {mutate(t, func(r map[string]any) { r["network"] = radius.Mainnet.Network() }), "not one this filter charges on"},
		"timeout too short":      {mutate(t, func(r map[string]any) { r["maxTimeoutSeconds"] = 5 }), "maxTimeoutSeconds"},
		"timeout too long":       {mutate(t, func(r map[string]any) { r["maxTimeoutSeconds"] = 3600 }), "maxTimeoutSeconds"},
		"failure open":           {mutate(t, func(r map[string]any) { r["failureMode"] = "Open" }), "failureMode"},
		"capture unknown":        {mutate(t, func(r map[string]any) { r["captureMode"] = "Later" }), "captureMode"},
		"typo in a key":          {mutate(t, func(r map[string]any) { r["payto"] = r["payTo"] }), "unknown field"},
		"http docs":              {mutate(t, func(r map[string]any) { r["help"] = map[string]any{"docsURL": "http://x.example"} }), "https"},
		"newline in message":     {mutate(t, func(r map[string]any) { r["help"] = map[string]any{"message": "a\r\nSet-Cookie: x"} }), "printable ASCII"},
		"long description":       {mutate(t, func(r map[string]any) { r["description"] = strings.Repeat("x", 300) }), "description"},
		"schema 2":               {[]byte(`{"schemaVersion":2,"routes":{}}`), "schemaVersion 2"},
		"no version":             {[]byte(`{"routes":{}}`), "schemaVersion 0"},
		"trailing data":          {append(example(t), []byte(`{}`)...), "after top-level value"},
		"bad key":                {[]byte(`{"schemaVersion":1,"routes":{"demo/httpbin":{}}}`), "httproute/<namespace>/<name>"},
		"key with rule":          {[]byte(`{"schemaVersion":1,"routes":{"httproute/demo/httpbin/rule/0":{}}}`), "httproute/<namespace>/<name>"},
		"not json":               {[]byte(`routes: {}`), "decode"},
	} {
		_, err := Parse(c.data, radius.Testnet)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want it to mention %q", name, err, c.want)
		}
	}
}

// A bad route is blocked, not dropped: the good routes still load, the bad ones are
// refused (Blocked), and the error names every one. Before 2026-11-19 one bad route
// rejected the whole table, so a new filter pod never became ready.
func TestParseBlocksBadRoutesAndNamesEach(t *testing.T) {
	var tbl map[string]any
	_ = json.Unmarshal(example(t), &tbl)
	routes := tbl["routes"].(map[string]any)
	good := routes["httproute/demo/httpbin"].(map[string]any)
	bad1, bad2 := map[string]any{}, map[string]any{}
	for k, v := range good {
		bad1[k], bad2[k] = v, v
	}
	bad1["price"], bad2["payTo"] = "-1", "nope"
	routes["httproute/demo/a"], routes["httproute/demo/b"] = bad1, bad2
	b, _ := json.Marshal(tbl)
	p, err := Parse(b, radius.Testnet)
	if err == nil || !strings.Contains(err.Error(), `"httproute/demo/a"`) || !strings.Contains(err.Error(), `"httproute/demo/b"`) {
		t.Fatalf("err %v", err)
	}
	if p == nil {
		t.Fatal("no table: the good route should still load")
	}
	if _, ok := p.Lookup("httproute/demo/httpbin"); !ok {
		t.Error("the good route is not priced")
	}
	for _, k := range []string{"httproute/demo/a", "httproute/demo/b"} {
		if _, ok := p.Lookup(k); ok {
			t.Errorf("%s is priced", k)
		}
		if _, ok := p.Blocked(k); !ok {
			t.Errorf("%s is not blocked", k)
		}
	}
	if got := p.BlockedKeys(); len(got) != 2 {
		t.Errorf("BlockedKeys = %v", got)
	}
}

// Every route-level mistake blocks its route; only a broken document rejects the table.
func TestRouteMistakesBlockTheRouteOnly(t *testing.T) {
	for name, data := range map[string][]byte{
		"typo in a key":     mutate(t, func(r map[string]any) { r["payto"] = r["payTo"] }),
		"price as a number": mutate(t, func(r map[string]any) { r["price"] = 1000 }),
		"network not served": mutate(t, func(r map[string]any) {
			r["network"] = radius.BaseSepolia.Network()
			r["asset"] = radius.BaseSepolia.Token.Address
		}),
		"mainnet not allowed":    mutate(t, func(r map[string]any) { r["network"] = radius.Mainnet.Network() }),
		"route is not an object": []byte(`{"schemaVersion":1,"routes":{"httproute/demo/httpbin":"free"}}`),
	} {
		p, err := Parse(data, radius.Testnet)
		if err == nil || p == nil {
			t.Errorf("%s: table %v, err %v; want the table with the route blocked", name, p, err)
			continue
		}
		if _, ok := p.Blocked("httproute/demo/httpbin"); !ok {
			t.Errorf("%s: route not blocked", name)
		}
	}
	for name, data := range map[string][]byte{
		"unknown top-level key": []byte(`{"schemaVersion":1,"routes":{},"route":{}}`),
		"bad blocked key":       []byte(`{"schemaVersion":1,"routes":{},"blocked":{"demo/x":"why"}}`),
		"blocked not a map":     []byte(`{"schemaVersion":1,"routes":{},"blocked":["httproute/demo/x"]}`),
		"routes not a map":      []byte(`{"schemaVersion":1,"routes":[]}`),
	} {
		if p, err := Parse(data, radius.Testnet); p != nil || err == nil {
			t.Errorf("%s: table %v, err %v; want the whole table rejected", name, p, err)
		}
	}
}

// The controller lists routes whose policy is invalid; they are blocked even if they
// also appear priced.
func TestControllerBlockedRoutes(t *testing.T) {
	var tbl map[string]any
	_ = json.Unmarshal(example(t), &tbl)
	tbl["blocked"] = map[string]any{"httproute/demo/httpbin": "price: not atomic units", "httproute/shop/cart": "network not served"}
	b, _ := json.Marshal(tbl)
	p, err := Parse(b, radius.Testnet)
	if err != nil {
		t.Fatalf("a table with controller-blocked routes is not an error: %v", err)
	}
	if _, ok := p.Lookup("httproute/demo/httpbin"); ok {
		t.Error("a blocked route is still priced")
	}
	if why, ok := p.Blocked("httproute/shop/cart"); !ok || !strings.Contains(why, "network not served") {
		t.Errorf("Blocked = %q, %v", why, ok)
	}
}

func TestMainnetWhenAllowed(t *testing.T) {
	b := mutate(t, func(r map[string]any) { r["network"] = radius.Mainnet.Network() })
	p, err := Parse(b, radius.Testnet, radius.Mainnet)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := p.Lookup("httproute/demo/httpbin"); e.Requirements.Network != "eip155:723487" {
		t.Errorf("network %s", e.Requirements.Network)
	}
}

func TestRouteKey(t *testing.T) {
	for in, want := range map[string]string{
		"httproute/demo/httpbin/rule/0/match/0":                "httproute/demo/httpbin",
		"httproute/demo/httpbin/rule/2/match/1/cors-preflight": "httproute/demo/httpbin",
		"httproute/a-b/c.d/rule/0/match/0":                     "httproute/a-b/c.d",
	} {
		if got, ok := RouteKey(in); !ok || got != want {
			t.Errorf("RouteKey(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"", "httproute/demo/httpbin", "grpcroute/demo/svc/rule/0/match/0",
		"httproute//httpbin/rule/0", "httproute/Demo/httpbin/rule/0", "httproute/demo/../rule/0",
	} {
		if got, ok := RouteKey(in); ok {
			t.Errorf("RouteKey(%q) = %q, want refusal", in, got)
		}
	}
}

func TestStore(t *testing.T) {
	s := NewStore()
	if s.Load() != nil {
		t.Fatal("empty store returned a table")
	}
	select {
	case <-s.Loaded():
		t.Fatal("loaded before a table was set")
	default:
	}
	p, _ := Parse(example(t), radius.Testnet)
	s.Set(p)
	s.Set(p) // a second Set must not close the channel twice
	<-s.Loaded()
	if s.Load() != p {
		t.Error("stored table not returned")
	}
	var none *Prices
	if _, ok := none.Lookup("x"); ok || none.Len() != 0 {
		t.Error("nil table found something")
	}
}

// A route is priced in its own network's token: USDC's address on a Radius route, or
// SBC's on a Base route, rejects the table.
func TestAssetMustBeTheNetworksToken(t *testing.T) {
	base := mutate(t, func(r map[string]any) {
		r["network"], r["asset"] = radius.BaseSepolia.Network(), radius.BaseSepolia.Token.Address
	})
	p, err := Parse(base, radius.Testnet, radius.BaseSepolia)
	if err != nil {
		t.Fatalf("a Base Sepolia route in USDC was refused: %v", err)
	}
	e, _ := p.Lookup("httproute/demo/httpbin")
	if e.Requirements.Extra.Name != "USDC" || e.Requirements.Extra.Version != "2" || e.Domain.ChainID != 84532 || e.Domain.Name != "USDC" {
		t.Errorf("entry = %+v, domain %+v", e.Requirements, e.Domain)
	}
	if _, err := Parse(base, radius.Testnet); err == nil || !strings.Contains(err.Error(), "not one this filter charges on") {
		t.Errorf("a network the filter was not started with was accepted: %v", err)
	}
	for name, data := range map[string][]byte{
		"USDC on Radius": mutate(t, func(r map[string]any) { r["asset"] = radius.BaseSepolia.Token.Address }),
		"SBC on Base":    mutate(t, func(r map[string]any) { r["network"] = radius.BaseSepolia.Network() }),
	} {
		if _, err := Parse(data, radius.Testnet, radius.BaseSepolia); err == nil || !strings.Contains(err.Error(), "is supported on") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
