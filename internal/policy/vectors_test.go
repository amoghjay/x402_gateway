package policy

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The conformance vectors: every gate that enforces a compiled policy must make these exact
// decisions. Each file is one policy and the cases run against it; the format is described in
// testdata/vectors/SOURCE.md.

type vectorFile struct {
	Description string          `json:"description"`
	Policy      json.RawMessage `json:"policy"`
	Cases       []vectorCase    `json:"cases"`
}

type vectorCase struct {
	Name    string `json:"name"`
	Request struct {
		Method   string  `json:"method"`
		Path     string  `json:"path"`
		ClientIP *string `json:"client_ip"`
		Caller   *struct {
			ID string `json:"id"`
		} `json:"caller"`
		Agent *struct {
			Category       string `json:"category"`
			SignatureAgent string `json:"signature_agent"`
		} `json:"agent"`
		Now uint64 `json:"now"`
	} `json:"request"`
	Transport *struct {
		RemoteAddr *string           `json:"remote_addr"`
		Headers    map[string]string `json:"headers"`
	} `json:"transport"`
	NoPolicy     string   `json:"no_policy"`
	Counts       []uint64 `json:"counts"`
	CounterError bool     `json:"counter_error"`
	Expected     struct {
		Counted  []any `json:"counted"`
		Decision any   `json:"decision"`
	} `json:"expected"`
}

func TestConformanceVectors(t *testing.T) {
	files, err := filepath.Glob("testdata/vectors/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no vector files: %v", err)
	}
	sort.Strings(files)
	total := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var vf vectorFile
		if err := json.Unmarshal(data, &vf); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		var p *Policy
		if string(vf.Policy) != "null" && len(vf.Policy) > 0 {
			if p, err = Parse(vf.Policy); err != nil {
				t.Fatalf("%s: the policy is refused: %v", file, err)
			}
		}
		for _, c := range vf.Cases {
			total++
			t.Run(filepath.Base(file)+"/"+c.Name, func(t *testing.T) { runCase(t, p, c) })
		}
	}
	// A guard against the vectors silently not loading. The reference runners assert 25.
	if total < 70 {
		t.Fatalf("ran %d cases, expected the full set (73 at the pinned commit)", total)
	}
}

func runCase(t *testing.T, p *Policy, c vectorCase) {
	t.Helper()
	if p == nil {
		assertJSON(t, "decision", decisionJSON(NoPolicy(c.NoPolicy)), c.Expected.Decision)
		return
	}
	r := Request{Method: c.Request.Method, Path: c.Request.Path, Now: c.Request.Now}
	if c.Request.ClientIP != nil {
		ip, err := netip.ParseAddr(*c.Request.ClientIP)
		if err != nil {
			t.Fatalf("client_ip %q: %v", *c.Request.ClientIP, err)
		}
		r.ClientIP = ip
	}
	if c.Request.Caller != nil {
		r.Caller = c.Request.Caller.ID
	}
	if c.Request.Agent != nil {
		r.Agent = &Agent{Category: c.Request.Agent.Category, SignatureAgent: c.Request.Agent.SignatureAgent}
	}
	if tr := c.Transport; tr != nil {
		var remote netip.Addr
		if tr.RemoteAddr != nil {
			remote = netip.MustParseAddr(*tr.RemoteAddr)
		}
		r.ClientIP = ResolveClientIP(p, func(name string) (string, bool) {
			v, ok := tr.Headers[name]
			return v, ok
		}, remote)
	}

	done, count := Select(p, r)
	if done != nil {
		if c.Expected.Counted != nil {
			t.Fatalf("decided without counting, but the case expects counters: %v", c.Expected.Counted)
		}
		assertJSON(t, "decision", decisionJSON(*done), c.Expected.Decision)
		return
	}

	counted := make([]any, 0, len(count.Counters))
	for _, k := range count.Counters {
		counted = append(counted, map[string]any{
			"rule": count.Rule.ID, "limit": k.Limit, "key": k.Key, "reset_s": float64(k.ResetS),
		})
	}
	assertJSON(t, "counted", counted, c.Expected.Counted)

	readings := make([]Reading, len(count.Counters))
	switch {
	case c.CounterError:
		if c.Counts != nil {
			t.Fatal("a case has both counts and counter_error")
		}
		for i := range readings {
			readings[i].Failed = true
		}
	default:
		if len(c.Counts) != len(count.Counters) {
			t.Fatalf("the case gives %d counts for %d counters", len(c.Counts), len(count.Counters))
		}
		for i, n := range c.Counts {
			readings[i].Count = n
		}
	}
	assertJSON(t, "decision", decisionJSON(Decide(p, count, readings)), c.Expected.Decision)
}

// decisionJSON is a decision in the shape the vectors assert: `rule` is null for no rule,
// `headers` is absent when there are none, and `accepts` is the action's options as written.
func decisionJSON(d Decision) any {
	out := map[string]any{"rule": nil, "type": d.Type}
	if d.Rule != "" {
		out["rule"] = d.Rule
	}
	switch d.Type {
	case TypeDeny:
		out["status"], out["code"] = float64(d.Status), d.Code
	case TypePaymentRequired:
		out["price"], out["description"] = d.Price, d.Description
		accepts := make([]any, 0, len(d.Accepts))
		for _, o := range d.Accepts {
			var v any
			if err := json.Unmarshal(o.Raw, &v); err != nil {
				panic(err)
			}
			accepts = append(accepts, v)
		}
		out["accepts"] = accepts
	}
	if len(d.Headers) > 0 {
		h := make(map[string]any, len(d.Headers))
		for k, v := range d.Headers {
			h[k] = v
		}
		out["headers"] = h
	}
	return out
}

func assertJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("%s differs\n got: %s\nwant: %s", what, g, w)
	}
}

// The vectors hold only valid policies, so the refusals are tested here: a document this gate
// cannot enforce in full must not be loaded at all.
func TestParseRefuses(t *testing.T) {
	const base = `{"schema":"radius.policy/0.1","version":3,"project_id":"p","generated_at":"t","mode":"test",
"defaults":{"unmatched":"allow","failure":{"no_policy":"allow","counter_error":"within"},"client_ip":"remote_addr","headers":{"ratelimit":true,"retry_after":true}},
"rules":[RULE]}`
	const option = `{"scheme":"exact","network":"eip155:72344","asset":"0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb","decimals":6,"amount":"1000","pay_to":"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed","facilitator":"https://facilitator.example"}`
	rule := func(body string) string {
		return strings.Replace(base, "RULE", `{"uid":"00000000-0000-4000-8000-000000000001","id":"r",`+body+`}`, 1)
	}
	charge := func(opt string) string {
		return rule(`"match":{},"action":{"type":"charge","price":"0.001","description":"d","accepts":[` + opt + `]}`)
	}
	if _, err := Parse([]byte(charge(option))); err != nil {
		t.Fatalf("the valid document is refused: %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(base, "RULE", "", 1))); err != nil {
		t.Fatalf("a policy with no rules is refused: %v", err)
	}

	cases := map[string]string{
		"not JSON":                `{`,
		"unknown schema":          strings.Replace(charge(option), "radius.policy/0.1", "radius.policy/0.3", 1),
		"version 0":               strings.Replace(charge(option), `"version":3`, `"version":0`, 1),
		"unknown unmatched":       strings.Replace(charge(option), `"unmatched":"allow"`, `"unmatched":"maybe"`, 1),
		"unknown counter_error":   strings.Replace(charge(option), `"counter_error":"within"`, `"counter_error":"sometimes"`, 1),
		"unknown client_ip":       strings.Replace(charge(option), `"client_ip":"remote_addr"`, `"client_ip":"peer"`, 1),
		"unknown action":          rule(`"match":{},"action":{"type":"redirect"}`),
		"deny without a code":     rule(`"match":{},"action":{"type":"deny","status":403}`),
		"deny with a 2xx":         rule(`"match":{},"action":{"type":"deny","status":200,"code":"c"}`),
		"unknown limit key":       rule(`"match":{},"rate_limit":{"key":"session","quota":1,"window_s":60},"action":{"type":"allow"}`),
		"quota 0":                 rule(`"match":{},"free_quota":{"key":"ip","quota":0,"window_s":60},"action":{"type":"allow"}`),
		"unknown method":          rule(`"match":{"endpoint":{"slug":"s","methods":["TRACE"],"path":"/a"}},"action":{"type":"allow"}`),
		"path without a slash":    rule(`"match":{"endpoint":{"slug":"s","methods":[],"path":"a"}},"action":{"type":"allow"}`),
		"wildcard in the middle":  rule(`"match":{"endpoint":{"slug":"s","methods":[],"path":"/a/*/b"}},"action":{"type":"allow"}`),
		"bad range":               rule(`"match":{"ip_in":["10.0.0.0/40"]},"action":{"type":"allow"}`),
		"unknown agent category":  rule(`"match":{"agent":{"category":["robot_vacuum"]}},"action":{"type":"allow"}`),
		"empty agent match":       rule(`"match":{"agent":{}},"action":{"type":"allow"}`),
		"bad uid":                 strings.Replace(charge(option), "00000000-0000-4000-8000-000000000001", "one", 1),
		"charge with no options":  rule(`"match":{},"action":{"type":"charge","price":"0.001","description":"d","accepts":[]}`),
		"unknown scheme":          charge(strings.Replace(option, `"exact"`, `"upto"`, 1)),
		"null facilitator":        charge(strings.Replace(option, `"https://facilitator.example"`, `null`, 1)),
		"facilitator over http":   charge(strings.Replace(option, `https://facilitator.example`, `http://facilitator.example`, 1)),
		"facilitator with a user": charge(strings.Replace(option, `https://facilitator.example`, `https://u:p@facilitator.example`, 1)),
		"amount 0":                charge(strings.Replace(option, `"1000"`, `"0"`, 1)),
		"amount not a number":     charge(strings.Replace(option, `"1000"`, `"1e3"`, 1)),
		"short pay_to":            charge(strings.Replace(option, `0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed`, `0x5aAe`, 1)),
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: the document was loaded", name)
		}
	}
}

// A field this gate does not know must not stop a document from loading, and an option's own
// bytes must survive, so a later addition to `extra` reaches the buyer untouched.
func TestParseKeepsWhatItDoesNotKnow(t *testing.T) {
	data, err := os.ReadFile("testdata/three_options.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	done, count := Select(p, Request{Method: "get", Path: "/anything/platform/paid/42?x=1", Now: 1})
	if count != nil || done == nil || done.Type != TypePaymentRequired {
		t.Fatalf("decision = %+v, count = %+v", done, count)
	}
	if len(done.Accepts) != 3 {
		t.Fatalf("offered %d options, want all 3 in order", len(done.Accepts))
	}
	if got := done.Accepts[2].Extra["assetTransferMethod"]; got != "permit2" {
		t.Errorf("third option's transfer method = %v, want permit2", got)
	}
	if !strings.Contains(string(done.Accepts[0].Raw), `"futureField"`) {
		t.Errorf("an unknown field of an option was dropped: %s", done.Accepts[0].Raw)
	}
	out, err := json.Marshal(done.Accepts[0])
	if err != nil || !strings.Contains(string(out), `"futureField"`) {
		t.Errorf("an option does not marshal as written: %s, %v", out, err)
	}
}

// The path rules a port most easily gets wrong, beyond what the vectors pin.
func TestPathMatches(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"/", "/", true},
		{"/", "//", true}, // one trailing slash is stripped, leaving the root
		{"/", "/a", false},
		{"/a", "/a/", true},
		{"/a", "/a//", false}, // only one trailing slash is stripped
		{"/a", "/A", false},   // literals are case-sensitive
		{"/a/:id", "/a/1", true},
		{"/a/:id", "/a/", false},
		{"/a/:id", "/a//b", false}, // an empty segment is not a parameter
		{"/a/:id", "/a/1/2", false},
		{"/a/*", "/a", true}, // * matches nothing too
		{"/a/*", "/a/b/c", true},
		{"/a/*", "/ab", false},
		{"/a b", "/a%20b", false}, // no percent-decoding
		{"/a", "/a?x=/b", true},
		{"/a", "/a#frag", true},
		{"/a", "a", false},
	}
	for _, c := range cases {
		segs, err := parsePattern(c.pattern)
		if err != nil {
			t.Fatalf("pattern %q: %v", c.pattern, err)
		}
		if got := pathMatches(segs, c.path); got != c.want {
			t.Errorf("pattern %q, path %q: got %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestParseIP(t *testing.T) {
	for text, want := range map[string]string{
		"203.0.113.5": "203.0.113.5", " 203.0.113.5:4711 ": "203.0.113.5", "2001:db8::1": "2001:db8::1",
		"[2001:db8::1]": "2001:db8::1", "[2001:db8::1]:443": "2001:db8::1",
	} {
		if got := ParseIP(text); got != netip.MustParseAddr(want) {
			t.Errorf("%q: got %v, want %s", text, got, want)
		}
	}
	for _, text := range []string{"", "unknown", "[203.0.113.5]", "2001:db8::1]", "203.0.113.5:port", "010.0.0.1", "fe80::1%eth0"} {
		if got := ParseIP(text); got.IsValid() {
			t.Errorf("%q: got %v, want no address", text, got)
		}
	}
}

func TestCounterKeyForIPv6(t *testing.T) {
	for ip, want := range map[string]string{
		"2001:db8:0:0:1::1":    "ip:2001:db8::/64",
		"::1":                  "ip:::/64",
		"::ffff:203.0.113.5":   "ip:203.0.113.5",
		"2001:db8:a:b:c:d:e:f": "ip:2001:db8:a:b::/64",
	} {
		if got := ipPart(Request{ClientIP: netip.MustParseAddr(ip)}); got != want {
			t.Errorf("%s: key part %q, want %q", ip, got, want)
		}
	}
	if got := ipPart(Request{}); got != "ip:unknown" {
		t.Errorf("no address: key part %q", got)
	}
}
