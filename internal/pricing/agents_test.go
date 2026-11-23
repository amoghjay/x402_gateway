package pricing

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/radius"
)

const agentsProject = "00000000-0000-4000-8000-0000000000aa"

// agentsTable loads a table whose one platform route's policy names origins (none when
// empty), with data[AgentsKey] = agents when agents is not "".
func agentsTable(t *testing.T, agents string, prev *Prices, origins ...string) *Prices {
	t.Helper()
	b, err := os.ReadFile("../policy/testdata/three_options.json")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	if len(origins) > 0 {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		m["schema"] = "radius.policy/0.2"
		list := make([]any, len(origins))
		for i, o := range origins {
			list[i] = o
		}
		m["rules"].([]any)[1].(map[string]any)["match"].(map[string]any)["agent"] = map[string]any{"signature_agent": list}
		out, _ := json.Marshal(m)
		doc = string(out)
	}
	p, err := Parse([]byte(`{"schemaVersion":1,"routes":{},"platform":{"httproute/shop/r":{"project":"`+agentsProject+`","noPolicy":"deny"}}}`), radius.Testnet)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]string{PolicyKey(agentsProject): doc}
	if agents != "" {
		data[AgentsKey] = agents
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p.LoadPolicies(data, prev, log)
	p.LoadAgents(data, prev, log)
	return p
}

const oneKey = `{"keys":[{"thumbprint":"t","x":"JrQLj5P_89iXES9-vFgrIy29clF9CC_oPPsw3c5D0bs"}]}`

func TestDirectoriesOnlyForNamedOrigins(t *testing.T) {
	p := agentsTable(t, `{"https://a.example":`+oneKey+`,"https://b.example":`+oneKey+`}`, nil, "https://A.example/")
	dirs := p.Directories(agentsProject)
	if dirs == nil {
		t.Fatal("no directories for a policy naming an origin")
	}
	if d, ok := dirs("https://a.example"); !ok || len(d.Keys) != 1 {
		t.Errorf("named origin: %+v %v", d, ok)
	}
	if _, ok := dirs("https://b.example"); ok {
		t.Error("keys for an origin the policy does not name")
	}
	if agentsTable(t, `{"https://a.example":`+oneKey+`}`, nil).Directories(agentsProject) != nil {
		t.Error("directories for a policy naming no origin")
	}
	if (*Prices)(nil).Directories(agentsProject) != nil {
		t.Error("nil table")
	}
}

func TestLoadAgentsKeepsTheLastGood(t *testing.T) {
	good := agentsTable(t, `{"https://a.example":`+oneKey+`}`, nil, "https://a.example")
	bad := agentsTable(t, `{not json`, good, "https://a.example")
	if _, ok := bad.Directories(agentsProject)("https://a.example"); !ok {
		t.Error("an unreadable value dropped the keys in force")
	}
	none := agentsTable(t, "", good, "https://a.example")
	if _, ok := none.Directories(agentsProject)("https://a.example"); ok {
		t.Error("keys kept after the controller removed them")
	}
}
