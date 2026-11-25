package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/pricing"
)

func example(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../internal/pricing/testdata/prices.json")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Replace(string(b), "httproute/demo/httpbin", name, 1)
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.Mkdir(good, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "prices.json"), []byte(example(t, "weather")), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "blocked.json")
	if err := os.WriteFile(blocked, []byte(strings.Replace(example(t, "weather"), "eip155:72344", "eip155:1", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"schemaVersion":`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{good}, 0, "priced    weather: 0.001 SBC on testnet"},
		{[]string{filepath.Join(good, "prices.json")}, 0, "ok: 1 priced, 0 platform"},
		{[]string{blocked}, 1, "BLOCKED   weather"},
		{[]string{broken}, 1, "unreadable"},
		{[]string{filepath.Join(dir, "missing")}, 1, "unreadable"},
		{[]string{"-route-keys", "httproute", good}, 1, "BLOCKED   weather"},
		{[]string{}, 2, ""},
		{[]string{"-networks", "nowhere", good}, 2, ""},
	} {
		var out, errOut bytes.Buffer
		if code := validate(c.args, &out, &errOut); code != c.code || !strings.Contains(out.String(), c.out) {
			t.Errorf("validate %v = %d, want %d with %q\n%s%s", c.args, code, c.code, c.out, out.String(), errOut.String())
		}
	}
}

func TestResolveDefaultsFollowTheMode(t *testing.T) {
	for _, c := range []struct {
		name   string
		c      config
		keys   pricing.KeyStyle
		refuse bool
		err    bool
	}{
		{"configmap", config{namespace: "x402-system"}, pricing.KeysHTTPRoute, false, false},
		{"configmap needs a namespace", config{}, 0, false, true},
		{"standalone needs no namespace", config{pricesDir: "/etc/x402"}, pricing.KeysName, true, false},
		{"standalone, served free on purpose", config{pricesDir: "/etc/x402", unpriced: "serve"}, pricing.KeysName, false, false},
		{"a mounted ConfigMap", config{pricesDir: "/etc/x402", routeKeys: "httproute"}, pricing.KeysHTTPRoute, true, false},
		{"bad -unpriced", config{pricesDir: "/etc/x402", unpriced: "free"}, 0, false, true},
		{"bad -route-keys", config{pricesDir: "/etc/x402", routeKeys: "names"}, 0, false, true},
	} {
		keys, refuse, err := c.c.resolve()
		if (err != nil) != c.err || (err == nil && (keys != c.keys || refuse != c.refuse)) {
			t.Errorf("%s: %v, %v, %v; want %v, %v, error %v", c.name, keys, refuse, err, c.keys, c.refuse, c.err)
		}
	}
}
