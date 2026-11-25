package pricing

import (
	"strings"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/radius"
)

// A standalone table is keyed by Envoy route names; the cluster's by HTTPRoute.
func TestKeyStyles(t *testing.T) {
	named := strings.Replace(string(example(t)), "httproute/demo/httpbin", "weather", 1)

	p, err := ParseKeys([]byte(named), KeysName, radius.Testnet)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Lookup("weather"); !ok {
		t.Error("a route-name key was not loaded")
	}
	if p, err := ParseKeys([]byte(named), KeysHTTPRoute, radius.Testnet); err == nil || p.Len() != 0 {
		t.Errorf("an HTTPRoute-keyed table took a plain name: %v", err)
	}
	bad := strings.Replace(string(example(t)), "httproute/demo/httpbin", "has space", 1)
	if p, err := ParseKeys([]byte(bad), KeysName, radius.Testnet); err == nil || p.Len() != 0 {
		t.Errorf("a name with a space was loaded: %v", err)
	}

	for _, c := range []struct {
		style KeyStyle
		xds   string
		key   string
		ok    bool
	}{
		{KeysName, "weather", "weather", true},
		{KeysName, "api/v1:paid", "api/v1:paid", true},
		{KeysName, "", "", false},
		{KeysName, strings.Repeat("a", 129), "", false},
		{KeysHTTPRoute, "httproute/demo/httpbin/rule/0/match/0", "httproute/demo/httpbin", true},
		{KeysHTTPRoute, "weather", "", false},
	} {
		k, ok := c.style.Key(c.xds)
		if ok != c.ok || (ok && k != c.key) {
			t.Errorf("%s.Key(%q) = %q, %v; want %q, %v", c.style, c.xds, k, ok, c.key, c.ok)
		}
	}
	for _, s := range []string{"httproute", "name"} {
		if k, err := ParseKeyStyle(s); err != nil || k.String() != s {
			t.Errorf("ParseKeyStyle(%q) = %v, %v", s, k, err)
		}
	}
	if _, err := ParseKeyStyle("names"); err == nil {
		t.Error("ParseKeyStyle took an unknown style")
	}
}
