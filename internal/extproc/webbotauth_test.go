package extproc

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
)

// The RFC 9421 B.1.4 test key.
const (
	wbaSeed  = "n4Ni-HpISpVObnQMW0wOhCKROaIKqKtW_2ZYb2p9KcU"
	wbaX     = "JrQLj5P_89iXES9-vFgrIy29clF9CC_oPPsw3c5D0bs"
	wbaThumb = "poqkLGiymh_W0uP6PZFw-dvez3QJT5SolqXBCW38r0U"
	wbaAgent = "https://agent.example"
)

// agentStore loads the test document with one more rule in front: a verified signature
// agent from wbaAgent may GET /anything/platform/blocked, which everyone else is denied.
// The table holds keys for wbaAgent and for an origin the policy does not name.
func agentStore(t *testing.T) *pricing.Store {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(compiledDoc(t)), &doc); err != nil {
		t.Fatal(err)
	}
	doc["schema"] = "radius.policy/0.2"
	rules := doc["rules"].([]any)
	blocked := rules[0].(map[string]any) // get-anything-platform-blocked/deny
	agentRule := map[string]any{
		"id":     "get-anything-platform-blocked/agents",
		"uid":    "00000000-0000-4000-8000-000000000099",
		"action": map[string]any{"type": "allow"},
		"match": map[string]any{
			"endpoint": blocked["match"].(map[string]any)["endpoint"],
			"agent":    map[string]any{"signature_agent": []any{wbaAgent}},
		},
	}
	doc["rules"] = append([]any{agentRule}, rules...)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p, err := pricing.Parse([]byte(`{"schemaVersion":1,"routes":{},"platform":{"httproute/shop/metered":{"project":"`+platformProject+`","noPolicy":"deny"}}}`), radius.Testnet)
	if err != nil {
		t.Fatal(err)
	}
	key := `{"keys":[{"kid":"` + wbaThumb + `","thumbprint":"` + wbaThumb + `","x":"` + wbaX + `"}]}`
	data := map[string]string{
		pricing.PolicyKey(platformProject): string(b),
		pricing.AgentsKey:                  `{"` + wbaAgent + `":` + key + `,"https://unnamed.example":` + key + `}`,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p.LoadPolicies(data, nil, log)
	p.LoadAgents(data, nil, log)
	s := pricing.NewStore()
	s.Set(p)
	return s
}

// signedHeaders signs @authority and signature-agent for the test requests' authority, at
// the platform test's clock, naming agent.
func signedHeaders(t *testing.T, key ed25519.PrivateKey, agent string) []string {
	t.Helper()
	params := `("@authority" "signature-agent");created=1760000000;expires=1760000060;keyid="` + wbaThumb + `";tag="web-bot-auth"`
	base := `"@authority": 34.173.224.114` + "\n" +
		`"signature-agent": "` + agent + `"` + "\n" +
		`"@signature-params": ` + params
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(base)))
	return []string{"signature-agent", `"` + agent + `"`, "signature-input", "sig1=" + params, "signature", "sig1=:" + sig + ":"}
}

func testSigningKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	seed, err := base64.RawURLEncoding.DecodeString(wbaSeed)
	if err != nil {
		t.Fatal(err)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func TestPlatformSignatureAgent(t *testing.T) {
	pt := newPlatformTest(t, agentStore(t))
	key := testSigningKey(t)
	const path = "/anything/platform/blocked"

	// Unsigned: the agent rule does not match, the deny rule does.
	denied(t, pt.request(pt.conn(), "GET", path, "203.0.113.5:4711"), 403, "not_for_sale")

	// Signed by the agent the rule names: let in.
	continued(t, pt.request(pt.conn(), "GET", path, "203.0.113.5:4711", signedHeaders(t, key, wbaAgent)...))
	if !strings.Contains(pt.logs.String(), `"signatureAgent":"`+wbaAgent+`"`) {
		t.Errorf("the verified agent is not logged: %s", pt.logs.String())
	}

	// The table has keys for an origin this policy does not name: not verified for it.
	denied(t, pt.request(pt.conn(), "GET", path, "203.0.113.5:4711", signedHeaders(t, key, "https://unnamed.example")...), 403, "not_for_sale")

	// Another key claiming the named agent: not verified, nothing refused for it either.
	other := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	denied(t, pt.request(pt.conn(), "GET", path, "203.0.113.5:4711", signedHeaders(t, other, wbaAgent)...), 403, "not_for_sale")

	// A signed agent on a path its rule does not cover gets that path's rules as usual.
	continued(t, pt.request(pt.conn(), "GET", "/anything/platform/free", "203.0.113.5:4711", signedHeaders(t, key, wbaAgent)...))
}

// A field sent on two lines is joined, as HTTP and RFC 9421 read it: a second, unverifiable
// signature on its own Signature-Input and Signature lines does not hide the good one.
func TestPlatformSignatureOnSeveralLines(t *testing.T) {
	pt := newPlatformTest(t, agentStore(t))
	h := signedHeaders(t, testSigningKey(t), wbaAgent)
	h = append(h,
		"signature-input", `zz=("@authority");created=1760000000;expires=1760000060;keyid="nope"`,
		"signature", "zz=:"+base64.StdEncoding.EncodeToString(make([]byte, 64))+":")
	continued(t, pt.request(pt.conn(), "GET", "/anything/platform/blocked", "203.0.113.5:4711", h...))
}
