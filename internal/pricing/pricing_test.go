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
		"capture sync":           {mutate(t, func(r map[string]any) { r["captureMode"] = "Sync" }), "captureMode"},
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

// One bad route rejects the whole table, and every bad route is named.
func TestParseRejectsWholeTableAndNamesEveryRoute(t *testing.T) {
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
	_, err := Parse(b, radius.Testnet)
	if err == nil || !strings.Contains(err.Error(), `"httproute/demo/a"`) || !strings.Contains(err.Error(), `"httproute/demo/b"`) {
		t.Fatalf("err %v", err)
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
