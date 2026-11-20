package pricing

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/radius"
)

const project = "00000000-0000-4000-8000-0000000000aa" // ../policy/testdata/three_options.json

func doc(t *testing.T, version string) string {
	t.Helper()
	b, err := os.ReadFile("../policy/testdata/three_options.json")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Replace(string(b), `"version": 2`, `"version": `+version, 1)
}

func TestParsePlatform(t *testing.T) {
	table := `{"schemaVersion":1,"routes":{},"platform":{
		"httproute/shop/metered":{"project":"` + project + `","noPolicy":"deny"},
		"httproute/shop/free":{"project":"` + project + `","noPolicy":"allow"}}}`
	p, err := Parse([]byte(table))
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := p.Platform("httproute/shop/metered"); !ok || r.Project != project || r.NoPolicy != "deny" {
		t.Errorf("metered: %+v, %v", r, ok)
	}
	if p.PlatformLen() != 2 || p.Len() != 0 {
		t.Errorf("platform %d, priced %d", p.PlatformLen(), p.Len())
	}
	if _, ok := (*Prices)(nil).Platform("x"); ok {
		t.Error("nil table has a platform route")
	}

	// A broken entry is blocked, never dropped (which would serve the route free).
	for name, entry := range map[string]string{
		"upper-case project": `{"project":"` + strings.ToUpper(project) + `","noPolicy":"deny"}`,
		"bad noPolicy":       `{"project":"` + project + `","noPolicy":"Deny"}`,
		"unknown field":      `{"project":"` + project + `","noPolicy":"deny","extra":1}`,
		"not an object":      `"x"`,
	} {
		p, err := Parse([]byte(`{"schemaVersion":1,"routes":{},"platform":{"httproute/shop/a":` + entry + `}}`))
		if p == nil || err == nil {
			t.Errorf("%s: table %v, err %v; want loaded with the route blocked", name, p, err)
			continue
		}
		if _, ok := p.Blocked("httproute/shop/a"); !ok {
			t.Errorf("%s: not blocked", name)
		}
		if _, ok := p.Platform("httproute/shop/a"); ok {
			t.Errorf("%s: still a platform route", name)
		}
	}

	// A route both priced and on a project is blocked, as neither is clearly meant.
	both := strings.Replace(string(example(t)), `"routes"`, `"platform":{"httproute/demo/httpbin":{"project":"`+project+`","noPolicy":"deny"}},"routes"`, 1)
	p, err = Parse([]byte(both), radius.Testnet)
	if err == nil {
		t.Fatal("a route with a price and a project loaded cleanly")
	}
	if _, ok := p.Blocked("httproute/demo/httpbin"); !ok {
		t.Error("a route with a price and a project is not blocked")
	}
	if _, ok := p.Lookup("httproute/demo/httpbin"); ok {
		t.Error("a route with a price and a project is priced")
	}

	// Blocked by the controller wins over a platform entry.
	p, _ = Parse([]byte(`{"schemaVersion":1,"routes":{},"blocked":{"httproute/shop/a":"conflict"},"platform":{"httproute/shop/a":{"project":"` + project + `","noPolicy":"deny"}}}`))
	if _, ok := p.Platform("httproute/shop/a"); ok {
		t.Error("a blocked route is a platform route")
	}
}

func TestLoadPolicies(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	table, err := Parse([]byte(`{"schemaVersion":1,"routes":{},"platform":{"httproute/shop/a":{"project":"` + project + `","noPolicy":"deny"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	load := func(data map[string]string, prev *Prices) *Prices {
		p := *table
		p.LoadPolicies(data, prev, log)
		return &p
	}

	none := load(map[string]string{}, nil)
	if none.Policy(project) != nil {
		t.Fatal("a policy without a document")
	}
	v3 := load(map[string]string{PolicyKey(project): doc(t, "3")}, none)
	if pol := v3.Policy(strings.ToUpper(project)); pol == nil || pol.Version != 3 {
		t.Fatalf("v3 not loaded: %+v", pol)
	}

	for name, d := range map[string]string{
		"invalid":         strings.Replace(doc(t, "4"), `"type": "deny"`, `"type": "redirect"`, 1),
		"unknown schema":  strings.Replace(doc(t, "4"), "radius.policy/0.1", "radius.policy/0.9", 1),
		"older":           doc(t, "2"),
		"another project": strings.Replace(doc(t, "4"), project, "00000000-0000-4000-8000-0000000000bb", 1),
	} {
		if pol := load(map[string]string{PolicyKey(project): d}, v3).Policy(project); pol == nil || pol.Version != 3 {
			t.Errorf("%s: the policy in force was replaced or dropped: %+v", name, pol)
		}
	}
	if pol := load(map[string]string{}, v3).Policy(project); pol == nil || pol.Version != 3 {
		t.Error("a document missing from the ConfigMap dropped the policy in force")
	}
	if pol := load(map[string]string{PolicyKey(project): doc(t, "4")}, v3).Policy(project); pol == nil || pol.Version != 4 {
		t.Error("v4 not loaded")
	}

	// A project no route names any more is not carried over.
	empty, _ := Parse([]byte(`{"schemaVersion":1,"routes":{}}`))
	p := *empty
	p.LoadPolicies(map[string]string{PolicyKey(project): doc(t, "3")}, v3, log)
	if p.Policy(project) != nil {
		t.Error("a policy for a project no route names")
	}
}
